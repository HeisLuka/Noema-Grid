package semantic

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrInvalidPreviewToken = errors.New("semantic: invalid preview token")
	ErrExpiredPreviewToken = errors.New("semantic: expired preview token")
)

type PreviewTokenClaims struct {
	Version              int               `json:"v"`
	SpaceID              int64             `json:"space_id"`
	PageID               int64             `json:"page_id"`
	SourceContentHash     string            `json:"source_hash"`
	Profile               ExtractionProfile `json:"profile"`
	CandidatePayloadHash  string            `json:"candidate_hash"`
	ExpiresAt             int64             `json:"exp"`
}

type PreviewSigner struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

func NewPreviewSigner(secret []byte, ttl time.Duration) (*PreviewSigner, error) {
	if len(secret) < 32 {
		return nil, errors.New("semantic preview signer: secret must be at least 32 bytes")
	}
	if ttl <= 0 {
		return nil, errors.New("semantic preview signer: ttl must be positive")
	}
	return &PreviewSigner{
		secret: append([]byte(nil), secret...),
		ttl:    ttl,
		now:    time.Now,
	}, nil
}

func (s *PreviewSigner) Sign(spaceID, pageID int64, sourceHash string, profile ExtractionProfile, candidateHash string) (string, PreviewTokenClaims, error) {
	if s == nil || len(s.secret) < 32 || s.now == nil || s.ttl <= 0 {
		return "", PreviewTokenClaims{}, errors.New("semantic preview signer: invalid signer")
	}
	if spaceID == 0 || pageID == 0 || strings.TrimSpace(sourceHash) == "" || strings.TrimSpace(candidateHash) == "" || !profile.Valid() {
		return "", PreviewTokenClaims{}, errors.New("semantic preview signer: incomplete claims")
	}
	claims := PreviewTokenClaims{
		Version:             1,
		SpaceID:             spaceID,
		PageID:              pageID,
		SourceContentHash:    sourceHash,
		Profile:              profile,
		CandidatePayloadHash: candidateHash,
		ExpiresAt:            s.now().Add(s.ttl).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", PreviewTokenClaims{}, fmt.Errorf("semantic preview signer: marshal: %w", err)
	}
	sig := s.mac(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig), claims, nil
}

func (s *PreviewSigner) Verify(token string) (PreviewTokenClaims, error) {
	if s == nil || len(s.secret) < 32 || s.now == nil {
		return PreviewTokenClaims{}, ErrInvalidPreviewToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return PreviewTokenClaims{}, ErrInvalidPreviewToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return PreviewTokenClaims{}, ErrInvalidPreviewToken
	}
	gotSig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return PreviewTokenClaims{}, ErrInvalidPreviewToken
	}
	if !hmac.Equal(gotSig, s.mac(payload)) {
		return PreviewTokenClaims{}, ErrInvalidPreviewToken
	}
	var claims PreviewTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return PreviewTokenClaims{}, ErrInvalidPreviewToken
	}
	if claims.Version != 1 || claims.SpaceID == 0 || claims.PageID == 0 || claims.SourceContentHash == "" || claims.CandidatePayloadHash == "" || !claims.Profile.Valid() || claims.ExpiresAt <= 0 {
		return PreviewTokenClaims{}, ErrInvalidPreviewToken
	}
	if !s.now().Before(time.Unix(claims.ExpiresAt, 0)) {
		return PreviewTokenClaims{}, ErrExpiredPreviewToken
	}
	return claims, nil
}

func (s *PreviewSigner) mac(payload []byte) []byte {
	m := hmac.New(sha256.New, s.secret)
	_, _ = m.Write(payload)
	return m.Sum(nil)
}
