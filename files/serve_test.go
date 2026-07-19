package files

import (
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
