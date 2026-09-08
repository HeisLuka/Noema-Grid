package semantic

import (
	"encoding/json"
	"testing"
)

func TestFingerprintIgnoresWordingAndSlotOrder(t *testing.T) {
	person := int64(42)
	place := int64(77)
	a := ClaimDraft{
		CanonicalText: "Фёдор Воронов находился на Донбассе в 1946 году",
		Predicate:     "person.location",
		ValidFrom:     "1946-01-01",
		ValidTo:       "1946-12-31",
		Qualifiers:    json.RawMessage(`{"certainty":"reported","scope":"year"}`),
		Slots: []Slot{
			{Role: "subject", EntityID: &person},
			{Role: "location", EntityID: &place},
		},
	}
	b := a
	b.CanonicalText = "In 1946 Fedor Voronov was in Donbas"
	b.Qualifiers = json.RawMessage(`{"scope":"year","certainty":"reported"}`)
	b.Slots = []Slot{
		{Role: "location", EntityID: &place},
		{Role: "subject", EntityID: &person},
	}

	fa, err := Fingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := Fingerprint(b)
	if err != nil {
		t.Fatal(err)
	}
	if fa != fb {
		t.Fatalf("same proposition produced different fingerprints:\n%s\n%s", fa, fb)
	}
}

func TestFingerprintSeparatesCompetingValues(t *testing.T) {
	person := int64(42)
	a := ClaimDraft{
		Predicate: "person.returned_home",
		Slots: []Slot{
			{Role: "subject", EntityID: &person},
			{Role: "year", LiteralKind: "integer", Literal: json.RawMessage(`1947`)},
		},
	}
	b := a
	b.Slots = append([]Slot(nil), a.Slots...)
	b.Slots[1].Literal = json.RawMessage(`1948`)

	fa, err := Fingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := Fingerprint(b)
	if err != nil {
		t.Fatal(err)
	}
	if fa == fb {
		t.Fatal("competing return years collapsed to one fingerprint")
	}
}

func TestFingerprintRejectsAmbiguousSlot(t *testing.T) {
	id := int64(1)
	_, err := Fingerprint(ClaimDraft{
		Predicate: "person.location",
		Slots:     []Slot{{Role: "subject", EntityID: &id, Literal: json.RawMessage(`"x"`)}},
	})
	if err == nil {
		t.Fatal("expected ambiguous slot to fail")
	}
}
