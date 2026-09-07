package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/zcag/tela/backend/internal/atlas/core"
	"github.com/zcag/tela/backend/internal/atlas/source"
)

// The per-run file cap was unreachable for every connector that matters: the
// check lived in inventoryStage's non-progress arm, and git + jira both
// implement ProgressConnector. It refused nothing for a month while 35 runs went
// over it. These tests pin the cap to the arm that actually runs — a connector
// WITH the progress upgrade — so it cannot regress back into a dead branch.

// capConn is a ProgressConnector that hands back n files, i.e. the shape of the
// real git connector as far as inventoryStage is concerned.
type capConn struct{ n int }

func (capConn) Type() string { return "test-cap" }
func (capConn) Acquire(context.Context, core.Source, string) (source.Snapshot, error) {
	return source.Snapshot{}, nil
}

func (c capConn) files() []core.File {
	out := make([]core.File, c.n)
	for i := range out {
		out[i] = core.File{Path: "f", Lang: "go"}
	}
	return out
}

func (c capConn) Inventory(context.Context, source.Snapshot, core.Source) ([]core.File, error) {
	return c.files(), nil
}

func (c capConn) InventoryWithProgress(_ context.Context, _ source.Snapshot, _ core.Source,
	onScan func(int), _ source.Progress) ([]core.File, source.InventoryReport, error) {
	if onScan != nil {
		onScan(c.n)
	}
	return c.files(), source.InventoryReport{Tracked: c.n, Langs: 1}, nil
}

func (capConn) Spine(context.Context, source.Snapshot, []core.File) ([]core.SpineItem, error) {
	return nil, nil
}

func (capConn) SpineWithProgress(context.Context, source.Snapshot, []core.File, source.Progress) ([]core.SpineItem, error) {
	return nil, nil
}

func (capConn) Delta(context.Context, source.Snapshot, core.Source, string, string) (source.ChangeSet, error) {
	return source.ChangeSet{}, nil
}

func (capConn) HasChanges(context.Context, core.Source, string) (bool, error) { return true, nil }

// capStore records SaveFiles so a test can assert nothing was persisted for a
// refused run. Embedding the interface keeps it to the two methods this stage
// touches; anything else would panic loudly rather than pass silently.
type capStore struct {
	EngineStore
	saved int
	calls int
}

func (s *capStore) SaveFiles(_ int64, files []core.File) error {
	s.calls++
	s.saved = len(files)
	return nil
}
func (s *capStore) AppendEvent(core.Event) error { return nil }

// runInventory wires a RunContext around a fake connector registered under its
// own source type, and runs the inventory stage.
func runInventory(t *testing.T, files, maxFiles int) (*capStore, error) {
	t.Helper()
	conn := capConn{n: files}
	connectors[conn.Type()] = conn
	t.Cleanup(func() { delete(connectors, conn.Type()) })

	st := &capStore{}
	rc := &RunContext{
		Source:   &core.Source{Type: core.SourceType(conn.Type())},
		Run:      &core.Run{ID: 1},
		Store:    st,
		MaxFiles: maxFiles,
	}
	return st, inventoryStage{}.Run(context.Background(), rc)
}

// TestInventoryCap_RefusesOversizedProgressConnector is the regression: a git-
// shaped connector over the cap must be refused. Before the fix this returned
// nil and the run went on to spend four hours of GPU.
func TestInventoryCap_RefusesOversizedProgressConnector(t *testing.T) {
	st, err := runInventory(t, 4373, 1000)
	if err == nil {
		t.Fatal("4,373 files against a 1,000 cap was accepted — the cap is back in a dead branch")
	}
	if !strings.Contains(err.Error(), "over this plan's limit") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	if st.calls != 0 {
		t.Errorf("a refused run persisted %d files; it must cost nothing past the clone", st.saved)
	}
}

// TestInventoryCap_AdmitsWithinCap keeps the guard from becoming a blanket
// refusal, and pins that an unlimited plan (0) still means unlimited.
func TestInventoryCap_AdmitsWithinCap(t *testing.T) {
	for _, tc := range []struct{ files, max int }{
		{999, 1000},  // under
		{1000, 1000}, // exactly at the cap is allowed
		{4373, 0},    // 0 = unlimited
	} {
		st, err := runInventory(t, tc.files, tc.max)
		if err != nil {
			t.Errorf("%d files under a cap of %d was refused: %v", tc.files, tc.max, err)
			continue
		}
		if st.saved != tc.files {
			t.Errorf("%d files under a cap of %d: saved %d", tc.files, tc.max, st.saved)
		}
	}
}
