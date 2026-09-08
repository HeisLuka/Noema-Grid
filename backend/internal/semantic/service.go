package semantic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrNotAuthorized = errors.New("semantic: not authorized")

// Service is the authorization boundary for semantic-core operations exposed to
// HTTP/MCP callers. Repository remains the persistence primitive; Service always
// checks the live Tela space_access view on every read or mutation.
type Service struct {
	db   *sql.DB
	repo *Repository
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db, repo: NewRepository(db)}
}

func (s *Service) IngestManual(ctx context.Context, userID int64, in ManualIngest) (IngestResult, error) {
	if s == nil || s.repo == nil {
		return IngestResult{}, errors.New("semantic service: nil repository")
	}
	return s.repo.IngestManualAuthorized(ctx, userID, in)
}

func (s *Service) ClaimTraces(ctx context.Context, userID, spaceID, claimID int64) ([]ClaimTrace, error) {
	if s == nil || s.repo == nil {
		return nil, errors.New("semantic service: nil repository")
	}
	return s.repo.ClaimTraces(ctx, userID, spaceID, claimID)
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func requireSpaceAccess(ctx context.Context, q rowQuerier, userID, spaceID int64, write bool) error {
	if userID == 0 || spaceID == 0 {
		return ErrNotAuthorized
	}

	var rank int
	err := q.QueryRowContext(ctx, `
SELECT COALESCE(max(CASE role
    WHEN 'owner' THEN 3
    WHEN 'editor' THEN 2
    WHEN 'viewer' THEN 1
    ELSE 0
END), 0)
FROM space_access
WHERE space_id = $1 AND user_id = $2`, spaceID, userID).Scan(&rank)
	if err != nil {
		return fmt.Errorf("semantic access check: %w", err)
	}
	if rank == 0 || (write && rank < 2) {
		return ErrNotAuthorized
	}
	return nil
}
