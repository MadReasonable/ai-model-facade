package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/configfile"
	"github.com/MadReasonable/ai-model-facade/internal/loghub"
)

// The main config of the admin tests holds no key itself: the keys are in
// the secrets file, with the llama-swap backend's apiKey.
var adminMain = strings.Replace(taskConfig, "  apiKeys:\n    - {name: openWebUi, key: secret}\n", `  adminApi:
    read: {allowedKeys: [bob]}
    write: {allowedKeys: [alice]}
`, 1)

const adminSecrets = `# keys
server:
  apiKeys:
    - {name: openWebUi, key: secret}
    - {name: alice, key: alices-key}
    - {name: bob, key: bobs-key}
backends:
  llamaSwap:
    apiKey: swap-key # llama-swap's own
`

type adminLive struct {
	*gateway
	live               *Live
	logs               *loghub.Hub
	dir, main, secrets string
}

// startAdmin writes the two files, with ${SWAP_URL} filled in, to a folder
// of their own, and serves a gateway started from them.
func startAdmin(t *testing.T, swapURL, main, secrets string) *adminLive {
	t.Helper()
	dir := t.TempDir()
	a := &adminLive{dir: dir, main: filepath.Join(dir, "config.yaml"), secrets: filepath.Join(dir, "secrets.yaml")}
	writeFile(t, a.main, strings.ReplaceAll(main, "${SWAP_URL}", swapURL), 0o644)
	writeFile(t, a.secrets, secrets, 0o600)
	files := configfile.Files{Config: a.main, Secrets: a.secrets}
	cfg, err := files.Load()
	if err != nil {
		t.Fatal(err)
	}
	a.logs = loghub.New()
	a.logs.Configure(cfg)
	log := slog.New(a.logs.Handler())
	g, err := Build(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	a.live = NewLive(g, log, new(slog.LevelVar), files)
	a.live.KeepLogs(a.logs)
	srv := httptest.NewServer(a.live)
	t.Cleanup(srv.Close)
	a.gateway = &gateway{Server: srv, g: g}
	return a
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// send makes a request with headers given as name, value pairs.
func (a *adminLive) send(t *testing.T, method, path, key, body string, headers ...string) response {
	t.Helper()
	req, _ := http.NewRequest(method, a.URL+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{resp, string(b)}
}

func (a *adminLive) expect(t *testing.T, r response, status int, what string) {
	t.Helper()
	if r.StatusCode != status {
		t.Fatalf("%s: got %d %s, want %d", what, r.StatusCode, r.body, status)
	}
}

// An edit goes to the draft, an apply puts it into service, and a save
// writes it to the file that holds each value, after a backup; the draft
// and the running config are read with GET, each with its ETag and the
// X-Gateway-Config header saying how it stands.
func TestAdmin_EditApplySave(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	secretsBefore := readFile(t, a.secrets)

	r := a.send(t, "GET", "/amf/config", "bobs-key", "")
	a.expect(t, r, http.StatusOK, "read")
	if gjson.Get(r.body, "backends.llamaSwap.apiKey").String() != "<redacted>" || !strings.HasPrefix(gjson.Get(r.body, "server.apiKeys.1.hash").String(), "sha256:") || strings.Contains(r.body, "alices-key") {
		t.Errorf("read shows a secret: %s", r.body)
	}
	if r.Header.Get("X-Gateway-Config") != "draft; source=files; edited=false; live=true; saved=true" || r.Header.Get("ETag") == "" {
		t.Errorf("headers: %v", r.Header)
	}
	if r := a.send(t, "GET", "/amf/config/backends/llamaSwap", "bobs-key", "", "Accept", "application/yaml"); r.StatusCode != http.StatusOK || !strings.Contains(r.body, "apiKey: <redacted> # llama-swap's own") {
		t.Errorf("YAML read: got %d %s", r.StatusCode, r.body)
	}
	a.expect(t, a.send(t, "GET", "/amf/config", "secret", ""), http.StatusForbidden, "read by a client")
	a.expect(t, a.send(t, "PUT", "/amf/config/models/task/description", "bobs-key", `"x"`), http.StatusForbidden, "edit by a reader")

	stale := a.send(t, "GET", "/amf/config/models/task", "alices-key", "").Header.Get("ETag")
	r = a.send(t, "PUT", "/amf/config/models/task/description", "alices-key", `"Renamed tasks"`)
	a.expect(t, r, http.StatusOK, "edit")
	if r.Header.Get("X-Gateway-Config") != "draft; source=files; edited=true; live=false; saved=false" {
		t.Errorf("after an edit: %s", r.Header.Get("X-Gateway-Config"))
	}
	if r := a.send(t, "GET", "/amf/config/models/task/description?live", "bobs-key", ""); r.body != "\"Background tasks\"\n" || r.Header.Get("X-Gateway-Config") != "live; source=files; edited=false; saved=true" {
		t.Errorf("live read: %s %s", r.body, r.Header.Get("X-Gateway-Config"))
	}
	a.expect(t, a.send(t, "PUT", "/amf/config/models/task/description", "alices-key", `"x"`, "If-Match", stale), http.StatusPreconditionFailed, "a stale If-Match")
	a.expect(t, a.send(t, "PUT", "/amf/config/models/task", "alices-key", `{}`, "If-None-Match", "*"), http.StatusPreconditionFailed, "If-None-Match on a node that exists")
	helper := "backend: llamaSwap\ncandidates:\n  - model: qwen2.5-coder-3b:q8_0\n    loadIfNone: true\n"
	a.expect(t, a.send(t, "PUT", "/amf/config/models/helper", "alices-key", helper, "Content-Type", "application/yaml", "If-None-Match", "*"), http.StatusCreated, "a new facade")
	if r := a.send(t, "PUT", "/amf/config/models/task/candidatez", "alices-key", `[]`); r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(r.body, "candidatez") {
		t.Errorf("unknown key: got %d %s", r.StatusCode, r.body)
	}
	a.expect(t, a.send(t, "PUT", "/amf/config/nothing/here", "alices-key", `1`), http.StatusNotFound, "a missing parent")

	a.expect(t, a.send(t, "POST", "/amf/config-apply", "alices-key", "", "If-Match", stale), http.StatusPreconditionFailed, "apply with a stale If-Match")
	draftTag := a.send(t, "GET", "/amf/config", "alices-key", "").Header.Get("ETag")
	r = a.send(t, "POST", "/amf/config-apply", "alices-key", "", "If-Match", draftTag)
	a.expect(t, r, http.StatusOK, "apply")
	if facadeIDs(t, a.gateway) != `"task","helper"` || gjson.Get(r.body, "models").Int() != 2 {
		t.Errorf("after apply: %s, facades %s", r.body, facadeIDs(t, a.gateway))
	}
	status := a.send(t, "GET", "/amf/config-status", "bobs-key", "").body
	if !gjson.Get(status, "draft.live").Bool() || gjson.Get(status, "live.saved").Bool() || !gjson.Get(status, "live.edited").Bool() || !gjson.Get(status, "files.0.saveable").Bool() || !gjson.Get(status, "backups.writable").Bool() {
		t.Errorf("status after apply: %s", status)
	}

	r = a.send(t, "POST", "/amf/config-save", "alices-key", "")
	a.expect(t, r, http.StatusOK, "save")
	backup := gjson.Get(r.body, "backup").String()
	if gjson.Get(r.body, "saved").Raw != `["config"]` || backup == "" {
		t.Errorf("save: %s", r.body)
	}
	if main := readFile(t, a.main); !strings.Contains(main, "description: Renamed tasks") || !strings.Contains(main, "helper:") {
		t.Errorf("config file:\n%s", main)
	}
	if readFile(t, a.secrets) != secretsBefore {
		t.Errorf("the secrets file changed:\n%s", readFile(t, a.secrets))
	}
	if old := readFile(t, filepath.Join(a.dir, "backups", backup, "config.yaml")); !strings.Contains(old, "description: Background tasks") {
		t.Errorf("backup:\n%s", old)
	}
	if info, err := os.Stat(filepath.Join(a.dir, "backups")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("backups folder: %v %v", info.Mode(), err)
	}
	if status := a.send(t, "GET", "/amf/config-status", "bobs-key", "").body; !gjson.Get(status, "live.saved").Bool() || gjson.Get(status, "files.0.changedOnDisk").Bool() {
		t.Errorf("status after save: %s", status)
	}
	if r := a.send(t, "GET", "/amf/config-backups", "bobs-key", ""); gjson.Get(r.body, "backups.0.id").String() != backup || gjson.Get(r.body, "backups.0.files").Raw != `["config","secrets"]` {
		t.Errorf("backups: %s", r.body)
	}

	a.expect(t, a.send(t, "POST", "/amf/config-load?backup="+backup, "alices-key", ""), http.StatusOK, "load the backup")
	if r := a.send(t, "GET", "/amf/config/models/task/description", "bobs-key", ""); r.body != "\"Background tasks\"\n" {
		t.Errorf("draft after loading the backup: %s", r.body)
	}
	if r := a.send(t, "POST", "/amf/config-load?backup=-1", "alices-key", ""); r.StatusCode != http.StatusOK || gjson.Get(r.body, "backup").String() != backup {
		t.Errorf("load the newest backup: got %d %s", r.StatusCode, r.body)
	}
	if r := a.send(t, "POST", "/amf/config-load?backup=-2", "alices-key", ""); r.StatusCode != http.StatusNotFound || !strings.Contains(r.body, "there are 1") {
		t.Errorf("load past the oldest backup: got %d %s", r.StatusCode, r.body)
	}
	a.expect(t, a.send(t, "POST", "/amf/config-load?backup=20000101-000000", "alices-key", ""), http.StatusNotFound, "load no backup")
	a.expect(t, a.send(t, "POST", "/amf/config-revert?backup=-1", "alices-key", ""), http.StatusBadRequest, "revert with a backup")
	a.expect(t, a.send(t, "POST", "/amf/config-revert", "alices-key", ""), http.StatusOK, "revert")
	if r := a.send(t, "GET", "/amf/config/models/task/description", "bobs-key", ""); r.body != "\"Renamed tasks\"\n" || !strings.Contains(r.Header.Get("X-Gateway-Config"), "; live=true;") {
		t.Errorf("draft after a revert: %s %s", r.body, r.Header.Get("X-Gateway-Config"))
	}

	// A file edited on the host since is not overwritten without ?force.
	a.expect(t, a.send(t, "PUT", "/amf/config/models/task/description", "alices-key", `"Third"`), http.StatusOK, "edit")
	a.expect(t, a.send(t, "POST", "/amf/config-apply", "alices-key", ""), http.StatusOK, "apply")
	writeFile(t, a.main, readFile(t, a.main)+"# edited on the host\n", 0o644)
	if r := a.send(t, "POST", "/amf/config-save", "alices-key", ""); r.StatusCode != http.StatusConflict || !strings.Contains(r.body, "changed on disk") {
		t.Errorf("save over a host edit: got %d %s", r.StatusCode, r.body)
	}
	a.expect(t, a.send(t, "POST", "/amf/config-save?force", "alices-key", ""), http.StatusOK, "forced save")

	// A reload takes the files, and says what it discarded.
	a.expect(t, a.send(t, "PUT", "/amf/config/models/task/description", "alices-key", `"Unapplied"`), http.StatusOK, "edit")
	if r := a.send(t, "POST", "/amf/reload", "alices-key", ""); !gjson.Get(r.body, "discardedDraft").Bool() || gjson.Get(r.body, "discardedUnsaved").Bool() {
		t.Errorf("reload: %s", r.body)
	}
	if r := a.send(t, "GET", "/amf/config/models/task/description", "bobs-key", ""); r.body != "\"Third\"\n" {
		t.Errorf("draft after a reload: %s", r.body)
	}
}

// A changed secret is saved to the secrets file, which keeps its mode.
func TestAdmin_SecretIsSavedToTheSecretsFile(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	mainBefore := readFile(t, a.main)
	a.expect(t, a.send(t, "PUT", "/amf/config/backends/llamaSwap/apiKey", "alices-key", `"new-swap-key"`), http.StatusOK, "edit")
	a.expect(t, a.send(t, "POST", "/amf/config-apply", "alices-key", ""), http.StatusOK, "apply")
	r := a.send(t, "POST", "/amf/config-save", "alices-key", "")
	a.expect(t, r, http.StatusOK, "save")
	if gjson.Get(r.body, "saved").Raw != `["secrets"]` || readFile(t, a.main) != mainBefore {
		t.Errorf("save: %s, config:\n%s", r.body, readFile(t, a.main))
	}
	if s := readFile(t, a.secrets); !strings.Contains(s, "apiKey: new-swap-key # llama-swap's own") || !strings.Contains(s, "# keys") {
		t.Errorf("secrets file:\n%s", s)
	}
	for _, path := range []string{a.secrets, filepath.Join(a.dir, "backups", gjson.Get(r.body, "backup").String(), "secrets.yaml")} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v, %v", path, info.Mode(), err)
		}
	}
}

// Apply refuses a draft that leaves no admin able to change the config.
func TestAdmin_ApplyRefusesALockout(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	a.expect(t, a.send(t, "PUT", "/amf/config/server/adminApi/write", "alices-key", `{"allowedKeys": []}`), http.StatusOK, "edit")
	if r := a.send(t, "POST", "/amf/config-apply", "alices-key", ""); r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(r.body, "leaves no key") {
		t.Errorf("apply: got %d %s", r.StatusCode, r.body)
	}
	a.expect(t, a.send(t, "PUT", "/amf/config/server/adminApi/write/allowedKeys", "alices-key", `["nobody"]`), http.StatusOK, "edit")
	if r := a.send(t, "POST", "/amf/config-apply", "alices-key", ""); r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(r.body, "'nobody' is not a name") {
		t.Errorf("apply of a wrong config: got %d %s", r.StatusCode, r.body)
	}
}

// A save to a folder the gateway can't write is refused before anything
// changes, and the status says why.
func TestAdmin_SaveRefusedWhereItCantWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a folder whatever its mode")
	}
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	a.expect(t, a.send(t, "PUT", "/amf/config/models/task/description", "alices-key", `"x"`), http.StatusOK, "edit")
	a.expect(t, a.send(t, "POST", "/amf/config-apply", "alices-key", ""), http.StatusOK, "apply")
	if err := os.Chmod(a.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(a.dir, 0o700) })
	status := a.send(t, "GET", "/amf/config-status", "bobs-key", "").body
	if gjson.Get(status, "files.0.saveable").Bool() || gjson.Get(status, "files.0.folderWritable").Bool() || !strings.Contains(gjson.Get(status, "files.0.reason").String(), "permission denied") || gjson.Get(status, "backups.writable").Bool() {
		t.Errorf("status: %s", status)
	}
	if r := a.send(t, "POST", "/amf/config-save", "alices-key", ""); r.StatusCode != http.StatusConflict || !strings.Contains(r.body, "can't save") {
		t.Errorf("save: got %d %s", r.StatusCode, r.body)
	}
}

// A save keeps server.adminApi.configBackups backups; with 0 it keeps none.
func TestAdmin_SaveKeepsConfigBackups(t *testing.T) {
	swap := newSwap(t)
	for _, keep := range []int{1, 0} {
		main := strings.Replace(adminMain, "  adminApi:\n", "  adminApi:\n    configBackups: "+string(rune('0'+keep))+"\n", 1)
		a := startAdmin(t, swap.URL, main, adminSecrets)
		for _, description := range []string{`"one"`, `"two"`} {
			a.expect(t, a.send(t, "PUT", "/amf/config/models/task/description", "alices-key", description), http.StatusOK, "edit")
			a.expect(t, a.send(t, "POST", "/amf/config-apply", "alices-key", ""), http.StatusOK, "apply")
			a.expect(t, a.send(t, "POST", "/amf/config-save", "alices-key", ""), http.StatusOK, "save")
		}
		entries, _ := os.ReadDir(filepath.Join(a.dir, "backups"))
		if len(entries) != keep {
			t.Errorf("keep %d: %d backups", keep, len(entries))
		}
	}
}

// The read endpoints are on for a writer too, and off with no admins.
func TestAdmin_ReadEndpointsFollowTheLists(t *testing.T) {
	swap := newSwap(t)
	writeOnly := strings.Replace(adminMain, "    read: {allowedKeys: [bob]}\n", "", 1)
	a := startAdmin(t, swap.URL, writeOnly, adminSecrets)
	a.expect(t, a.send(t, "GET", "/amf/config", "alices-key", ""), http.StatusOK, "read by a writer")
	a.expect(t, a.send(t, "GET", "/amf/config", "bobs-key", ""), http.StatusForbidden, "read by a key no list names")
	off := startAdmin(t, swap.URL, strings.Replace(writeOnly, "  adminApi:\n    write: {allowedKeys: [alice]}\n", "", 1), adminSecrets)
	if r := off.send(t, "GET", "/amf/config", "alices-key", ""); r.StatusCode != http.StatusNotFound || !strings.Contains(r.body, "server.adminApi.read") {
		t.Errorf("no admins: got %d %s", r.StatusCode, r.body)
	}
}
