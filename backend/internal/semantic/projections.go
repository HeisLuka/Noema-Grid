package semantic

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	ErrInvalidProjection  = errors.New("semantic: invalid projection")
	ErrProjectionConflict = errors.New("semantic: projection conflict")
)

const ProjectionTypeEntityTimeline = "entity_timeline"

type TimelineSheetSpec struct {
	Type            string `json:"type"`
	SubjectEntityID int64  `json:"subject_entity_id"`
	StartYear       int    `json:"start_year"`
	EndYear         int    `json:"end_year"`
	SheetName       string `json:"sheet_name,omitempty"`
}

type ProjectionInput struct {
	Name string            `json:"name"`
	Kind string            `json:"kind,omitempty"`
	Spec TimelineSheetSpec `json:"spec"`
}

type ProjectionRecord struct {
	ID                 int64           `json:"id"`
	SpaceID            int64           `json:"space_id"`
	Name               string          `json:"name"`
	Kind               string          `json:"kind"`
	Spec               json.RawMessage `json:"spec"`
	TargetPageID       *int64          `json:"target_page_id,omitempty"`
	TargetSheetName    string          `json:"target_sheet_name,omitempty"`
	Revision           int             `json:"revision"`
	LastMaterializedAt *time.Time      `json:"last_materialized_at,omitempty"`
}

type ProjectionBindingDraft struct {
	CellRef     string `json:"cell_ref"`
	EntityID    *int64 `json:"entity_id,omitempty"`
	ClaimID     *int64 `json:"claim_id,omitempty"`
	GapID       *int64 `json:"gap_id,omitempty"`
	DisplayRole string `json:"display_role"`
}

type ProjectionSnapshot struct {
	ProjectionID    int64                    `json:"projection_id"`
	SpaceID         int64                    `json:"space_id"`
	Name            string                   `json:"name"`
	SheetName       string                   `json:"sheet_name"`
	TargetPageID    *int64                   `json:"target_page_id,omitempty"`
	CurrentRevision int                      `json:"current_revision"`
	NextRevision    int                      `json:"next_revision"`
	Body            string                   `json:"body"`
	SnapshotHash    string                   `json:"snapshot_hash"`
	Bindings        []ProjectionBindingDraft `json:"bindings"`
}

type ResolvedProjectionBinding struct {
	BindingID         int64        `json:"binding_id"`
	ProjectionID      int64        `json:"projection_id"`
	ProjectionRevision int         `json:"projection_revision"`
	DisplayRole       string       `json:"display_role"`
	EntityID          *int64       `json:"entity_id,omitempty"`
	EntityName        string       `json:"entity_name,omitempty"`
	ClaimID           *int64       `json:"claim_id,omitempty"`
	ClaimText         string       `json:"claim_text,omitempty"`
	Predicate         string       `json:"predicate,omitempty"`
	GapID             *int64       `json:"gap_id,omitempty"`
	Traces            []ClaimTrace `json:"traces,omitempty"`
}

type CellResolution struct {
	PageID             int64                       `json:"page_id"`
	SheetName          string                      `json:"sheet_name"`
	CellRef            string                      `json:"cell_ref"`
	ProjectionID       int64                       `json:"projection_id"`
	ProjectionRevision int                         `json:"projection_revision"`
	Bindings           []ResolvedProjectionBinding `json:"bindings"`
}

func (s *Service) CreateProjection(ctx context.Context, userID, spaceID int64, in ProjectionInput) (ProjectionRecord, error) {
	if s == nil || s.db == nil {
		return ProjectionRecord{}, errors.New("semantic projection: nil database")
	}
	if err := requireSpaceAccess(ctx, s.db, userID, spaceID, true); err != nil {
		return ProjectionRecord{}, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return ProjectionRecord{}, fmt.Errorf("%w: name is required", ErrInvalidProjection)
	}
	kind := strings.TrimSpace(strings.ToLower(in.Kind))
	if kind == "" {
		kind = "timeline"
	}
	if kind != "timeline" {
		return ProjectionRecord{}, fmt.Errorf("%w: unsupported kind %q", ErrInvalidProjection, in.Kind)
	}
	spec, err := normalizeTimelineSpec(in.Spec)
	if err != nil {
		return ProjectionRecord{}, err
	}
	if err := ensureProjectionSubject(ctx, s.db, spaceID, spec.SubjectEntityID); err != nil {
		return ProjectionRecord{}, err
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return ProjectionRecord{}, fmt.Errorf("semantic projection: marshal spec: %w", err)
	}

	var out ProjectionRecord
	var rawSpec []byte
	var targetPage sql.NullInt64
	var targetSheet sql.NullString
	var last sql.NullTime
	err = s.db.QueryRowContext(ctx, `
INSERT INTO sem_projections(space_id, name, kind, spec, target_sheet_name, created_by)
VALUES ($1,$2,$3,$4::jsonb,$5,$6)
RETURNING id, space_id, name, kind, spec, target_page_id, target_sheet_name, revision, last_materialized_at`,
		spaceID, name, kind, string(specJSON), spec.SheetName, userID,
	).Scan(&out.ID, &out.SpaceID, &out.Name, &out.Kind, &rawSpec, &targetPage, &targetSheet, &out.Revision, &last)
	if err != nil {
		return ProjectionRecord{}, wrap("create projection", err)
	}
	out.Spec = append(json.RawMessage(nil), rawSpec...)
	if targetPage.Valid {
		v := targetPage.Int64
		out.TargetPageID = &v
	}
	if targetSheet.Valid {
		out.TargetSheetName = targetSheet.String
	}
	if last.Valid {
		v := last.Time
		out.LastMaterializedAt = &v
	}
	return out, nil
}

// BuildProjectionSnapshot is a write-authorized preparation step for materialization.
// It does not mutate either the semantic graph or the target page. The caller must
// serialize materializers for a projection and then pass this exact snapshot to
// FinalizeProjectionSnapshot after the managed Tela page has been written.
func (s *Service) BuildProjectionSnapshot(ctx context.Context, userID, spaceID, projectionID int64) (ProjectionSnapshot, error) {
	if s == nil || s.db == nil {
		return ProjectionSnapshot{}, errors.New("semantic projection: nil database")
	}
	if err := requireSpaceAccess(ctx, s.db, userID, spaceID, true); err != nil {
		return ProjectionSnapshot{}, err
	}
	projection, err := s.getProjection(ctx, spaceID, projectionID)
	if err != nil {
		return ProjectionSnapshot{}, err
	}
	var spec TimelineSheetSpec
	if err := json.Unmarshal(projection.Spec, &spec); err != nil {
		return ProjectionSnapshot{}, fmt.Errorf("%w: stored timeline spec is invalid", ErrInvalidProjection)
	}
	spec, err = normalizeTimelineSpec(spec)
	if err != nil {
		return ProjectionSnapshot{}, err
	}

	var subjectName string
	if err := s.db.QueryRowContext(ctx, `
SELECT canonical_name
FROM sem_entities
WHERE space_id=$1 AND id=$2 AND state='active'`, spaceID, spec.SubjectEntityID).Scan(&subjectName); errors.Is(err, sql.ErrNoRows) {
		return ProjectionSnapshot{}, ErrNotFound
	} else if err != nil {
		return ProjectionSnapshot{}, wrap("load projection subject", err)
	}

	claims, err := s.timelineClaims(ctx, spaceID, spec)
	if err != nil {
		return ProjectionSnapshot{}, err
	}
	body, bindings := renderTimelineProjection(subjectName, spec, claims)
	hash := projectionBodyHash(body)
	return ProjectionSnapshot{
		ProjectionID:    projection.ID,
		SpaceID:         projection.SpaceID,
		Name:            projection.Name,
		SheetName:       spec.SheetName,
		TargetPageID:    projection.TargetPageID,
		CurrentRevision: projection.Revision,
		NextRevision:    projection.Revision + 1,
		Body:            body,
		SnapshotHash:    hash,
		Bindings:        bindings,
	}, nil
}

// FinalizeProjectionSnapshot atomically advances the semantic projection revision
// and installs the corresponding cell bindings. It independently verifies the
// page's managed-projection props and exact body, so the caller cannot bind a
// semantic snapshot to an arbitrary or concurrently modified Tela page.
func (s *Service) FinalizeProjectionSnapshot(ctx context.Context, userID int64, snapshot ProjectionSnapshot, pageID int64) (ProjectionRecord, error) {
	if s == nil || s.db == nil {
		return ProjectionRecord{}, errors.New("semantic projection: nil database")
	}
	if snapshot.SpaceID == 0 || snapshot.ProjectionID == 0 || pageID == 0 || snapshot.NextRevision != snapshot.CurrentRevision+1 {
		return ProjectionRecord{}, fmt.Errorf("%w: malformed materialization snapshot", ErrInvalidProjection)
	}
	if snapshot.SnapshotHash == "" || projectionBodyHash(snapshot.Body) != snapshot.SnapshotHash {
		return ProjectionRecord{}, fmt.Errorf("%w: snapshot hash mismatch", ErrInvalidProjection)
	}
	if err := validateProjectionBindings(snapshot.Bindings); err != nil {
		return ProjectionRecord{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProjectionRecord{}, wrap("begin projection finalize", err)
	}
	defer tx.Rollback()
	if err := requireSpaceAccess(ctx, tx, userID, snapshot.SpaceID, true); err != nil {
		return ProjectionRecord{}, err
	}

	var currentRevision int
	var currentTarget sql.NullInt64
	err = tx.QueryRowContext(ctx, `
SELECT revision, target_page_id
FROM sem_projections
WHERE space_id=$1 AND id=$2
FOR UPDATE`, snapshot.SpaceID, snapshot.ProjectionID).Scan(&currentRevision, &currentTarget)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectionRecord{}, ErrNotFound
	}
	if err != nil {
		return ProjectionRecord{}, wrap("lock projection", err)
	}
	if currentRevision != snapshot.CurrentRevision || snapshot.NextRevision != currentRevision+1 {
		return ProjectionRecord{}, ErrProjectionConflict
	}
	if currentTarget.Valid && currentTarget.Int64 != pageID {
		return ProjectionRecord{}, ErrProjectionConflict
	}

	var pageBody string
	var propsRaw []byte
	err = tx.QueryRowContext(ctx, `
SELECT body, props
FROM pages
WHERE id=$1 AND space_id=$2 AND deleted_at IS NULL`, pageID, snapshot.SpaceID).Scan(&pageBody, &propsRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectionRecord{}, ErrNotFound
	}
	if err != nil {
		return ProjectionRecord{}, wrap("load managed projection page", err)
	}
	var props map[string]any
	if err := json.Unmarshal(propsRaw, &props); err != nil {
		return ProjectionRecord{}, ErrProjectionConflict
	}
	if !projectionPagePropsMatch(props, snapshot) || pageBody != snapshot.Body {
		return ProjectionRecord{}, ErrProjectionConflict
	}

	for _, binding := range snapshot.Bindings {
		_, err := tx.ExecContext(ctx, `
INSERT INTO sem_projection_bindings(
    space_id, projection_id, projection_revision, page_id, sheet_name, cell_ref,
    entity_id, claim_id, gap_id, display_role)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			snapshot.SpaceID, snapshot.ProjectionID, snapshot.NextRevision, pageID,
			snapshot.SheetName, binding.CellRef, binding.EntityID, binding.ClaimID, binding.GapID, binding.DisplayRole)
		if err != nil {
			return ProjectionRecord{}, wrap("insert projection binding", err)
		}
	}

	res, err := tx.ExecContext(ctx, `
UPDATE sem_projections
SET target_page_id=$1,
    target_sheet_name=$2,
    revision=$3,
    last_materialized_at=CURRENT_TIMESTAMP,
    updated_at=CURRENT_TIMESTAMP
WHERE space_id=$4 AND id=$5 AND revision=$6`,
		pageID, snapshot.SheetName, snapshot.NextRevision, snapshot.SpaceID, snapshot.ProjectionID, snapshot.CurrentRevision)
	if err != nil {
		return ProjectionRecord{}, wrap("advance projection revision", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return ProjectionRecord{}, ErrProjectionConflict
	}
	if err := tx.Commit(); err != nil {
		return ProjectionRecord{}, wrap("commit projection finalize", err)
	}
	return s.getProjection(ctx, snapshot.SpaceID, snapshot.ProjectionID)
}

func (s *Service) ResolveSheetCell(ctx context.Context, userID, spaceID, pageID int64, sheetName, cellRef string) (CellResolution, error) {
	if s == nil || s.db == nil || s.repo == nil {
		return CellResolution{}, errors.New("semantic projection resolve: nil database")
	}
	if err := requireSpaceAccess(ctx, s.db, userID, spaceID, false); err != nil {
		return CellResolution{}, err
	}
	sheetName = strings.TrimSpace(sheetName)
	cellRef = strings.ToUpper(strings.TrimSpace(cellRef))
	if sheetName == "" || !a1CellRE.MatchString(cellRef) {
		return CellResolution{}, fmt.Errorf("%w: invalid sheet or cell reference", ErrInvalidProjection)
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT b.id, b.projection_id, b.projection_revision, b.display_role,
       b.entity_id, COALESCE(e.canonical_name,''),
       b.claim_id, COALESCE(c.canonical_text,''), COALESCE(c.predicate,''),
       b.gap_id
FROM sem_projections p
JOIN sem_projection_bindings b
  ON b.space_id=p.space_id AND b.projection_id=p.id AND b.projection_revision=p.revision
LEFT JOIN sem_entities e ON e.space_id=b.space_id AND e.id=b.entity_id
LEFT JOIN sem_claims c ON c.space_id=b.space_id AND c.id=b.claim_id
JOIN pages pg ON pg.id=b.page_id AND pg.space_id=b.space_id AND pg.deleted_at IS NULL
WHERE p.space_id=$1 AND p.target_page_id=$2
  AND b.page_id=$2 AND b.sheet_name=$3 AND b.cell_ref=$4
ORDER BY b.display_role, b.id`, spaceID, pageID, sheetName, cellRef)
	if err != nil {
		return CellResolution{}, wrap("resolve sheet cell", err)
	}
	defer rows.Close()

	out := CellResolution{PageID: pageID, SheetName: sheetName, CellRef: cellRef}
	for rows.Next() {
		var b ResolvedProjectionBinding
		var entityID, claimID, gapID sql.NullInt64
		if err := rows.Scan(&b.BindingID, &b.ProjectionID, &b.ProjectionRevision, &b.DisplayRole,
			&entityID, &b.EntityName, &claimID, &b.ClaimText, &b.Predicate, &gapID); err != nil {
			return CellResolution{}, wrap("scan sheet cell binding", err)
		}
		if out.ProjectionID == 0 {
			out.ProjectionID = b.ProjectionID
			out.ProjectionRevision = b.ProjectionRevision
		}
		if entityID.Valid {
			v := entityID.Int64
			b.EntityID = &v
		}
		if claimID.Valid {
			v := claimID.Int64
			b.ClaimID = &v
			traces, err := s.repo.ClaimTraces(ctx, userID, spaceID, v)
			if err != nil {
				return CellResolution{}, err
			}
			b.Traces = traces
		}
		if gapID.Valid {
			v := gapID.Int64
			b.GapID = &v
		}
		out.Bindings = append(out.Bindings, b)
	}
	if err := rows.Err(); err != nil {
		return CellResolution{}, wrap("iterate sheet cell bindings", err)
	}
	if len(out.Bindings) == 0 {
		return CellResolution{}, ErrNotFound
	}
	return out, nil
}

type timelineClaim struct {
	ID             int64
	CanonicalText  string
	Predicate      string
	ValidFrom      time.Time
	ValidTo        sql.NullTime
	LocationName   string
	OccupationName string
}

func (s *Service) timelineClaims(ctx context.Context, spaceID int64, spec TimelineSheetSpec) ([]timelineClaim, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT c.id, c.canonical_text, c.predicate, c.valid_from, c.valid_to,
       COALESCE((
           SELECT e.canonical_name
           FROM sem_claim_slots cs
           JOIN sem_entities e ON e.id=cs.entity_id AND e.space_id=cs.space_id
           WHERE cs.space_id=c.space_id AND cs.claim_id=c.id AND cs.role='location'
           ORDER BY cs.ordinal, cs.id LIMIT 1
       ), ''),
       COALESCE((
           SELECT e.canonical_name
           FROM sem_claim_slots cs
           JOIN sem_entities e ON e.id=cs.entity_id AND e.space_id=cs.space_id
           WHERE cs.space_id=c.space_id AND cs.claim_id=c.id AND cs.role='occupation'
           ORDER BY cs.ordinal, cs.id LIMIT 1
       ), '')
FROM sem_claims c
WHERE c.space_id=$1 AND c.state='active' AND c.valid_from IS NOT NULL
  AND EXISTS (
      SELECT 1 FROM sem_claim_slots subject
      WHERE subject.space_id=c.space_id AND subject.claim_id=c.id
        AND subject.role='subject' AND subject.entity_id=$2
  )
  AND c.valid_from <= make_date($4,12,31)
  AND COALESCE(c.valid_to,c.valid_from) >= make_date($3,1,1)
ORDER BY c.predicate, c.id`, spaceID, spec.SubjectEntityID, spec.StartYear, spec.EndYear)
	if err != nil {
		return nil, wrap("query timeline claims", err)
	}
	defer rows.Close()
	var out []timelineClaim
	for rows.Next() {
		var c timelineClaim
		if err := rows.Scan(&c.ID, &c.CanonicalText, &c.Predicate, &c.ValidFrom, &c.ValidTo, &c.LocationName, &c.OccupationName); err != nil {
			return nil, wrap("scan timeline claim", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("iterate timeline claims", err)
	}
	return out, nil
}

func renderTimelineProjection(subjectName string, spec TimelineSheetSpec, claims []timelineClaim) (string, []ProjectionBindingDraft) {
	byYear := make(map[int][]timelineClaim, spec.EndYear-spec.StartYear+1)
	for _, claim := range claims {
		from := claim.ValidFrom.Year()
		to := from
		if claim.ValidTo.Valid {
			to = claim.ValidTo.Time.Year()
		}
		if from < spec.StartYear {
			from = spec.StartYear
		}
		if to > spec.EndYear {
			to = spec.EndYear
		}
		for year := from; year <= to; year++ {
			byYear[year] = append(byYear[year], claim)
		}
	}
	for year := range byYear {
		sort.SliceStable(byYear[year], func(i, j int) bool {
			if byYear[year][i].Predicate != byYear[year][j].Predicate {
				return byYear[year][i].Predicate < byYear[year][j].Predicate
			}
			return byYear[year][i].ID < byYear[year][j].ID
		})
	}

	var b strings.Builder
	b.WriteString("## Sheet: ")
	b.WriteString(spec.SheetName)
	b.WriteString("\n\n| Person")
	for year := spec.StartYear; year <= spec.EndYear; year++ {
		fmt.Fprintf(&b, " | %d", year)
	}
	b.WriteString(" |\n| ---")
	for year := spec.StartYear; year <= spec.EndYear; year++ {
		b.WriteString(" | ---")
	}
	b.WriteString(" |\n| ")
	b.WriteString(escapeProjectionCell(subjectName))

	bindings := []ProjectionBindingDraft{{CellRef: "A2", EntityID: int64Ptr(spec.SubjectEntityID), DisplayRole: "subject"}}
	for year := spec.StartYear; year <= spec.EndYear; year++ {
		b.WriteString(" | ")
		cellClaims := byYear[year]
		labels := make([]string, 0, len(cellClaims))
		for _, claim := range cellClaims {
			labels = append(labels, escapeProjectionCell(timelineClaimLabel(claim)))
			cell := columnName(2+(year-spec.StartYear)) + "2"
			bindings = append(bindings, ProjectionBindingDraft{
				CellRef:     cell,
				ClaimID:     int64Ptr(claim.ID),
				DisplayRole: "claim:" + strconv.FormatInt(claim.ID, 10),
			})
		}
		b.WriteString(strings.Join(labels, "; "))
	}
	b.WriteString(" |\n")
	return b.String(), bindings
}

func timelineClaimLabel(c timelineClaim) string {
	switch c.Predicate {
	case "person.liberated":
		return "liberated"
	case "person.returned_home":
		return "returned home"
	case "person.location":
		if strings.TrimSpace(c.LocationName) != "" {
			return c.LocationName
		}
	case "person.occupation":
		if strings.TrimSpace(c.OccupationName) != "" {
			return c.OccupationName
		}
	}
	return c.CanonicalText
}

func normalizeTimelineSpec(in TimelineSheetSpec) (TimelineSheetSpec, error) {
	out := in
	out.Type = strings.TrimSpace(strings.ToLower(out.Type))
	if out.Type == "" {
		out.Type = ProjectionTypeEntityTimeline
	}
	if out.Type != ProjectionTypeEntityTimeline {
		return TimelineSheetSpec{}, fmt.Errorf("%w: unsupported projection type %q", ErrInvalidProjection, in.Type)
	}
	if out.SubjectEntityID <= 0 {
		return TimelineSheetSpec{}, fmt.Errorf("%w: subject_entity_id is required", ErrInvalidProjection)
	}
	if out.StartYear < 1 || out.EndYear < out.StartYear || out.EndYear > 9999 || out.EndYear-out.StartYear > 200 {
		return TimelineSheetSpec{}, fmt.Errorf("%w: invalid year range", ErrInvalidProjection)
	}
	out.SheetName = strings.TrimSpace(out.SheetName)
	if out.SheetName == "" {
		out.SheetName = "Timeline"
	}
	if len(out.SheetName) > 100 || strings.ContainsAny(out.SheetName, "\r\n") {
		return TimelineSheetSpec{}, fmt.Errorf("%w: invalid sheet name", ErrInvalidProjection)
	}
	return out, nil
}

func ensureProjectionSubject(ctx context.Context, q rowQuerier, spaceID, entityID int64) error {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM sem_entities WHERE space_id=$1 AND id=$2 AND state='active'`, spaceID, entityID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return wrap("validate projection subject", err)
	}
	return nil
}

func (s *Service) getProjection(ctx context.Context, spaceID, projectionID int64) (ProjectionRecord, error) {
	var out ProjectionRecord
	var rawSpec []byte
	var targetPage sql.NullInt64
	var targetSheet sql.NullString
	var last sql.NullTime
	err := s.db.QueryRowContext(ctx, `
SELECT id, space_id, name, kind, spec, target_page_id, target_sheet_name, revision, last_materialized_at
FROM sem_projections
WHERE space_id=$1 AND id=$2`, spaceID, projectionID).Scan(
		&out.ID, &out.SpaceID, &out.Name, &out.Kind, &rawSpec, &targetPage, &targetSheet, &out.Revision, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectionRecord{}, ErrNotFound
	}
	if err != nil {
		return ProjectionRecord{}, wrap("get projection", err)
	}
	out.Spec = append(json.RawMessage(nil), rawSpec...)
	if targetPage.Valid {
		v := targetPage.Int64
		out.TargetPageID = &v
	}
	if targetSheet.Valid {
		out.TargetSheetName = targetSheet.String
	}
	if last.Valid {
		v := last.Time
		out.LastMaterializedAt = &v
	}
	return out, nil
}

func projectionBodyHash(body string) string {
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:])
}

func projectionPagePropsMatch(props map[string]any, snapshot ProjectionSnapshot) bool {
	managed, _ := props["semantic_projection_managed"].(bool)
	sheet, _ := props["sheet"].(bool)
	if !managed || !sheet {
		return false
	}
	projectionID, ok := projectionJSONInt64(props["semantic_projection_id"])
	if !ok || projectionID != snapshot.ProjectionID {
		return false
	}
	revision, ok := projectionJSONInt64(props["semantic_projection_revision"])
	if !ok || int(revision) != snapshot.NextRevision {
		return false
	}
	hash, _ := props["semantic_projection_snapshot_hash"].(string)
	return hash == snapshot.SnapshotHash
}

func projectionJSONInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		i := int64(x)
		return i, float64(i) == x
	case json.Number:
		i, err := x.Int64()
		return i, err == nil
	case int:
		return int64(x), true
	case int64:
		return x, true
	case string:
		i, err := strconv.ParseInt(x, 10, 64)
		return i, err == nil
	default:
		return 0, false
	}
}

func validateProjectionBindings(bindings []ProjectionBindingDraft) error {
	seen := make(map[string]struct{}, len(bindings))
	for _, b := range bindings {
		b.CellRef = strings.ToUpper(strings.TrimSpace(b.CellRef))
		if !a1CellRE.MatchString(b.CellRef) || strings.TrimSpace(b.DisplayRole) == "" {
			return fmt.Errorf("%w: invalid projection binding", ErrInvalidProjection)
		}
		targets := 0
		if b.EntityID != nil {
			targets++
		}
		if b.ClaimID != nil {
			targets++
		}
		if b.GapID != nil {
			targets++
		}
		if targets != 1 {
			return fmt.Errorf("%w: projection binding must target exactly one semantic object", ErrInvalidProjection)
		}
		key := b.CellRef + "\x00" + b.DisplayRole
		if _, ok := seen[key]; ok {
			return fmt.Errorf("%w: duplicate projection binding", ErrInvalidProjection)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func escapeProjectionCell(v string) string {
	v = strings.ReplaceAll(v, "\r\n", "\n")
	v = strings.ReplaceAll(v, "\r", "\n")
	v = strings.ReplaceAll(v, "\n", "<br>")
	v = strings.ReplaceAll(v, "|", "\\|")
	return strings.TrimSpace(v)
}

func columnName(n int) string {
	if n <= 0 {
		return ""
	}
	var out string
	for n > 0 {
		n--
		out = string(rune('A'+(n%26))) + out
		n /= 26
	}
	return out
}

func int64Ptr(v int64) *int64 { return &v }

var a1CellRE = regexp.MustCompile(`^[A-Z]+[1-9][0-9]*$`)
