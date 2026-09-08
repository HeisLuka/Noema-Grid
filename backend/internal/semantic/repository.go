package semantic

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// IngestManual persists the Phase-0 provenance chain and canonical claim in one
// transaction. It is intentionally a low-level repository primitive for tests
// and internal maintenance jobs. User-facing callers should go through Service,
// which uses IngestManualAuthorized and checks live space_access in the same tx.
func (r *Repository) IngestManual(ctx context.Context, in ManualIngest) (IngestResult, error) {
	return r.ingestManual(ctx, 0, false, in)
}

// IngestManualAuthorized performs the same atomic ingest, but the write is
// rejected unless userID is a live owner/editor of the target space. The access
// check and write share one transaction, avoiding a check-then-write race.
func (r *Repository) IngestManualAuthorized(ctx context.Context, userID int64, in ManualIngest) (IngestResult, error) {
	return r.ingestManual(ctx, userID, true, in)
}

func (r *Repository) ingestManual(ctx context.Context, userID int64, authorize bool, in ManualIngest) (IngestResult, error) {
	if r == nil || r.db == nil {
		return IngestResult{}, errors.New("semantic ingest: nil database")
	}
	if in.Source.SpaceID == 0 || in.Claim.SpaceID == 0 || in.Source.SpaceID != in.Claim.SpaceID {
		return IngestResult{}, errors.New("semantic ingest: source and claim must share a non-zero space")
	}
	fingerprint, err := Fingerprint(in.Claim)
	if err != nil {
		return IngestResult{}, err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return IngestResult{}, fmt.Errorf("semantic ingest: begin: %w", err)
	}
	defer tx.Rollback()
	if authorize {
		if err := requireSpaceAccess(ctx, tx, userID, in.Source.SpaceID, true); err != nil {
			return IngestResult{}, err
		}
	}

	res := IngestResult{Fingerprint: fingerprint}
	if res.SourceID, err = upsertSource(ctx, tx, in.Source); err != nil {
		return IngestResult{}, err
	}
	if res.SourceVersionID, err = upsertSourceVersion(ctx, tx, in.Source.SpaceID, res.SourceID, in.Version); err != nil {
		return IngestResult{}, err
	}
	if res.SourceRegionID, err = upsertSourceRegion(ctx, tx, in.Source.SpaceID, res.SourceVersionID, in.Region); err != nil {
		return IngestResult{}, err
	}
	if res.ClaimID, err = upsertClaim(ctx, tx, in.Claim, fingerprint); err != nil {
		return IngestResult{}, err
	}
	if err = replaceClaimSlots(ctx, tx, in.Claim.SpaceID, res.ClaimID, in.Claim.Slots); err != nil {
		return IngestResult{}, err
	}
	if res.ClaimInstanceID, err = upsertClaimInstance(ctx, tx, in.Claim.SpaceID, res.ClaimID, res.SourceRegionID, in.Instance); err != nil {
		return IngestResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return IngestResult{}, fmt.Errorf("semantic ingest: commit: %w", err)
	}
	return res, nil
}

func upsertSource(ctx context.Context, tx *sql.Tx, s SourceDraft) (int64, error) {
	metadata := defaultJSON(s.Metadata)
	var id int64

	// Logical source identity is kind-specific. It deliberately does not use a
	// generic COALESCE key because page ids, file ids and URIs are different
	// namespaces with different lifecycle semantics.
	switch s.Kind {
	case "tela_page":
		if s.PageID == nil {
			return 0, errors.New("semantic source: tela_page requires page_id")
		}
		err := tx.QueryRowContext(ctx, `
INSERT INTO sem_sources(space_id, kind, page_id, title, metadata, created_by)
VALUES ($1,'tela_page',$2,$3,$4::jsonb,$5)
ON CONFLICT (space_id, page_id) WHERE page_id IS NOT NULL
DO UPDATE SET title = EXCLUDED.title, metadata = EXCLUDED.metadata, updated_at = tela_now()
RETURNING id`, s.SpaceID, *s.PageID, s.Title, metadata, s.CreatedBy).Scan(&id)
		return id, wrap("upsert page source", err)
	case "tela_file":
		if s.SpaceFileID == nil {
			return 0, errors.New("semantic source: tela_file requires space_file_id")
		}
		err := tx.QueryRowContext(ctx, `
INSERT INTO sem_sources(space_id, kind, space_file_id, title, metadata, created_by)
VALUES ($1,'tela_file',$2,$3,$4::jsonb,$5)
ON CONFLICT (space_id, space_file_id) WHERE space_file_id IS NOT NULL
DO UPDATE SET title = EXCLUDED.title, metadata = EXCLUDED.metadata, updated_at = tela_now()
RETURNING id`, s.SpaceID, *s.SpaceFileID, s.Title, metadata, s.CreatedBy).Scan(&id)
		return id, wrap("upsert file source", err)
	case "url", "manual":
		if s.URI == nil || strings.TrimSpace(*s.URI) == "" {
			return 0, fmt.Errorf("semantic source: %s requires uri", s.Kind)
		}
		err := tx.QueryRowContext(ctx, `
INSERT INTO sem_sources(space_id, kind, uri, title, metadata, created_by)
VALUES ($1,$2,$3,$4,$5::jsonb,$6)
ON CONFLICT (space_id, uri) WHERE uri IS NOT NULL
DO UPDATE SET title = EXCLUDED.title, metadata = EXCLUDED.metadata, updated_at = tela_now()
RETURNING id`, s.SpaceID, s.Kind, strings.TrimSpace(*s.URI), s.Title, metadata, s.CreatedBy).Scan(&id)
		return id, wrap("upsert uri source", err)
	default:
		return 0, fmt.Errorf("semantic source: unsupported kind %q", s.Kind)
	}
}

func upsertSourceVersion(ctx context.Context, tx *sql.Tx, spaceID, sourceID int64, v SourceVersionDraft) (int64, error) {
	if strings.TrimSpace(v.ContentHash) == "" {
		return 0, errors.New("semantic source version: content_hash is required")
	}
	observedAt := nullableString(v.ObservedAt)
	var id int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO sem_source_versions(space_id, source_id, content_hash, observed_at, mime, byte_size, metadata)
VALUES ($1,$2,$3,COALESCE($4,tela_now()),$5,$6,$7::jsonb)
ON CONFLICT (source_id, content_hash)
DO UPDATE SET source_id = EXCLUDED.source_id
RETURNING id`, spaceID, sourceID, strings.TrimSpace(v.ContentHash), observedAt, v.MIME, v.ByteSize, defaultJSON(v.Metadata)).Scan(&id)
	return id, wrap("upsert source version", err)
}

func upsertSourceRegion(ctx context.Context, tx *sql.Tx, spaceID, versionID int64, r SourceRegionDraft) (int64, error) {
	if strings.TrimSpace(r.LocatorKind) == "" || strings.TrimSpace(r.ExcerptHash) == "" {
		return 0, errors.New("semantic source region: locator_kind and excerpt_hash are required")
	}
	var id int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO sem_source_regions(space_id, source_version_id, locator_kind, locator, excerpt, excerpt_hash)
VALUES ($1,$2,$3,$4::jsonb,$5,$6)
ON CONFLICT (source_version_id, locator_kind, locator, excerpt_hash)
DO UPDATE SET source_version_id = EXCLUDED.source_version_id
RETURNING id`, spaceID, versionID, strings.TrimSpace(r.LocatorKind), defaultJSON(r.Locator), r.Excerpt, strings.TrimSpace(r.ExcerptHash)).Scan(&id)
	return id, wrap("upsert source region", err)
}

func upsertClaim(ctx context.Context, tx *sql.Tx, c ClaimDraft, fingerprint string) (int64, error) {
	claimType := strings.TrimSpace(c.ClaimType)
	if claimType == "" {
		claimType = "fact"
	}
	var id int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO sem_claims(
    space_id, canonical_text, claim_type, predicate, claim_fingerprint,
    valid_from, valid_to, time_precision, time_note, qualifiers, created_by)
VALUES ($1,$2,$3,$4,$5,NULLIF($6,'')::date,NULLIF($7,'')::date,NULLIF($8,''),$9,$10::jsonb,$11)
ON CONFLICT (space_id, claim_fingerprint) WHERE claim_fingerprint IS NOT NULL AND state = 'active'
DO UPDATE SET canonical_text = EXCLUDED.canonical_text,
              updated_at = tela_now()
RETURNING id`, c.SpaceID, c.CanonicalText, claimType, strings.TrimSpace(strings.ToLower(c.Predicate)), fingerprint,
		strings.TrimSpace(c.ValidFrom), strings.TrimSpace(c.ValidTo), strings.TrimSpace(c.TimePrecision), c.TimeNote,
		defaultJSON(c.Qualifiers), c.CreatedBy).Scan(&id)
	return id, wrap("upsert claim", err)
}

func replaceClaimSlots(ctx context.Context, tx *sql.Tx, spaceID, claimID int64, slots []Slot) error {
	// A fingerprint collision/retry must describe the same structural slots. We
	// therefore validate rather than blindly mutate identity-bearing data.
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sem_claim_slots WHERE space_id=$1 AND claim_id=$2`, spaceID, claimID).Scan(&existing); err != nil {
		return wrap("count claim slots", err)
	}
	if existing > 0 {
		return nil
	}
	for _, s := range slots {
		_, err := tx.ExecContext(ctx, `
INSERT INTO sem_claim_slots(space_id, claim_id, role, ordinal, entity_id, literal, literal_kind)
VALUES ($1,$2,$3,$4,$5,$6::jsonb,NULLIF($7,''))`, spaceID, claimID, strings.TrimSpace(strings.ToLower(s.Role)), s.Ordinal,
			s.EntityID, nullableJSON(s.Literal), strings.TrimSpace(strings.ToLower(s.LiteralKind)))
		if err != nil {
			return wrap("insert claim slot", err)
		}
	}
	return nil
}

func upsertClaimInstance(ctx context.Context, tx *sql.Tx, spaceID, claimID, regionID int64, in ClaimInstanceDraft) (int64, error) {
	stance := strings.TrimSpace(strings.ToLower(in.Stance))
	if stance == "" {
		stance = "affirms"
	}
	if stance != "affirms" && stance != "denies" {
		return 0, fmt.Errorf("semantic claim instance: unsupported stance %q", in.Stance)
	}
	createdBy := strings.TrimSpace(in.CreatedBy)
	if createdBy == "" {
		createdBy = "manual"
	}
	var id int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO sem_claim_instances(
    space_id, claim_id, source_region_id, original_text,
    proposed_canonical_text, context, stance, extraction_confidence, created_by)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (claim_id, source_region_id, stance)
DO UPDATE SET original_text = EXCLUDED.original_text,
              proposed_canonical_text = EXCLUDED.proposed_canonical_text,
              context = EXCLUDED.context,
              extraction_confidence = EXCLUDED.extraction_confidence
RETURNING id`, spaceID, claimID, regionID, in.OriginalText, in.ProposedCanonicalText,
		in.Context, stance, in.ExtractionConfidence, createdBy).Scan(&id)
	return id, wrap("upsert claim instance", err)
}

// ClaimTraces returns provenance only after a live authorization join through
// space_access. No cached semantic permission is consulted.
func (r *Repository) ClaimTraces(ctx context.Context, userID, spaceID, claimID int64) ([]ClaimTrace, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT c.id, c.canonical_text, COALESCE(c.predicate,''), COALESCE(c.claim_fingerprint,''),
       ci.id, ci.stance, ci.original_text,
       sr.id, sr.locator_kind, sr.locator, sr.excerpt,
       sv.id, sv.content_hash,
       s.id, s.kind, s.title, s.uri
FROM sem_claims c
JOIN space_access sa ON sa.space_id = c.space_id AND sa.user_id = $1
JOIN sem_claim_instances ci ON ci.claim_id = c.id AND ci.space_id = c.space_id
JOIN sem_source_regions sr ON sr.id = ci.source_region_id AND sr.space_id = c.space_id
JOIN sem_source_versions sv ON sv.id = sr.source_version_id AND sv.space_id = c.space_id
JOIN sem_sources s ON s.id = sv.source_id AND s.space_id = c.space_id
WHERE c.space_id = $2 AND c.id = $3
ORDER BY ci.id`, userID, spaceID, claimID)
	if err != nil {
		return nil, wrap("query claim traces", err)
	}
	defer rows.Close()

	var out []ClaimTrace
	for rows.Next() {
		var x ClaimTrace
		var locator []byte
		if err := rows.Scan(&x.ClaimID, &x.CanonicalText, &x.Predicate, &x.Fingerprint,
			&x.InstanceID, &x.Stance, &x.OriginalText,
			&x.SourceRegionID, &x.LocatorKind, &locator, &x.Excerpt,
			&x.SourceVersionID, &x.ContentHash,
			&x.SourceID, &x.SourceKind, &x.SourceTitle, &x.SourceURI); err != nil {
			return nil, wrap("scan claim trace", err)
		}
		x.Locator = append(json.RawMessage(nil), locator...)
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("iterate claim traces", err)
	}
	return out, nil
}

func defaultJSON(v json.RawMessage) string {
	if len(v) == 0 {
		return "{}"
	}
	return string(v)
}

func nullableJSON(v json.RawMessage) any {
	if len(v) == 0 {
		return nil
	}
	return string(v)
}

func nullableString(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return strings.TrimSpace(v)
}

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("semantic %s: %w", op, err)
}
