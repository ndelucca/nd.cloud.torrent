package files

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestHandler mounts a Handler over a temp root the way the server does.
func newTestHandler(t *testing.T) (http.Handler, string) {
	t.Helper()
	root := t.TempDir()
	h := http.StripPrefix("/download/", &Handler{Root: func() string { return root }})
	return h, root
}

// TestServedContentIsSandboxed covers the stored-XSS hole.
//
// Content-Type comes from the file extension, so a torrent carrying an
// index.html was served as text/html from the app's own origin. nosniff does not
// help when the declared type *is* text/html, so the script ran same-origin and
// could drive every /api/* mutation and DELETE /download/*. Without the CSP
// header this test fails.
func TestServedContentIsSandboxed(t *testing.T) {
	h, root := newTestHandler(t)
	evil := `<script>fetch("/api/torrents/x", {method: "DELETE"})</script>`
	if err := os.WriteFile(filepath.Join(root, "evil.html"), []byte(evil), 0644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/download/evil.html", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// The premise: this really is served as a document, which is why the header
	// is needed. If this stops being true the test is no longer meaningful.
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html — the test no longer covers its case", ct)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Fatalf("Content-Security-Policy = %q, want sandbox", csp)
	}
}

// TestZipIsSandboxed pins that the directory path carries the header too — it
// shares the GET arm, and a future split must not drop it.
func TestZipIsSandboxed(t *testing.T) {
	h, root := newTestHandler(t)
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "a.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/download/dir", nil))

	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Fatalf("Content-Security-Policy = %q, want sandbox", csp)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type = %q, want application/zip", ct)
	}
}

// TestHandlerMethods covers the response for each verb, including that a
// traversal attempt answers a generic 404 rather than naming the path.
func TestHandlerMethods(t *testing.T) {
	cases := []struct {
		name, method, path string
		want               int
	}{
		{"get file", http.MethodGet, "/download/ok.txt", http.StatusOK},
		{"head file", http.MethodHead, "/download/ok.txt", http.StatusOK},
		{"missing file", http.MethodGet, "/download/nope.txt", http.StatusNotFound},
		{"traversal", http.MethodGet, "/download/../../etc/passwd", http.StatusNotFound},
		{"delete file", http.MethodDelete, "/download/ok.txt", http.StatusOK},
		{"unsupported verb", http.MethodPut, "/download/ok.txt", http.StatusMethodNotAllowed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, root := newTestHandler(t)
			if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("hello"), 0644); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
			if rec.Code != c.want {
				t.Fatalf("%s %s = %d, want %d", c.method, c.path, rec.Code, c.want)
			}
			if rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), root) {
				t.Fatalf("404 body leaked the resolved path: %q", rec.Body.String())
			}
		})
	}
}

// TestDeleteRemovesTree pins that DELETE is recursive — the reason the caller
// must gate it.
func TestDeleteRemovesTree(t *testing.T) {
	h, root := newTestHandler(t)
	if err := os.MkdirAll(filepath.Join(root, "dir", "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "sub", "a.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/download/dir", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(root, "dir")); !os.IsNotExist(err) {
		t.Fatalf("directory survived the delete: %v", err)
	}
}

// TestZipMatchesTheListing pins that the archive and the tree agree on what
// exists. serveZip walked everything while List filters hidden entries, so a
// download folder's zip carried dotfiles the UI said were not there.
func TestZipMatchesTheListing(t *testing.T) {
	h, root := newTestHandler(t)
	dir := filepath.Join(root, "dir")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"visible.txt", ".hidden", filepath.Join(".git", "config")} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/download/dir", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("zip does not parse: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if len(names) != 1 || !strings.HasSuffix(names[0], "visible.txt") {
		t.Fatalf("archive = %v, want only visible.txt — hidden entries must not be included", names)
	}
}

// TestZipOfHiddenDirectoryStillWorks is the other half: the visibility rule
// applies to entries, never to the directory the user asked for. Applying it to
// the walk root would return an empty archive, which is the same mistake the
// listing walk had.
func TestZipOfHiddenDirectoryStillWorks(t *testing.T) {
	h, root := newTestHandler(t)
	dir := filepath.Join(root, ".config")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/download/.config", nil))

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("zip does not parse: %v", err)
	}
	if len(zr.File) != 1 {
		t.Fatalf("archive has %d entries, want 1", len(zr.File))
	}
}

// TestZipStopsWhenTheClientLeaves pins that an abandoned download does not keep
// walking the tree with nowhere to write.
func TestZipStopsWhenTheClientLeaves(t *testing.T) {
	h, root := newTestHandler(t)
	dir := filepath.Join(root, "dir")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		p := filepath.Join(dir, "f"+string(rune('a'+i%26))+string(rune('a'+i/26))+".txt")
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is already gone
	req := httptest.NewRequest(http.MethodGet, "/download/dir", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		return // a truncated archive is the expected outcome
	}
	if len(zr.File) >= 50 {
		t.Fatalf("walk produced %d entries for a cancelled request; it should have stopped", len(zr.File))
	}
}
