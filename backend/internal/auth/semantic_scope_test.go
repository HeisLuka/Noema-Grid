package auth

import (
	"net/http"
	"testing"
)

func TestScopeAllowsRequestSemanticPreviewCarveOut(t *testing.T) {
	preview := "/api/spaces/17/semantic/pages/42/preview"
	commit := "/api/spaces/17/semantic/commit"

	if !scopeAllowsRequest(ScopeRead, http.MethodPost, preview) {
		t.Fatal("read-scope PAT should be allowed to call read-only semantic preview")
	}
	if scopeAllowsRequest(ScopeRead, http.MethodPost, commit) {
		t.Fatal("read-scope PAT must not be allowed to commit semantic state")
	}
	if scopeAllowsRequest(ScopeRead, http.MethodPost, "/api/spaces/17/semantic/pages/42/not-preview") {
		t.Fatal("semantic preview carve-out matched a neighboring write-shaped path")
	}
	if !scopeAllowsRequest(ScopeWrite, http.MethodPost, preview) {
		t.Fatal("write-scope PAT should retain normal access to semantic preview")
	}
}
