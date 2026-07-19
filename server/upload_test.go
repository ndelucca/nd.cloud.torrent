package server

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// The multipart upload path had no test at all. It is reachable without
// authentication, it writes to the engine, and server/CLAUDE.md documents
// maxUploadBody as the thing standing between an anonymous POST and a full temp
// filesystem — ParseMultipartForm bounds only what is buffered in RAM, and the
// remainder spills to disk with no limit of its own.

// testTorrentFile builds a real .torrent for a small local payload. The engine
// package has its own copy; duplicating a fixture across two test binaries is
// cheaper than a shared helper package that would have to import
// anacrolix/torrent, which internal/ is charter-bound to stay clear of.
func testTorrentFile(t *testing.T, name string) []byte {
	t.Helper()
	payload := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(payload, bytes.Repeat([]byte("x"), 1<<16), 0644); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 1 << 14}
	if err := info.BuildFromFilePath(payload); err != nil {
		t.Fatal(err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := (&metainfo.MetaInfo{InfoBytes: infoBytes}).Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// multipartBody encodes files as a multipart form under the "torrent" field,
// which is the field addUploadedTorrents reads.
func multipartBody(t *testing.T, field string, files map[string][]byte) (string, *bytes.Buffer) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for name, data := range files {
		part, err := w.CreateFormFile(field, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return w.FormDataContentType(), &body
}

// postMultipart drives the real handler chain, so the same-origin middleware and
// the size cap are both in play.
func postMultipart(t *testing.T, s *Server, contentType string, body *bytes.Buffer) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/torrentfile", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	return rec
}

func TestMultipartUploadAddsTorrents(t *testing.T) {
	s := newTestServer(t)
	ct, body := multipartBody(t, "torrent", map[string][]byte{
		"a.torrent": testTorrentFile(t, "a.bin"),
		"b.torrent": testTorrentFile(t, "b.bin"),
	})

	rec := postMultipart(t, s, ct, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %q", rec.Code, rec.Body.String())
	}
	if got := len(s.engine.GetTorrents()); got != 2 {
		t.Fatalf("torrents = %d, want 2", got)
	}
}

// TestMultipartPartialSuccessIsReported pins the case server/CLAUDE.md describes
// and no test demonstrated: an upload where some files parse and some do not
// applies the good ones and still reports a 400 naming the bad ones. Swallowing
// the failures would leave a user who uploaded five files and got three with no
// way to find out which two were lost.
func TestMultipartPartialSuccessIsReported(t *testing.T) {
	s := newTestServer(t)
	ct, body := multipartBody(t, "torrent", map[string][]byte{
		"good.torrent": testTorrentFile(t, "good.bin"),
		"bad.torrent":  []byte("this is not bencode"),
	})

	rec := postMultipart(t, s, ct, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if msg := rec.Body.String(); !strings.Contains(msg, "bad.torrent") {
		t.Errorf("body = %q, want it to name the file that failed", msg)
	}
	// The good one still landed. Gating the apply on every file parsing would
	// make one malformed upload discard the rest.
	if got := len(s.engine.GetTorrents()); got != 1 {
		t.Fatalf("torrents = %d, want the good one to have been added", got)
	}
}

func TestMultipartWithNoFileIsRejected(t *testing.T) {
	s := newTestServer(t)
	ct, body := multipartBody(t, "notthefield", map[string][]byte{
		"a.torrent": testTorrentFile(t, "a.bin"),
	})

	rec := postMultipart(t, s, ct, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := len(s.engine.GetTorrents()); got != 0 {
		t.Fatalf("torrents = %d, want 0", got)
	}
}

// TestMultipartBodyIsCapped is the DoS bound. maxUploadBody wraps the body in a
// MaxBytesReader before ParseMultipartForm runs, because ParseMultipartForm caps
// only the in-RAM portion and streams the rest to temp files unbounded. Without
// the wrapper an anonymous POST of a multi-gigabyte body fills the temp
// filesystem; per-part limits cannot help, since by the time they apply the
// bytes are already on disk.
func TestMultipartBodyIsCapped(t *testing.T) {
	s := newTestServer(t)
	// One part comfortably over maxUploadBody (32 MiB). The content is
	// irrelevant — the read must fail before any parse is attempted.
	ct, body := multipartBody(t, "torrent", map[string][]byte{
		"huge.torrent": bytes.Repeat([]byte("x"), maxUploadBody+(1<<20)),
	})

	rec := postMultipart(t, s, ct, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an oversize body", rec.Code)
	}
	// Assert the *reason*, not just the status. Without the cap this body still
	// answers 400 — it spills to a temp file, then fails to parse as a torrent —
	// so a status-only assertion passes against the very bug this pins. The
	// MaxBytesReader message is the only thing that distinguishes "we refused to
	// read it" from "we read all 33 MiB and disliked it".
	if msg := rec.Body.String(); !strings.Contains(msg, "too large") {
		t.Fatalf("body = %q, want it to report the body was too large; "+
			"a parse failure here means the cap did not fire", msg)
	}
	if got := len(s.engine.GetTorrents()); got != 0 {
		t.Fatalf("torrents = %d, want 0", got)
	}
}

// TestMultipartUploadRejectsCrossOrigin pins that the upload path is behind the
// same-origin middleware like every other mutation. It is the largest anonymous
// write surface on the server, so "it is covered because everything is" is worth
// one explicit assertion.
func TestMultipartUploadRejectsCrossOrigin(t *testing.T) {
	s := newTestServer(t)
	ct, body := multipartBody(t, "torrent", map[string][]byte{
		"a.torrent": testTorrentFile(t, "a.bin"),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/torrentfile", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := len(s.engine.GetTorrents()); got != 0 {
		t.Fatalf("torrents = %d, want 0", got)
	}
}

// TestTorrentFileAcceptsRawBytes covers the non-multipart half of the same
// handler, which dispatches on Content-Type.
func TestTorrentFileAcceptsRawBytes(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/torrentfile",
		bytes.NewReader(testTorrentFile(t, "raw.bin")))
	req.Header.Set("Content-Type", "application/octet-stream")
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %q", rec.Code, rec.Body.String())
	}
	if got := len(s.engine.GetTorrents()); got != 1 {
		t.Fatalf("torrents = %d, want 1", got)
	}
}
