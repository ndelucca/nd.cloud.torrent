package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// templateSources returns every shipped template's raw text.
func templateSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil || len(entries) == 0 {
		t.Fatalf("no templates found: %v", err)
	}
	out := map[string]string{}
	for _, name := range entries {
		b, err := fs.ReadFile(templateFS, name)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = string(b)
	}
	return out
}

var (
	// hx-get/post/delete and sse-connect, i.e. every server path a template asks for.
	urlAttrRe = regexp.MustCompile(`(?:hx-(?:get|post|delete)|sse-connect)="([^"]+)"`)
	// The action placeholder a Go template leaves in a path.
	actionRe  = regexp.MustCompile(`\{\{[^}]*\}\}`)
	sseSwapRe = regexp.MustCompile(`sse-swap="([^"]+)"`)
)

// TestTemplateURLsAreDeclared pins the half of the htmx contract that lives in
// this package: every path a template asks the server for is declared in
// KnownRoutes or is a route the server owns.
//
// Nothing tied these together before. A region name, a fragment URL, a DOM id
// and a swap target were string-matched across Go consts, {{define}} names and
// HTML attributes, with no single place listing them and no test asserting that
// a URL in a template is a URL anything answers. The server side of this is
// TestKnownRoutesResolve, which walks KnownRoutes against the real mux.
func TestTemplateURLsAreDeclared(t *testing.T) {
	// Routes owned by the server rather than this package. Listed explicitly so
	// adding one is a decision, not an accident.
	serverOwned := map[string]bool{
		"/api/add":                   true,
		"/api/torrentfile":           true,
		"/api/configure":             true,
		"/api/torrents/{hash}/start": true,
		"/api/torrents/{hash}/stop":  true,
		"/api/torrents/{hash}":       true,
		"/download/{path}":           true,
	}
	known := map[string]bool{}
	for _, r := range KnownRoutes {
		known[r] = true
	}

	for name, src := range templateSources(t) {
		for _, m := range urlAttrRe.FindAllStringSubmatch(src, -1) {
			raw := m[1]
			if !strings.HasPrefix(raw, "/") {
				continue // relative or javascript: — not a server path
			}
			// Normalise the template actions to the pattern wildcards the route
			// table uses, so the two are comparable.
			norm := actionRe.ReplaceAllString(raw, "{hash}")
			if strings.HasPrefix(norm, "/download/") {
				norm = "/download/{path}"
			}
			if !known[norm] && !serverOwned[norm] {
				t.Errorf("%s asks for %q (normalised %q), which is not a declared route",
					name, raw, norm)
			}
		}
	}
}

// TestSSESwapNamesAreEmitted pins the other half, in both directions: every
// region a template listens for is one the renderer emits, and every region the
// renderer emits is listened for somewhere. The reverse direction is the one
// that catches a rename applied on only one side.
func TestSSESwapNamesAreEmitted(t *testing.T) {
	emitted := map[string]bool{
		torrentListEvent:      true,
		statsEvent:            true,
		downloadsChangedEvent: true,
	}

	listened := map[string]bool{}
	for name, src := range templateSources(t) {
		for _, m := range sseSwapRe.FindAllStringSubmatch(src, -1) {
			region := m[1]
			// The per-torrent rows are namespaced by infohash.
			if strings.HasPrefix(region, torrentEventPrefix) &&
				strings.Contains(region, "{{") {
				listened[torrentEventPrefix] = true
				continue
			}
			listened[region] = true
			if !emitted[region] {
				t.Errorf("%s listens for sse-swap=%q, which the renderer never emits", name, region)
			}
		}
	}

	// hx-trigger carries the ping regions rather than sse-swap.
	for _, src := range templateSources(t) {
		for region := range emitted {
			if strings.Contains(src, "sse:"+region) {
				listened[region] = true
			}
		}
	}

	for region := range emitted {
		if !listened[region] {
			t.Errorf("the renderer emits %q but no template listens for it", region)
		}
	}
	if !listened[torrentEventPrefix] {
		t.Errorf("the renderer emits %q regions but no template listens for them", torrentEventPrefix)
	}
}
