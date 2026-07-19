package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// Run serves until ctx is cancelled, then shuts down gracefully. It is
// one-shot: the hub latches closed, so a second call would serve no events.
//
// It returns nil for any completed shutdown, including one that overran its
// drain budget — Ctrl-C is a requested action, and a slow-draining download is
// not a failed run. A non-nil error means serving genuinely failed (the port
// could not be bound, TLS could not start), which is what main exits 1 on.
func (s *Server) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	// Registered before cancel so it runs after it — defers are LIFO, and
	// waiting before cancelling would deadlock. Joining the loops means no
	// engine call is in flight when main's deferred Close releases the engine.
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	// Covers every return path, including a failed Listen below. Idempotent.
	defer s.ui.Close()

	wg.Add(2)
	go func() { defer wg.Done(); s.pollLoop(ctx) }()
	go func() { defer wg.Done(); s.statsLoop(ctx) }()

	host := s.opts.Host
	if host == "" {
		host = "0.0.0.0"
	}
	addr := net.JoinHostPort(host, fmt.Sprint(s.opts.Port))
	proto := "http"
	if s.isTLS {
		proto += "s"
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: s.handler(),
		// Without these a single idle connection can hold a goroutine forever.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: /events is long-lived and downloads can be large.
	}

	// Bind before announcing, so the logged URL is only printed once it is real.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	log.Printf("Listening at %s://%s", proto, addr)

	if s.opts.Open {
		openhost := host
		if openhost == "0.0.0.0" {
			openhost = "localhost"
		}
		url := fmt.Sprintf("%s://%s:%d", proto, openhost, s.opts.Port)
		go func() {
			if err := openBrowser(url); err != nil {
				log.Printf("failed to open browser: %s", err)
			}
		}()
	}

	errc := make(chan error, 1)
	go func() {
		if s.isTLS {
			errc <- srv.ServeTLS(ln, s.opts.CertPath, s.opts.KeyPath)
		} else {
			errc <- srv.Serve(ln)
		}
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Printf("Shutting down…")

		// Release the SSE streams BEFORE Shutdown. Shutdown waits for
		// connections to become idle and does not cancel request contexts, so a
		// long-lived /events handler is never released by it — one connected
		// browser burns the entire drain budget.
		s.ui.Close()

		shutdownCtx, done := context.WithTimeout(context.Background(), shutdownTimeout)
		defer done()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// Only real transfers can reach this now — a large download or a zip
			// still streaming. Stop waiting on them.
			log.Printf("graceful shutdown exceeded %s, closing remaining connections: %s",
				shutdownTimeout, err)
			_ = srv.Close()
		}
		return nil
	}
}
