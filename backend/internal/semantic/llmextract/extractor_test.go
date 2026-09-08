package llmextract

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zcag/tela/backend/internal/semantic"
)

type fakeCompleter struct {
	out        string
	err        error
	calls      int
	systemSeen string
	userSeen   string
}

func (f *fakeCompleter) Complete(_ context.Context, systemPrompt, userPrompt string) (string, error) {
	f.calls++
	f.systemSeen = systemPrompt
	f.userSeen = userPrompt
	return f.out, f.err
}

func TestExtractorGenealogyGroundsFedorCandidates(t *testing.T) {
	body := "Освобождён в 1945 году, домой Фёдор Воронов вернулся в 1948 году."
	fake := &fakeCompleter{out: "```json\n" + `{
  "entities":[{"key":"e:1","kind":"person","canonical_name":"Фёдор Воронов","aliases":["Федор Воронов"]}],
  "regions":[{"key":"r:1","locator_kind":"text_quote","locator":{"occurrence":1},"excerpt":"домой Фёдор Воронов вернулся в 1948 году.","excerpt_hash":"model-must-not-control-this"}],
  "claims":[{
    "key":"c:1",
    "canonical_text":"Фёдор Воронов вернулся домой в 1948 году",
    "claim_type":"fact",
    "predicate":"person.returned_home",
    "valid_from":"1948-01-01",
    "valid_to":"1948-12-31",
    "time_precision":"year",
    "slots":[{"role":"subject","ordinal":0,"entity_key":"e:1"},{"role":"year","ordinal":0,"literal":1948,"literal_kind":"integer"}],
    "instances":[{"region_key":"r:1","original_text":"Фёдор Воронов вернулся в 1948 году.","stance":"affirms","extraction_confidence":0.98}]
  }],
  "gaps":[{"key":"g:1","question":"Что объясняет интервал между освобождением в 1945 и возвращением в 1948 году?","related_claim_key":"c:1","related_entity_key":"e:1","importance":0.8}]
}` + "\n```"}

	extractor := New(fake)
	got, err := extractor.Extract(context.Background(), semantic.ExtractionInput{
		Profile: semantic.ProfileGenealogy,
		Snapshot: semantic.PageSnapshot{
			SpaceID: 1,
			PageID:  2,
			Title:   "Фёдор Воронов",
			Body:    body,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fake.calls != 1 {
		t.Fatalf("completion calls=%d, want 1", fake.calls)
	}
	if !strings.Contains(fake.systemSeen, "UNTRUSTED DATA") || !strings.Contains(fake.systemSeen, "GENEALOGY") {
		t.Fatalf("system prompt lost security/profile instructions: %q", fake.systemSeen)
	}
	if !strings.Contains(fake.userSeen, `"body":"Освобождён`) {
		t.Fatalf("source was not encoded into user prompt: %q", fake.userSeen)
	}
	if len(got.Claims) != 1 || len(got.Regions) != 1 || got.Claims[0].Predicate != "person.returned_home" {
		t.Fatalf("unexpected candidates: %+v", got)
	}
	if got.Regions[0].ExcerptHash == "" || got.Regions[0].ExcerptHash == "model-must-not-control-this" {
		t.Fatalf("excerpt hash was not recomputed server-side: %q", got.Regions[0].ExcerptHash)
	}
}

func TestExtractorRejectsHallucinatedEvidenceSpan(t *testing.T) {
	fake := &fakeCompleter{out: `{
  "entities":[{"key":"e:1","kind":"person","canonical_name":"Фёдор Воронов"}],
  "regions":[{"key":"r:1","locator_kind":"text_quote","locator":{},"excerpt":"Фёдор был в Москве в 1946 году."}],
  "claims":[{"key":"c:1","canonical_text":"Фёдор был в Москве в 1946 году","claim_type":"fact","predicate":"person.location","slots":[{"role":"subject","ordinal":0,"entity_key":"e:1"}],"instances":[{"region_key":"r:1","original_text":"Фёдор был в Москве в 1946 году.","stance":"affirms"}]}]
}`}
	extractor := New(fake)
	_, err := extractor.Extract(context.Background(), semantic.ExtractionInput{
		Profile: semantic.ProfileGenealogy,
		Snapshot: semantic.PageSnapshot{Title: "Фёдор", Body: "Домой вернулся в 1948 году."},
	})
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("hallucinated evidence error=%v, want ErrInvalidOutput", err)
	}
}

func TestDecodeCandidateSetRejectsTrailingProseAndUnknownFields(t *testing.T) {
	if _, err := decodeCandidateSet(`{"entities":[]} trailing`); !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("trailing prose error=%v, want ErrInvalidOutput", err)
	}
	if _, err := decodeCandidateSet(`{"entities":[],"made_up":true}`); !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("unknown field error=%v, want ErrInvalidOutput", err)
	}
}

func TestExtractorRejectsOversizedSourceBeforeLLM(t *testing.T) {
	fake := &fakeCompleter{out: `{}`}
	extractor := New(fake)
	_, err := extractor.Extract(context.Background(), semantic.ExtractionInput{
		Profile: semantic.ProfileTechnical,
		Snapshot: semantic.PageSnapshot{Body: strings.Repeat("x", maxSemanticSourceBytes+1)},
	})
	if !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("oversized source error=%v, want ErrSourceTooLarge", err)
	}
	if fake.calls != 0 {
		t.Fatal("LLM was called for an oversized source")
	}
}

func TestSystemPromptEncodesDirectNegationPolicy(t *testing.T) {
	prompt := systemPrompt(semantic.ProfileGenealogy)
	if !strings.Contains(prompt, `stance="denies"`) || !strings.Contains(prompt, "Competing values are separate claims") {
		t.Fatalf("negation/competing-value policy missing from prompt: %q", prompt)
	}
}
