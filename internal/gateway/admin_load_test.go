package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/configfile"
)

func (a *adminLive) changedOnDisk(t *testing.T) bool {
	t.Helper()
	return gjson.Get(a.send(t, "GET", "/amf/config-status", "bobs-key", "").body, "files.0.changedOnDisk").Bool()
}

// config-load reads the files on disk into the draft. Once the draft that
// load gave is applied, edited or not, the files count as seen, so a save
// over them needs no ?force; files only looked at, or edited again since the
// load, still count as changed on disk.
func TestAdmin_LoadFromTheFiles(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	hostEdit := func(from, to string) {
		t.Helper()
		writeFile(t, a.main, strings.Replace(readFile(t, a.main), from, to, 1), 0o644)
	}

	hostEdit("description: Background tasks", "description: Edited on the host")
	if !a.changedOnDisk(t) {
		t.Fatal("a host edit doesn't show as changed on disk")
	}
	r := a.send(t, "POST", "/amf/config-load", "alices-key", "")
	a.expect(t, r, http.StatusOK, "load the files")
	if gjson.Get(r.body, "backup").Exists() {
		t.Errorf("load: %s", r.body)
	}
	if r := a.send(t, "GET", "/amf/config/models/task/description", "bobs-key", ""); r.body != "\"Edited on the host\"\n" {
		t.Errorf("draft after the load: %s", r.body)
	}
	if r := a.send(t, "GET", "/amf/config/models/task/description?live", "bobs-key", ""); r.body != "\"Background tasks\"\n" {
		t.Errorf("the load changed what is running: %s", r.body)
	}
	a.expect(t, a.send(t, "PUT", "/amf/config/models/task/params/max_tokens", "alices-key", `500`), http.StatusOK, "edit the loaded draft")
	a.expect(t, a.send(t, "POST", "/amf/config-apply", "alices-key", ""), http.StatusOK, "apply")
	if a.changedOnDisk(t) {
		t.Error("the applied load still counts as changed on disk")
	}
	a.expect(t, a.send(t, "POST", "/amf/config-save", "alices-key", ""), http.StatusOK, "save without ?force")
	if main := readFile(t, a.main); !strings.Contains(main, "description: Edited on the host") || !strings.Contains(main, "max_tokens: 500") {
		t.Errorf("config file:\n%s", main)
	}

	// Loaded only to look, then reverted: still unseen, so a save that
	// would write over it is refused.
	hostEdit("description: Edited on the host", "description: Edited again")
	a.expect(t, a.send(t, "POST", "/amf/config-load", "alices-key", ""), http.StatusOK, "load")
	a.expect(t, a.send(t, "POST", "/amf/config-revert", "alices-key", ""), http.StatusOK, "revert")
	a.expect(t, a.send(t, "PUT", "/amf/config/models/task/params/max_tokens", "alices-key", `600`), http.StatusOK, "edit")
	a.expect(t, a.send(t, "POST", "/amf/config-apply", "alices-key", ""), http.StatusOK, "apply")
	if !a.changedOnDisk(t) {
		t.Error("files only looked at count as seen")
	}
	a.expect(t, a.send(t, "POST", "/amf/config-save", "alices-key", ""), http.StatusConflict, "save over files only looked at")

	// Edited again after the load: the edit since is unseen.
	a.expect(t, a.send(t, "POST", "/amf/config-load", "alices-key", ""), http.StatusOK, "load")
	hostEdit("description: Edited again", "description: Edited after the load")
	a.expect(t, a.send(t, "POST", "/amf/config-apply", "alices-key", ""), http.StatusOK, "apply")
	if !a.changedOnDisk(t) {
		t.Error("an edit made after the load counts as seen")
	}
	a.expect(t, a.send(t, "POST", "/amf/config-save", "alices-key", ""), http.StatusConflict, "save over an edit made after the load")

	// A file that doesn't parse leaves the draft as it was.
	hostEdit("models:", "models: [")
	before := a.send(t, "GET", "/amf/config", "bobs-key", "").Header.Get("ETag")
	a.expect(t, a.send(t, "POST", "/amf/config-load", "alices-key", ""), http.StatusUnprocessableEntity, "load a broken file")
	if after := a.send(t, "GET", "/amf/config", "bobs-key", "").Header.Get("ETag"); after != before {
		t.Error("a failed load changed the draft")
	}
}

// A backup made before the gateway had a secrets file loads with its
// literal secrets moved to the gateway's secrets file, which notes say.
func TestAdmin_LoadABackupWithAnotherLayout(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	old := strings.Replace(strings.ReplaceAll(adminMain, "${SWAP_URL}", swap.URL), "  adminApi:\n", `  apiKeys:
    - {name: openWebUi, key: secret}
    - {name: alice, key: alices-key}
    - {name: bob, key: bobs-key}
  adminApi:
`, 1)
	old = strings.Replace(old, `baseUrl: "`+swap.URL+`"}`, `baseUrl: "`+swap.URL+`", apiKey: old-swap-key}`, 1)
	dir := filepath.Join(a.dir, "backups", "20261101-120000")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "config.yaml"), old, 0o600)

	r := a.send(t, "POST", "/amf/config-load?backup=20261101-120000", "alices-key", "")
	a.expect(t, r, http.StatusOK, "load the backup")
	if notes := gjson.Get(r.body, "notes").String(); !strings.Contains(notes, "backends.llamaSwap.apiKey moves to the secrets file") || !strings.Contains(notes, "the key of 'alice' moves") {
		t.Errorf("notes: %s", notes)
	}
	a.expect(t, a.send(t, "POST", "/amf/config-apply", "alices-key", ""), http.StatusOK, "apply")
	a.expect(t, a.send(t, "POST", "/amf/config-save", "alices-key", ""), http.StatusOK, "save")
	if main := readFile(t, a.main); strings.Contains(main, "old-swap-key") || strings.Contains(main, "alices-key") {
		t.Errorf("config file:\n%s", main)
	}
	if secrets := readFile(t, a.secrets); !strings.Contains(secrets, "apiKey: old-swap-key") || !strings.Contains(secrets, "key: alices-key") {
		t.Errorf("secrets file:\n%s", secrets)
	}
}

// A reload says when it discards a change applied but not saved.
func TestAdmin_ReloadSaysItDiscardedUnsaved(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	a.expect(t, a.send(t, "PUT", "/amf/config/models/task/description", "alices-key", `"applied, not saved"`), http.StatusOK, "edit")
	a.expect(t, a.send(t, "POST", "/amf/config-apply", "alices-key", ""), http.StatusOK, "apply")
	r := a.send(t, "POST", "/amf/reload", "alices-key", "")
	if !gjson.Get(r.body, "discardedUnsaved").Bool() || gjson.Get(r.body, "discardedDraft").Bool() {
		t.Errorf("reload: %s", r.body)
	}
	if r := a.send(t, "GET", "/amf/config/models/task/description?live", "bobs-key", ""); r.body != "\"Background tasks\"\n" {
		t.Errorf("running after the reload: %s", r.body)
	}
}

// A load that can't read the files is a 500, and a backup wrong by itself a
// 422; either way the draft stays as it was.
func TestAdmin_LoadFailures(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	dir := filepath.Join(a.dir, "backups", "20261101-120000")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "config.yaml"), strings.Replace(readFile(t, a.main), "  task:\n", "  task:\n    candidatez: []\n", 1), 0o600)
	before := a.send(t, "GET", "/amf/config", "bobs-key", "").Header.Get("ETag")
	if r := a.send(t, "POST", "/amf/config-load?backup=20261101-120000", "alices-key", ""); r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(r.body, "candidatez") {
		t.Errorf("a wrong backup: got %d %s", r.StatusCode, r.body)
	}
	if err := os.Remove(a.secrets); err != nil {
		t.Fatal(err)
	}
	if r := a.send(t, "POST", "/amf/config-load", "alices-key", ""); r.StatusCode != http.StatusInternalServerError || !strings.Contains(r.body, "secrets.yaml") {
		t.Errorf("an unreadable file: got %d %s", r.StatusCode, r.body)
	}
	if after := a.send(t, "GET", "/amf/config", "bobs-key", "").Header.Get("ETag"); after != before {
		t.Error("a failed load changed the draft")
	}
}

// A backup with a secrets file, loaded on a gateway started without one, has
// that file merged into the config, which notes say, and a save writes it
// there.
func TestAdmin_LoadABackupIntoAGatewayWithoutSecrets(t *testing.T) {
	swap := newSwap(t)
	dir := t.TempDir()
	a := &adminLive{dir: dir, main: filepath.Join(dir, "config.yaml")}
	keys := "  apiKeys:\n    - {name: openWebUi, key: secret}\n    - {name: alice, key: alices-key}\n    - {name: bob, key: bobs-key}\n"
	writeFile(t, a.main, strings.Replace(strings.ReplaceAll(adminMain, "${SWAP_URL}", swap.URL), "  adminApi:\n", keys+"  adminApi:\n", 1), 0o644)
	files := configfile.Files{Config: a.main}
	cfg, err := files.Load()
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(context.Background(), cfg, discard)
	if err != nil {
		t.Fatal(err)
	}
	a.live = NewLive(g, discard, new(slog.LevelVar), files)
	srv := httptest.NewServer(a.live)
	t.Cleanup(srv.Close)
	a.gateway = &gateway{Server: srv, g: g}

	backup := filepath.Join(dir, "backups", "20261101-120000")
	if err := os.MkdirAll(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(backup, "config.yaml"), strings.ReplaceAll(adminMain, "${SWAP_URL}", swap.URL), 0o600)
	writeFile(t, filepath.Join(backup, "secrets.yaml"), adminSecrets, 0o600)
	r := a.send(t, "POST", "/amf/config-load?backup=-1", "alices-key", "")
	a.expect(t, r, http.StatusOK, "load the backup")
	if notes := gjson.Get(r.body, "notes").String(); !strings.Contains(notes, "backends.llamaSwap.apiKey is written to the main config in plaintext") || !strings.Contains(notes, "the key of 'alice' is written to the main config") {
		t.Errorf("notes: %s", notes)
	}
	a.expect(t, a.send(t, "POST", "/amf/config-apply", "alices-key", ""), http.StatusOK, "apply")
	a.expect(t, a.send(t, "POST", "/amf/config-save", "alices-key", ""), http.StatusOK, "save")
	if main := readFile(t, a.main); !strings.Contains(main, "apiKey: swap-key") || !strings.Contains(main, "key: alices-key") {
		t.Errorf("config file:\n%s", main)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets.yaml")); err == nil {
		t.Error("a save wrote a secrets file the gateway doesn't have")
	}
}

// config-status reports where the draft and the running config came from
// and whether each was edited since: a path edit marks it edited, a PUT of
// the config itself makes it the API's, a load makes it the files' or a
// backup's, revert and apply carry them across, and a save or a reload makes
// the running config the files' again, since it then holds what they do.
func TestAdmin_StatusTracksWhereEachCopyCameFrom(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	state := func(want string) {
		t.Helper()
		s := a.send(t, "GET", "/amf/config-status", "bobs-key", "").body
		got := gjson.Get(s, "draft.source").String() + " " + gjson.Get(s, "draft.edited").String() + " / " + gjson.Get(s, "live.source").String() + " " + gjson.Get(s, "live.edited").String()
		if got != want {
			t.Errorf("draft / live: %s, want %s", got, want)
		}
	}
	state("files false / files false")
	a.expect(t, a.send(t, "PUT", "/amf/config/models/task/description", "alices-key", `"one"`), http.StatusOK, "edit")
	state("files true / files false")
	whole := a.send(t, "GET", "/amf/config", "bobs-key", "", "Accept", "application/yaml").body
	a.expect(t, a.send(t, "PUT", "/amf/config", "alices-key", whole, "Content-Type", "application/yaml"), http.StatusOK, "a PUT of the config")
	state("api false / files false")
	a.expect(t, a.send(t, "PUT", "/amf/config/models/task/description", "alices-key", `"two"`), http.StatusOK, "edit")
	a.expect(t, a.send(t, "POST", "/amf/config-apply", "alices-key", ""), http.StatusOK, "apply")
	state("api true / api true")
	a.expect(t, a.send(t, "POST", "/amf/config-save", "alices-key", ""), http.StatusOK, "save")
	state("api true / files false") // the save didn't touch the draft
	if s := a.send(t, "GET", "/amf/config-status", "bobs-key", "").body; !gjson.Get(s, "live.saved").Bool() || !gjson.Get(s, "draft.saved").Bool() {
		t.Errorf("after a save: %s", s)
	}
	r := a.send(t, "POST", "/amf/config-load?backup=-1", "alices-key", "")
	state("backup:" + gjson.Get(r.body, "backup").String() + " false / files false")
	a.expect(t, a.send(t, "PUT", "/amf/config/models/task/description", "alices-key", `"three"`), http.StatusOK, "edit")
	a.expect(t, a.send(t, "POST", "/amf/config-apply", "alices-key", ""), http.StatusOK, "apply")
	state("backup:" + gjson.Get(r.body, "backup").String() + " true / backup:" + gjson.Get(r.body, "backup").String() + " true")
	a.expect(t, a.send(t, "POST", "/amf/config-load", "alices-key", ""), http.StatusOK, "load the files")
	state("files false / backup:" + gjson.Get(r.body, "backup").String() + " true")
	a.expect(t, a.send(t, "POST", "/amf/config-revert", "alices-key", ""), http.StatusOK, "revert")
	state("backup:" + gjson.Get(r.body, "backup").String() + " true / backup:" + gjson.Get(r.body, "backup").String() + " true")
	a.expect(t, a.send(t, "POST", "/amf/config-load", "alices-key", ""), http.StatusOK, "load the files")
	if h := a.send(t, "GET", "/amf/config", "bobs-key", "").Header.Get("X-Gateway-Config"); h != "draft; source=files; edited=false; live=false; saved=true" {
		t.Errorf("header: %s", h)
	}
	a.expect(t, a.send(t, "POST", "/amf/reload", "alices-key", ""), http.StatusOK, "reload")
	state("files false / files false")
}
