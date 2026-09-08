package semantic

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var ErrNotFound = errors.New("semantic: not found")

type ExtractionProfile string

const (
	ProfileGenealogy           ExtractionProfile = "genealogy"
	ProfileResearchDense       ExtractionProfile = "research_dense"
	ProfileTechnical           ExtractionProfile = "technical"
	ProfileArgumentativeSparse ExtractionProfile = "argumentative_sparse"
)

func (p ExtractionProfile) Valid() bool {
	switch p {
	case ProfileGenealogy, ProfileResearchDense, ProfileTechnical, ProfileArgumentativeSparse:
		return true
	default:
		return false
	}
}

type PageSnapshot struct {
	SpaceID     int64  `json:"space_id"`
	PageID      int64  `json:"page_id"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	ContentHash string `json:"content_hash"`
}

type CandidateEntity struct {
	Key           string   `json:"key"`
	Kind          string   `json:"kind"`
	CanonicalName string   `json:"canonical_name"`
	Aliases       []string `json:"aliases,omitempty"`
}

type CandidateSlot struct {
	Role        string          `json:"role"`
	Ordinal     int             `json:"ordinal"`
	EntityKey   *string         `json:"entity_key,omitempty"`
	Literal     json.RawMessage `json:"literal,omitempty"`
	LiteralKind string          `json:"literal_kind,omitempty"`
}

type CandidateInstance struct {
	RegionKey             string   `json:"region_key"`
	OriginalText          string   `json:"original_text"`
	ProposedCanonicalText string   `json:"proposed_canonical_text,omitempty"`
	Context               string   `json:"context,omitempty"`
	Stance                string   `json:"stance"`
	ExtractionConfidence  *float64 `json:"extraction_confidence,omitempty"`
}

type CandidateClaim struct {
	Key           string              `json:"key"`
	CanonicalText string              `json:"canonical_text"`
	ClaimType     string              `json:"claim_type"`
	Predicate     string              `json:"predicate,omitempty"`
	ValidFrom     string              `json:"valid_from,omitempty"`
	ValidTo       string              `json:"valid_to,omitempty"`
	TimePrecision string              `json:"time_precision,omitempty"`
	Qualifiers    json.RawMessage     `json:"qualifiers,omitempty"`
	Slots         []CandidateSlot     `json:"slots,omitempty"`
	Instances     []CandidateInstance `json:"instances,omitempty"`
}

type CandidateRegion struct {
	Key         string          `json:"key"`
	LocatorKind string          `json:"locator_kind"`
	Locator     json.RawMessage `json:"locator"`
	Excerpt     string          `json:"excerpt"`
	ExcerptHash string          `json:"excerpt_hash"`
}

type CandidateRelation struct {
	Key          string   `json:"key"`
	FromClaimKey string   `json:"from_claim_key"`
	ToClaimKey   string   `json:"to_claim_key"`
	RelationType string   `json:"relation_type"`
	Reason       string   `json:"reason,omitempty"`
	Confidence   *float64 `json:"confidence,omitempty"`
}

type CandidateGap struct {
	Key              string          `json:"key"`
	Question         string          `json:"question"`
	RelatedClaimKey  string          `json:"related_claim_key,omitempty"`
	RelatedEntityKey string          `json:"related_entity_key,omitempty"`
	Importance       *float64        `json:"importance,omitempty"`
	Context          json.RawMessage `json:"context,omitempty"`
}

type CandidateSet struct {
	Entities  []CandidateEntity   `json:"entities,omitempty"`
	Claims    []CandidateClaim    `json:"claims,omitempty"`
	Regions   []CandidateRegion   `json:"regions,omitempty"`
	Relations []CandidateRelation `json:"relations,omitempty"`
	Gaps      []CandidateGap      `json:"gaps,omitempty"`
	Warnings  []string            `json:"warnings,omitempty"`
}

type ExtractionInput struct {
	Snapshot PageSnapshot
	Profile  ExtractionProfile
}

type Extractor interface {
	Extract(context.Context, ExtractionInput) (CandidateSet, error)
}

type Preview struct {
	SpaceID              int64             `json:"space_id"`
	PageID               int64             `json:"page_id"`
	Profile              ExtractionProfile `json:"profile"`
	SourceContentHash    string            `json:"source_content_hash"`
	Candidates           CandidateSet      `json:"candidates"`
	CandidatePayloadHash string            `json:"candidate_payload_hash"`
	PreviewToken         string            `json:"preview_token"`
	ExpiresAt            string            `json:"expires_at"`
}

type PreviewService struct {
	db        *sql.DB
	extractor Extractor
	signer    *PreviewSigner
}

func NewPreviewService(db *sql.DB, extractor Extractor, signer *PreviewSigner) (*PreviewService, error) {
	if db == nil {
		return nil, errors.New("semantic preview: nil database")
	}
	if extractor == nil {
		return nil, errors.New("semantic preview: nil extractor")
	}
	if signer == nil {
		return nil, errors.New("semantic preview: nil signer")
	}
	return &PreviewService{db: db, extractor: extractor, signer: signer}, nil
}

// PreviewPage is strictly read-only with respect to the semantic core. It reads
// the source page under live Tela access, extracts candidates through an injected
// adapter, hashes the exact source snapshot and normalized candidate payload, and
// signs both into a short-lived token for a later explicit commit.
func (s *PreviewService) PreviewPage(ctx context.Context, userID, spaceID, pageID int64, profile ExtractionProfile) (Preview, error) {
	if s == nil || s.db == nil || s.extractor == nil || s.signer == nil {
		return Preview{}, errors.New("semantic preview: invalid service")
	}
	if pageID == 0 || !profile.Valid() {
		return Preview{}, errors.New("semantic preview: valid page_id and profile are required")
	}
	if err := requireSpaceAccess(ctx, s.db, userID, spaceID, false); err != nil {
		return Preview{}, err
	}

	snapshot, err := readPageSnapshot(ctx, s.db, spaceID, pageID)
	if err != nil {
		return Preview{}, err
	}
	candidates, err := s.extractor.Extract(ctx, ExtractionInput{Snapshot: snapshot, Profile: profile})
	if err != nil {
		return Preview{}, fmt.Errorf("semantic preview: extract: %w", err)
	}
	if err := validateCandidateSet(candidates); err != nil {
		return Preview{}, err
	}
	candidateHash, err := CandidatePayloadHash(candidates)
	if err != nil {
		return Preview{}, err
	}
	token, claims, err := s.signer.Sign(spaceID, pageID, snapshot.ContentHash, profile, candidateHash)
	if err != nil {
		return Preview{}, err
	}
	return Preview{
		SpaceID:              spaceID,
		PageID:               pageID,
		Profile:              profile,
		SourceContentHash:     snapshot.ContentHash,
		Candidates:           candidates,
		CandidatePayloadHash: candidateHash,
		PreviewToken:         token,
		ExpiresAt:            time.Unix(claims.ExpiresAt, 0).UTC().Format(time.RFC3339),
	}, nil
}

func readPageSnapshot(ctx context.Context, db *sql.DB, spaceID, pageID int64) (PageSnapshot, error) {
	var title, body string
	err := db.QueryRowContext(ctx, `
SELECT title, body
FROM pages
WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL`, pageID, spaceID).Scan(&title, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return PageSnapshot{}, ErrNotFound
	}
	if err != nil {
		return PageSnapshot{}, fmt.Errorf("semantic preview: read page: %w", err)
	}
	return PageSnapshot{
		SpaceID:     spaceID,
		PageID:      pageID,
		Title:       title,
		Body:        body,
		ContentHash: PageSnapshotHash(title, body),
	}, nil
}

func PageSnapshotHash(title, body string) string {
	h := sha256.New()
	_, _ = h.Write([]byte("noema-page-v1\x00"))
	_, _ = h.Write([]byte(title))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil))
}

func CandidatePayloadHash(in CandidateSet) (string, error) {
	normalized, err := normalizeCandidateSet(in)
	if err != nil {
		return "", fmt.Errorf("semantic preview: normalize candidates: %w", err)
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("semantic preview: marshal candidates: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func validateCandidateSet(in CandidateSet) error {
	seen := make(map[string]string)
	add := func(kind, key string) error {
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("semantic preview: %s candidate has empty key", kind)
		}
		if prev, ok := seen[key]; ok {
			return fmt.Errorf("semantic preview: candidate key %q reused by %s and %s", key, prev, kind)
		}
		seen[key] = kind
		return nil
	}

	for _, x := range in.Entities {
		if err := add("entity", x.Key); err != nil {
			return err
		}
	}
	for _, x := range in.Claims {
		if err := add("claim", x.Key); err != nil {
			return err
		}
	}
	for _, x := range in.Regions {
		if err := add("region", x.Key); err != nil {
			return err
		}
	}
	for _, x := range in.Relations {
		if err := add("relation", x.Key); err != nil {
			return err
		}
	}
	for _, x := range in.Gaps {
		if err := add("gap", x.Key); err != nil {
			return err
		}
	}

	is := func(key, kind string) bool { return seen[strings.TrimSpace(key)] == kind }
	for _, x := range in.Claims {
		if strings.TrimSpace(x.CanonicalText) == "" {
			return fmt.Errorf("semantic preview: claim %q has empty canonical_text", x.Key)
		}
		for _, slot := range x.Slots {
			if strings.TrimSpace(slot.Role) == "" || slot.Ordinal < 0 {
				return fmt.Errorf("semantic preview: claim %q has invalid slot", x.Key)
			}
			if (slot.EntityKey == nil) == (len(slot.Literal) == 0) {
				return fmt.Errorf("semantic preview: claim %q slot %q must have exactly one entity_key or literal", x.Key, slot.Role)
			}
			if slot.EntityKey != nil && !is(*slot.EntityKey, "entity") {
				return fmt.Errorf("semantic preview: claim %q slot %q references unknown entity %q", x.Key, slot.Role, *slot.EntityKey)
			}
		}
		for _, instance := range x.Instances {
			if !is(instance.RegionKey, "region") {
				return fmt.Errorf("semantic preview: claim %q instance references unknown region %q", x.Key, instance.RegionKey)
			}
			stance := strings.TrimSpace(strings.ToLower(instance.Stance))
			if stance != "affirms" && stance != "denies" {
				return fmt.Errorf("semantic preview: claim %q instance has unsupported stance %q", x.Key, instance.Stance)
			}
		}
	}
	for _, x := range in.Relations {
		if !is(x.FromClaimKey, "claim") || !is(x.ToClaimKey, "claim") {
			return fmt.Errorf("semantic preview: relation %q references unknown claim", x.Key)
		}
		if strings.TrimSpace(x.FromClaimKey) == strings.TrimSpace(x.ToClaimKey) {
			return fmt.Errorf("semantic preview: relation %q is a self relation", x.Key)
		}
		if !validRelationType(strings.TrimSpace(strings.ToLower(x.RelationType))) {
			return fmt.Errorf("semantic preview: relation %q has unsupported type %q", x.Key, x.RelationType)
		}
	}
	for _, x := range in.Gaps {
		if strings.TrimSpace(x.Question) == "" {
			return fmt.Errorf("semantic preview: gap %q has empty question", x.Key)
		}
		if x.RelatedClaimKey != "" && !is(x.RelatedClaimKey, "claim") {
			return fmt.Errorf("semantic preview: gap %q references unknown claim %q", x.Key, x.RelatedClaimKey)
		}
		if x.RelatedEntityKey != "" && !is(x.RelatedEntityKey, "entity") {
			return fmt.Errorf("semantic preview: gap %q references unknown entity %q", x.Key, x.RelatedEntityKey)
		}
	}
	return nil
}

func normalizeCandidateSet(in CandidateSet) (CandidateSet, error) {
	out := in
	out.Entities = append([]CandidateEntity(nil), in.Entities...)
	for i := range out.Entities {
		out.Entities[i].Aliases = append([]string(nil), out.Entities[i].Aliases...)
		sort.Strings(out.Entities[i].Aliases)
	}
	sort.Slice(out.Entities, func(i, j int) bool { return out.Entities[i].Key < out.Entities[j].Key })

	out.Claims = append([]CandidateClaim(nil), in.Claims...)
	for i := range out.Claims {
		var err error
		out.Claims[i].Qualifiers, err = canonicalJSON(out.Claims[i].Qualifiers)
		if err != nil {
			return CandidateSet{}, fmt.Errorf("claim %q qualifiers: %w", out.Claims[i].Key, err)
		}
		out.Claims[i].Slots = append([]CandidateSlot(nil), out.Claims[i].Slots...)
		for j := range out.Claims[i].Slots {
			out.Claims[i].Slots[j].Literal, err = canonicalJSON(out.Claims[i].Slots[j].Literal)
			if err != nil {
				return CandidateSet{}, fmt.Errorf("claim %q slot %q literal: %w", out.Claims[i].Key, out.Claims[i].Slots[j].Role, err)
			}
		}
		sort.Slice(out.Claims[i].Slots, func(a, b int) bool {
			if out.Claims[i].Slots[a].Role == out.Claims[i].Slots[b].Role {
				return out.Claims[i].Slots[a].Ordinal < out.Claims[i].Slots[b].Ordinal
			}
			return out.Claims[i].Slots[a].Role < out.Claims[i].Slots[b].Role
		})
		out.Claims[i].Instances = append([]CandidateInstance(nil), out.Claims[i].Instances...)
		sort.Slice(out.Claims[i].Instances, func(a, b int) bool {
			if out.Claims[i].Instances[a].RegionKey == out.Claims[i].Instances[b].RegionKey {
				return out.Claims[i].Instances[a].Stance < out.Claims[i].Instances[b].Stance
			}
			return out.Claims[i].Instances[a].RegionKey < out.Claims[i].Instances[b].RegionKey
		})
	}
	sort.Slice(out.Claims, func(i, j int) bool { return out.Claims[i].Key < out.Claims[j].Key })

	out.Regions = append([]CandidateRegion(nil), in.Regions...)
	for i := range out.Regions {
		var err error
		out.Regions[i].Locator, err = canonicalJSON(out.Regions[i].Locator)
		if err != nil {
			return CandidateSet{}, fmt.Errorf("region %q locator: %w", out.Regions[i].Key, err)
		}
	}
	sort.Slice(out.Regions, func(i, j int) bool { return out.Regions[i].Key < out.Regions[j].Key })

	out.Relations = append([]CandidateRelation(nil), in.Relations...)
	sort.Slice(out.Relations, func(i, j int) bool { return out.Relations[i].Key < out.Relations[j].Key })

	out.Gaps = append([]CandidateGap(nil), in.Gaps...)
	for i := range out.Gaps {
		var err error
		out.Gaps[i].Context, err = canonicalJSON(out.Gaps[i].Context)
		if err != nil {
			return CandidateSet{}, fmt.Errorf("gap %q context: %w", out.Gaps[i].Key, err)
		}
	}
	sort.Slice(out.Gaps, func(i, j int) bool { return out.Gaps[i].Key < out.Gaps[j].Key })

	out.Warnings = append([]string(nil), in.Warnings...)
	sort.Strings(out.Warnings)
	return out, nil
}

func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if !json.Valid(raw) {
		return nil, errors.New("invalid json")
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}
