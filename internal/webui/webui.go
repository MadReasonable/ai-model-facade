// Package webui is the admin UI: a page and its modules, built into the
// binary, that the gateway serves under /amf/ui/. The page is static and
// holds no secret, so anyone who reaches it may load it; everything it shows
// comes from the admin endpoints, with the key its user signs in with.
//
// The files are served as they are in static/, with no build step: the
// UI's modules import Preact and htm, vendored in static/vendor, and an
// import map in index.html resolves the one bare import between them.
package webui

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
)

//go:embed static
var embedded embed.FS

// file is one file the UI serves.
type file struct {
	body        []byte
	contentType string
	etag        string
}

// UI serves the admin UI's files.
type UI struct {
	files map[string]file // by path under static/, "index.html" for the page
	csp   string
}

// importMap is the page's one inline script, which the CSP allows by its
// hash, so no other inline script can run.
var importMap = regexp.MustCompile(`(?s)<script type="importmap">(.*?)</script>`)

// New reads the embedded files. It fails only if the files built in are
// wrong, which a test catches.
func New() (*UI, error) {
	ui := &UI{files: map[string]file{}}
	err := fs.WalkDir(embedded, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := embedded.ReadFile(p)
		if err != nil {
			return err
		}
		contentType := mime.TypeByExtension(path.Ext(p))
		switch path.Ext(p) {
		case ".mjs", ".js":
			contentType = "text/javascript; charset=utf-8"
		case ".md", "":
			contentType = "text/plain; charset=utf-8"
		}
		sum := sha256.Sum256(body)
		ui.files[strings.TrimPrefix(p, "static/")] = file{body, contentType, `"` + hex.EncodeToString(sum[:12]) + `"`}
		return nil
	})
	if err != nil {
		return nil, err
	}
	page, ok := ui.files["index.html"]
	if !ok {
		return nil, fmt.Errorf("webui: no index.html")
	}
	m := importMap.FindSubmatch(page.body)
	if m == nil {
		return nil, fmt.Errorf("webui: index.html has no import map")
	}
	sum := sha256.Sum256(m[1])
	// Everything comes from the gateway itself: scripts, styles and the
	// admin endpoints the page calls. The page may not be framed, so a site
	// can't overlay it to trick a signed-in admin into a click.
	ui.csp = strings.Join([]string{
		"default-src 'none'",
		"script-src 'self' 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'",
		"style-src 'self'",
		"img-src 'self' data:",
		"connect-src 'self'",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'none'",
	}, "; ")
	return ui, nil
}

// ServeHTTP serves the file at the request's path under prefix, the page
// for prefix itself. Every response is checked again on each load, by its
// ETag, so a gateway upgraded in place serves its new UI at once.
func (ui *UI) ServeHTTP(w http.ResponseWriter, r *http.Request, prefix string) {
	name := strings.TrimPrefix(r.URL.Path, prefix)
	if name == "" {
		name = "index.html"
	}
	f, ok := ui.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.contentType)
	h.Set("Content-Security-Policy", ui.csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", f.etag)
	if r.Header.Get("If-None-Match") == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(f.body)
}
