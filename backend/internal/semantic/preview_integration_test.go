package semantic

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/zcag/tela/backend/internal/testdb"
)

type fakePreviewExtractor struct {
	calls int
	last  ExtractionInput
}

func (f *fakePreviewExtractor) Extract(_ context.Context, in ExtractionInput) (CandidateSet, error) {
	f.calls++
	f.last = in
	personKey := "e:fedor"
	return CandidateSet{
		Entities: []CandidateEntity{{
			Key:           personKey,
			Kind:          "person",
			CanonicalName: "Фёдор Воронов",
			Aliases:       []string{"Федор Воронов", "Fedor Voronov"},
		}},
		Regions: []CandidateRegion{{
			Key:         "r:return-1948",
			LocatorKind: "paragraph",
			Locator:     json.RawMessage(`{"paragraph":1}`),
			Excerpt:     "Освобождён в 1945 году, домой вернулся в 1948 году.",
			ExcerptHash: "fixture-excerpt-return-1948",
		}},
		Claims: []CandidateClaim{{
			Key:           "c:return-1948",
			CanonicalText: "Фёдор Воронов вернулся домой в 1948 году",
			ClaimType:     "fact",
			Predicate:     "person.returned_home",
			ValidFrom:     "1948-01-01",
			ValidTo:       "1948-12-31",
			TimePrecision: "year",
			Slots: []CandidateSlot{
				{Role: "subject", EntityKey: &personKey},
				{Role: "year", LiteralKind: "integer", Literal: json.RawMessage(`1948`)},
			},
			Instances: []CandidateInstance{{
				RegionKey:    "r:return-1948",
				OriginalText: "Освобождён в 1945 году, домой вернулся в 1948 году.",
				Stance:       "affirms",
			}},
		}},
		Gaps: []CandidateGap{{
			Key:              "g:1945-1948",
			Question:         "Что объясняет интервал между освобождением в 1945 и возвращением домой в 1948 году?",
			RelatedClaimKey:  "c:return-1948",
			RelatedEntityKey: personKey,
		}},
	}, nil
}

func TestPreviewPageIsReadOnlyAuthorizedAndSnapshotBound(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()

	var ownerID, viewerID, outsiderID, spaceID, pageID int64
	for _, user := range []struct {
		name string
		out  *int64
	}{
		{"preview-owner", &ownerID},
		{"preview-viewer", &viewerID},
		{"preview-outsider", &outsiderID},
	} {
		if err := d.QueryRow(`INSERT INTO users(username,password_hash) VALUES ($1,'x') RETURNING id`, user.name).Scan(user.out); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.QueryRow(`INSERT INTO spaces(name,slug) VALUES ('Preview Test','preview-test') RETURNING id`).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO space_members(space_id,user_id,role) VALUES ($1,$2,'owner'),($1,$3,'viewer')`, spaceID, ownerID, viewerID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`
INSERT INTO pages(space_id,title,body)
VALUES ($1,'Фёдор Воронов','Освобождён в 1945 году, домой вернулся в 1948 году.')
RETURNING id`, spaceID).Scan(&pageID); err != nil {
		t.Fatal(err)
	}

	extractor := &fakePreviewExtractor{}
	signer, err := NewPreviewSigner([]byte("0123456789abcdef0123456789abcdef"), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	signer.now = func() time.Time { return fixedNow }
	previewSvc, err := NewPreviewService(d, extractor, signer)
	if err != nil {
		t.Fatal(err)
	}

	ownerPreview, err := previewSvc.PreviewPage(ctx, ownerID, spaceID, pageID, ProfileGenealogy)
	if err != nil {
		t.Fatal(err)
	}
	if extractor.calls != 1 || extractor.last.Snapshot.PageID != pageID || extractor.last.Profile != ProfileGenealogy {
		t.Fatalf("unexpected extractor invocation: calls=%d last=%+v", extractor.calls, extractor.last)
	}
	if ownerPreview.SourceContentHash != PageSnapshotHash("Фёдор Воронов", "Освобождён в 1945 году, домой вернулся в 1948 году.") {
		t.Fatalf("unexpected source hash %q", ownerPreview.SourceContentHash)
	}
	claims, err := signer.Verify(ownerPreview.PreviewToken)
	if err != nil {
		t.Fatal(err)
	}
	if claims.SpaceID != spaceID || claims.PageID != pageID || claims.SourceContentHash != ownerPreview.SourceContentHash || claims.CandidatePayloadHash != ownerPreview.CandidatePayloadHash || claims.Profile != ProfileGenealogy {
		t.Fatalf("preview token is not bound to preview payload: %+v preview=%+v", claims, ownerPreview)
	}

	// Viewer may preview/read semantic candidates, but preview itself must not
	// create any canonical semantic rows for either owner or viewer.
	if _, err := previewSvc.PreviewPage(ctx, viewerID, spaceID, pageID, ProfileGenealogy); err != nil {
		t.Fatalf("viewer preview failed: %v", err)
	}
	assertSemanticCoreEmpty(t, d, spaceID)

	callsBeforeUnauthorized := extractor.calls
	if _, err := previewSvc.PreviewPage(ctx, outsiderID, spaceID, pageID, ProfileGenealogy); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("outsider preview error = %v, want ErrNotAuthorized", err)
	}
	if extractor.calls != callsBeforeUnauthorized {
		t.Fatal("extractor ran for unauthorized user")
	}

	if _, err := previewSvc.PreviewPage(ctx, ownerID, spaceID, pageID+999999, ProfileGenealogy); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing page error = %v, want ErrNotFound", err)
	}
	if extractor.calls != callsBeforeUnauthorized {
		t.Fatal("extractor ran for missing page")
	}

	if _, err := previewSvc.PreviewPage(ctx, ownerID, spaceID, pageID, ExtractionProfile("bogus")); err == nil {
		t.Fatal("invalid extraction profile unexpectedly succeeded")
	}
	if extractor.calls != callsBeforeUnauthorized {
		t.Fatal("extractor ran for invalid profile")
	}

	if _, err := d.Exec(`UPDATE pages SET title='Фёдор Воронов — обновлено' WHERE id=$1`, pageID); err != nil {
		t.Fatal(err)
	}
	changedPreview, err := previewSvc.PreviewPage(ctx, ownerID, spaceID, pageID, ProfileGenealogy)
	if err != nil {
		t.Fatal(err)
	}
	if changedPreview.SourceContentHash == ownerPreview.SourceContentHash {
		t.Fatal("page mutation did not stale the old preview snapshot hash")
	}
	if changedPreview.PreviewToken == ownerPreview.PreviewToken {
		t.Fatal("page mutation did not change preview token")
	}

	callsBeforeTrash := extractor.calls
	if _, err := d.Exec(`UPDATE pages SET deleted_at=tela_now() WHERE id=$1`, pageID); err != nil {
		t.Fatal(err)
	}
	if _, err := previewSvc.PreviewPage(ctx, ownerID, spaceID, pageID, ProfileGenealogy); !errors.Is(err, ErrNotFound) {
		t.Fatalf("trashed page error = %v, want ErrNotFound", err)
	}
	if extractor.calls != callsBeforeTrash {
		t.Fatal("extractor ran for trashed page")
	}
	assertSemanticCoreEmpty(t, d, spaceID)
}

func assertSemanticCoreEmpty(t *testing.T, d *sql.DB, spaceID int64) {
	t.Helper()
	var count int
	if err := d.QueryRow(`
SELECT
  (SELECT count(*) FROM sem_sources WHERE space_id=$1) +
  (SELECT count(*) FROM sem_source_versions WHERE space_id=$1) +
  (SELECT count(*) FROM sem_source_regions WHERE space_id=$1) +
  (SELECT count(*) FROM sem_entities WHERE space_id=$1) +
  (SELECT count(*) FROM sem_claims WHERE space_id=$1) +
  (SELECT count(*) FROM sem_claim_instances WHERE space_id=$1) +
  (SELECT count(*) FROM sem_gaps WHERE space_id=$1)`, spaceID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("preview wrote %d semantic rows", count)
	}
}
