package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/zcag/tela/backend/internal/auth"
	"github.com/zcag/tela/backend/internal/semantic"
)

const (
	semanticPreviewTTL      = 15 * time.Minute
	semanticMaxRequestBytes = 4 << 20
	semanticMaxIdemKeyLen   = 256
)

var errSemanticExtractorUnavailable = errors.New("semantic extractor unavailable")

type unavailableSemanticExtractor struct{}

func (unavailableSemanticExtractor) Extract(context.Context, semantic.ExtractionInput) (semantic.CandidateSet, error) {
	return semantic.CandidateSet{}, errSemanticExtractorUnavailable
}

// semanticExtractor is the single transport seam for semantic extraction. The
// transport/API slice deliberately ships without an LLM implementation; the
// next adapter slice replaces this method while PreviewService remains agnostic.
func (s *Server) semanticExtractor() semantic.Extractor {
	return unavailableSemanticExtractor{}
}

type semanticPreviewRequest struct {
	Profile semantic.ExtractionProfile `json:"profile"`
}

type semanticPreviewResponse struct {
	Preview semantic.Preview `json:"preview"`
}

type semanticCommitResponse struct {
	Result semantic.CommitResult `json:"result"`
}

// SemanticPreview exposes the read-only half of semanticize_page over REST.
// The route space is membership-gated before the semantic service performs its
// own live ACL check, so a space-scoped PAT cannot escape its ceiling.
func (s *Server) SemanticPreview(w http.ResponseWriter, r *http.Request) {
	u, ok := requireUser(w, r)
	if !ok {
		return
	}
	spaceID, ok := parseIDParam(w, r, "id")
	if !ok {
		return
	}
	pageID, ok := parseIDParam(w, r, "page_id")
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, semanticMaxRequestBytes)
	var req semanticPreviewRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "could not parse request body")
		return
	}
	k, _ := auth.APIKeyFromContext(r.Context())
	preview, ae := s.semanticPreviewCore(r.Context(), u, k, spaceID, pageID, req.Profile, s.semanticExtractor())
	if ae != nil {
		writeError(w, ae.Status, ae.Code, ae.Message)
		return
	}
	writeJSON(w, http.StatusOK, semanticPreviewResponse{Preview: preview})
}

// SemanticCommit accepts an explicit subset of a previously signed preview.
// Idempotency is transport-owned by the HTTP header, then persisted atomically
// by semantic.CommitService under the semantic.commit namespace.
func (s *Server) SemanticCommit(w http.ResponseWriter, r *http.Request) {
	u, ok := requireUser(w, r)
	if !ok {
		return
	}
	spaceID, ok := parseIDParam(w, r, "id")
	if !ok {
		return
	}
	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idemKey == "" {
		writeError(w, http.StatusBadRequest, "semantic_idempotency_key_required", "Idempotency-Key header is required")
		return
	}
	if len(idemKey) > semanticMaxIdemKeyLen {
		writeError(w, http.StatusBadRequest, "semantic_invalid_idempotency_key", "Idempotency-Key header is too long")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, semanticMaxRequestBytes)
	var in semantic.CommitInput
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "could not parse request body")
		return
	}
	// The JSON field is intentionally ignored so retries cannot accidentally use
	// two competing idempotency channels. REST always uses the standard header.
	in.IdempotencyKey = idemKey
	k, _ := auth.APIKeyFromContext(r.Context())
	result, ae := s.semanticCommitCore(r.Context(), u, k, spaceID, in)
	if ae != nil {
		writeError(w, ae.Status, ae.Code, ae.Message)
		return
	}
	writeJSON(w, http.StatusOK, semanticCommitResponse{Result: result})
}

func (s *Server) semanticPreviewCore(
	ctx context.Context,
	u *auth.User,
	k *auth.APIKey,
	spaceID, pageID int64,
	profile semantic.ExtractionProfile,
	extractor semantic.Extractor,
) (semantic.Preview, *apiErr) {
	if u == nil {
		return semantic.Preview{}, &apiErr{http.StatusUnauthorized, "unauthorized", "not authenticated"}
	}
	if !profile.Valid() {
		return semantic.Preview{}, &apiErr{http.StatusUnprocessableEntity, "semantic_invalid_profile", "unsupported semantic extraction profile"}
	}
	if _, ae := s.membershipCore(ctx, u, k, spaceID); ae != nil {
		return semantic.Preview{}, ae
	}
	if extractor == nil {
		return semantic.Preview{}, &apiErr{http.StatusServiceUnavailable, "semantic_extractor_unavailable", "semantic extraction is not configured"}
	}
	signer, ae := s.semanticPreviewSigner()
	if ae != nil {
		return semantic.Preview{}, ae
	}
	svc, err := semantic.NewPreviewService(s.DB, extractor, signer)
	if err != nil {
		return semantic.Preview{}, semanticAPIError(err)
	}
	preview, err := svc.PreviewPage(ctx, u.ID, spaceID, pageID, profile)
	if err != nil {
		return semantic.Preview{}, semanticAPIError(err)
	}
	return preview, nil
}

func (s *Server) semanticCommitCore(
	ctx context.Context,
	u *auth.User,
	k *auth.APIKey,
	routeSpaceID int64,
	in semantic.CommitInput,
) (semantic.CommitResult, *apiErr) {
	if u == nil {
		return semantic.CommitResult{}, &apiErr{http.StatusUnauthorized, "unauthorized", "not authenticated"}
	}
	if _, ae := s.membershipCore(ctx, u, k, routeSpaceID); ae != nil {
		return semantic.CommitResult{}, ae
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return semantic.CommitResult{}, &apiErr{http.StatusBadRequest, "semantic_idempotency_key_required", "idempotency key is required"}
	}
	if len(in.IdempotencyKey) > semanticMaxIdemKeyLen {
		return semantic.CommitResult{}, &apiErr{http.StatusBadRequest, "semantic_invalid_idempotency_key", "idempotency key is too long"}
	}

	signer, ae := s.semanticPreviewSigner()
	if ae != nil {
		return semantic.CommitResult{}, ae
	}
	claims, err := signer.Verify(strings.TrimSpace(in.PreviewToken))
	if err != nil {
		return semantic.CommitResult{}, semanticAPIError(err)
	}
	if claims.SpaceID != routeSpaceID {
		return semantic.CommitResult{}, &apiErr{http.StatusUnprocessableEntity, "semantic_invalid_preview", "preview token belongs to a different space"}
	}
	if err := semantic.ValidateCandidateSet(in.Candidates); err != nil {
		return semantic.CommitResult{}, &apiErr{http.StatusUnprocessableEntity, "semantic_invalid_selection", "candidate graph is invalid"}
	}
	if _, err := semantic.CandidatePayloadHash(in.Candidates); err != nil {
		return semantic.CommitResult{}, &apiErr{http.StatusUnprocessableEntity, "semantic_invalid_selection", "candidate payload is invalid"}
	}

	svc, err := semantic.NewCommitService(s.DB, signer)
	if err != nil {
		return semantic.CommitResult{}, semanticAPIError(err)
	}
	result, err := svc.CommitPreview(ctx, u.ID, in)
	if err != nil {
		return semantic.CommitResult{}, semanticAPIError(err)
	}
	return result, nil
}

func (s *Server) semanticPreviewSigner() (*semantic.PreviewSigner, *apiErr) {
	if len(s.shareSecret) == 0 {
		return nil, &apiErr{http.StatusInternalServerError, "semantic_internal", "semantic signing is unavailable"}
	}
	// Domain separation keeps preview tokens cryptographically independent from
	// every other protocol that uses Tela's stable persisted share root secret.
	mac := hmac.New(sha256.New, s.shareSecret)
	_, _ = mac.Write([]byte("noema-semantic-preview-v1"))
	signer, err := semantic.NewPreviewSigner(mac.Sum(nil), semanticPreviewTTL)
	if err != nil {
		return nil, &apiErr{http.StatusInternalServerError, "semantic_internal", "semantic signing is unavailable"}
	}
	return signer, nil
}

func semanticAPIError(err error) *apiErr {
	switch {
	case errors.Is(err, errSemanticExtractorUnavailable):
		return &apiErr{http.StatusServiceUnavailable, "semantic_extractor_unavailable", "semantic extraction is not configured"}
	case errors.Is(err, semantic.ErrNotAuthorized):
		return &apiErr{http.StatusForbidden, "semantic_not_authorized", "semantic operation is not authorized"}
	case errors.Is(err, semantic.ErrNotFound):
		return &apiErr{http.StatusNotFound, "semantic_not_found", "semantic source or object was not found"}
	case errors.Is(err, semantic.ErrStalePreview):
		return &apiErr{http.StatusConflict, "semantic_stale_preview", "source changed after the preview was created"}
	case errors.Is(err, semantic.ErrInvalidSelection):
		return &apiErr{http.StatusUnprocessableEntity, "semantic_invalid_selection", "semantic selection is invalid"}
	case errors.Is(err, semantic.ErrInvalidPreviewToken):
		return &apiErr{http.StatusUnprocessableEntity, "semantic_invalid_preview", "preview token or candidate payload is invalid"}
	case errors.Is(err, semantic.ErrExpiredPreviewToken):
		return &apiErr{http.StatusConflict, "semantic_expired_preview", "preview token has expired"}
	case errors.Is(err, semantic.ErrIdempotencyConflict):
		return &apiErr{http.StatusConflict, "semantic_idempotency_conflict", "idempotency key was already used for another operation"}
	case errors.Is(err, semantic.ErrCommitInProgress):
		return &apiErr{http.StatusConflict, "semantic_commit_in_progress", "a commit with this idempotency key is still in progress"}
	default:
		return &apiErr{http.StatusInternalServerError, "semantic_internal", "semantic operation failed"}
	}
}
