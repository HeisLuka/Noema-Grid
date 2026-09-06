package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"testing"
)

// attachments_test.go covers the page-attachments surface: listing the
// space_files parented to a page (with the embedded flag) and the public,
// content-addressed blob serve (inline for images, forced-download otherwise).

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// seedAttachment inserts a live space_file parented to a page, returning its hash.
func seedAttachment(t *testing.T, d *sql.DB, spaceID, pageID int64, name, mime string, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if _, err := d.ExecContext(context.Background(), `
		INSERT INTO space_files (space_id, parent_page_id, name, content_hash, mime, data, byte_size)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		spaceID, pageID, name, hash, mime, data, int64(len(data))); err != nil {
		t.Fatalf("insert space_file %q: %v", name, err)
	}
	return hash
}

func TestAttachments_ListAndEmbeddedFlag(t *testing.T) {
	ts, d := newWiredServer(t)
	uid := seedUser(t, d, "owner", "pw-owner-123", false)
	spaceID := seedSpace(t, d, "Engineering", "eng", uid)
	// Body references one file's hash → embedded; the other is loose.
	pdfHashPlaceholder := sha256.Sum256([]byte("PDFDATA"))
	body := "See attached " + hex.EncodeToString(pdfHashPlaceholder[:]) + " inline.\n"
	pageID := seedPageInSpace(t, d, spaceID, nil, "Doc", body)
	hPdf := seedAttachment(t, d, spaceID, pageID, "report.pdf", "application/pdf", []byte("PDFDATA"))
	seedAttachment(t, d, spaceID, pageID, "notes.txt", "text/plain", []byte("loose file"))

	client := loginClient(t, ts, "owner", "pw-owner-123")
	resp, err := client.Get(ts.URL + "/api/pages/" + itoa(pageID) + "/attachments")
	if err != nil {
		t.Fatalf("GET attachments: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Attachments []attachmentOut `json:"attachments"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Attachments) != 2 {
		t.Fatalf("got %d attachments, want 2", len(out.Attachments))
	}
	byName := map[string]attachmentOut{}
	for _, a := range out.Attachments {
		byName[a.Name] = a
	}
	if !byName["report.pdf"].Embedded {
		t.Error("report.pdf should be embedded (its hash is in the body)")
	}
	if byName["notes.txt"].Embedded {
		t.Error("notes.txt should NOT be embedded")
	}
	wantURL := "/api/files/" + itoa(spaceID) + "/" + hPdf + ".pdf"
	if byName["report.pdf"].URL != wantURL {
		t.Errorf("report.pdf url = %q, want %q", byName["report.pdf"].URL, wantURL)
	}
	// The share path travels with every attachment payload so no client derives
	// it (the frontend's Copy link just prefixes its own origin).
	wantShare := "/f/" + hPdf[:fileHashShortLen] + "/report.pdf"
	if byName["report.pdf"].SharePath != wantShare {
		t.Errorf("report.pdf share_path = %q, want %q", byName["report.pdf"].SharePath, wantShare)
	}
}

func TestAttachments_ServeInlineVsDownload(t *testing.T) {
	ts, d := newWiredServer(t)
	uid := seedUser(t, d, "owner", "pw-owner-123", false)
	spaceID := seedSpace(t, d, "Engineering", "eng", uid)
	pageID := seedPageInSpace(t, d, spaceID, nil, "Doc", "")
	hPdf := seedAttachment(t, d, spaceID, pageID, "report.pdf", "application/pdf", []byte("%PDF-1.4 body"))
	hPng := seedAttachment(t, d, spaceID, pageID, "logo.png", "image/png", []byte("PNGBYTES"))

	// Serve is PUBLIC — no auth client needed.
	get := func(path string) (*http.Response, string) {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	resp, body := get("/api/files/" + itoa(spaceID) + "/" + hPdf + ".pdf")
	if resp.StatusCode != http.StatusOK || body != "%PDF-1.4 body" {
		t.Fatalf("pdf serve: status=%d body=%q", resp.StatusCode, body)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd[:10] != "attachment" {
		t.Errorf("pdf Content-Disposition = %q, want attachment", cd)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff on file serve")
	}

	resp, _ = get("/api/files/" + itoa(spaceID) + "/" + hPng + ".png")
	if cd := resp.Header.Get("Content-Disposition"); cd[:6] != "inline" {
		t.Errorf("png Content-Disposition = %q, want inline", cd)
	}

	// Unknown hash → 404 (the hash is the capability). Serve is content-addressed
	// and falls back across spaces, so a VALID hash under the "wrong" space still
	// resolves — this is what lets body embeds survive a cross-space page move.
	if resp, _ := get("/api/files/" + itoa(spaceID) + "/deadbeef.pdf"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("bad-hash status = %d, want 404", resp.StatusCode)
	}
	if resp, body := get("/api/files/99999/" + hPdf + ".pdf"); resp.StatusCode != http.StatusOK || body != "%PDF-1.4 body" {
		t.Errorf("cross-space serve: status=%d body=%q, want 200 + bytes", resp.StatusCode, body)
	}
}

// A cross-space page move must carry the page's space_files along, so they don't
// strand in the old space (wrong quota, wrong /dav tree, lost on its deletion).
// The body embed keeps resolving via the content-addressed serve fallback even
// though its URL still hard-codes the old space.
func TestAttachments_CrossSpaceMoveMigratesFiles(t *testing.T) {
	ts, d := newWiredServer(t)
	owner := seedUser(t, d, "owner", "pw-owner-123", false)
	src := seedSpace(t, d, "Source", "src", owner)
	dst := seedSpace(t, d, "Dest", "dst", owner)

	// Page lives in src; its body embeds the image via the src-space URL.
	img := []byte("PNGBYTES")
	sum := sha256.Sum256(img)
	hImg := hex.EncodeToString(sum[:])
	body := "![logo](/api/files/" + itoa(src) + "/" + hImg + ".png)\n"
	pageID := seedPageInSpace(t, d, src, nil, "Doc", body)
	seedAttachment(t, d, src, pageID, "logo.png", "image/png", img)

	client := loginClient(t, ts, "owner", "pw-owner-123")

	// Move the page to dst.
	mvReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/pages/"+itoa(pageID)+"/move",
		bytes.NewReader([]byte(`{"space_id":`+itoa(dst)+`}`)))
	mvReq.Header.Set("Content-Type", "application/json")
	mvResp, err := client.Do(mvReq)
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	mvResp.Body.Close()
	if mvResp.StatusCode != http.StatusOK {
		t.Fatalf("move status=%d, want 200", mvResp.StatusCode)
	}

	// The file row followed the page into dst.
	var fileSpace int64
	if err := d.QueryRowContext(context.Background(),
		`SELECT space_id FROM space_files WHERE parent_page_id = $1`, pageID).Scan(&fileSpace); err != nil {
		t.Fatalf("read space_file: %v", err)
	}
	if fileSpace != dst {
		t.Fatalf("space_file.space_id = %d after move, want %d (dst)", fileSpace, dst)
	}

	get := func(path string) int {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// New dst-space URL serves; the OLD src-space URL still in the body also
	// serves via the content-addressed fallback (no body rewrite needed).
	if code := get("/api/files/" + itoa(dst) + "/" + hImg + ".png"); code != http.StatusOK {
		t.Errorf("dst-space serve = %d, want 200", code)
	}
	if code := get("/api/files/" + itoa(src) + "/" + hImg + ".png"); code != http.StatusOK {
		t.Errorf("old src-space embed serve = %d, want 200 (fallback)", code)
	}

	// list_attachments now advertises the dst URL.
	resp, err := client.Get(ts.URL + "/api/pages/" + itoa(pageID) + "/attachments")
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Attachments []attachmentOut `json:"attachments"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Attachments) != 1 {
		t.Fatalf("got %d attachments, want 1", len(out.Attachments))
	}
	wantURL := "/api/files/" + itoa(dst) + "/" + hImg + ".png"
	if out.Attachments[0].URL != wantURL {
		t.Errorf("attachment url = %q, want %q", out.Attachments[0].URL, wantURL)
	}
}

// uploadAttachment POSTs a multipart file to the page attachments endpoint.
func uploadAttachment(t *testing.T, client *http.Client, baseURL string, pageID int64, filename string, data []byte) (attachmentOut, int) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", baseURL+"/api/pages/"+itoa(pageID)+"/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Attachment attachmentOut `json:"attachment"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Attachment, resp.StatusCode
}

func TestAttachments_UploadDedupeAndCollision(t *testing.T) {
	ts, d := newWiredServer(t)
	uid := seedUser(t, d, "owner", "pw-owner-123", false)
	spaceID := seedSpace(t, d, "Engineering", "eng", uid)
	pageID := seedPageInSpace(t, d, spaceID, nil, "Doc", "")
	client := loginClient(t, ts, "owner", "pw-owner-123")

	// First upload.
	a1, st := uploadAttachment(t, client, ts.URL, pageID, "image.png", []byte("AAAA"))
	if st != http.StatusOK || a1.Name != "image.png" {
		t.Fatalf("upload1: status=%d name=%q", st, a1.Name)
	}
	// Identical bytes + name → dedupe (same name, no new row).
	a2, _ := uploadAttachment(t, client, ts.URL, pageID, "image.png", []byte("AAAA"))
	if a2.Name != "image.png" || a2.Hash != a1.Hash {
		t.Fatalf("dedupe failed: name=%q hash=%q", a2.Name, a2.Hash)
	}
	// Different bytes, same name → disambiguated, first must survive.
	a3, _ := uploadAttachment(t, client, ts.URL, pageID, "image.png", []byte("BBBB"))
	if a3.Name == "image.png" {
		t.Fatalf("collision not disambiguated: %q", a3.Name)
	}
	if countLiveFiles(t, d, spaceID) != 2 {
		t.Fatalf("want 2 distinct files, got %d", countLiveFiles(t, d, spaceID))
	}

	// Both retrievable via their serve URLs.
	for _, a := range []attachmentOut{a1, a3} {
		resp, _ := http.Get(ts.URL + a.URL)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("serve %q = %d", a.URL, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Delete a1.
	req, _ := http.NewRequest("DELETE", ts.URL+"/api/pages/"+itoa(pageID)+"/attachments/"+itoa(a1.ID), nil)
	resp, _ := client.Do(req)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	if countLiveFiles(t, d, spaceID) != 1 {
		t.Fatalf("after delete: %d live files, want 1", countLiveFiles(t, d, spaceID))
	}
}
