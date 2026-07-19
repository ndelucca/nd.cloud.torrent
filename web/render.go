package web

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

// parseTemplates loads every fragment. Templates are addressed exclusively by
// their {{define}} name, never by filename: template.ParseFS names templates by
// base name, so two files called row.html in different directories would
// silently collide, and asking for a name no file provides yields an *empty*
// template that renders nothing without erroring.
//
// The error is returned rather than panicked because the recursive fsnode
// template can fail html/template's contextual autoescaper with
// ErrOutputContext at parse time; a package-level Must would turn a template
// edit into a startup panic.
func parseTemplates() (*template.Template, error) {
	t, err := template.New("cloud-torrent").Funcs(templateFuncs()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return t, nil
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"bytes":   humanBytes,
		"round":   func(f float64) string { return fmt.Sprintf("%.0f", f) },
		"pct":     func(f float32) string { return fmt.Sprintf("%.2f", f) },
		"ago":     humanAgo,
		"urlpath": urlPath,
	}
}

// urlPath percent-encodes each segment of a slash-separated path for use in a
// URL.
//
// Every URL attribute needs this, including the ones html/template recognises.
// An htmx attribute like hx-delete is plain text to the escaper, so a file named
// "a#b.mkv" produced hx-delete="/download/a#b.mkv"; the browser dropped
// everything from the "#" and the server deleted a *different* file named "a",
// answering 200. "?" truncated the same way and "%" made the request fail
// outright. But href and src are no better: they get the URL *normalizer*, and
// urlProcessor in html/template/url.go passes '#', '?' and '&' through
// unescaped by design, because it normalizes a URL rather than escaping a path.
// File names come from torrents, so all of this is attacker-reachable.
//
// TestURLAttributesUseURLPath is what keeps this from being a convention.
//
// Segments are escaped individually so the separators survive. PathEscape
// leaves "/" alone, which is why splitting first is necessary.
func urlPath(p string) string {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return strings.Join(parts, "/")
}

// humanBytes renders a byte count with a metric prefix.
func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	e := int(math.Floor(math.Log(float64(n)) / math.Log(1000)))
	if e >= len(units) {
		e = len(units) - 1
	}
	v := float64(n) / math.Pow(1000, float64(e))
	if e == 0 {
		return fmt.Sprintf("%.0f %s", v, units[e])
	}
	return fmt.Sprintf("%.1f %s", v, units[e])
}

// percentOf guards the division by zero that used to produce +Inf and break
// JSON marshalling of the whole state document.
func percentOf(n, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}

// humanAgo renders a past instant as elapsed time ("3 hours ago").
func humanAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	if time.Since(t) < time.Minute {
		return "just now"
	}
	return humanSince(t) + " ago"
}

// humanSince renders the elapsed time itself, without the "ago". The stats
// footer wants a duration ("up 3 hours"), not a past tense — reusing humanAgo
// there produced "up 3 hours ago".
func humanSince(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "moments"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

// renderer owns the parsed templates and the per-region cache of the last bytes
// rendered for each SSE event.
//
// Change detection compares *rendered output*, not source data. Comparing
// source data would mean maintaining an Equal method per view model and
// remembering to update it whenever a template changes; the failure mode there
// is a silently stale UI. Comparing output is self-maintaining.
//
// The rendered bytes are retained rather than hashed because a client that
// connects mid-tick must immediately receive the current body of every region
// rather than wait for the next change. Given they are retained anyway,
// bytes.Equal is as cheap as hashing and has no collision question.
type renderer struct {
	tmpl *template.Template

	// One map, not two. Framing is a pure function of (event, body) and the
	// event is the key, so a separate body map carried no information the
	// framed one did not — it was two things to keep in step for nothing.
	mu         sync.Mutex
	framedBody map[string][]byte // event name -> last body, SSE-framed
}

func newRenderer(t *template.Template) *renderer {
	return &renderer{
		tmpl:       t,
		framedBody: map[string][]byte{},
	}
}

// execute runs a template and returns its body, trimmed and checked. It is the
// only place a template is executed, which is what makes checkFragment cover
// the pulled fragments and the page as well as the streamed regions.
func (r *renderer) execute(tmplName string, data any) ([]byte, error) {
	var buf bytes.Buffer
	if err := r.tmpl.ExecuteTemplate(&buf, tmplName, data); err != nil {
		return nil, fmt.Errorf("render %s: %w", tmplName, err)
	}
	// Trim before framing. A {{define}} block starts with a newline, which would
	// otherwise put a stray text node ahead of the element and cost a "data: "
	// line per blank line on every push.
	body := bytes.TrimSpace(buf.Bytes())
	if err := checkFragment(tmplName, body); err != nil {
		return nil, err
	}
	return body, nil
}

// render executes the named template and returns SSE-framed bytes, or nil if
// the output is byte-identical to the previous render of this event.
func (r *renderer) render(event, tmplName string, data any) ([]byte, error) {
	body, err := r.execute(tmplName, data)
	if err != nil {
		return nil, err
	}
	return r.store(event, body), nil
}

// errBareText rejects a fragment that is not wrapped in an element.
var errBareText = errors.New("fragment must be wrapped in an element")

// checkFragment enforces the one rule idiomorph does not enforce for us.
//
// Verified in Chromium 150 (htmx 2.0.10 + idiomorph 0.7.4): a bare-text payload
// swapped with hx-swap="morph:…" produces an EMPTY target. No console error, no
// htmx event, the data: line arrives intact — the DOM is just blank. A plain
// innerHTML swap of the same payload works, which is what makes it easy to miss.
func checkFragment(name string, body []byte) error {
	if t := bytes.TrimSpace(body); len(t) > 0 && t[0] != '<' {
		return fmt.Errorf("template %q: %w (starts with %q)", name, errBareText,
			string(t[:min(20, len(t))]))
	}
	return nil
}

// store caches body under event and returns the framed bytes, or nil if
// unchanged.
func (r *renderer) store(event string, body []byte) []byte {
	framed := frameSSE(event, body)
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.framedBody[event]; ok && bytes.Equal(prev, framed) {
		return nil
	}
	r.framedBody[event] = framed
	return framed
}

// framed returns the cached framing for a region. Only tests read it; the
// render path gets the frame back from store.
func (r *renderer) framed(event string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.framedBody[event]
}

// snapshot returns every region's current framed body as one buffer, for a
// newly connected subscriber.
//
// Order does not matter: every region name is fixed and its element exists in
// the page the browser already has. It mattered when region names were created
// dynamically — an element cannot listen for a name before it exists, so a frame
// arriving ahead of its element was silently discarded — and that constraint
// left with them.
//
// One buffer rather than a slice of frames: SSE frames are self-delimiting, so
// the caller makes a single Write and Flush instead of one pair per region.
func (r *renderer) snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.framedBody))
	for name := range r.framedBody {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []byte
	for _, name := range names {
		out = append(out, r.framedBody[name]...)
	}
	return out
}

// frameSSE wraps a body in SSE framing. Every line of the payload needs its own
// "data: " prefix, so rendered HTML — which is full of newlines — cannot be
// written as a single data line.
func frameSSE(event string, body []byte) []byte {
	var b bytes.Buffer
	b.WriteString("event: ")
	b.WriteString(event)
	b.WriteByte('\n')
	if len(body) == 0 {
		// An empty data line still delivers the event, which is what makes the
		// listener-cleanup trick in forget work.
		b.WriteString("data:\n")
	} else {
		body = bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
		for _, line := range bytes.Split(body, []byte("\n")) {
			b.WriteString("data: ")
			b.Write(line)
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')
	return b.Bytes()
}
