package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zcag/tela/backend/internal/auth"
)

// attachment_uploads.go — the signed-PUT upload handshake. An MCP agent calls
// request_attachment_upload for a short-lived signed PUT URL, the host PUTs the
// raw bytes out-of-band (so they never ride through the model's context), then
// confirm_attachment_upload returns the stored file's ref. This is the tier
// above the inline-base64 upload_attachment (capped at mcpInlineUploadCap) and
// mirrors Notion's create→send→reference lifecycle. It reuses the share-secret
// HMAC (like the PDF print token) and the content-addressed space_files store
// (free dedup), and `PUT /api/uploads/{token}` self-authenticates via the token
// on auth.IsPublicPath — the bytes are authorized by the signed grant, not a
// session. Only usable on hosts that can make an outbound HTTP PUT; otherwise
// upload_attachment (base64) stays the universal fallback.

const uploadTokenTTL = 5 * time.Minute

// uploadToken is the signed payload bound into the PUT URL. Short field names
// keep the token compact.
type uploadToken struct {
	U string `json:"u"` // upload_id (also the attachment_uploads key)
	P int64  `json:"p"` // page_id
	N string `json:"n"` // filename
	M string `json:"m"` // mime hint (magic bytes still win for images)
	X int64  `json:"x"` // max bytes
	E int64  `json:"e"` // expiry (unix seconds)
}

func (s *Server) uploadTokenKey() []byte {
	h := hmac.New(sha256.New, s.shareSecret)
	h.Write([]byte("tela-attachment-upload-v1"))
	return h.Sum(nil)
}

func (s *Server) mintUploadToken(t uploadToken) string {
	payload, _ := json.Marshal(t)
	mac := hmac.New(sha256.New, s.uploadTokenKey())
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) verifyUploadToken(tok string) (uploadToken, bool) {
	parts := strings.SplitN(tok, ".", 2)
	if len(parts) != 2 {
		return uploadToken{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return uploadToken{}, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return uploadToken{}, false
	}
	mac := hmac.New(sha256.New, s.uploadTokenKey())
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return uploadToken{}, false
	}
	var t uploadToken
	if err := json.Unmarshal(payload, &t); err != nil {
		return uploadToken{}, false
	}
	if time.Now().Unix() > t.E {
		return uploadToken{}, false
	}
	return t, true
}

// uploadTicket is what request_attachment_upload hands back.
type uploadTicket struct {
	UploadID  string `json:"upload_id"`
	PutURL    string `json:"put_url"`
	ExpiresAt string `json:"expires_at"`
	MaxBytes  int64  `json:"max_bytes"`
}

// requestAttachmentUploadCore stages an upload and mints a signed PUT URL
// (editor+). Stateless token + a tracking row so confirm can find the file.
func (s *Server) requestAttachmentUploadCore(ctx context.Context, u *auth.User, k *auth.APIKey, pageID int64, name, mime string) (uploadTicket, *apiErr) {
	if strings.TrimSpace(name) == "" {
		return uploadTicket{}, &apiErr{http.StatusBadRequest, "bad_request", "name is required"}
	}
	page, err := selectPageByID(ctx, s.DB, pageID)
	if errors.Is(err, sql.ErrNoRows) {
		return uploadTicket{}, &apiErr{http.StatusForbidden, "forbidden", "not a member"}
	}
	if err != nil {
		return uploadTicket{}, &apiErr{http.StatusInternalServerError, "internal", "lookup page failed"}
	}
	role, ae := s.membershipCore(ctx, u, k, page.SpaceID)
	if ae != nil {
		return uploadTicket{}, ae
	}
	if !canEdit(role) {
		return uploadTicket{}, &apiErr{http.StatusForbidden, "viewer_no_write", "editor or owner role required"}
	}

	// Opportunistic sweep of stale tracking rows — bounds table growth without a
	// cron. created_at is sortable text (YYYY-MM-DD HH:MM:SS UTC).
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM attachment_uploads WHERE created_at < $1`,
		time.Now().Add(-24*time.Hour).UTC().Format("2006-01-02 15:04:05"))

	idb := make([]byte, 16)
	if _, err := rand.Read(idb); err != nil {
		return uploadTicket{}, &apiErr{http.StatusInternalServerError, "internal", "id generation failed"}
	}
	uploadID := hex.EncodeToString(idb)
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO attachment_uploads (upload_id, page_id) VALUES ($1, $2)`,
		uploadID, pageID); err != nil {
		return uploadTicket{}, &apiErr{http.StatusInternalServerError, "internal", "stage upload failed"}
	}

	exp := time.Now().Add(uploadTokenTTL)
	tok := s.mintUploadToken(uploadToken{
		U: uploadID, P: pageID, N: sanitizeUploadName(name), M: mime, X: davFileMaxBytes(), E: exp.Unix(),
	})
	return uploadTicket{
		UploadID:  uploadID,
		PutURL:    canonicalBaseURL() + "/api/uploads/" + tok,
		ExpiresAt: exp.UTC().Format(time.RFC3339),
		MaxBytes:  davFileMaxBytes(),
	}, nil
}

// UploadAttachmentBytes handles PUT /api/uploads/{token}. PUBLIC
// (auth.IsPublicPath): the signed token IS the authorization (minted for an
// editor on this page), so there's no session. Single-use, size-capped by the
// token, stored content-addressed into space_files.
func (s *Server) UploadAttachmentBytes(w http.ResponseWriter, r *http.Request) {
	t, ok := s.verifyUploadToken(r.PathValue("token"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "invalid or expired upload token")
		return
	}
	ctx := r.Context()

	// Single-use: the tracking row must exist and not be completed yet.
	var completed sql.NullString
	err := s.DB.QueryRowContext(ctx,
		`SELECT completed_at FROM attachment_uploads WHERE upload_id = $1 AND page_id = $2`,
		t.U, t.P).Scan(&completed)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "unknown upload")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "lookup upload failed")
		return
	}
	if completed.Valid {
		writeError(w, http.StatusConflict, "already_uploaded", "this upload URL was already used")
		return
	}

	page, err := selectPageByID(ctx, s.DB, t.P)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "page not found")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, t.X)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "upload exceeds the size limit")
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "empty upload")
		return
	}
	if ae := s.checkStorageQuota(ctx, page.SpaceID, int64(len(data))); ae != nil {
		writeError(w, ae.Status, ae.Code, ae.Message)
		return
	}

	sf, err := s.createPageUploadFile(ctx, page.SpaceID, t.P, t.N, data)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "store upload failed")
		return
	}
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE attachment_uploads SET space_file_id = $1, completed_at = tela_now() WHERE upload_id = $2`,
		sf.id, t.U); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "record upload failed")
		return
	}

	a := attachmentOut{
		ID: sf.id, Name: sf.name, Mime: sf.mime, ByteSize: sf.size, Hash: sf.hash,
		Embedded: strings.Contains(page.Body, sf.hash),
	}
	a.fillLinks(page.SpaceID)
	writeJSON(w, http.StatusOK, map[string]any{"attachment": newMCPAttachment(a)})
}

// confirmAttachmentUploadCore returns the stored file's ref for an upload_id —
// for hosts that couldn't read the PUT response. Any member may read the ref;
// the bytes were already authorized by the upload token.
func (s *Server) confirmAttachmentUploadCore(ctx context.Context, u *auth.User, k *auth.APIKey, uploadID string) (attachmentOut, *apiErr) {
	var pageID int64
	var fileID sql.NullInt64
	err := s.DB.QueryRowContext(ctx,
		`SELECT page_id, space_file_id FROM attachment_uploads WHERE upload_id = $1`,
		uploadID).Scan(&pageID, &fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return attachmentOut{}, &apiErr{http.StatusNotFound, "not_found", "unknown upload_id"}
	}
	if err != nil {
		return attachmentOut{}, &apiErr{http.StatusInternalServerError, "internal", "lookup upload failed"}
	}
	page, err := selectPageByID(ctx, s.DB, pageID)
	if errors.Is(err, sql.ErrNoRows) {
		return attachmentOut{}, &apiErr{http.StatusForbidden, "forbidden", "not a member"}
	}
	if err != nil {
		return attachmentOut{}, &apiErr{http.StatusInternalServerError, "internal", "lookup page failed"}
	}
	if _, ae := s.membershipCore(ctx, u, k, page.SpaceID); ae != nil {
		return attachmentOut{}, ae
	}
	if !fileID.Valid {
		return attachmentOut{}, &apiErr{http.StatusConflict, "not_uploaded", "the bytes have not been PUT to the upload URL yet"}
	}

	var a attachmentOut
	err = s.DB.QueryRowContext(ctx,
		`SELECT id, name, mime, byte_size, content_hash FROM space_files WHERE id = $1 AND deleted_at IS NULL`,
		fileID.Int64).Scan(&a.ID, &a.Name, &a.Mime, &a.ByteSize, &a.Hash)
	if errors.Is(err, sql.ErrNoRows) {
		return attachmentOut{}, &apiErr{http.StatusNotFound, "not_found", "uploaded file no longer exists"}
	}
	if err != nil {
		return attachmentOut{}, &apiErr{http.StatusInternalServerError, "internal", "load file failed"}
	}
	a.fillLinks(page.SpaceID)
	a.Embedded = strings.Contains(page.Body, a.Hash)
	_, _ = s.DB.ExecContext(ctx, `UPDATE attachment_uploads SET confirmed_at = tela_now() WHERE upload_id = $1`, uploadID)
	return a, nil
}
