package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/jpillora/cloud-torrent/server"
	"github.com/jpillora/opts"
)

var version = "0.0.0-src" //set with ldflags

func main() {
	o := server.DefaultOptions()

	p := opts.New(&o)
	p.Version(version)
	p.PkgRepo()
	p.SetLineWidth(96)
	p.Parse()

	// Ctrl-C and SIGTERM trigger a graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s, err := server.New(o, version)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	if err := s.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
