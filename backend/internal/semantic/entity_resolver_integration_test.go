package semantic

import (
	"context"
	"errors"
	"testing"

	"github.com/zcag/tela/backend/internal/testdb"
)

func TestResolveEntityCandidateHintsNeverAutoMergesAmbiguousAlias(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	var ownerID, viewerID, outsiderID, spaceID int64
	for _, u := range []struct {
		name string
		out  *int64
	}{
		{"resolver-owner", &ownerID},
		{"resolver-viewer", &viewerID},
		{"resolver-outsider", &outsiderID},
	} {
		if err := d.QueryRow(`INSERT INTO users(username,password_hash) VALUES ($1,'x') RETURNING id`, u.name).Scan(u.out); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.QueryRow(`INSERT INTO spaces(name,slug) VALUES ('Resolver Test','resolver-test') RETURNING id`).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO space_members(space_id,user_id,role) VALUES ($1,$2,'owner'),($1,$3,'viewer')`, spaceID, ownerID, viewerID); err != nil {
		t.Fatal(err)
	}

	svc := NewService(d)
	first, err := svc.CreateEntity(ctx, ownerID, EntityDraft{
		SpaceID:       spaceID,
		Kind:          "person",
		CanonicalName: "Фёдор Воронов",
		Aliases:       []EntityAliasDraft{{Alias: "Федор Воронов"}},
		CreatedBy:     &ownerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.CreateEntity(ctx, ownerID, EntityDraft{
		SpaceID:       spaceID,
		Kind:          "person",
		CanonicalName: "Фёдор Петрович Воронов",
		Aliases:       []EntityAliasDraft{{Alias: "Федор Воронов"}},
		CreatedBy:     &ownerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Same alias, different kind: it must not be offered as a person identity.
	if _, err := svc.CreateEntity(ctx, ownerID, EntityDraft{
		SpaceID:       spaceID,
		Kind:          "organization",
		CanonicalName: "Федор Воронов",
		CreatedBy:     &ownerID,
	}); err != nil {
		t.Fatal(err)
	}

	hints, err := svc.ResolveEntityCandidateHints(ctx, viewerID, spaceID, []CandidateEntity{{
		Key:           "e:fedor",
		Kind:          "person",
		CanonicalName: "Фёдор Воронов",
		Aliases:       []string{"Федор Воронов"},
	}, {
		Key:           "e:donbas",
		Kind:          "place",
		CanonicalName: "Донбасс",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hints) != 2 {
		t.Fatalf("hints=%+v, want 2", hints)
	}
	if hints[0].Status != EntityResolutionAmbiguous || len(hints[0].Matches) != 2 {
		t.Fatalf("Fedor hint=%+v, want ambiguous two-person match", hints[0])
	}
	if hints[0].Matches[0].EntityID != first.ID || hints[0].Matches[1].EntityID != second.ID {
		t.Fatalf("unexpected deterministic match order: %+v", hints[0].Matches)
	}
	for _, match := range hints[0].Matches {
		if match.Kind != "person" {
			t.Fatalf("cross-kind alias leaked into person hint: %+v", match)
		}
	}
	if hints[1].Status != EntityResolutionNone || len(hints[1].Matches) != 0 {
		t.Fatalf("Donbas hint=%+v, want none", hints[1])
	}

	if _, err := svc.ResolveEntityCandidateHints(ctx, outsiderID, spaceID, []CandidateEntity{{Key: "e:fedor", Kind: "person", CanonicalName: "Фёдор Воронов"}}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("outsider resolver error=%v, want ErrNotAuthorized", err)
	}
}

func TestResolveEntityCandidateHintsSingleHitIsStillOnlySuggestion(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	var ownerID, spaceID int64
	if err := d.QueryRow(`INSERT INTO users(username,password_hash) VALUES ('resolver-single-owner','x') RETURNING id`).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO spaces(name,slug) VALUES ('Resolver Single','resolver-single') RETURNING id`).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO space_members(space_id,user_id,role) VALUES ($1,$2,'owner')`, spaceID, ownerID); err != nil {
		t.Fatal(err)
	}
	svc := NewService(d)
	entity, err := svc.CreateEntity(ctx, ownerID, EntityDraft{SpaceID: spaceID, Kind: "place", CanonicalName: "Донбасс", CreatedBy: &ownerID})
	if err != nil {
		t.Fatal(err)
	}
	hints, err := svc.ResolveEntityCandidateHints(ctx, ownerID, spaceID, []CandidateEntity{{Key: "e:donbas", Kind: "place", CanonicalName: "Донбасс"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hints) != 1 || hints[0].Status != EntityResolutionSingle || len(hints[0].Matches) != 1 || hints[0].Matches[0].EntityID != entity.ID {
		t.Fatalf("single exact hint=%+v", hints)
	}
}
