package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/ndelucca/nd.cloud.torrent/internal/cli"
	"github.com/ndelucca/nd.cloud.torrent/server"
)

var version = "0.0.0-src" //set with ldflags

const repo = "https://github.com/ndelucca/nd.cloud.torrent"

func main() {
	o := server.DefaultOptions()

	// Flags are registered explicitly rather than derived from struct tags, so
	// the CLI surface is readable in one place. Defaults come from the struct,
	// which is why DefaultOptions is applied first.
	app := cli.New("nd-cloud-torrent", version, repo)
	app.String(&o.Title, "title", "t", "TITLE", "Title of this instance")
	app.Int(&o.Port, "port", "p", "PORT", "Listening port")
	app.String(&o.Host, "host", "", "", "Listening interface (default all)")
	app.String(&o.Auth, "auth", "a", "AUTH", "Optional basic auth in form 'user:password'")
	app.String(&o.ConfigPath, "config-path", "c", "", "Configuration file path")
	app.String(&o.KeyPath, "key-path", "k", "", "TLS Key file path")
	app.String(&o.CertPath, "cert-path", "", "", "TLS Certificate file path")
	app.Bool(&o.Log, "log", "l", "", "Enable request logging")
	app.Bool(&o.Open, "open", "o", "", "Open now with your default browser")

	if err := app.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, cli.ErrHelp) {
			return // help or version already printed
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

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
