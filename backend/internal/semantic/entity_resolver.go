package semantic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	EntityResolutionNone      = "none"
	EntityResolutionSingle    = "single_exact_alias"
	EntityResolutionAmbiguous = "ambiguous_exact_alias"
)

type EntityResolutionMatch struct {
	EntityID       int64    `json:"entity_id"`
	Kind           string   `json:"kind"`
	CanonicalName  string   `json:"canonical_name"`
	MatchedAliases []string `json:"matched_aliases"`
}

type EntityResolutionHint struct {
	CandidateKey string                  `json:"candidate_key"`
	Status       string                  `json:"status"`
	Matches      []EntityResolutionMatch `json:"matches,omitempty"`
}

// ResolveEntityCandidateHints is a deterministic, read-only first pass for
// candidate entity resolution. It deliberately never selects an identity: even
// one exact alias hit is only a suggestion. The eventual CommitPreview caller
// must explicitly choose create_new or an existing_entity_id, and commit checks
// that choice again inside its write transaction.
func (s *Service) ResolveEntityCandidateHints(ctx context.Context, userID, spaceID int64, candidates []CandidateEntity) ([]EntityResolutionHint, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("semantic entity resolver: nil database")
	}
	if err := requireSpaceAccess(ctx, s.db, userID, spaceID, false); err != nil {
		return nil, err
	}

	out := make([]EntityResolutionHint, 0, len(candidates))
	for _, candidate := range candidates {
		hint, err := resolveOneEntityCandidate(ctx, s.db, spaceID, candidate)
		if err != nil {
			return nil, err
		}
		out = append(out, hint)
	}
	return out, nil
}

func resolveOneEntityCandidate(ctx context.Context, db *sql.DB, spaceID int64, candidate CandidateEntity) (EntityResolutionHint, error) {
	hint := EntityResolutionHint{CandidateKey: candidate.Key, Status: EntityResolutionNone}
	kind := strings.ToLower(strings.TrimSpace(candidate.Kind))
	if !validEntityKind(kind) {
		return hint, nil
	}

	aliases := append([]string{candidate.CanonicalName}, candidate.Aliases...)
	normalized := make(map[string]struct{}, len(aliases))
	for _, alias := range aliases {
		norm := NormalizeAlias(alias)
		if norm != "" {
			normalized[norm] = struct{}{}
		}
	}
	if len(normalized) == 0 {
		return hint, nil
	}

	type aggregate struct {
		match   EntityResolutionMatch
		aliases map[string]struct{}
	}
	byID := make(map[int64]*aggregate)
	for norm := range normalized {
		rows, err := db.QueryContext(ctx, `
SELECT e.id, e.kind, e.canonical_name, a.alias
FROM sem_entity_aliases a
JOIN sem_entities e ON e.id = a.entity_id AND e.space_id = a.space_id
WHERE e.space_id = $1
  AND e.state = 'active'
  AND e.kind = $2
  AND a.normalized_alias = $3
ORDER BY e.id`, spaceID, kind, norm)
		if err != nil {
			return EntityResolutionHint{}, fmt.Errorf("semantic entity resolver: exact alias query: %w", err)
		}
		for rows.Next() {
			var entityID int64
			var entityKind, canonicalName, matchedAlias string
			if err := rows.Scan(&entityID, &entityKind, &canonicalName, &matchedAlias); err != nil {
				rows.Close()
				return EntityResolutionHint{}, fmt.Errorf("semantic entity resolver: scan: %w", err)
			}
			agg := byID[entityID]
			if agg == nil {
				agg = &aggregate{
					match: EntityResolutionMatch{EntityID: entityID, Kind: entityKind, CanonicalName: canonicalName},
					aliases: make(map[string]struct{}),
				}
				byID[entityID] = agg
			}
			agg.aliases[matchedAlias] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return EntityResolutionHint{}, fmt.Errorf("semantic entity resolver: iterate: %w", err)
		}
		rows.Close()
	}

	ids := make([]int64, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		agg := byID[id]
		for alias := range agg.aliases {
			agg.match.MatchedAliases = append(agg.match.MatchedAliases, alias)
		}
		sort.Strings(agg.match.MatchedAliases)
		hint.Matches = append(hint.Matches, agg.match)
	}
	switch len(hint.Matches) {
	case 0:
		hint.Status = EntityResolutionNone
	case 1:
		hint.Status = EntityResolutionSingle
	default:
		hint.Status = EntityResolutionAmbiguous
	}
	return hint, nil
}
