package reqlog

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// capture redirects the package logger for the duration of a test.
func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := logger
	logger = log.New(&buf, "", 0)
	t.Cleanup(func() { logger = prev })
	return &buf
}

// TestWriteDeadlineReachesRealWriter is the regression test for the bug this
// package was written to fix.
//
// serveEvents sets a per-write deadline on the SSE stream through an
// http.ResponseController, which finds the real writer by walking Unwrap. The
// previous implementation (jpillora/requestlog) did not implement Unwrap, so
// with --log enabled the deadline silently did not apply and a stalled client
// could block the streaming goroutine forever.
func TestWriteDeadlineReachesRealWriter(t *testing.T) {
	capture(t)

	// Buffered channel, not a variable: the handler runs on the server's
	// goroutine and Get can return before it finishes.
	deadlineErr := make(chan error, 1)
	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadlineErr <- http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Second))
	}))

	// httptest.NewRecorder does not support deadlines, so this needs a real
	// connection to be meaningful.
	ts := httptest.NewServer(h)
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case err := <-deadlineErr:
		if err != nil {
			t.Errorf("SetWriteDeadline through the log wrapper: %v "+
				"(the recorder must implement Unwrap)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never ran")
	}
}

// TestFlusherSurvivesWrapping guards the other half of the same seam:
// serveEvents type-asserts the writer to http.Flusher and returns 500 if that
// fails. An embedded ResponseWriter does not promote Flush.
func TestFlusherSurvivesWrapping(t *testing.T) {
	capture(t)

	// A channel rather than a plain bool: Flush sends the response headers, so
	// the client's Get returns while the handler is still running and reading a
	// variable it writes would be a data race.
	flushed := make(chan bool, 1)
	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			flushed <- false
			return
		}
		_, _ = w.Write([]byte("x"))
		f.Flush()
		flushed <- true
	}))

	ts := httptest.NewServer(h)
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case ok := <-flushed:
		if !ok {
			t.Error("wrapped writer is not an http.Flusher: SSE would 500")
		}
	case <-time.After(2 * time.Second):
		t.Error("handler did not reach Flush")
	}
}

func TestLogLineRecordsStatusAndSize(t *testing.T) {
	buf := capture(t)

	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/some/path", nil))

	line := buf.String()
	for _, want := range []string{"GET", "/some/path", "418", "5B"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line %q missing %q", strings.TrimSpace(line), want)
		}
	}
}

// TestImplicitStatusIsOK covers a handler that writes a body without ever
// calling WriteHeader — net/http sends 200, and logging 0 would be wrong.
func TestImplicitStatusIsOK(t *testing.T) {
	buf := capture(t)

	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !strings.Contains(buf.String(), " 200 ") {
		t.Errorf("implicit status not logged as 200: %q", strings.TrimSpace(buf.String()))
	}
}

// TestLoopbackIPOmitted keeps local development output clean, matching the
// previous behaviour.
func TestLoopbackIPOmitted(t *testing.T) {
	if got := remoteIP("127.0.0.1:5555"); got != "" {
		t.Errorf("loopback IP = %q, want empty", got)
	}
	if got := remoteIP("[::1]:5555"); got != "" {
		t.Errorf("IPv6 loopback = %q, want empty", got)
	}
	if got := remoteIP("192.168.1.5:5555"); got != "192.168.1.5" {
		t.Errorf("remote IP = %q, want 192.168.1.5", got)
	}
}

func TestByteSize(t *testing.T) {
	cases := map[int64]string{0: "", 512: "512B", 1024: "1.0KB", 1536: "1.5KB", 1048576: "1.0MB"}
	for n, want := range cases {
		if got := byteSize(n); got != want {
			t.Errorf("byteSize(%d) = %q, want %q", n, got, want)
		}
	}
}
