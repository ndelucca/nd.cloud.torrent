package server

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jpillora/backoff"
)

// defaultSearchConfig ships with the binary. See github.com/jpillora/scraper for
// the config specification; cloud-torrent uses "<id>/item" handlers.
//
// Note: several of these providers are long dead (rarbg and zooqle shut down)
// and the CSS selectors for the rest are unversioned and brittle. Treat search
// as best-effort.
//
//go:embed search-config.json
var defaultSearchConfig []byte

const (
	searchConfigInterval = 30 * time.Minute
	maxSearchConfig      = 1 << 20 // 1 MiB
)

// fetchSearchConfigLoop periodically refreshes the scraper config from
// Options.SearchConfigURL. It is a no-op unless the user opted in: the URL is a
// remote, unsigned document that dictates which hosts this server will contact,
// so it is not something to enable by default.
func (s *Server) fetchSearchConfigLoop(ctx context.Context) {
	if s.opts.SearchConfigURL == "" {
		return
	}
	log.Printf("Fetching search providers from %s", s.opts.SearchConfigURL)
	b := backoff.Backoff{Max: searchConfigInterval}
	for {
		delay := searchConfigInterval
		if err := s.fetchSearchConfig(ctx); err != nil {
			log.Printf("search config fetch failed: %s", err)
			delay = b.Duration()
		} else {
			b.Reset()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (s *Server) fetchSearchConfig(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.opts.SearchConfigURL, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	newConfig, err := io.ReadAll(io.LimitReader(resp.Body, maxSearchConfig))
	if err != nil {
		return err
	}
	newConfig, err = normalize(newConfig)
	if err != nil {
		return err
	}
	if bytes.Equal(s.searchConfig, newConfig) {
		return nil //unchanged
	}
	if err := s.scraper.LoadConfig(newConfig); err != nil {
		return err
	}
	s.searchConfig = newConfig
	s.state.Update(func(st *State) { st.SearchProviders = s.scraper.Config })
	log.Printf("Loaded new search providers")
	return nil
}

func normalize(input []byte) ([]byte, error) {
	output := bytes.Buffer{}
	if err := json.Indent(&output, input, "", "  "); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// safeSearchParams rejects query values that could break out of the host part of
// a provider URL.
//
// The scraper only URL-escapes a template param when its placeholder appears
// after a "?" in the template. Several shipped providers interpolate in path
// position (e.g. "https://1337x.to{{item}}"), so an unescaped "@" or "//" would
// redirect the outbound request to an attacker-chosen host.
func safeSearchParams(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, values := range r.URL.Query() {
			for _, v := range values {
				if strings.ContainsAny(v, "@\\") || strings.Contains(v, "//") {
					http.Error(w, "Invalid search parameter", http.StatusBadRequest)
					return
				}
			}
		}
		h.ServeHTTP(w, r)
	})
}
