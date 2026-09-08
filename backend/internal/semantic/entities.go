package semantic

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type EntityAliasDraft struct {
	Alias          string
	SourceRegionID *int64
	Confidence     *float64
}

type EntityDraft struct {
	SpaceID       int64
	Kind          string
	CanonicalName string
	Description   string
	ExternalIDs   json.RawMessage
	Props         json.RawMessage
	Aliases       []EntityAliasDraft
	CreatedBy     *int64
}

type EntityRecord struct {
	ID            int64
	SpaceID       int64
	Kind          string
	CanonicalName string
	Description   string
	ExternalIDs   json.RawMessage
	Props         json.RawMessage
	State         string
}

type EntityCandidate struct {
	ID            int64
	Kind          string
	CanonicalName string
	MatchedAlias  string
}

// CreateEntity deliberately does not merge by name. Names and aliases are not
// identities; a false merge is much more expensive than a duplicate. The later
// EntityResolver may rank candidates, but an explicit create stays explicit.
func (s *Service) CreateEntity(ctx context.Context, userID int64, in EntityDraft) (EntityRecord, error) {
	if s == nil || s.db == nil {
		return EntityRecord{}, errors.New("semantic entity: nil database")
	}
	kind := strings.TrimSpace(strings.ToLower(in.Kind))
	name := strings.TrimSpace(in.CanonicalName)
	if in.SpaceID == 0 || name == "" {
		return EntityRecord{}, errors.New("semantic entity: space_id and canonical_name are required")
	}
	if !validEntityKind(kind) {
		return EntityRecord{}, fmt.Errorf("semantic entity: unsupported kind %q", in.Kind)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EntityRecord{}, fmt.Errorf("semantic entity: begin: %w", err)
	}
	defer tx.Rollback()
	if err := requireSpaceAccess(ctx, tx, userID, in.SpaceID, true); err != nil {
		return EntityRecord{}, err
	}

	var out EntityRecord
	var externalIDs, props []byte
	err = tx.QueryRowContext(ctx, `
INSERT INTO sem_entities(space_id, kind, canonical_name, description, external_ids, props, created_by)
VALUES ($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7)
RETURNING id, space_id, kind, canonical_name, description, external_ids, props, state`,
		in.SpaceID, kind, name, in.Description, defaultJSON(in.ExternalIDs), defaultJSON(in.Props), in.CreatedBy,
	).Scan(&out.ID, &out.SpaceID, &out.Kind, &out.CanonicalName, &out.Description, &externalIDs, &props, &out.State)
	if err != nil {
		return EntityRecord{}, wrap("create entity", err)
	}
	out.ExternalIDs = append(json.RawMessage(nil), externalIDs...)
	out.Props = append(json.RawMessage(nil), props...)

	aliases := make(map[string]EntityAliasDraft, len(in.Aliases)+1)
	canonicalNorm := NormalizeAlias(name)
	aliases[canonicalNorm] = EntityAliasDraft{Alias: name}
	for _, alias := range in.Aliases {
		norm := NormalizeAlias(alias.Alias)
		if norm == "" {
			continue
		}
		alias.Alias = strings.TrimSpace(alias.Alias)
		aliases[norm] = alias
	}
	for norm, alias := range aliases {
		if err := insertEntityAlias(ctx, tx, in.SpaceID, out.ID, norm, alias, in.CreatedBy); err != nil {
			return EntityRecord{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return EntityRecord{}, fmt.Errorf("semantic entity: commit: %w", err)
	}
	return out, nil
}

func insertEntityAlias(ctx context.Context, tx *sql.Tx, spaceID, entityID int64, normalized string, in EntityAliasDraft, createdBy *int64) error {
	if normalized == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO sem_entity_aliases(space_id, entity_id, alias, normalized_alias, source_region_id, confidence, created_by)
VALUES ($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (entity_id, normalized_alias)
DO UPDATE SET alias = EXCLUDED.alias,
              source_region_id = COALESCE(EXCLUDED.source_region_id, sem_entity_aliases.source_region_id),
              confidence = COALESCE(EXCLUDED.confidence, sem_entity_aliases.confidence)`,
		spaceID, entityID, in.Alias, normalized, in.SourceRegionID, in.Confidence, createdBy)
	return wrap("insert entity alias", err)
}

// FindEntityCandidatesByAlias is a safe deterministic first pass for entity
// resolution. It returns every exact normalized-alias hit and never silently
// picks one when multiple entities share a name.
func (s *Service) FindEntityCandidatesByAlias(ctx context.Context, userID, spaceID int64, alias string) ([]EntityCandidate, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("semantic entity search: nil database")
	}
	if err := requireSpaceAccess(ctx, s.db, userID, spaceID, false); err != nil {
		return nil, err
	}
	norm := NormalizeAlias(alias)
	if norm == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT e.id, e.kind, e.canonical_name, a.alias
FROM sem_entity_aliases a
JOIN sem_entities e ON e.id = a.entity_id AND e.space_id = a.space_id
JOIN space_access sa ON sa.space_id = e.space_id AND sa.user_id = $1
WHERE e.space_id = $2
  AND e.state = 'active'
  AND a.normalized_alias = $3
ORDER BY e.id`, userID, spaceID, norm)
	if err != nil {
		return nil, wrap("find entity candidates", err)
	}
	defer rows.Close()
	var out []EntityCandidate
	for rows.Next() {
		var x EntityCandidate
		if err := rows.Scan(&x.ID, &x.Kind, &x.CanonicalName, &x.MatchedAlias); err != nil {
			return nil, wrap("scan entity candidate", err)
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("iterate entity candidates", err)
	}
	return out, nil
}

func (s *Service) GetEntity(ctx context.Context, userID, spaceID, entityID int64) (EntityRecord, error) {
	if s == nil || s.db == nil {
		return EntityRecord{}, errors.New("semantic get entity: nil database")
	}
	var out EntityRecord
	var externalIDs, props []byte
	err := s.db.QueryRowContext(ctx, `
SELECT e.id, e.space_id, e.kind, e.canonical_name, e.description, e.external_ids, e.props, e.state
FROM sem_entities e
JOIN space_access sa ON sa.space_id = e.space_id AND sa.user_id = $1
WHERE e.space_id = $2 AND e.id = $3`, userID, spaceID, entityID).Scan(
		&out.ID, &out.SpaceID, &out.Kind, &out.CanonicalName, &out.Description, &externalIDs, &props, &out.State,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return EntityRecord{}, ErrNotAuthorized
	}
	if err != nil {
		return EntityRecord{}, wrap("get entity", err)
	}
	out.ExternalIDs = append(json.RawMessage(nil), externalIDs...)
	out.Props = append(json.RawMessage(nil), props...)
	return out, nil
}

func NormalizeAlias(v string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(v)), " "))
}

func validEntityKind(kind string) bool {
	switch kind {
	case "person", "organization", "place", "event", "concept", "object", "work", "other":
		return true
	default:
		return false
	}
}
