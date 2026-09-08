package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/zcag/tela/backend/internal/auth"
	"github.com/zcag/tela/backend/internal/llm"
	"github.com/zcag/tela/backend/internal/semantic"
	"github.com/zcag/tela/backend/internal/testdb"
)

type semanticAPIFakeCompleter struct {
	out   string
	calls int
}

func (f *semanticAPIFakeCompleter) Complete(_ context.Context, _, _ string) (string, error) {
	f.calls++
	return f.out, nil
}

func (f *semanticAPIFakeCompleter) Model() string { return "semantic-test-model" }

func TestSemanticPreviewUsesConfiguredTelaLLM(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	var ownerID, spaceID, pageID int64
	if err := d.QueryRow(`INSERT INTO users(username,password_hash) VALUES ('semantic-llm-owner','x') RETURNING id`).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO spaces(name,slug) VALUES ('Semantic LLM','semantic-llm') RETURNING id`).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO space_members(space_id,user_id,role) VALUES ($1,$2,'owner')`, spaceID, ownerID); err != nil {
		t.Fatal(err)
	}
	body := "Освобождён в 1945 году, домой Фёдор Воронов вернулся в 1948 году."
	if err := d.QueryRow(`INSERT INTO pages(space_id,title,body) VALUES ($1,'Фёдор Воронов',$2) RETURNING id`, spaceID, body).Scan(&pageID); err != nil {
		t.Fatal(err)
	}

	fake := &semanticAPIFakeCompleter{out: `{
  "entities":[{"key":"e:1","kind":"person","canonical_name":"Фёдор Воронов"}],
  "regions":[{"key":"r:1","locator_kind":"text_quote","locator":{"occurrence":1},"excerpt":"домой Фёдор Воронов вернулся в 1948 году."}],
  "claims":[{"key":"c:1","canonical_text":"Фёдор Воронов вернулся домой в 1948 году","claim_type":"fact","predicate":"person.returned_home","valid_from":"1948-01-01","valid_to":"1948-12-31","time_precision":"year","slots":[{"role":"subject","ordinal":0,"entity_key":"e:1"},{"role":"year","ordinal":0,"literal":1948,"literal_kind":"integer"}],"instances":[{"region_key":"r:1","original_text":"Фёдор Воронов вернулся в 1948 году.","stance":"affirms","extraction_confidence":0.99}]}]
}`}
	s := &Server{
		DB:          d,
		shareSecret: []byte("semantic-llm-api-root-secret"),
		llm:         llm.NewServiceWithCompleter(fake),
	}
	owner := &auth.User{ID: ownerID, Username: "semantic-llm-owner"}
	preview, ae := s.semanticPreviewCore(ctx, owner, nil, spaceID, pageID, semantic.ProfileGenealogy, s.semanticExtractor())
	if ae != nil {
		t.Fatalf("semantic preview failed: %+v", ae)
	}
	if fake.calls != 1 {
		t.Fatalf("Tela LLM calls=%d, want 1", fake.calls)
	}
	if len(preview.Candidates.Claims) != 1 || preview.Candidates.Claims[0].Predicate != "person.returned_home" || preview.PreviewToken == "" {
		t.Fatalf("unexpected semantic preview: %+v", preview)
	}
}

func TestSemanticPreviewMapsUngroundedLLMOutputToBadGateway(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	var ownerID, spaceID, pageID int64
	if err := d.QueryRow(`INSERT INTO users(username,password_hash) VALUES ('semantic-llm-bad-owner','x') RETURNING id`).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO spaces(name,slug) VALUES ('Semantic LLM Bad','semantic-llm-bad') RETURNING id`).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO space_members(space_id,user_id,role) VALUES ($1,$2,'owner')`, spaceID, ownerID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO pages(space_id,title,body) VALUES ($1,'Source','Домой вернулся в 1948 году.') RETURNING id`, spaceID).Scan(&pageID); err != nil {
		t.Fatal(err)
	}

	fake := &semanticAPIFakeCompleter{out: `{
  "entities":[{"key":"e:1","kind":"person","canonical_name":"Фёдор Воронов"}],
  "regions":[{"key":"r:1","locator_kind":"text_quote","locator":{},"excerpt":"Фёдор Воронов был в Москве в 1946 году."}],
  "claims":[{"key":"c:1","canonical_text":"Фёдор Воронов был в Москве в 1946 году","claim_type":"fact","predicate":"person.location","slots":[{"role":"subject","ordinal":0,"entity_key":"e:1"}],"instances":[{"region_key":"r:1","original_text":"Фёдор Воронов был в Москве в 1946 году.","stance":"affirms"}]}]
}`}
	s := &Server{DB: d, shareSecret: []byte("semantic-llm-api-root-secret"), llm: llm.NewServiceWithCompleter(fake)}
	owner := &auth.User{ID: ownerID, Username: "semantic-llm-bad-owner"}
	_, ae := s.semanticPreviewCore(ctx, owner, nil, spaceID, pageID, semantic.ProfileGenealogy, s.semanticExtractor())
	if ae == nil || ae.Status != http.StatusBadGateway || ae.Code != "semantic_extractor_invalid_output" {
		t.Fatalf("ungrounded model output error=%+v, want 502 semantic_extractor_invalid_output", ae)
	}
}
