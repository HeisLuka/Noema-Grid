package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/zcag/tela/backend/internal/testdb"
)

func TestFedorVoronovFixtureContracts(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()

	var ownerID, viewerID, outsiderID, spaceID int64
	for _, item := range []struct {
		name string
		out  *int64
	}{
		{"noema-fedor-owner", &ownerID},
		{"noema-fedor-viewer", &viewerID},
		{"noema-fedor-outsider", &outsiderID},
	} {
		if err := d.QueryRow(`INSERT INTO users(username,password_hash) VALUES ($1,'x') RETURNING id`, item.name).Scan(item.out); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.QueryRow(`INSERT INTO spaces(name,slug) VALUES ('Fedor Fixture','fedor-fixture') RETURNING id`).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO space_members(space_id,user_id,role) VALUES ($1,$2,'owner'),($1,$3,'viewer')`, spaceID, ownerID, viewerID); err != nil {
		t.Fatal(err)
	}

	svc := NewService(d)
	fedor, err := svc.CreateEntity(ctx, ownerID, EntityDraft{
		SpaceID:       spaceID,
		Kind:          "person",
		CanonicalName: "Фёдор Воронов",
		Description:   "Genealogy fixture person",
		Aliases: []EntityAliasDraft{
			{Alias: "Федор Воронов"},
			{Alias: "Fedor Voronov"},
		},
		CreatedBy: &ownerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	donbas, err := svc.CreateEntity(ctx, ownerID, EntityDraft{
		SpaceID:       spaceID,
		Kind:          "place",
		CanonicalName: "Донбасс",
		Aliases:       []EntityAliasDraft{{Alias: "Donbas"}},
		CreatedBy:     &ownerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	tractorDriver, err := svc.CreateEntity(ctx, ownerID, EntityDraft{
		SpaceID:       spaceID,
		Kind:          "concept",
		CanonicalName: "тракторист",
		Aliases:       []EntityAliasDraft{{Alias: "tractor driver"}},
		CreatedBy:     &ownerID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Exact aliases are candidate retrieval only. The service never silently
	// merges entities by a human name.
	candidates, err := svc.FindEntityCandidatesByAlias(ctx, ownerID, spaceID, "  ФЁДОР   ВОРОНОВ ")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != fedor.ID {
		t.Fatalf("unexpected exact-alias resolution: %+v", candidates)
	}
	viewerCandidates, err := svc.FindEntityCandidatesByAlias(ctx, viewerID, spaceID, "Fedor Voronov")
	if err != nil || len(viewerCandidates) != 1 || viewerCandidates[0].ID != fedor.ID {
		t.Fatalf("viewer should be able to resolve entities: candidates=%+v err=%v", viewerCandidates, err)
	}
	if _, err := svc.CreateEntity(ctx, viewerID, EntityDraft{SpaceID: spaceID, Kind: "person", CanonicalName: "Forbidden"}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("viewer semantic write should be forbidden, got %v", err)
	}
	if _, err := svc.FindEntityCandidatesByAlias(ctx, outsiderID, spaceID, "Фёдор Воронов"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("outsider semantic read should be forbidden, got %v", err)
	}

	c1 := mustIngestFixtureClaim(t, ctx, svc, ownerID, fixtureClaim{
		spaceID: spaceID,
		uri:     "manual://fedor-voronov/liberated-1945",
		hash:    "fedor-liberated-v1",
		excerpt: "Фёдор Воронов был освобождён в 1945 году.",
		claim: ClaimDraft{
			SpaceID:       spaceID,
			CanonicalText: "Фёдор Воронов был освобождён в 1945 году",
			ClaimType:     "fact",
			Predicate:     "person.liberated",
			ValidFrom:     "1945-01-01",
			ValidTo:       "1945-12-31",
			TimePrecision: "year",
			Slots: []Slot{
				entitySlot("subject", fedor.ID),
				literalSlot("year", "integer", `1945`),
			},
			CreatedBy: &ownerID,
		},
		stance: "affirms",
	})

	// C2: two independent affirming regions plus one direct denial all resolve
	// to ONE proposition. Wording is deliberately different across sources.
	c2a := mustIngestFixtureClaim(t, ctx, svc, ownerID, fixtureClaim{
		spaceID: spaceID,
		uri:     "manual://fedor-voronov/returned-1948-a",
		hash:    "fedor-return-a-v1",
		excerpt: "Домой Фёдор вернулся в 1948 году.",
		claim:   returnedHomeClaim(spaceID, fedor.ID, 1948, "Фёдор Воронов вернулся домой в 1948 году", &ownerID),
		stance:  "affirms",
	})
	c2b := mustIngestFixtureClaim(t, ctx, svc, ownerID, fixtureClaim{
		spaceID: spaceID,
		uri:     "manual://fedor-voronov/returned-1948-b",
		hash:    "fedor-return-b-v1",
		excerpt: "В сорок восьмом он наконец пришёл домой.",
		claim:   returnedHomeClaim(spaceID, fedor.ID, 1948, "Возвращение Фёдора Воронова домой датируется 1948 годом", &ownerID),
		stance:  "affirms",
	})
	c2deny := mustIngestFixtureClaim(t, ctx, svc, ownerID, fixtureClaim{
		spaceID: spaceID,
		uri:     "manual://fedor-voronov/returned-1948-denial",
		hash:    "fedor-return-denial-v1",
		excerpt: "В 1948 году Фёдор Воронов ещё не вернулся домой.",
		claim:   returnedHomeClaim(spaceID, fedor.ID, 1948, "Фёдор Воронов вернулся домой в 1948 году", &ownerID),
		stance:  "denies",
	})
	if c2a.ClaimID != c2b.ClaimID || c2a.ClaimID != c2deny.ClaimID {
		t.Fatalf("same proposition split across claims: a=%+v b=%+v deny=%+v", c2a, c2b, c2deny)
	}
	if c2a.ClaimInstanceID == c2b.ClaimInstanceID || c2a.ClaimInstanceID == c2deny.ClaimInstanceID || c2b.ClaimInstanceID == c2deny.ClaimInstanceID {
		t.Fatal("independent evidence regions unexpectedly collapsed to one instance")
	}

	c3 := mustIngestFixtureClaim(t, ctx, svc, ownerID, fixtureClaim{
		spaceID: spaceID,
		uri:     "manual://fedor-voronov/donbas-1945-1948",
		hash:    "fedor-donbas-v1",
		excerpt: "После освобождения и до возвращения домой Фёдор находился на Донбассе.",
		claim: ClaimDraft{
			SpaceID:       spaceID,
			CanonicalText: "Фёдор Воронов находился на Донбассе между освобождением и возвращением домой",
			ClaimType:     "hypothesis",
			Predicate:     "person.location",
			ValidFrom:     "1945-01-01",
			ValidTo:       "1948-12-31",
			TimePrecision: "year_range",
			Slots: []Slot{
				entitySlot("subject", fedor.ID),
				entitySlot("location", donbas.ID),
			},
			CreatedBy: &ownerID,
		},
		stance: "affirms",
	})

	c4 := mustIngestFixtureClaim(t, ctx, svc, ownerID, fixtureClaim{
		spaceID: spaceID,
		uri:     "manual://fedor-voronov/tractor-driver",
		hash:    "fedor-occupation-v1",
		excerpt: "После возвращения работал трактористом.",
		claim: ClaimDraft{
			SpaceID:       spaceID,
			CanonicalText: "После возвращения Фёдор Воронов работал трактористом",
			ClaimType:     "fact",
			Predicate:     "person.occupation",
			ValidFrom:     "1948-01-01",
			TimePrecision: "year_or_later",
			Slots: []Slot{
				entitySlot("subject", fedor.ID),
				entitySlot("occupation", tractorDriver.ID),
			},
			CreatedBy: &ownerID,
		},
		stance: "affirms",
	})

	// C5 is not a denial instance of C2: it is a competing value for the same
	// property, so it must remain a separate Claim connected by contradicts.
	c5 := mustIngestFixtureClaim(t, ctx, svc, ownerID, fixtureClaim{
		spaceID: spaceID,
		uri:     "manual://fedor-voronov/returned-1947",
		hash:    "fedor-return-1947-v1",
		excerpt: "Фёдор Воронов вернулся домой в 1947 году.",
		claim:   returnedHomeClaim(spaceID, fedor.ID, 1947, "Фёдор Воронов вернулся домой в 1947 году", &ownerID),
		stance:  "affirms",
	})
	if c5.ClaimID == c2a.ClaimID {
		t.Fatal("competing return years collapsed to one claim")
	}
	if _, err := svc.CreateClaimRelation(ctx, ownerID, ClaimRelationDraft{
		SpaceID:      spaceID,
		FromClaimID:  c5.ClaimID,
		ToClaimID:    c2a.ClaimID,
		RelationType: "contradicts",
		Reason:       "Competing year for person.returned_home",
		CreatedBy:    "fedor-fixture",
	}); err != nil {
		t.Fatal(err)
	}

	importance := 0.95
	gapID, err := svc.CreateGap(ctx, ownerID, GapDraft{
		SpaceID:         spaceID,
		Question:        "Что объясняет интервал между освобождением в 1945 году и возвращением домой в 1948 году?",
		Importance:      &importance,
		RelatedClaimID:  &c2a.ClaimID,
		RelatedEntityID: &fedor.ID,
		Context:         json.RawMessage(`{"fixture":"fedor-voronov"}`),
		CreatedBy:       "fedor-fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gapID == 0 {
		t.Fatal("gap was not created")
	}

	traces, err := svc.ClaimTraces(ctx, ownerID, spaceID, c2a.ClaimID)
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 3 {
		t.Fatalf("C2 should have three exact evidence regions, got %+v", traces)
	}
	var affirm, deny int
	for _, tr := range traces {
		switch tr.Stance {
		case "affirms":
			affirm++
		case "denies":
			deny++
		default:
			t.Fatalf("unexpected evidence stance %q", tr.Stance)
		}
		if tr.SourceVersionID == 0 || tr.SourceRegionID == 0 || tr.ContentHash == "" || tr.Excerpt == "" {
			t.Fatalf("broken provenance chain: %+v", tr)
		}
	}
	if affirm != 2 || deny != 1 {
		t.Fatalf("unexpected C2 stance distribution affirms=%d denies=%d", affirm, deny)
	}

	var claimCount, instanceCount, fedorSlotCount, contradictionCount, gapCount int
	checks := []struct {
		query string
		args  []any
		out   *int
	}{
		{`SELECT count(*) FROM sem_claims WHERE space_id=$1`, []any{spaceID}, &claimCount},
		{`SELECT count(*) FROM sem_claim_instances WHERE space_id=$1`, []any{spaceID}, &instanceCount},
		{`SELECT count(*) FROM sem_claim_slots WHERE space_id=$1 AND entity_id=$2`, []any{spaceID, fedor.ID}, &fedorSlotCount},
		{`SELECT count(*) FROM sem_claim_relations WHERE space_id=$1 AND from_claim_id=$2 AND to_claim_id=$3 AND relation_type='contradicts'`, []any{spaceID, c5.ClaimID, c2a.ClaimID}, &contradictionCount},
		{`SELECT count(*) FROM sem_gaps WHERE space_id=$1 AND id=$2 AND status='open'`, []any{spaceID, gapID}, &gapCount},
	}
	for _, check := range checks {
		if err := d.QueryRow(check.query, check.args...).Scan(check.out); err != nil {
			t.Fatal(err)
		}
	}
	if claimCount != 5 || instanceCount != 7 || fedorSlotCount != 5 || contradictionCount != 1 || gapCount != 1 {
		t.Fatalf("unexpected fixture graph counts: claims=%d instances=%d fedorSlots=%d contradictions=%d gaps=%d (ids c1=%d c3=%d c4=%d)",
			claimCount, instanceCount, fedorSlotCount, contradictionCount, gapCount, c1.ClaimID, c3.ClaimID, c4.ClaimID)
	}
}

type fixtureClaim struct {
	spaceID int64
	uri     string
	hash    string
	excerpt string
	claim   ClaimDraft
	stance  string
}

func mustIngestFixtureClaim(t *testing.T, ctx context.Context, svc *Service, userID int64, in fixtureClaim) IngestResult {
	t.Helper()
	uri := in.uri
	res, err := svc.IngestManual(ctx, userID, ManualIngest{
		Source: SourceDraft{
			SpaceID: in.spaceID,
			Kind:    "manual",
			URI:     &uri,
			Title:   "Fedor Voronov fixture",
		},
		Version: SourceVersionDraft{ContentHash: in.hash},
		Region: SourceRegionDraft{
			LocatorKind: "fixture",
			Locator:     json.RawMessage(fmt.Sprintf(`{"key":%q}`, in.uri)),
			Excerpt:     in.excerpt,
			ExcerptHash: in.hash + "-excerpt",
		},
		Claim: in.claim,
		Instance: ClaimInstanceDraft{
			OriginalText: in.excerpt,
			Stance:       in.stance,
			CreatedBy:    "fedor-fixture",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func returnedHomeClaim(spaceID, personID int64, year int, text string, createdBy *int64) ClaimDraft {
	return ClaimDraft{
		SpaceID:       spaceID,
		CanonicalText: text,
		ClaimType:     "fact",
		Predicate:     "person.returned_home",
		ValidFrom:     fmt.Sprintf("%04d-01-01", year),
		ValidTo:       fmt.Sprintf("%04d-12-31", year),
		TimePrecision: "year",
		Slots: []Slot{
			entitySlot("subject", personID),
			literalSlot("year", "integer", fmt.Sprintf("%d", year)),
		},
		CreatedBy: createdBy,
	}
}

func entitySlot(role string, id int64) Slot {
	return Slot{Role: role, EntityID: &id}
}

func literalSlot(role, kind, raw string) Slot {
	return Slot{Role: role, LiteralKind: kind, Literal: json.RawMessage(raw)}
}
