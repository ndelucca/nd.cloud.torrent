package ctstatic

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed files/*
var staticFiles embed.FS

// FileSystemHandler serves the embedded assets with files/ as the web root, so
// files/css/ct.css is served at /css/ct.css.
//
// The panic is a build-time assertion, not error handling: staticFiles is an
// embed.FS with a literal subdirectory, so fs.Sub can only fail if "files" is
// missing from the binary — which means the embed directive above did not
// match, and there is nothing to serve.
func FileSystemHandler() http.Handler {
	sub, err := fs.Sub(staticFiles, "files")
	if err != nil {
		panic("static: embedded files/ is missing: " + err.Error())
	}
	return http.FileServer(http.FS(sub))
}
