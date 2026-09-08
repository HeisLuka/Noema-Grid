package semantic

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Fingerprint computes deterministic structural claim identity. CanonicalText is
// intentionally excluded: wording may change without changing the proposition.
func Fingerprint(d ClaimDraft) (string, error) {
	predicate := strings.TrimSpace(strings.ToLower(d.Predicate))
	if predicate == "" {
		return "", errors.New("semantic fingerprint: predicate is required")
	}
	if len(d.Slots) == 0 {
		return "", errors.New("semantic fingerprint: at least one slot is required")
	}

	type normalizedSlot struct {
		Role        string          `json:"role"`
		Ordinal     int             `json:"ordinal"`
		EntityID    *int64          `json:"entity_id,omitempty"`
		Literal     json.RawMessage `json:"literal,omitempty"`
		LiteralKind string          `json:"literal_kind,omitempty"`
	}
	type payload struct {
		Predicate  string           `json:"predicate"`
		ValidFrom  string           `json:"valid_from,omitempty"`
		ValidTo    string           `json:"valid_to,omitempty"`
		Qualifiers json.RawMessage  `json:"qualifiers"`
		Slots      []normalizedSlot `json:"slots"`
	}

	slots := make([]normalizedSlot, 0, len(d.Slots))
	for _, s := range d.Slots {
		role := strings.TrimSpace(strings.ToLower(s.Role))
		if role == "" {
			return "", errors.New("semantic fingerprint: slot role is required")
		}
		if s.Ordinal < 0 {
			return "", fmt.Errorf("semantic fingerprint: negative ordinal for role %q", role)
		}
		hasEntity := s.EntityID != nil
		hasLiteral := len(bytes.TrimSpace(s.Literal)) > 0
		if hasEntity == hasLiteral {
			return "", fmt.Errorf("semantic fingerprint: slot %q must have exactly one target", role)
		}
		var literal json.RawMessage
		if hasLiteral {
			var v any
			if err := json.Unmarshal(s.Literal, &v); err != nil {
				return "", fmt.Errorf("semantic fingerprint: invalid literal for role %q: %w", role, err)
			}
			b, err := json.Marshal(v)
			if err != nil {
				return "", err
			}
			literal = b
		}
		slots = append(slots, normalizedSlot{
			Role:        role,
			Ordinal:     s.Ordinal,
			EntityID:    s.EntityID,
			Literal:     literal,
			LiteralKind: strings.TrimSpace(strings.ToLower(s.LiteralKind)),
		})
	}
	sort.Slice(slots, func(i, j int) bool {
		if slots[i].Role != slots[j].Role {
			return slots[i].Role < slots[j].Role
		}
		return slots[i].Ordinal < slots[j].Ordinal
	})

	qualifiers := json.RawMessage(`{}`)
	if len(bytes.TrimSpace(d.Qualifiers)) > 0 {
		var v any
		if err := json.Unmarshal(d.Qualifiers, &v); err != nil {
			return "", fmt.Errorf("semantic fingerprint: invalid qualifiers: %w", err)
		}
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		qualifiers = b
	}

	b, err := json.Marshal(payload{
		Predicate:  predicate,
		ValidFrom:  strings.TrimSpace(d.ValidFrom),
		ValidTo:    strings.TrimSpace(d.ValidTo),
		Qualifiers: qualifiers,
		Slots:      slots,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
