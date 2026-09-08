package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type ClaimRelationDraft struct {
	SpaceID        int64
	FromClaimID    int64
	ToClaimID      int64
	RelationType   string
	Reason         string
	Confidence     *float64
	SourceRegionID *int64
	CreatedBy      string
}

type GapDraft struct {
	SpaceID         int64
	Question        string
	Importance      *float64
	RelatedClaimID  *int64
	RelatedEntityID *int64
	Context         json.RawMessage
	CreatedBy       string
}

func (s *Service) CreateClaimRelation(ctx context.Context, userID int64, in ClaimRelationDraft) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("semantic relation: nil database")
	}
	if in.SpaceID == 0 || in.FromClaimID == 0 || in.ToClaimID == 0 || in.FromClaimID == in.ToClaimID {
		return 0, errors.New("semantic relation: valid distinct claim ids are required")
	}
	relationType := strings.TrimSpace(strings.ToLower(in.RelationType))
	if !validRelationType(relationType) {
		return 0, fmt.Errorf("semantic relation: unsupported type %q", in.RelationType)
	}
	createdBy := strings.TrimSpace(in.CreatedBy)
	if createdBy == "" {
		createdBy = "manual"
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("semantic relation: begin: %w", err)
	}
	defer tx.Rollback()
	if err := requireSpaceAccess(ctx, tx, userID, in.SpaceID, true); err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `
INSERT INTO sem_claim_relations(space_id, from_claim_id, to_claim_id, relation_type, reason, confidence, source_region_id, created_by)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT (from_claim_id, to_claim_id, relation_type)
DO UPDATE SET reason = EXCLUDED.reason,
              confidence = EXCLUDED.confidence,
              source_region_id = COALESCE(EXCLUDED.source_region_id, sem_claim_relations.source_region_id)
RETURNING id`, in.SpaceID, in.FromClaimID, in.ToClaimID, relationType, in.Reason, in.Confidence, in.SourceRegionID, createdBy).Scan(&id)
	if err != nil {
		return 0, wrap("create claim relation", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("semantic relation: commit: %w", err)
	}
	return id, nil
}

func (s *Service) CreateGap(ctx context.Context, userID int64, in GapDraft) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("semantic gap: nil database")
	}
	question := strings.TrimSpace(in.Question)
	if in.SpaceID == 0 || question == "" {
		return 0, errors.New("semantic gap: space_id and question are required")
	}
	createdBy := strings.TrimSpace(in.CreatedBy)
	if createdBy == "" {
		createdBy = "manual"
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("semantic gap: begin: %w", err)
	}
	defer tx.Rollback()
	if err := requireSpaceAccess(ctx, tx, userID, in.SpaceID, true); err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `
INSERT INTO sem_gaps(space_id, question, importance, related_claim_id, related_entity_id, context, created_by)
VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7)
RETURNING id`, in.SpaceID, question, in.Importance, in.RelatedClaimID, in.RelatedEntityID, defaultJSON(in.Context), createdBy).Scan(&id)
	if err != nil {
		return 0, wrap("create gap", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("semantic gap: commit: %w", err)
	}
	return id, nil
}

func validRelationType(v string) bool {
	switch v {
	case "supports", "contradicts", "qualifies", "requires", "assumes", "specifies", "defines", "derives_from":
		return true
	default:
		return false
	}
}
