package semantic

import "testing"

func TestNormalizeAlias(t *testing.T) {
	got := NormalizeAlias("  ФЁДОР   ВОРОНОВ  ")
	if got != "фёдор воронов" {
		t.Fatalf("NormalizeAlias() = %q, want %q", got, "фёдор воронов")
	}
}
