package api

import (
	"bytes"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

func ogGet(t *testing.T, base, path string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+path, nil)
	resp, err := http.DefaultClient.Do(req) // cookieless: also pins the IsPublicPath bypass
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func assertPNG(t *testing.T, base, path string) {
	t.Helper()
	resp, body := ogGet(t, base, path)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("%s: status=%d ct=%q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if img, err := png.Decode(bytes.NewReader(body)); err != nil {
		t.Fatalf("%s: png decode: %v", path, err)
	} else if img.Bounds().Dx() != 1200 || img.Bounds().Dy() != 630 {
		t.Fatalf("%s: bounds=%v want 1200x630", path, img.Bounds())
	}
}

func TestFeatureOG_GraphDiscover(t *testing.T) {
	ts, _ := newWiredServer(t)
	for _, c := range []struct{ path, want string }{
		{"/graph", "Knowledge graph"},
		{"/discover", "Discover"},
	} {
		resp, body := ogGet(t, ts.URL, c.path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status=%d want 200", c.path, resp.StatusCode)
		}
		if !strings.Contains(string(body), `og:title" content="`+c.want) {
			t.Fatalf("%s: og:title missing %q\n%s", c.path, c.want, body)
		}
		assertPNG(t, ts.URL, c.path+"/og.png")
	}
}

func TestRootOG(t *testing.T) {
	ts, _ := newWiredServer(t)

	// No custom-domain org on the request → the generic tela apex card. (The
	// org-branded variant is exercised by the host→org middleware in prod; here
	// we pin the envelope + that "/" is public and renders an image.)
	resp, body := ogGet(t, ts.URL, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("root OG status=%d want 200 (must bypass auth)", resp.StatusCode)
	}
	s := string(body)
	for _, want := range []string{`og:title" content="tela`, `og:image" content=`, "/og.png", `og:type" content="website"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("root OG missing %q\n%s", want, s)
		}
	}
	assertPNG(t, ts.URL, "/og.png")
}

func TestSpaceOG(t *testing.T) {
	ts, d := newWiredServer(t)
	admin := seedUser(t, d, "admin", "adminpw12", true)
	sid := seedSpace(t, d, "Engineering Wiki", "engineering-wiki", admin)

	// A private space (the default) must NOT leak an OG card to crawlers.
	if respPriv, _ := ogGet(t, ts.URL, fmt.Sprintf("/spaces/%d", sid)); respPriv.StatusCode != http.StatusNotFound {
		t.Fatalf("private space OG status=%d want 404", respPriv.StatusCode)
	}
	if _, err := d.Exec(`UPDATE spaces SET visibility = 'public' WHERE id = $1`, sid); err != nil {
		t.Fatalf("publish space: %v", err)
	}

	resp, body := ogGet(t, ts.URL, fmt.Sprintf("/spaces/%d", sid))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("space OG status=%d want 200", resp.StatusCode)
	}
	s := string(body)
	if !strings.Contains(s, `og:title" content="Engineering Wiki`) {
		t.Fatalf("space OG missing the space name in og:title\n%s", s)
	}
	if !strings.Contains(s, fmt.Sprintf("/spaces/%d/og.png", sid)) {
		t.Fatalf("space OG missing og:image pointing at the space card\n%s", s)
	}
	assertPNG(t, ts.URL, fmt.Sprintf("/spaces/%d/og.png", sid))

	// Missing space → HTML 404.
	if resp404, _ := ogGet(t, ts.URL, "/spaces/99999"); resp404.StatusCode != http.StatusNotFound {
		t.Fatalf("missing space: status=%d want 404", resp404.StatusCode)
	}
}

// A card title with no spaces — a filename, a URL — used to hit the "one word
// longer than the line" path and get truncated at the first line, so the file
// card showed "attachment-sharing-spec…" and never the extension. It must break
// at punctuation instead, keeping every character.
func TestWrapLines_BreaksAtPunctuationInsteadOfTruncating(t *testing.T) {
	face, err := opentype.NewFace(ogBoldFont, &opentype.FaceOptions{
		Size: ogTitleSize, DPI: 72, Hinting: font.HintingFull,
	})
	if err != nil {
		t.Fatalf("title face: %v", err)
	}
	defer face.Close()

	const name = "attachment-sharing-spec.pdf"
	lines := wrapLines(face, name, ogDrawableWidth, 2)
	if len(lines) < 2 {
		t.Fatalf("expected the filename to wrap, got %q", lines)
	}
	if joined := strings.Join(lines, ""); joined != name {
		t.Errorf("wrapped text = %q, want the filename intact", joined)
	}
	// Ordinary prose is untouched: it still wraps on spaces.
	if got := wrapLines(face, "Hello", ogDrawableWidth, 2); len(got) != 1 || got[0] != "Hello" {
		t.Errorf("short title = %q, want one unchanged line", got)
	}
}
