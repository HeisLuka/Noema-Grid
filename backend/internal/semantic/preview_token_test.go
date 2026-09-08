package semantic

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPreviewSignerRoundTripTamperAndExpiry(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	signer, err := NewPreviewSigner([]byte("0123456789abcdef0123456789abcdef"), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	signer.now = func() time.Time { return now }

	token, want, err := signer.Sign(11, 22, strings.Repeat("a", 64), ProfileGenealogy, strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	got, err := signer.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("verified claims = %+v, want %+v", got, want)
	}

	parts := strings.Split(token, ".")
	parts[0] = mutateBase64TokenPart(parts[0])
	if _, err := signer.Verify(strings.Join(parts, ".")); !errors.Is(err, ErrInvalidPreviewToken) {
		t.Fatalf("tampered token error = %v, want ErrInvalidPreviewToken", err)
	}

	signer.now = func() time.Time { return now.Add(10 * time.Minute) }
	if _, err := signer.Verify(token); !errors.Is(err, ErrExpiredPreviewToken) {
		t.Fatalf("expired token error = %v, want ErrExpiredPreviewToken", err)
	}
}

func TestCandidatePayloadHashIsOrderAndJSONFormattingStable(t *testing.T) {
	e1 := "e1"
	first := CandidateSet{
		Entities: []CandidateEntity{
			{Key: "e2", Kind: "place", CanonicalName: "Донбасс", Aliases: []string{"Donbas", "Донбасс"}},
			{Key: "e1", Kind: "person", CanonicalName: "Фёдор Воронов", Aliases: []string{"Fedor Voronov", "Федор Воронов"}},
		},
		Claims: []CandidateClaim{{
			Key:           "c1",
			CanonicalText: "Фёдор Воронов вернулся домой в 1948 году",
			ClaimType:     "fact",
			Predicate:     "person.returned_home",
			Qualifiers:    json.RawMessage(`{ "certainty": "reported", "a": 1 }`),
			Slots: []CandidateSlot{
				{Role: "year", Ordinal: 0, LiteralKind: "integer", Literal: json.RawMessage(` 1948 `)},
				{Role: "subject", Ordinal: 0, EntityKey: &e1},
			},
		}},
		Regions: []CandidateRegion{{Key: "r1", LocatorKind: "paragraph", Locator: json.RawMessage(`{"paragraph": 1}`), Excerpt: "Домой вернулся в 1948 году.", ExcerptHash: "x"}},
		Warnings: []string{"z warning", "a warning"},
	}
	second := CandidateSet{
		Entities: []CandidateEntity{
			{Key: "e1", Kind: "person", CanonicalName: "Фёдор Воронов", Aliases: []string{"Федор Воронов", "Fedor Voronov"}},
			{Key: "e2", Kind: "place", CanonicalName: "Донбасс", Aliases: []string{"Донбасс", "Donbas"}},
		},
		Claims: []CandidateClaim{{
			Key:           "c1",
			CanonicalText: "Фёдор Воронов вернулся домой в 1948 году",
			ClaimType:     "fact",
			Predicate:     "person.returned_home",
			Qualifiers:    json.RawMessage(`{"a":1,"certainty":"reported"}`),
			Slots: []CandidateSlot{
				{Role: "subject", Ordinal: 0, EntityKey: &e1},
				{Role: "year", Ordinal: 0, LiteralKind: "integer", Literal: json.RawMessage(`1948`)},
			},
		}},
		Regions: []CandidateRegion{{Key: "r1", LocatorKind: "paragraph", Locator: json.RawMessage(`{ "paragraph" : 1 }`), Excerpt: "Домой вернулся в 1948 году.", ExcerptHash: "x"}},
		Warnings: []string{"a warning", "z warning"},
	}

	h1, err := CandidatePayloadHash(first)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := CandidatePayloadHash(second)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("candidate hashes differ: %s != %s", h1, h2)
	}
}

func TestPageSnapshotHashCoversTitleAndBody(t *testing.T) {
	base := PageSnapshotHash("Fedor", "body")
	if base == PageSnapshotHash("Fedor changed", "body") {
		t.Fatal("title change did not change page snapshot hash")
	}
	if base == PageSnapshotHash("Fedor", "body changed") {
		t.Fatal("body change did not change page snapshot hash")
	}
}

func mutateBase64TokenPart(s string) string {
	if s == "" {
		return "A"
	}
	b := []byte(s)
	if b[len(b)-1] == 'A' {
		b[len(b)-1] = 'B'
	} else {
		b[len(b)-1] = 'A'
	}
	return string(b)
}
