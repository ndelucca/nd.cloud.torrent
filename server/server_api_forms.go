package server

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ndelucca/nd.cloud.torrent/engine"
)

// maxUploadMemory is how much of a multipart upload is buffered in RAM before
// the rest spills to a temp file. A .torrent is small; anything much larger is
// not one.
const maxUploadMemory = 8 << 20

// maxUploadBody caps the whole multipart request. ParseMultipartForm bounds only
// what is buffered in RAM — the remainder spills to temp files with no limit at
// all, so without this an unauthenticated POST of a multi-gigabyte body fills
// the temp filesystem. Per-part limits do not help: by the time they apply, the
// body has already landed on disk.
const maxUploadBody = 32 << 20

// addURI dispatches a user-supplied string to the right engine call by looking
// at its scheme.
func (s *Server) addURI(r *http.Request, uri string) error {
	switch {
	case uri == "":
		return badRequest("Nothing to add")
	case strings.HasPrefix(uri, "magnet:"):
		return s.engine.NewMagnet(uri)
	case strings.HasPrefix(uri, "http://"), strings.HasPrefix(uri, "https://"):
		body, err := fetchRemoteTorrent(r.Context(), uri)
		if err != nil {
			return err
		}
		return s.engine.NewTorrentFile(body)
	default:
		return badRequest("Expected a magnet: link or an http(s) URL to a .torrent")
	}
}

// addUploadedTorrents handles a multipart upload, which is what lets the client
// report progress: htmx emits htmx:xhr:progress only for a real multipart
// request. The AngularJS UI read each file with FileReader and POSTed the raw
// bytes, so there was no progress to report at all.
func (s *Server) addUploadedTorrents(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
	if err := r.ParseMultipartForm(maxUploadMemory); err != nil {
		return badRequest("Malformed upload: %s", err)
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	files := r.MultipartForm.File["torrent"]
	if len(files) == 0 {
		return badRequest("No .torrent file in the upload")
	}

	var added int
	var failures []string
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", fh.Filename, err))
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, maxAPIBody))
		_ = f.Close()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", fh.Filename, err))
			continue
		}
		if err := s.engine.NewTorrentFile(data); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", fh.Filename, err))
			continue
		}
		added++
	}

	// Partial success is reported rather than swallowed: uploading five files
	// and silently getting three is worse than being told which two failed.
	if len(failures) > 0 {
		if added == 0 {
			return badRequest("%s", strings.Join(failures, "; "))
		}
		return badRequest("Added %d of %d; %s", added, len(files), strings.Join(failures, "; "))
	}
	return nil
}

// parseConfig builds a Config from a form-encoded body.
//
// current is the starting point so a form that omits a field leaves it alone
// rather than zeroing it.
func parseConfig(r *http.Request, data []byte, current engine.Config) (engine.Config, error) {
	v := formValues(r, data)
	if v == nil {
		return current, badRequest("Expected a form-encoded configuration")
	}

	c := current
	if s, ok := firstValue(v, "DownloadDirectory"); ok {
		if strings.TrimSpace(s) == "" {
			return c, badRequest("Download directory cannot be empty")
		}
		c.DownloadDirectory = s
	}
	if s, ok := firstValue(v, "IncomingPort"); ok {
		p, err := strconv.Atoi(s)
		if err != nil {
			return c, badRequest("Incoming port must be a number")
		}
		c.IncomingPort = p
	}
	// Checkboxes are paired with a hidden field of the same name, so an
	// unchecked box still submits "false". url.Values keeps both in order and
	// the checkbox, when present, comes last.
	for name, target := range map[string]*bool{
		"AutoStart":         &c.AutoStart,
		"EnableUpload":      &c.EnableUpload,
		"EnableSeeding":     &c.EnableSeeding,
		"DisableEncryption": &c.DisableEncryption,
	} {
		if vals, ok := v[name]; ok && len(vals) > 0 {
			*target = vals[len(vals)-1] == "true"
		}
	}
	return c, nil
}

func firstValue(v url.Values, key string) (string, bool) {
	vals, ok := v[key]
	if !ok || len(vals) == 0 {
		return "", false
	}
	return vals[0], true
}
