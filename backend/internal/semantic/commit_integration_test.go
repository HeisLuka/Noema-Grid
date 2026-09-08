package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/zcag/tela/backend/internal/testdb"
)

func TestCommitPreviewStaleIdempotencyAndCrossSourceClaimIdentity(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()

	var ownerID, viewerID, spaceID, page1ID, page2ID int64
	if err := d.QueryRow(`INSERT INTO users(username,password_hash) VALUES ('commit-owner','x') RETURNING id`).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO users(username,password_hash) VALUES ('commit-viewer','x') RETURNING id`).Scan(&viewerID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO spaces(name,slug) VALUES ('Commit Test','commit-test') RETURNING id`).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO space_members(space_id,user_id,role) VALUES ($1,$2,'owner'),($1,$3,'viewer')`, spaceID, ownerID, viewerID); err != nil {
		t.Fatal(err)
	}

	title1 := "Фёдор Воронов — источник A"
	body1 := "Домой Фёдор Воронов вернулся в 1948 году."
	if err := d.QueryRow(`INSERT INTO pages(space_id,title,body) VALUES ($1,$2,$3) RETURNING id`, spaceID, title1, body1).Scan(&page1ID); err != nil {
		t.Fatal(err)
	}
	title2 := "Фёдор Воронов — источник B"
	body2 := "Возвращение домой датируется 1948 годом."
	if err := d.QueryRow(`INSERT INTO pages(space_id,title,body) VALUES ($1,$2,$3) RETURNING id`, spaceID, title2, body2).Scan(&page2ID); err != nil {
		t.Fatal(err)
	}

	signer, err := NewPreviewSigner([]byte("0123456789abcdef0123456789abcdef"), 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 9, 8, 13, 0, 0, 0, time.UTC)
	signer.now = func() time.Time { return fixedNow }
	commitSvc, err := NewCommitService(d, signer)
	if err != nil {
		t.Fatal(err)
	}

	firstCandidates := returnHomeCandidates(body1, "Фёдор Воронов вернулся домой в 1948 году", 1948)
	firstToken := mustSignPreview(t, signer, spaceID, page1ID, title1, body1, firstCandidates)
	first, err := commitSvc.CommitPreview(ctx, ownerID, CommitInput{
		PreviewToken:   firstToken,
		Candidates:     firstCandidates,
		AcceptedKeys:   []string{"c:return", "g:return-interval"},
		EntityChoices:  map[string]EntityChoice{"e:fedor": {CreateNew: true}},
		IdempotencyKey: "commit-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.Entities["e:fedor"] == 0 || first.Claims["c:return"] == 0 || first.Regions["r:return"] == 0 || first.Gaps["g:return-interval"] == 0 {
		t.Fatalf("incomplete first commit result: %+v", first)
	}

	var sources, versions, regions, entities, claimsCount, instances, gaps int
	if err := d.QueryRow(`
SELECT
  (SELECT count(*) FROM sem_sources WHERE space_id=$1),
  (SELECT count(*) FROM sem_source_versions WHERE space_id=$1),
  (SELECT count(*) FROM sem_source_regions WHERE space_id=$1),
  (SELECT count(*) FROM sem_entities WHERE space_id=$1),
  (SELECT count(*) FROM sem_claims WHERE space_id=$1),
  (SELECT count(*) FROM sem_claim_instances WHERE space_id=$1),
  (SELECT count(*) FROM sem_gaps WHERE space_id=$1)`, spaceID).Scan(&sources, &versions, &regions, &entities, &claimsCount, &instances, &gaps); err != nil {
		t.Fatal(err)
	}
	if sources != 1 || versions != 1 || regions != 1 || entities != 1 || claimsCount != 1 || instances != 1 || gaps != 1 {
		t.Fatalf("unexpected first commit counts: sources=%d versions=%d regions=%d entities=%d claims=%d instances=%d gaps=%d", sources, versions, regions, entities, claimsCount, instances, gaps)
	}

	// A successful idempotent replay returns the original result even after the
	// source page later changes; it does not try to create anything again.
	if _, err := d.Exec(`UPDATE pages SET body=body || ' Добавлена новая строка.' WHERE id=$1`, page1ID); err != nil {
		t.Fatal(err)
	}
	replay, err := commitSvc.CommitPreview(ctx, ownerID, CommitInput{
		PreviewToken:   firstToken,
		Candidates:     firstCandidates,
		AcceptedKeys:   []string{"c:return", "g:return-interval"},
		EntityChoices:  map[string]EntityChoice{"e:fedor": {CreateNew: true}},
		IdempotencyKey: "commit-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed || replay.Claims["c:return"] != first.Claims["c:return"] || replay.Entities["e:fedor"] != first.Entities["e:fedor"] {
		t.Fatalf("bad idempotency replay: first=%+v replay=%+v", first, replay)
	}

	// The same old token with a NEW idempotency key must be rejected as stale.
	if _, err := commitSvc.CommitPreview(ctx, ownerID, CommitInput{
		PreviewToken:   firstToken,
		Candidates:     firstCandidates,
		AcceptedKeys:   []string{"c:return"},
		EntityChoices:  map[string]EntityChoice{"e:fedor": {ExistingEntityID: ptrInt64(first.Entities["e:fedor"])}},
		IdempotencyKey: "commit-a-stale",
	}); !errors.Is(err, ErrStalePreview) {
		t.Fatalf("stale commit error = %v, want ErrStalePreview", err)
	}

	// Candidate tampering is rejected before the transaction regardless of the
	// current page state.
	tampered := firstCandidates
	tampered.Claims = append([]CandidateClaim(nil), firstCandidates.Claims...)
	tampered.Claims[0].CanonicalText = "Подменённое утверждение"
	if _, err := commitSvc.CommitPreview(ctx, ownerID, CommitInput{
		PreviewToken:   firstToken,
		Candidates:     tampered,
		AcceptedKeys:   []string{"c:return"},
		EntityChoices:  map[string]EntityChoice{"e:fedor": {ExistingEntityID: ptrInt64(first.Entities["e:fedor"])}},
		IdempotencyKey: "commit-a-tampered",
	}); !errors.Is(err, ErrInvalidPreviewToken) {
		t.Fatalf("tampered commit error = %v, want ErrInvalidPreviewToken", err)
	}

	// Viewer can preview, but cannot commit canonical semantic state.
	if _, err := commitSvc.CommitPreview(ctx, viewerID, CommitInput{
		PreviewToken:   mustSignPreview(t, signer, spaceID, page2ID, title2, body2, returnHomeCandidates(body2, "Возвращение Фёдора Воронова домой датируется 1948 годом", 1948)),
		Candidates:     returnHomeCandidates(body2, "Возвращение Фёдора Воронова домой датируется 1948 годом", 1948),
		AcceptedKeys:   []string{"c:return"},
		EntityChoices:  map[string]EntityChoice{"e:fedor": {ExistingEntityID: ptrInt64(first.Entities["e:fedor"])}},
		IdempotencyKey: "viewer-forbidden",
	}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("viewer commit error = %v, want ErrNotAuthorized", err)
	}

	// Second independent source, different wording, explicit reuse of the known
	// person entity. Structural identity must attach a new instance to the SAME
	// Claim rather than create a duplicate proposition.
	secondCandidates := returnHomeCandidates(body2, "Возвращение Фёдора Воронова домой датируется 1948 годом", 1948)
	secondToken := mustSignPreview(t, signer, spaceID, page2ID, title2, body2, secondCandidates)
	second, err := commitSvc.CommitPreview(ctx, ownerID, CommitInput{
		PreviewToken:   secondToken,
		Candidates:     secondCandidates,
		AcceptedKeys:   []string{"c:return"},
		EntityChoices:  map[string]EntityChoice{"e:fedor": {ExistingEntityID: ptrInt64(first.Entities["e:fedor"])}},
		IdempotencyKey: "commit-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Claims["c:return"] != first.Claims["c:return"] {
		t.Fatalf("same proposition split across sources: first=%d second=%d", first.Claims["c:return"], second.Claims["c:return"])
	}
	if second.Entities["e:fedor"] != first.Entities["e:fedor"] {
		t.Fatalf("explicit entity reuse failed: first=%d second=%d", first.Entities["e:fedor"], second.Entities["e:fedor"])
	}
	if err := d.QueryRow(`SELECT count(*) FROM sem_claim_instances WHERE space_id=$1 AND claim_id=$2`, spaceID, first.Claims["c:return"]).Scan(&instances); err != nil {
		t.Fatal(err)
	}
	if instances != 2 {
		t.Fatalf("same claim should have two source instances, got %d", instances)
	}

	// A competing value is a separate proposition, even with the same subject.
	competingCandidates := returnHomeCandidates(body2, "Фёдор Воронов вернулся домой в 1947 году", 1947)
	competingToken := mustSignPreview(t, signer, spaceID, page2ID, title2, body2, competingCandidates)
	competing, err := commitSvc.CommitPreview(ctx, ownerID, CommitInput{
		PreviewToken:   competingToken,
		Candidates:     competingCandidates,
		AcceptedKeys:   []string{"c:return"},
		EntityChoices:  map[string]EntityChoice{"e:fedor": {ExistingEntityID: ptrInt64(first.Entities["e:fedor"])}},
		IdempotencyKey: "commit-c-1947",
	})
	if err != nil {
		t.Fatal(err)
	}
	if competing.Claims["c:return"] == first.Claims["c:return"] {
		t.Fatal("competing return years collapsed into one claim")
	}
}

func returnHomeCandidates(excerpt, canonical string, year int) CandidateSet {
	entityKey := "e:fedor"
	return CandidateSet{
		Entities: []CandidateEntity{{
			Key:           entityKey,
			Kind:          "person",
			CanonicalName: "Фёдор Воронов",
			Aliases:       []string{"Федор Воронов", "Fedor Voronov"},
		}},
		Regions: []CandidateRegion{{
			Key:         "r:return",
			LocatorKind: "fixture",
			Locator:     json.RawMessage(`{"key":"return"}`),
			Excerpt:     excerpt,
		}},
		Claims: []CandidateClaim{{
			Key:           "c:return",
			CanonicalText: canonical,
			ClaimType:     "fact",
			Predicate:     "person.returned_home",
			ValidFrom:     fmt.Sprintf("%04d-01-01", year),
			ValidTo:       fmt.Sprintf("%04d-12-31", year),
			TimePrecision: "year",
			Slots: []CandidateSlot{
				{Role: "subject", EntityKey: &entityKey},
				{Role: "year", LiteralKind: "integer", Literal: json.RawMessage(fmt.Sprintf("%d", year))},
			},
			Instances: []CandidateInstance{{
				RegionKey:    "r:return",
				OriginalText: excerpt,
				Stance:       "affirms",
			}},
		}},
		Gaps: []CandidateGap{{
			Key:              "g:return-interval",
			Question:         "Почему возвращение домой произошло только в 1948 году?",
			RelatedClaimKey:  "c:return",
			RelatedEntityKey: entityKey,
		}},
	}
}

func mustSignPreview(t *testing.T, signer *PreviewSigner, spaceID, pageID int64, title, body string, candidates CandidateSet) string {
	t.Helper()
	hash, err := CandidatePayloadHash(candidates)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := signer.Sign(spaceID, pageID, PageSnapshotHash(title, body), ProfileGenealogy, hash)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func ptrInt64(v int64) *int64 { return &v }
