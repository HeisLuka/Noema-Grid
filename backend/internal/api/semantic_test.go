package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zcag/tela/backend/internal/auth"
	"github.com/zcag/tela/backend/internal/semantic"
	"github.com/zcag/tela/backend/internal/testdb"
)

type apiSemanticFakeExtractor struct {
	calls int
	set   semantic.CandidateSet
}

func (f *apiSemanticFakeExtractor) Extract(_ context.Context, _ semantic.ExtractionInput) (semantic.CandidateSet, error) {
	f.calls++
	return f.set, nil
}

func TestSemanticPreviewCoreMembershipAndExtractorBoundary(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	var viewerID, outsiderID, spaceID, pageID int64
	if err := d.QueryRow(`INSERT INTO users(username,password_hash) VALUES ('semantic-api-viewer','x') RETURNING id`).Scan(&viewerID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO users(username,password_hash) VALUES ('semantic-api-outsider','x') RETURNING id`).Scan(&outsiderID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO spaces(name,slug) VALUES ('Semantic API','semantic-api') RETURNING id`).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO space_members(space_id,user_id,role) VALUES ($1,$2,'viewer')`, spaceID, viewerID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO pages(space_id,title,body) VALUES ($1,'Source','Body') RETURNING id`, spaceID).Scan(&pageID); err != nil {
		t.Fatal(err)
	}

	s := &Server{DB: d, shareSecret: []byte("semantic-api-test-root-secret")}
	extractor := &apiSemanticFakeExtractor{}
	viewer := &auth.User{ID: viewerID, Username: "semantic-api-viewer"}
	preview, ae := s.semanticPreviewCore(ctx, viewer, nil, spaceID, pageID, semantic.ProfileGenealogy, extractor)
	if ae != nil {
		t.Fatalf("viewer preview failed: %+v", ae)
	}
	if preview.PageID != pageID || extractor.calls != 1 {
		t.Fatalf("unexpected preview/calls: preview=%+v calls=%d", preview, extractor.calls)
	}

	outsider := &auth.User{ID: outsiderID, Username: "semantic-api-outsider"}
	if _, ae := s.semanticPreviewCore(ctx, outsider, nil, spaceID, pageID, semantic.ProfileGenealogy, extractor); ae == nil || ae.Status != http.StatusForbidden || ae.Code != "forbidden" {
		t.Fatalf("outsider error = %+v, want Tela membership forbidden", ae)
	}
	if extractor.calls != 1 {
		t.Fatal("extractor ran before membership gate")
	}

	if _, ae := s.semanticPreviewCore(ctx, viewer, nil, spaceID, pageID, semantic.ProfileGenealogy, s.semanticExtractor()); ae == nil || ae.Status != http.StatusServiceUnavailable || ae.Code != "semantic_extractor_unavailable" {
		t.Fatalf("unconfigured extractor error = %+v", ae)
	}
}

func TestSemanticCommitCoreBindsTokenToRouteSpaceAndWriteRole(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	var ownerID, viewerID, spaceA, spaceB, pageA int64
	if err := d.QueryRow(`INSERT INTO users(username,password_hash) VALUES ('semantic-commit-owner','x') RETURNING id`).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO users(username,password_hash) VALUES ('semantic-commit-viewer','x') RETURNING id`).Scan(&viewerID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO spaces(name,slug) VALUES ('Semantic A','semantic-a') RETURNING id`).Scan(&spaceA); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO spaces(name,slug) VALUES ('Semantic B','semantic-b') RETURNING id`).Scan(&spaceB); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`
INSERT INTO space_members(space_id,user_id,role) VALUES
 ($1,$3,'owner'),($2,$3,'owner'),($1,$4,'viewer')`, spaceA, spaceB, ownerID, viewerID); err != nil {
		t.Fatal(err)
	}
	body := "Фёдор Воронов вернулся домой в 1948 году."
	if err := d.QueryRow(`INSERT INTO pages(space_id,title,body) VALUES ($1,'Фёдор Воронов',$2) RETURNING id`, spaceA, body).Scan(&pageA); err != nil {
		t.Fatal(err)
	}

	s := &Server{DB: d, shareSecret: []byte("semantic-api-test-root-secret")}
	signer, ae := s.semanticPreviewSigner()
	if ae != nil {
		t.Fatal(ae.Message)
	}
	candidates := apiSemanticCandidateFixture(body)
	candidateHash, err := semantic.CandidatePayloadHash(candidates)
	if err != nil {
		t.Fatal(err)
	}
	wrongSpaceToken, _, err := signer.Sign(spaceB, pageA, semantic.PageSnapshotHash("Фёдор Воронов", body), semantic.ProfileGenealogy, candidateHash)
	if err != nil {
		t.Fatal(err)
	}
	owner := &auth.User{ID: ownerID, Username: "semantic-commit-owner"}
	if _, ae := s.semanticCommitCore(ctx, owner, nil, spaceA, semantic.CommitInput{
		PreviewToken:   wrongSpaceToken,
		Candidates:     candidates,
		AcceptedKeys:   []string{"c:return"},
		EntityChoices:  map[string]semantic.EntityChoice{"e:fedor": {CreateNew: true}},
		IdempotencyKey: "route-space-mismatch",
	}); ae == nil || ae.Status != http.StatusUnprocessableEntity || ae.Code != "semantic_invalid_preview" {
		t.Fatalf("route-space mismatch error = %+v", ae)
	}

	validToken, _, err := signer.Sign(spaceA, pageA, semantic.PageSnapshotHash("Фёдор Воронов", body), semantic.ProfileGenealogy, candidateHash)
	if err != nil {
		t.Fatal(err)
	}
	viewer := &auth.User{ID: viewerID, Username: "semantic-commit-viewer"}
	if _, ae := s.semanticCommitCore(ctx, viewer, nil, spaceA, semantic.CommitInput{
		PreviewToken:   validToken,
		Candidates:     candidates,
		AcceptedKeys:   []string{"c:return"},
		EntityChoices:  map[string]semantic.EntityChoice{"e:fedor": {CreateNew: true}},
		IdempotencyKey: "viewer-write-denied",
	}); ae == nil || ae.Status != http.StatusForbidden || ae.Code != "semantic_not_authorized" {
		t.Fatalf("viewer commit error = %+v, want semantic_not_authorized", ae)
	}
}

func TestSemanticCommitRESTRequiresHeaderIdempotency(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/spaces/7/semantic/commit", strings.NewReader(`{}`))
	req.SetPathValue("id", "7")
	req = req.WithContext(auth.WithUser(req.Context(), &auth.User{ID: 1, Username: "rest-user"}))
	rr := httptest.NewRecorder()

	s.SemanticCommit(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got errorBody
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Code != "semantic_idempotency_key_required" {
		t.Fatalf("code=%q body=%s", got.Code, rr.Body.String())
	}
}

func apiSemanticCandidateFixture(excerpt string) semantic.CandidateSet {
	entityKey := "e:fedor"
	return semantic.CandidateSet{
		Entities: []semantic.CandidateEntity{{
			Key:           entityKey,
			Kind:          "person",
			CanonicalName: "Фёдор Воронов",
		}},
		Regions: []semantic.CandidateRegion{{
			Key:         "r:return",
			LocatorKind: "fixture",
			Locator:     json.RawMessage(`{"key":"return"}`),
			Excerpt:     excerpt,
		}},
		Claims: []semantic.CandidateClaim{{
			Key:           "c:return",
			CanonicalText: "Фёдор Воронов вернулся домой в 1948 году",
			ClaimType:     "fact",
			Predicate:     "person.returned_home",
			ValidFrom:     "1948-01-01",
			ValidTo:       "1948-12-31",
			TimePrecision: "year",
			Slots: []semantic.CandidateSlot{
				{Role: "subject", EntityKey: &entityKey},
				{Role: "year", LiteralKind: "integer", Literal: json.RawMessage(`1948`)},
			},
			Instances: []semantic.CandidateInstance{{
				RegionKey:    "r:return",
				OriginalText: excerpt,
				Stance:       "affirms",
			}},
		}},
	}
}
