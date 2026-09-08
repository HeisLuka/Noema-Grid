package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/zcag/tela/backend/internal/auth"
	"github.com/zcag/tela/backend/internal/semantic"
)

// Keep the high bits stable so Noema projection locks occupy their own advisory
// lock namespace. The projection id is mixed into the low bits. Every
// materializer takes this session-level PostgreSQL lock across build -> Tela page
// write -> semantic finalize, so concurrent app processes cannot interleave two
// revisions of the same projection.
const semanticProjectionLockNamespace int64 = 0x4e6f656d00000000

type semanticProjectionMaterialization struct {
	ProjectionID int64  `json:"projection_id"`
	PageID       int64  `json:"page_id"`
	SheetName    string `json:"sheet_name"`
	Revision     int    `json:"revision"`
	SnapshotHash string `json:"snapshot_hash"`
}

func (s *Server) semanticMaterializeProjectionCore(
	ctx context.Context,
	u *auth.User,
	k *auth.APIKey,
	spaceID, projectionID int64,
) (semanticProjectionMaterialization, *apiErr) {
	if s == nil || s.DB == nil {
		return semanticProjectionMaterialization{}, &apiErr{http.StatusInternalServerError, "semantic_internal", "semantic projection service is unavailable"}
	}
	if u == nil {
		return semanticProjectionMaterialization{}, &apiErr{http.StatusUnauthorized, "unauthorized", "not authenticated"}
	}
	if projectionID <= 0 {
		return semanticProjectionMaterialization{}, &apiErr{http.StatusBadRequest, "semantic_invalid_projection", "projection id must be positive"}
	}
	// Preserve Tela's PAT space ceiling and ordinary membership error semantics
	// before touching the semantic subsystem. Build/Finalize independently check
	// live space_access and require editor+ for the actual materialization.
	if _, ae := s.membershipCore(ctx, u, k, spaceID); ae != nil {
		return semanticProjectionMaterialization{}, ae
	}

	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return semanticProjectionMaterialization{}, &apiErr{http.StatusInternalServerError, "semantic_internal", "acquire semantic projection lock failed"}
	}
	defer conn.Close()
	lockKey := semanticProjectionLockNamespace ^ projectionID
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return semanticProjectionMaterialization{}, &apiErr{http.StatusInternalServerError, "semantic_internal", "acquire semantic projection lock failed"}
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey)
	}()

	svc := semantic.NewService(s.DB)
	snapshot, err := svc.BuildProjectionSnapshot(ctx, u.ID, spaceID, projectionID)
	if err != nil {
		return semanticProjectionMaterialization{}, semanticProjectionAPIError(err)
	}
	props := semanticProjectionProps(nil, snapshot)
	writeCtx := withAgentWrite(ctx)

	var pageID int64
	if snapshot.TargetPageID == nil {
		page, ae := s.createPageCore(writeCtx, u, k, pageCreateRequest{
			SpaceID: spaceID,
			Title:   snapshot.Name,
			Body:    snapshot.Body,
			Props:   props,
		}, false)
		if ae != nil {
			return semanticProjectionMaterialization{}, ae
		}
		pageID = page.ID
	} else {
		page, ae := s.getPageCore(ctx, u, k, *snapshot.TargetPageID)
		if ae != nil {
			return semanticProjectionMaterialization{}, ae
		}
		if page.SpaceID != spaceID || !semanticManagedProjectionPageMatches(page.Props, snapshot) {
			return semanticProjectionMaterialization{}, semanticProjectionAPIError(semantic.ErrProjectionConflict)
		}
		pageID = page.ID
		props = semanticProjectionProps(page.Props, snapshot)
		title := snapshot.Name
		body := snapshot.Body
		if _, ae := s.updatePageCore(writeCtx, u, k, page.ID, pageUpdateRequest{
			Title: &title,
			Body:  &body,
			Props: props,
		}, true); ae != nil {
			return semanticProjectionMaterialization{}, ae
		}
	}

	projection, err := svc.FinalizeProjectionSnapshot(ctx, u.ID, snapshot, pageID)
	if err != nil {
		return semanticProjectionMaterialization{}, semanticProjectionAPIError(err)
	}
	return semanticProjectionMaterialization{
		ProjectionID: projection.ID,
		PageID:       pageID,
		SheetName:    snapshot.SheetName,
		Revision:     projection.Revision,
		SnapshotHash: snapshot.SnapshotHash,
	}, nil
}

func semanticProjectionAPIError(err error) *apiErr {
	switch {
	case errors.Is(err, semantic.ErrProjectionConflict):
		return &apiErr{http.StatusConflict, "semantic_projection_conflict", "semantic projection conflicts with the managed page"}
	case errors.Is(err, semantic.ErrInvalidProjection):
		return &apiErr{http.StatusUnprocessableEntity, "semantic_invalid_projection", "semantic projection is invalid"}
	default:
		return semanticAPIError(err)
	}
}

func semanticProjectionProps(existing map[string]any, snapshot semantic.ProjectionSnapshot) map[string]any {
	out := make(map[string]any, len(existing)+5)
	for k, v := range existing {
		out[k] = v
	}
	out["sheet"] = true
	out["semantic_projection_managed"] = true
	out["semantic_projection_id"] = snapshot.ProjectionID
	out["semantic_projection_revision"] = snapshot.NextRevision
	out["semantic_projection_snapshot_hash"] = snapshot.SnapshotHash
	return out
}

// Before rewriting an existing target we accept only a page that is already
// explicitly managed by this projection and still advertises the semantic
// revision we built from. Manual body edits are derived-view edits and may be
// overwritten; mutation/removal of the management contract is a hard conflict.
func semanticManagedProjectionPageMatches(props map[string]any, snapshot semantic.ProjectionSnapshot) bool {
	if props == nil {
		return false
	}
	managed, _ := props["semantic_projection_managed"].(bool)
	sheet, _ := props["sheet"].(bool)
	if !managed || !sheet {
		return false
	}
	projectionID, ok := semanticProjectionPropInt64(props["semantic_projection_id"])
	if !ok || projectionID != snapshot.ProjectionID {
		return false
	}
	revision, ok := semanticProjectionPropInt64(props["semantic_projection_revision"])
	return ok && int(revision) == snapshot.CurrentRevision
}

func semanticProjectionPropInt64(v any) (int64, bool) {
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
