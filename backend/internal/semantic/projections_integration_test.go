package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zcag/tela/backend/internal/testdb"
)

func TestTimelineProjectionMaterializesRevisionedBindingsWithExactProvenance(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	var ownerID, viewerID, outsiderID, spaceID int64
	for _, item := range []struct {
		username string
		id       *int64
	}{
		{"projection-owner", &ownerID},
		{"projection-viewer", &viewerID},
		{"projection-outsider", &outsiderID},
	} {
		if err := d.QueryRow(`INSERT INTO users(username,password_hash) VALUES ($1,'x') RETURNING id`, item.username).Scan(item.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.QueryRow(`INSERT INTO spaces(name,slug) VALUES ('Projection Fixture','projection-fixture') RETURNING id`).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO space_members(space_id,user_id,role) VALUES ($1,$2,'owner'),($1,$3,'viewer')`, spaceID, ownerID, viewerID); err != nil {
		t.Fatal(err)
	}

	svc := NewService(d)
	fedor, err := svc.CreateEntity(ctx, ownerID, EntityDraft{SpaceID: spaceID, Kind: "person", CanonicalName: "Фёдор Воронов", CreatedBy: &ownerID})
	if err != nil {
		t.Fatal(err)
	}
	donbas, err := svc.CreateEntity(ctx, ownerID, EntityDraft{SpaceID: spaceID, Kind: "place", CanonicalName: "Донбасс", CreatedBy: &ownerID})
	if err != nil {
		t.Fatal(err)
	}
	tractor, err := svc.CreateEntity(ctx, ownerID, EntityDraft{SpaceID: spaceID, Kind: "concept", CanonicalName: "тракторист", CreatedBy: &ownerID})
	if err != nil {
		t.Fatal(err)
	}

	c1 := mustIngestFixtureClaim(t, ctx, svc, ownerID, fixtureClaim{
		spaceID: spaceID, uri: "manual://projection/liberated", hash: "projection-liberated-v1",
		excerpt: "Фёдор Воронов был освобождён в 1945 году.",
		claim: ClaimDraft{SpaceID: spaceID, CanonicalText: "Фёдор Воронов был освобождён в 1945 году", ClaimType: "fact", Predicate: "person.liberated", ValidFrom: "1945-01-01", ValidTo: "1945-12-31", TimePrecision: "year", Slots: []Slot{entitySlot("subject", fedor.ID), literalSlot("year", "integer", `1945`)}, CreatedBy: &ownerID},
		stance: "affirms",
	})
	c2 := mustIngestFixtureClaim(t, ctx, svc, ownerID, fixtureClaim{
		spaceID: spaceID, uri: "manual://projection/returned", hash: "projection-returned-v1",
		excerpt: "Домой Фёдор вернулся в 1948 году.",
		claim: returnedHomeClaim(spaceID, fedor.ID, 1948, "Фёдор Воронов вернулся домой в 1948 году", &ownerID), stance: "affirms",
	})
	c3 := mustIngestFixtureClaim(t, ctx, svc, ownerID, fixtureClaim{
		spaceID: spaceID, uri: "manual://projection/donbas", hash: "projection-donbas-v1",
		excerpt: "После освобождения и до возвращения домой Фёдор находился на Донбассе.",
		claim: ClaimDraft{SpaceID: spaceID, CanonicalText: "Фёдор Воронов находился на Донбассе между освобождением и возвращением домой", ClaimType: "fact", Predicate: "person.location", ValidFrom: "1945-01-01", ValidTo: "1948-12-31", TimePrecision: "year_range", Slots: []Slot{entitySlot("subject", fedor.ID), entitySlot("location", donbas.ID)}, CreatedBy: &ownerID},
		stance: "affirms",
	})
	c4 := mustIngestFixtureClaim(t, ctx, svc, ownerID, fixtureClaim{
		spaceID: spaceID, uri: "manual://projection/occupation", hash: "projection-occupation-v1",
		excerpt: "После возвращения работал трактористом.",
		claim: ClaimDraft{SpaceID: spaceID, CanonicalText: "После возвращения Фёдор Воронов работал трактористом", ClaimType: "fact", Predicate: "person.occupation", ValidFrom: "1948-01-01", TimePrecision: "year_or_later", Slots: []Slot{entitySlot("subject", fedor.ID), entitySlot("occupation", tractor.ID)}, CreatedBy: &ownerID},
		stance: "affirms",
	})

	projection, err := svc.CreateProjection(ctx, ownerID, spaceID, ProjectionInput{
		Name: "Фёдор Воронов — 1945–1948",
		Spec: TimelineSheetSpec{SubjectEntityID: fedor.ID, StartYear: 1945, EndYear: 1948, SheetName: "Timeline"},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := svc.BuildProjectionSnapshot(ctx, ownerID, spaceID, projection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CurrentRevision != 0 || snapshot.NextRevision != 1 || snapshot.SheetName != "Timeline" {
		t.Fatalf("unexpected first snapshot: %+v", snapshot)
	}
	for _, want := range []string{
		"## Sheet: Timeline",
		"| Person | 1945 | 1946 | 1947 | 1948 |",
		"| Фёдор Воронов | liberated; Донбасс | Донбасс | Донбасс | Донбасс; тракторист; returned home |",
	} {
		if !strings.Contains(snapshot.Body, want) {
			t.Fatalf("projection body missing %q:\n%s", want, snapshot.Body)
		}
	}

	props := map[string]any{
		"sheet": true,
		"semantic_projection_managed": true,
		"semantic_projection_id": projection.ID,
		"semantic_projection_revision": snapshot.NextRevision,
		"semantic_projection_snapshot_hash": snapshot.SnapshotHash,
	}
	propsJSON, _ := json.Marshal(props)
	var pageID int64
	if err := d.QueryRow(`INSERT INTO pages(space_id,title,body,props) VALUES ($1,$2,$3,$4::jsonb) RETURNING id`, spaceID, projection.Name, snapshot.Body, string(propsJSON)).Scan(&pageID); err != nil {
		t.Fatal(err)
	}
	materialized, err := svc.FinalizeProjectionSnapshot(ctx, ownerID, snapshot, pageID)
	if err != nil {
		t.Fatal(err)
	}
	if materialized.Revision != 1 || materialized.TargetPageID == nil || *materialized.TargetPageID != pageID {
		t.Fatalf("unexpected finalized projection: %+v", materialized)
	}

	resolved, err := svc.ResolveSheetCell(ctx, viewerID, spaceID, pageID, "Timeline", "e2")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ProjectionRevision != 1 || len(resolved.Bindings) != 3 {
		t.Fatalf("unexpected 1948 resolution: %+v", resolved)
	}
	wantClaims := map[int64]bool{c2.ClaimID: false, c3.ClaimID: false, c4.ClaimID: false}
	for _, binding := range resolved.Bindings {
		if binding.ClaimID == nil {
			t.Fatalf("1948 cell contains non-claim binding: %+v", binding)
		}
		if _, ok := wantClaims[*binding.ClaimID]; !ok {
			t.Fatalf("unexpected claim in 1948 cell: %+v", binding)
		}
		wantClaims[*binding.ClaimID] = true
		if len(binding.Traces) == 0 || binding.Traces[0].SourceRegionID == 0 || binding.Traces[0].SourceVersionID == 0 || binding.Traces[0].Excerpt == "" || binding.Traces[0].ContentHash == "" {
			t.Fatalf("cell did not trace to exact evidence: %+v", binding)
		}
	}
	for claimID, seen := range wantClaims {
		if !seen {
			t.Fatalf("claim %d was not bound into E2", claimID)
		}
	}
	if _, err := svc.ResolveSheetCell(ctx, outsiderID, spaceID, pageID, "Timeline", "E2"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("outsider resolution error=%v, want ErrNotAuthorized", err)
	}

	second, err := svc.BuildProjectionSnapshot(ctx, ownerID, spaceID, projection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.CurrentRevision != 1 || second.NextRevision != 2 {
		t.Fatalf("unexpected second snapshot revision: %+v", second)
	}
	props["semantic_projection_revision"] = second.NextRevision
	props["semantic_projection_snapshot_hash"] = second.SnapshotHash
	propsJSON, _ = json.Marshal(props)
	if _, err := d.Exec(`UPDATE pages SET body=$1,props=$2::jsonb WHERE id=$3`, second.Body, string(propsJSON), pageID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FinalizeProjectionSnapshot(ctx, ownerID, second, pageID); err != nil {
		t.Fatal(err)
	}
	var rev1Bindings, rev2Bindings int
	if err := d.QueryRow(`SELECT count(*) FROM sem_projection_bindings WHERE projection_id=$1 AND projection_revision=1`, projection.ID).Scan(&rev1Bindings); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM sem_projection_bindings WHERE projection_id=$1 AND projection_revision=2`, projection.ID).Scan(&rev2Bindings); err != nil {
		t.Fatal(err)
	}
	if rev1Bindings == 0 || rev2Bindings == 0 || rev1Bindings != rev2Bindings {
		t.Fatalf("revisioned bindings not retained: rev1=%d rev2=%d", rev1Bindings, rev2Bindings)
	}
	resolved2, err := svc.ResolveSheetCell(ctx, viewerID, spaceID, pageID, "Timeline", "E2")
	if err != nil || resolved2.ProjectionRevision != 2 {
		t.Fatalf("current binding resolution should use rev2: %+v err=%v", resolved2, err)
	}

	third, err := svc.BuildProjectionSnapshot(ctx, ownerID, spaceID, projection.ID)
	if err != nil {
		t.Fatal(err)
	}
	props["semantic_projection_managed"] = false
	props["semantic_projection_revision"] = third.NextRevision
	props["semantic_projection_snapshot_hash"] = third.SnapshotHash
	propsJSON, _ = json.Marshal(props)
	if _, err := d.Exec(`UPDATE pages SET body=$1,props=$2::jsonb WHERE id=$3`, third.Body, string(propsJSON), pageID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FinalizeProjectionSnapshot(ctx, ownerID, third, pageID); !errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("unmanaged page finalize error=%v, want ErrProjectionConflict", err)
	}
	var finalRevision int
	if err := d.QueryRow(`SELECT revision FROM sem_projections WHERE id=$1`, projection.ID).Scan(&finalRevision); err != nil {
		t.Fatal(err)
	}
	if finalRevision != 2 {
		t.Fatalf("conflicting finalize advanced revision to %d", finalRevision)
	}

	_ = c1 // C1 is asserted through the rendered 1945 cell.
}
