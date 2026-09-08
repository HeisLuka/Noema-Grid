// Package llmextract adapts Tela's existing chat-completion service to Noema's
// semantic.Extractor contract. It is deliberately outside package semantic so
// the canonical data model, identity rules, and repositories never depend on an
// LLM implementation.
package llmextract

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zcag/tela/backend/internal/semantic"
)

const maxSemanticSourceBytes = 128 << 10 // 128 KiB for the first non-chunked extractor

var (
	ErrInvalidOutput  = errors.New("semantic llm extractor: invalid output")
	ErrSourceTooLarge = errors.New("semantic llm extractor: source too large")
)

// Completer is the narrow subset of llm.Service used by semantic extraction.
// Keeping this interface local makes the adapter unit-testable without an HTTP
// model backend while *llm.Service satisfies it directly.
type Completer interface {
	Complete(context.Context, string, string) (string, error)
}

type Extractor struct {
	llm Completer
}

func New(llm Completer) *Extractor { return &Extractor{llm: llm} }

func (e *Extractor) Extract(ctx context.Context, in semantic.ExtractionInput) (semantic.CandidateSet, error) {
	if e == nil || e.llm == nil {
		return semantic.CandidateSet{}, errors.New("semantic llm extractor: nil completer")
	}
	if !in.Profile.Valid() {
		return semantic.CandidateSet{}, fmt.Errorf("%w: unsupported profile %q", ErrInvalidOutput, in.Profile)
	}
	if len(in.Snapshot.Title)+len(in.Snapshot.Body) > maxSemanticSourceBytes {
		return semantic.CandidateSet{}, ErrSourceTooLarge
	}

	userPrompt, err := sourcePrompt(in.Snapshot, in.Profile)
	if err != nil {
		return semantic.CandidateSet{}, fmt.Errorf("semantic llm extractor: source prompt: %w", err)
	}
	out, err := e.llm.Complete(ctx, systemPrompt(in.Profile), userPrompt)
	if err != nil {
		return semantic.CandidateSet{}, err
	}

	candidates, err := decodeCandidateSet(out)
	if err != nil {
		return semantic.CandidateSet{}, err
	}
	if err := semantic.ValidateCandidateSet(candidates); err != nil {
		return semantic.CandidateSet{}, fmt.Errorf("%w: %v", ErrInvalidOutput, err)
	}
	if err := validateSourceGrounding(in.Snapshot, &candidates); err != nil {
		return semantic.CandidateSet{}, err
	}
	if err := validateExtractionVocabulary(candidates); err != nil {
		return semantic.CandidateSet{}, err
	}
	return candidates, nil
}

func decodeCandidateSet(raw string) (semantic.CandidateSet, error) {
	payload := strings.TrimSpace(raw)
	if strings.HasPrefix(payload, "```") {
		lines := strings.Split(payload, "\n")
		if len(lines) < 3 || !strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "```") {
			return semantic.CandidateSet{}, fmt.Errorf("%w: unterminated markdown fence", ErrInvalidOutput)
		}
		payload = strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
	}
	if payload == "" {
		return semantic.CandidateSet{}, fmt.Errorf("%w: empty completion", ErrInvalidOutput)
	}

	dec := json.NewDecoder(strings.NewReader(payload))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var out semantic.CandidateSet
	if err := dec.Decode(&out); err != nil {
		return semantic.CandidateSet{}, fmt.Errorf("%w: decode: %v", ErrInvalidOutput, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, context.Canceled) && err == nil {
		return semantic.CandidateSet{}, fmt.Errorf("%w: multiple JSON values", ErrInvalidOutput)
	} else if err != nil && !errors.Is(err, errors.New("EOF")) {
		// json.Decoder returns io.EOF for a clean end. Avoid accepting arbitrary
		// prose after the object simply because the first Decode succeeded.
		if !strings.Contains(err.Error(), "EOF") {
			return semantic.CandidateSet{}, fmt.Errorf("%w: trailing data: %v", ErrInvalidOutput, err)
		}
	}
	return out, nil
}

func validateSourceGrounding(snapshot semantic.PageSnapshot, candidates *semantic.CandidateSet) error {
	regions := make(map[string]string, len(candidates.Regions))
	for i := range candidates.Regions {
		region := &candidates.Regions[i]
		region.Excerpt = strings.TrimSpace(region.Excerpt)
		if region.Excerpt == "" {
			return fmt.Errorf("%w: region %q has empty excerpt", ErrInvalidOutput, region.Key)
		}
		if !strings.Contains(snapshot.Body, region.Excerpt) && !strings.Contains(snapshot.Title, region.Excerpt) {
			return fmt.Errorf("%w: region %q excerpt is not an exact source span", ErrInvalidOutput, region.Key)
		}
		sum := sha256.Sum256([]byte(region.Excerpt))
		region.ExcerptHash = hex.EncodeToString(sum[:])
		regions[region.Key] = region.Excerpt
	}

	for _, claim := range candidates.Claims {
		if len(claim.Instances) == 0 {
			return fmt.Errorf("%w: claim %q has no evidence instance", ErrInvalidOutput, claim.Key)
		}
		for _, instance := range claim.Instances {
			excerpt, ok := regions[instance.RegionKey]
			if !ok {
				return fmt.Errorf("%w: claim %q references missing region %q", ErrInvalidOutput, claim.Key, instance.RegionKey)
			}
			text := strings.TrimSpace(instance.OriginalText)
			if text == "" || !strings.Contains(excerpt, text) {
				return fmt.Errorf("%w: claim %q instance text is not an exact subspan of region %q", ErrInvalidOutput, claim.Key, instance.RegionKey)
			}
		}
	}
	return nil
}

func validateExtractionVocabulary(c semantic.CandidateSet) error {
	entityKinds := map[string]bool{
		"person": true, "organization": true, "place": true, "event": true,
		"concept": true, "object": true, "work": true, "other": true,
	}
	claimTypes := map[string]bool{
		"fact": true, "hypothesis": true, "inference": true,
		"definition": true, "prediction": true,
	}
	for _, e := range c.Entities {
		if !entityKinds[strings.ToLower(strings.TrimSpace(e.Kind))] {
			return fmt.Errorf("%w: entity %q has unsupported kind %q", ErrInvalidOutput, e.Key, e.Kind)
		}
		if strings.TrimSpace(e.CanonicalName) == "" {
			return fmt.Errorf("%w: entity %q has empty canonical_name", ErrInvalidOutput, e.Key)
		}
	}
	for _, claim := range c.Claims {
		if !claimTypes[strings.ToLower(strings.TrimSpace(claim.ClaimType))] {
			return fmt.Errorf("%w: claim %q has unsupported claim_type %q", ErrInvalidOutput, claim.Key, claim.ClaimType)
		}
		if strings.TrimSpace(claim.Predicate) == "" {
			return fmt.Errorf("%w: claim %q has empty predicate", ErrInvalidOutput, claim.Key)
		}
		for _, slot := range claim.Slots {
			if slot.EntityKey == nil && len(bytes.TrimSpace(slot.Literal)) > 0 && strings.TrimSpace(slot.LiteralKind) == "" {
				return fmt.Errorf("%w: claim %q literal slot %q has no literal_kind", ErrInvalidOutput, claim.Key, slot.Role)
			}
		}
	}
	return nil
}
