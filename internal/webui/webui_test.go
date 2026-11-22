package webui

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"
)

func serve(t *testing.T, ui *UI, target string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	ui.ServeHTTP(w, req, "/amf/ui/")
	return w
}

// The page is served with a CSP that lets only the gateway's own files and
// the page's import map run, and every module the page loads is among the
// files built in, so the UI can't break on a file left out of static/.
func TestUI_ServesThePageAndEveryModuleItLoads(t *testing.T) {
	ui, err := New()
	if err != nil {
		t.Fatal(err)
	}
	page := serve(t, ui, "/amf/ui/")
	if page.Code != http.StatusOK || page.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("page: %d %s", page.Code, page.Header().Get("Content-Type"))
	}
	m := importMap.FindStringSubmatch(page.Body.String())
	sum := sha256.Sum256([]byte(m[1]))
	csp := page.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self' 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") || !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "unsafe") {
		t.Errorf("CSP: %s", csp)
	}
	if page.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers: %v", page.Header())
	}
	// Every file a page or module refers to by a relative path must be
	// there, and a module must be served as JavaScript, or the browser
	// refuses it.
	ref := regexp.MustCompile(`(?:from\s*|import\s*|href=|src=)["'](\./[^"']+)["']`)
	seen := map[string]bool{}
	queue := []string{"index.html"}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		w := serve(t, ui, "/amf/ui/"+name)
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d", name, w.Code)
			continue
		}
		if strings.HasSuffix(name, ".mjs") && w.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
			t.Errorf("%s: Content-Type %s", name, w.Header().Get("Content-Type"))
		}
		for _, m := range ref.FindAllStringSubmatch(w.Body.String(), -1) {
			queue = append(queue, path.Join(path.Dir(name), m[1]))
		}
	}
	var imports struct {
		Imports map[string]string `json:"imports"`
	}
	if err := json.Unmarshal([]byte(m[1]), &imports); err != nil {
		t.Fatalf("import map: %v", err)
	}
	for _, target := range imports.Imports {
		if w := serve(t, ui, "/amf/ui/"+path.Clean(target)); w.Code != http.StatusOK {
			t.Errorf("the import map's %s: %d", target, w.Code)
		}
	}
	for _, want := range []string{"app.mjs", "app.css", "vendor/hooks.mjs", "vendor/htm.mjs"} {
		if !seen[want] {
			t.Errorf("%s isn't reached from the page", want)
		}
	}
	if w := serve(t, ui, "/amf/ui/nothing.js"); w.Code != http.StatusNotFound {
		t.Errorf("a file that isn't there: %d", w.Code)
	}
}

// A file is checked again on every load, by its ETag, so a gateway upgraded
// in place serves its new UI at once, and one unchanged costs a 304.
func TestUI_RevalidatesByETag(t *testing.T) {
	ui, err := New()
	if err != nil {
		t.Fatal(err)
	}
	first := serve(t, ui, "/amf/ui/app.mjs")
	tag := first.Header().Get("ETag")
	if tag == "" || first.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("headers: %v", first.Header())
	}
	if w := serve(t, ui, "/amf/ui/app.mjs", "If-None-Match", tag); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Errorf("unchanged: %d with %d bytes", w.Code, w.Body.Len())
	}
}

// The manifest a browser installs the page by is served as one, parses,
// starts and scopes the app where the page is, under any base path, and
// names only icons the UI serves; the CSP lets the page load it.
func TestUI_ServesItsManifest(t *testing.T) {
	ui, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if csp := serve(t, ui, "/amf/ui/").Header().Get("Content-Security-Policy"); !strings.Contains(csp, "manifest-src 'self'") {
		t.Errorf("CSP: %s", csp)
	}
	w := serve(t, ui, "/amf/ui/manifest.webmanifest")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/manifest+json" {
		t.Fatalf("manifest: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	var manifest struct {
		ShortName string `json:"short_name"`
		StartURL  string `json:"start_url"`
		Scope     string `json:"scope"`
		Display   string `json:"display"`
		Icons     []struct {
			Src     string `json:"src"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ShortName != "Model Facade" || manifest.StartURL != "./" || manifest.Scope != "./" || manifest.Display != "standalone" {
		t.Errorf("manifest: %+v", manifest)
	}
	purposes := map[string]bool{}
	for _, icon := range manifest.Icons {
		purposes[icon.Purpose] = true
		if got := serve(t, ui, "/amf/ui/"+icon.Src); got.Code != http.StatusOK || !strings.HasPrefix(got.Header().Get("Content-Type"), icon.Type) {
			t.Errorf("icon %s: %d %s", icon.Src, got.Code, got.Header().Get("Content-Type"))
		}
	}
	if !purposes["any"] || !purposes["maskable"] {
		t.Errorf("icon purposes: %v", purposes)
	}
}
