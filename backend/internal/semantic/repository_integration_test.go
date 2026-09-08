package semantic

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zcag/tela/backend/internal/testdb"
)

func TestManualIngestIsIdempotentAndSpaceAuthorized(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()

	var ownerID, outsiderID, spaceID int64
	if err := d.QueryRow(`INSERT INTO users(username,password_hash) VALUES ('noema-owner','x') RETURNING id`).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO users(username,password_hash) VALUES ('noema-outsider','x') RETURNING id`).Scan(&outsiderID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO spaces(name,slug) VALUES ('Noema Test','noema-test') RETURNING id`).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO space_members(space_id,user_id,role) VALUES ($1,$2,'owner')`, spaceID, ownerID); err != nil {
		t.Fatal(err)
	}

	uri := "manual://fedor-voronov/return-home"
	in := ManualIngest{
		Source: SourceDraft{
			SpaceID:   spaceID,
			Kind:      "manual",
			URI:       &uri,
			Title:     "Fedor Voronov fixture",
			CreatedBy: &ownerID,
		},
		Version: SourceVersionDraft{ContentHash: "fixture-v1"},
		Region: SourceRegionDraft{
			LocatorKind: "paragraph",
			Locator:     json.RawMessage(`{"paragraph":1}`),
			Excerpt:     "Домой вернулся в 1948 году.",
			ExcerptHash: "excerpt-1948",
		},
		Claim: ClaimDraft{
			SpaceID:       spaceID,
			CanonicalText: "Фёдор Воронов вернулся домой в 1948 году",
			ClaimType:     "fact",
			Predicate:     "person.returned_home",
			Slots: []Slot{
				{Role: "subject", LiteralKind: "text", Literal: json.RawMessage(`"Фёдор Воронов"`)},
				{Role: "year", LiteralKind: "integer", Literal: json.RawMessage(`1948`)},
			},
			CreatedBy: &ownerID,
		},
		Instance: ClaimInstanceDraft{
			OriginalText: "Домой вернулся в 1948 году.",
			Stance:       "affirms",
			CreatedBy:    "test-fixture",
		},
	}

	r := NewRepository(d)
	first, err := r.IngestManual(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.IngestManual(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.ClaimID != second.ClaimID || first.ClaimInstanceID != second.ClaimInstanceID || first.SourceRegionID != second.SourceRegionID {
		t.Fatalf("retry duplicated semantic objects: first=%+v second=%+v", first, second)
	}

	traces, err := r.ClaimTraces(ctx, ownerID, first.ClaimID)
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 1 || traces[0].ContentHash != "fixture-v1" || traces[0].Excerpt != "Домой вернулся в 1948 году." {
		t.Fatalf("unexpected provenance trace: %+v", traces)
	}

	leaked, err := r.ClaimTraces(ctx, outsiderID, first.ClaimID)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaked) != 0 {
		t.Fatalf("semantic read leaked across live space_access: %+v", leaked)
	}
}

func TestDatabaseRejectsCrossSpaceClaimSlot(t *testing.T) {
	d := testdb.New(t)
	var s1, s2, claimID, entityID int64
	if err := d.QueryRow(`INSERT INTO spaces(name,slug) VALUES ('S1','sem-s1') RETURNING id`).Scan(&s1); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO spaces(name,slug) VALUES ('S2','sem-s2') RETURNING id`).Scan(&s2); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO sem_entities(space_id,kind,canonical_name) VALUES ($1,'person','Fedor') RETURNING id`, s1).Scan(&entityID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO sem_claims(space_id,canonical_text,predicate,claim_fingerprint) VALUES ($1,'x','test.x','fp-x') RETURNING id`, s2).Scan(&claimID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO sem_claim_slots(space_id,claim_id,role,entity_id) VALUES ($1,$2,'subject',$3)`, s2, claimID, entityID); err == nil {
		t.Fatal("cross-space entity reference unexpectedly succeeded")
	}
}
