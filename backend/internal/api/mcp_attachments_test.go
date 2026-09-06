package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zcag/tela/backend/internal/auth"
)

// TestMCP_AttachmentTools exercises upload_attachment → list_attachments →
// delete_attachment end-to-end over the MCP transport: an agent uploads an
// image by base64, sees it listed with a ready-to-embed markdown snippet, then
// removes it.
func TestMCP_AttachmentTools(t *testing.T) {
	ts, d := newWiredServer(t)
	alice := seedUser(t, d, "alice", "alicepw12", false)
	space := seedSpace(t, d, "Alice Space", "alice-space", alice)
	var pageID int64
	if err := d.QueryRow(`INSERT INTO pages (space_id, parent_id, title, body, position)
	                       VALUES ($1, NULL, 'P', 'body', 0) RETURNING id`, space).Scan(&pageID); err != nil {
		t.Fatalf("seed page: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sess := mcpSession(t, ctx, ts, seedReadKey(t, d, alice, auth.ScopeWrite))

	// Upload a 1x1 png by base64.
	var up struct {
		Attachment struct {
			ID       int64  `json:"id"`
			Mime     string `json:"mime"`
			URL      string `json:"url"`
			Markdown string `json:"markdown"`
			ShareURL string `json:"share_url"`
		} `json:"attachment"`
	}
	mcpCallJSON(t, ctx, sess, "upload_attachment", map[string]any{
		"page_id":     pageID,
		"name":        "pixel.png",
		"data_base64": base64.StdEncoding.EncodeToString(pngOnePixel),
	}, &up)
	if up.Attachment.ID == 0 || up.Attachment.Mime != "image/png" {
		t.Fatalf("upload result = %+v", up.Attachment)
	}
	if !strings.HasPrefix(up.Attachment.Markdown, "![pixel.png](") {
		t.Errorf("markdown snippet = %q, want an inline image embed", up.Attachment.Markdown)
	}
	if !strings.HasPrefix(up.Attachment.URL, "/api/files/") {
		t.Errorf("url = %q, want /api/files/…", up.Attachment.URL)
	}
	// share_url is the link an agent hands a person: absolute, the file PAGE
	// (which previews + unfurls), never the blob.
	if !strings.Contains(up.Attachment.ShareURL, "/f/") || !strings.HasSuffix(up.Attachment.ShareURL, "/pixel.png") {
		t.Errorf("share_url = %q, want an absolute /f/{hash}/pixel.png", up.Attachment.ShareURL)
	}
	if strings.Contains(up.Attachment.ShareURL, "/api/files/") {
		t.Errorf("share_url must not be the blob URL: %q", up.Attachment.ShareURL)
	}

	// list_attachments shows it.
	var ls struct {
		Attachments []struct {
			ID int64 `json:"id"`
		} `json:"attachments"`
	}
	mcpCallJSON(t, ctx, sess, "list_attachments", map[string]any{"page_id": pageID}, &ls)
	if len(ls.Attachments) != 1 || ls.Attachments[0].ID != up.Attachment.ID {
		t.Fatalf("list after upload = %+v", ls.Attachments)
	}

	// delete_attachment removes it.
	var del struct {
		OK bool `json:"ok"`
	}
	mcpCallJSON(t, ctx, sess, "delete_attachment", map[string]any{
		"page_id": pageID, "id": up.Attachment.ID,
	}, &del)
	if !del.OK {
		t.Fatalf("delete ok = %v", del.OK)
	}

	mcpCallJSON(t, ctx, sess, "list_attachments", map[string]any{"page_id": pageID}, &ls)
	if len(ls.Attachments) != 0 {
		t.Fatalf("list after delete = %+v", ls.Attachments)
	}
}

// TestMCP_UploadAttachmentCap verifies the inline base64 cap rejects an oversize
// payload (so a giant blob can't ride through the model context) while still
// allowing small files. The cap is shrunk for the test to avoid shipping MBs.
func TestMCP_UploadAttachmentCap(t *testing.T) {
	ts, d := newWiredServer(t)
	alice := seedUser(t, d, "alice", "alicepw12", false)
	space := seedSpace(t, d, "Alice Space", "alice-space", alice)
	var pageID int64
	if err := d.QueryRow(`INSERT INTO pages (space_id, parent_id, title, body, position)
	                       VALUES ($1, NULL, 'P', 'body', 0) RETURNING id`, space).Scan(&pageID); err != nil {
		t.Fatalf("seed page: %v", err)
	}

	orig := mcpInlineUploadCap
	mcpInlineUploadCap = 64
	defer func() { mcpInlineUploadCap = orig }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sess := mcpSession(t, ctx, ts, seedReadKey(t, d, alice, auth.ScopeWrite))

	// 128 bytes > the (shrunk) 64-byte cap → tool error, nothing stored.
	over := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 128))
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "upload_attachment", Arguments: map[string]any{
		"page_id": pageID, "name": "big.bin", "data_base64": over,
	}})
	if err != nil {
		t.Fatalf("call over-cap: %v", err)
	}
	if !res.IsError {
		t.Fatalf("over-cap upload should be a tool error, got success")
	}

	// A small payload still succeeds under the same cap.
	under := base64.StdEncoding.EncodeToString([]byte("hi"))
	res2, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "upload_attachment", Arguments: map[string]any{
		"page_id": pageID, "name": "small.txt", "data_base64": under,
	}})
	if err != nil {
		t.Fatalf("call under-cap: %v", err)
	}
	if res2.IsError {
		t.Fatalf("under-cap upload should succeed: %v", res2.Content)
	}
}

// list_space_files is the only tool that can find a file parented to the space
// ROOT — a synced/imported file beside the page tree, which list_attachments
// (per-page, by definition) never sees.
func TestMCP_ListSpaceFiles(t *testing.T) {
	ts, d := newWiredServer(t)
	alice := seedUser(t, d, "alice", "alicepw12", false)
	space := seedSpace(t, d, "Alice Space", "alice-space", alice)
	var pageID int64
	if err := d.QueryRow(`INSERT INTO pages (space_id, parent_id, title, body, position)
	                       VALUES ($1, NULL, 'Doc', 'body', 0) RETURNING id`, space).Scan(&pageID); err != nil {
		t.Fatalf("seed page: %v", err)
	}
	seedAttachment(t, d, space, pageID, "on-page.pdf", "application/pdf", []byte("%PDF-1.4"))
	if _, err := d.Exec(`
		INSERT INTO space_files (space_id, parent_page_id, name, content_hash, mime, data, byte_size)
		VALUES ($1, NULL, 'synced-cv.pdf', $2, 'application/pdf', $3, 8)`,
		space, strings.Repeat("b", 64), []byte("%PDF-1.4")); err != nil {
		t.Fatalf("seed root file: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sess := mcpSession(t, ctx, ts, seedReadKey(t, d, alice, auth.ScopeRead))

	var out struct {
		Files []struct {
			Name         string `json:"name"`
			ParentPageID int64  `json:"parent_page_id"`
			ParentTitle  string `json:"parent_title"`
			ShareURL     string `json:"share_url"`
			DownloadURL  string `json:"download_url"`
		} `json:"files"`
	}
	mcpCallJSON(t, ctx, sess, "list_space_files", map[string]any{"space_id": space}, &out)
	if len(out.Files) != 2 {
		t.Fatalf("space files = %+v, want the page file AND the root file", out.Files)
	}
	byName := map[string]int{}
	for i, f := range out.Files {
		byName[f.Name] = i
	}
	root := out.Files[byName["synced-cv.pdf"]]
	if root.ParentPageID != 0 || root.ParentTitle != "" {
		t.Errorf("root file should have no parent: %+v", root)
	}
	if !strings.Contains(root.ShareURL, "/f/") || !strings.Contains(root.DownloadURL, "/api/files/") {
		t.Errorf("root file links = %+v", root)
	}
	if p := out.Files[byName["on-page.pdf"]]; p.ParentPageID != pageID || p.ParentTitle != "Doc" {
		t.Errorf("page file should name its parent: %+v", p)
	}

	// The root filter narrows to exactly the file no page lists.
	mcpCallJSON(t, ctx, sess, "list_space_files", map[string]any{
		"space_id": space, "parent_page_id": "root",
	}, &out)
	if len(out.Files) != 1 || out.Files[0].Name != "synced-cv.pdf" {
		t.Fatalf("root filter = %+v", out.Files)
	}
}
