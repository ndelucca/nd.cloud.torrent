// Package reqlog logs one line per HTTP request. Stdlib only, and deliberately
// uncoloured.
package reqlog

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"
)

const timeFormat = "2006/01/02 15:04:05.000"

var logger = log.New(os.Stdout, "", 0)

// Wrap logs every request that passes through h, after it completes. A
// long-lived response (the SSE stream, a large download) therefore logs once, at
// disconnect, with its full duration and byte count.
func Wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w}
		h.ServeHTTP(rec, r)
		logger.Printf("%s %s %s %d %s%s%s",
			start.Format(timeFormat),
			r.Method,
			// EscapedPath, not Path: Path is already percent-decoded, so a
			// request for "/a%0A..." would write a newline into a line-oriented
			// log and let a caller forge entries.
			r.URL.EscapedPath(),
			rec.status(),
			fmtDuration(time.Since(start)),
			optional(" ", byteSize(rec.size)),
			optional(" (", remoteIP(r.RemoteAddr), ")"),
		)
	})
}

// recorder counts what was written without altering it.
type recorder struct {
	http.ResponseWriter
	code int
	size int64
}

func (rec *recorder) WriteHeader(code int) {
	if rec.code == 0 {
		rec.code = code
	}
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *recorder) Write(b []byte) (int, error) {
	n, err := rec.ResponseWriter.Write(b)
	rec.size += int64(n)
	return n, err
}

// status reports the code actually sent: a handler that only writes a body gets
// an implicit 200 from net/http and never calls WriteHeader.
func (rec *recorder) status() int {
	if rec.code == 0 {
		return http.StatusOK
	}
	return rec.code
}

// Unwrap exposes the underlying writer to http.ResponseController.
//
// This is load-bearing, not boilerplate. web.ServeEvents sets a per-write
// deadline through a ResponseController, which reaches the real writer by
// walking Unwrap. Without it SetWriteDeadline returns ErrNotSupported and the
// SSE stream runs with no write timeout at all — silently, because the caller
// has nothing useful to do with that error and discards it.
func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// Flush is separate from Unwrap because serveEvents type-asserts the writer to
// http.Flusher directly and returns 500 if that fails; an embedded
// ResponseWriter does not promote Flush, since it is not part of the interface.
func (rec *recorder) Flush() {
	//nolint:errcheck // a writer that cannot flush is one we cannot help.
	_ = http.NewResponseController(rec.ResponseWriter).Flush()
}

// fmtDuration trims a duration to three significant-ish digits: "1.234567s"
// reads as "1.23s", which is all a request log needs.
func fmtDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	default:
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
}

// byteSize renders a size, or "" for zero so the caller can omit the field.
//
// Base 1000, which must stay in step with web.humanBytes: a log that disagrees
// with the screen about the size of the same download is worse than either
// convention on its own.
func byteSize(n int64) string {
	if n <= 0 {
		return ""
	}
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// remoteIP returns the peer address, or "" for loopback so local requests do
// not carry a pointless "(127.0.0.1)" on every line.
func remoteIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsLoopback() {
		return ""
	}
	return host
}

// optional returns the parts joined only if the middle one is non-empty.
func optional(prefix, value string, suffix ...string) string {
	if value == "" {
		return ""
	}
	out := prefix + value
	for _, s := range suffix {
		out += s
	}
	return out
}
