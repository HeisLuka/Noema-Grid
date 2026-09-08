package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// APIKey is the subset of the api_keys row that authenticated bearer-token
// handlers need. Attached to request context by Middleware; consumers gate
// downstream operations on Scope and (optionally) SpaceID.
type APIKey struct {
	ID      int64
	UserID  int64
	Scope   string
	SpaceID *int64 // nil = inherit the user's normal space-membership
}

// Scope constants match the CHECK constraint on api_keys.scope.
const (
	ScopeRead  = "read"
	ScopeWrite = "write"
	ScopeAdmin = "admin"
)

// BearerPrefix is the leading token in the Authorization header. Together
// with the canonical `tela_pat_` prefix it identifies a Tela personal access
// token, distinguishing the bearer path from other future credential schemes.
const BearerPrefix = "Bearer "

// TokenPrefix is the literal "tela_pat_" preamble emitted on key creation.
// Stored separately from the random body so reads can recognise tokens at a
// glance and so future key formats can use a different prefix without losing
// addressability.
const TokenPrefix = "tela_pat_"

// apiKeyBodyBytes is the entropy of the random body (43 base64url chars).
// Matches the share-token convention.
const apiKeyBodyBytes = 32

// apiKeyTokenBodyRe matches the 43-char base64url body after stripping the
// tela_pat_ prefix. Used to early-reject malformed Authorization headers
// before touching the DB.
var apiKeyTokenBodyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

const apiKeyCtxKey contextKey = 2

// WithAPIKey returns a context that carries k, used by Middleware after a
// bearer token has been validated. Handlers read it back via APIKeyFromContext.
func WithAPIKey(ctx context.Context, k *APIKey) context.Context {
	return context.WithValue(ctx, apiKeyCtxKey, k)
}

// APIKeyFromContext returns the API key attached by Middleware. The second
// return is false when the request was authenticated via session cookie (or
// is anonymous), so handlers can branch on "is this a bearer-authed call".
func APIKeyFromContext(ctx context.Context) (*APIKey, bool) {
	k, ok := ctx.Value(apiKeyCtxKey).(*APIKey)
	return k, ok
}

// apiKeySecret is cached at first read so the bearer middleware and the
// /api/api_keys CRUD handlers see the same bytes. Without the cache an unset
// TELA_API_KEY_SECRET would mint a fresh random secret for each caller and
// every newly-issued key would 401 on its first bearer request.
var (
	apiKeySecretOnce sync.Once
	apiKeySecretVal  []byte
)

// LoadAPIKeySecret reads TELA_API_KEY_SECRET and returns its raw bytes.
// On an empty value falls back to a freshly generated per-process key and
// logs a banner — without a stable secret every restart invalidates every
// outstanding PAT. Decode hex when the value looks hex-encoded (matches the
// `openssl rand -hex 32` recommendation from .env.example), otherwise use the
// raw bytes — both shapes work because HMAC keys are length-agnostic.
//
// Idempotent (sync.Once cached). The cache is process-lifetime, which means
// changing the env at runtime requires a restart — matches every other
// secret/secret-cookie pattern in Tela.
func LoadAPIKeySecret() []byte {
	apiKeySecretOnce.Do(func() {
		if v := os.Getenv("TELA_API_KEY_SECRET"); v != "" {
			apiKeySecretVal = decodeAPIKeySecret(v)
			return
		}
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			// Boot-fatal: no usable HMAC secret. Keep fatal semantics.
			slog.Error("auth: generate api-key secret", "err", err)
			os.Exit(1)
		}
		apiKeySecretVal = buf
		slog.Warn("auth: TELA_API_KEY_SECRET not set and no persisted secret — generated a " +
			"random per-process key; ALL API keys invalidate on restart " +
			"(only hit when InitAPIKeySecret was not called)")
	})
	return apiKeySecretVal
}

// SecretStore persists a generated secret so it survives process restarts.
// Implemented by *settings.Store; the interface keeps the auth package free of
// a settings import so the dependency direction stays clean.
type SecretStore interface {
	GetOrInitSecret(ctx context.Context, key string, nbytes int) ([]byte, error)
}

// InitAPIKeySecret resolves the api-key HMAC secret with precedence
// env override → persisted store value → freshly generated-and-persisted, and
// primes the package cache so LoadAPIKeySecret returns it. Call once at boot
// before any bearer request is served. This is what fixes the "random
// per-process secret invalidates every PAT on restart" footgun for operators
// who don't set TELA_API_KEY_SECRET: the generated secret is persisted once.
const apiKeySecretPlaceholder = "replace-me-with-openssl-rand-hex-32"

func InitAPIKeySecret(ctx context.Context, store SecretStore) error {
	if v := os.Getenv("TELA_API_KEY_SECRET"); v != "" {
		if v == apiKeySecretPlaceholder {
			slog.Warn("SECURITY: TELA_API_KEY_SECRET is set to the public placeholder from .env.example — " +
				"the HMAC key is publicly known. Run: openssl rand -hex 32")
		}
		setAPIKeySecret(decodeAPIKeySecret(v))
		return nil
	}
	b, err := store.GetOrInitSecret(ctx, "api_key", 32)
	if err != nil {
		return err
	}
	setAPIKeySecret(b)
	return nil
}

// decodeAPIKeySecret mirrors the env path: hex-decode when it looks hex, else
// use the raw bytes (HMAC keys are length-agnostic).
func decodeAPIKeySecret(v string) []byte {
	if decoded, err := hex.DecodeString(v); err == nil && len(decoded) > 0 {
		return decoded
	}
	return []byte(v)
}

// setAPIKeySecret primes the cache by consuming the sync.Once with b, so a
// later LoadAPIKeySecret returns b rather than generating a per-process key.
func setAPIKeySecret(b []byte) {
	apiKeySecretOnce.Do(func() { apiKeySecretVal = b })
}

// ResetAPIKeySecretCache is exposed for tests so each newWiredServer call can
// pin a deterministic TELA_API_KEY_SECRET. Must be called BEFORE any handler
// that reads the secret. Not for production use.
func ResetAPIKeySecretCache() {
	apiKeySecretOnce = sync.Once{}
	apiKeySecretVal = nil
}

// HMACAPIKey returns the hex-encoded HMAC-SHA256 of rawKey under secret.
// Single source of truth for both bearer middleware lookup and CRUD-route
// insertion, so the two paths can never drift.
func HMACAPIKey(secret []byte, rawKey string) string {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(rawKey))
	return hex.EncodeToString(h.Sum(nil))
}

// NewAPIKey generates a fresh raw token and returns (raw, prefix, hmacHex).
// raw is the value returned to the user (once) on POST /api/api_keys; prefix
// is the first 8 chars of the random body (after the tela_pat_ preamble) so
// the management UI can tell rotated keys apart — slicing raw[:8] would yield
// the literal "tela_pat" for every key, which is identical across the table.
// hmacHex is stored in api_keys.key_hmac.
func NewAPIKey(secret []byte) (string, string, string, error) {
	buf := make([]byte, apiKeyBodyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", "", err
	}
	raw := TokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	prefix := raw[len(TokenPrefix) : len(TokenPrefix)+8]
	return raw, prefix, HMACAPIKey(secret, raw), nil
}

// ErrInvalidAPIKey marks a bearer token that did not resolve to an active
// row (missing / revoked / expired / wrong shape). Callers treat this as 401.
// Distinct from ErrInvalidSession so future logging / metrics can distinguish
// the two failure modes without parsing the error message.
var ErrInvalidAPIKey = errors.New("auth: invalid api key")

// LookupAPIKey resolves a bearer token to an APIKey row + stamps last_used_at
// asynchronously. Best-effort: a failed last_used_at update is logged and
// dropped so a transient DB hiccup never 401s a valid key.
//
// Constant-time compare happens at the DB level: the SELECT keys off
// idx_api_keys_hmac (UNIQUE), so there is no value-by-value comparison the
// caller could time. The hash itself is computed locally before the SELECT.
func LookupAPIKey(ctx context.Context, d *sql.DB, secret []byte, rawKey string) (*APIKey, error) {
	if !strings.HasPrefix(rawKey, TokenPrefix) {
		return nil, ErrInvalidAPIKey
	}
	body := strings.TrimPrefix(rawKey, TokenPrefix)
	if !apiKeyTokenBodyRe.MatchString(body) {
		return nil, ErrInvalidAPIKey
	}
	hmacHex := HMACAPIKey(secret, rawKey)

	var (
		k         APIKey
		spaceID   sql.NullInt64
		expiresAt sql.NullString
		isActive  int
	)
	err := d.QueryRowContext(ctx, `
		SELECT k.id, k.user_id, k.scope, k.space_id, k.expires_at, u.is_active
		  FROM api_keys k
		  JOIN users u ON u.id = k.user_id
		 WHERE k.key_hmac = $1
		   AND k.revoked_at IS NULL`, hmacHex).
		Scan(&k.ID, &k.UserID, &k.Scope, &spaceID, &expiresAt, &isActive)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidAPIKey
	}
	if err != nil {
		return nil, err
	}
	if isActive != 1 {
		// Inactive user → reject the key the same way as a revoked row.
		return nil, ErrInvalidAPIKey
	}
	if expiresAt.Valid {
		t, perr := time.Parse("2006-01-02 15:04:05", expiresAt.String)
		if perr == nil && !t.After(time.Now().UTC()) {
			return nil, ErrInvalidAPIKey
		}
	}
	if spaceID.Valid {
		v := spaceID.Int64
		k.SpaceID = &v
	}

	// Non-blocking last_used_at stamp. The goroutine outlives the request
	// intentionally — failures are logged and dropped. Using d.ExecContext
	// with a fresh background context keeps it independent of the request
	// timeline so the response isn't held up by the write.
	go func(id int64) {
		if _, ierr := d.ExecContext(context.Background(),
			`UPDATE api_keys SET last_used_at = tela_now() WHERE id = $1`, id); ierr != nil {
			slog.Error("auth: api_key last_used_at update failed", "key_id", id, "err", ierr)
		}
	}(k.ID)

	return &k, nil
}

// userForAPIKey loads the User row paired with an authenticated API key. The
// session-mode middleware path attaches a *User; bearer-mode mirrors that so
// downstream handlers can keep treating the caller as a "user" without
// special-casing per credential type. Scope/space_id gating happens via the
// APIKey context entry, not via User fields.
func userForAPIKey(ctx context.Context, d *sql.DB, userID int64) (*User, error) {
	var (
		u       User
		email   sql.NullString
		isAdmin int
	)
	err := d.QueryRowContext(ctx,
		`SELECT id, username, email, is_instance_admin FROM users WHERE id = $1 AND is_active = 1`,
		userID).Scan(&u.ID, &u.Username, &email, &isAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidAPIKey
	}
	if err != nil {
		return nil, err
	}
	u.Email = email.String
	u.IsInstanceAdmin = isAdmin == 1
	return &u, nil
}

// UserForAPIKey resolves the *User behind a validated PAT. Exported wrapper
// over userForAPIKey for the MCP Streamable-HTTP transport's bearer verifier,
// which self-authenticates outside Middleware and needs to build the same
// *User the cookie/bearer paths attach to context.
func UserForAPIKey(ctx context.Context, d *sql.DB, userID int64) (*User, error) {
	return userForAPIKey(ctx, d, userID)
}

// extractBearerToken returns the raw token from the Authorization header, or
// "" when the header is absent / non-Bearer / non-tela_pat_. Callers use the
// empty return to decide whether to attempt bearer auth or fall through to
// cookie-session auth.
func extractBearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, BearerPrefix) {
		return ""
	}
	tok := strings.TrimSpace(strings.TrimPrefix(h, BearerPrefix))
	if !strings.HasPrefix(tok, TokenPrefix) {
		return ""
	}
	return tok
}

// scopeAllowsMethod returns true when the bearer token's scope permits the
// HTTP method. read = GET only; write/admin permit any method. Per-route
// admin gating (user CRUD, api_keys management) is enforced inside the
// handlers themselves via RequireAdminScope.
func scopeAllowsMethod(scope, method string) bool {
	switch scope {
	case ScopeRead:
		return method == http.MethodGet || method == http.MethodHead
	case ScopeWrite, ScopeAdmin:
		return true
	}
	return false
}

// scopeAllowsRequest is the path-aware extension of scopeAllowsMethod. The
// default policy (scopeAllowsMethod) is unchanged for every route except the
// targeted carve-outs listed here. Used by Middleware in place of the bare
// method check.
//
// Carve-outs:
//
//   - POST /api/feedback (M17.A.1) — submitting meta-feedback about Tela /
//     tela-mcp themselves is allowed for every scope (including read).
//   - POST /api/spaces/{id}/semantic/pages/{page_id}/preview — Noema preview is
//     strictly read-only despite using POST for a structured request body. Read
//     PATs may preview; canonical persistence remains on the separate commit
//     route and is NOT carved out.
func scopeAllowsRequest(scope, method, path string) bool {
	if method == http.MethodPost && (path == "/api/feedback" || isSemanticPreviewPath(path)) {
		switch scope {
		case ScopeRead, ScopeWrite, ScopeAdmin:
			return true
		}
		return false
	}
	return scopeAllowsMethod(scope, method)
}

func isSemanticPreviewPath(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) == 7 &&
		parts[0] == "api" &&
		parts[1] == "spaces" &&
		parts[3] == "semantic" &&
		parts[4] == "pages" &&
		parts[6] == "preview"
}
