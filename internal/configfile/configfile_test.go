package configfile

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/config"
)

const testConfig = `server:
  listen: 127.0.0.1:0
backends:
  local: {type: llamaSwap, baseUrl: "http://127.0.0.1:1"}
models:
  assistant:
    backend: local
    candidates: [{model: m1, loadIfNone: true}]
`

const testSecrets = `backends:
  local:
    apiKey: sk-old
`

// A save that changes both files and fails renaming the second puts the
// first back from the backup, so the two files never disagree, and leaves
// no new file behind.
func TestSave_PutsTheFilesBackIfARenameFails(t *testing.T) {
	dir := t.TempDir()
	f := Files{Config: filepath.Join(dir, "config.yaml"), Secrets: filepath.Join(dir, "secrets.yaml")}
	if err := os.WriteFile(f.Config, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Secrets, []byte(testSecrets), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	saved := cfg.Source
	running, _, _, err := saved.Put(config.Path{"models", "assistant", "description"}, []byte(`"Answers questions"`), config.JSON)
	if err != nil {
		t.Fatal(err)
	}
	if running, _, _, err = running.Put(config.Path{"backends", "local", "apiKey"}, []byte(`"sk-new"`), config.JSON); err != nil {
		t.Fatal(err)
	}

	renamed := 0
	rename = func(from, to string) error {
		if to == f.Secrets {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: errors.New("disk on fire")}
		}
		renamed++
		return os.Rename(from, to)
	}
	t.Cleanup(func() { rename = os.Rename })

	_, err = f.Save(running, saved, false, 10)
	var saveErr *SaveError
	if !errors.As(err, &saveErr) || saveErr.Conflict || !strings.Contains(err.Error(), "secrets.yaml") || !strings.Contains(err.Error(), "put back") {
		t.Fatalf("err = %v", err)
	}
	// The config was renamed into place, then put back from the backup.
	if renamed != 2 {
		t.Errorf("%d renames into place, want the config's and its restore", renamed)
	}
	for path, want := range map[string]string{f.Config: testConfig, f.Secrets: testSecrets} {
		if got, _ := os.ReadFile(path); string(got) != want {
			t.Errorf("%s:\n%s\nwant:\n%s", filepath.Base(path), got, want)
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("left behind: %s", e.Name())
		}
	}
	if backups, err := f.Backups(); err != nil || len(backups) != 1 || len(backups[0].Files) != 2 {
		t.Errorf("backups %v, %v; want one, of both files", backups, err)
	}
}

// A backup can be named by its index from the newest, -1 for the newest,
// counting a second save in one second after the first, and a folder that
// isn't a whole backup is no backup.
func TestReadBackup_ByIndexFromTheNewest(t *testing.T) {
	dir := t.TempDir()
	f := Files{Config: filepath.Join(dir, "config.yaml"), Secrets: filepath.Join(dir, "secrets.yaml")}
	for path, content := range map[string]string{
		"20261118-090000/config.yaml":    "oldest",
		"20261119-090000/config.yaml":    "first that second",
		"20261119-090000-2/config.yaml":  "second that second",
		"20261119-090000-2/secrets.yaml": "its secrets",
		".backup-123/config.yaml":        "unfinished",
	} {
		path = filepath.Join(dir, "backups", path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ ref, id, main, secrets string }{
		{"-1", "20261119-090000-2", "second that second", "its secrets"},
		{"-2", "20261119-090000", "first that second", ""},
		{"-3", "20261118-090000", "oldest", ""},
		{"20261118-090000", "20261118-090000", "oldest", ""},
	} {
		id, main, secrets, err := f.readBackup(tc.ref)
		if err != nil || id != tc.id || string(main) != tc.main || string(secrets) != tc.secrets {
			t.Errorf("%s: %s %q %q %v", tc.ref, id, main, secrets, err)
		}
	}
	for _, ref := range []string{"-4", "-0", "-", "../config.yaml", "20261117-090000", ".backup-123"} {
		if _, _, _, err := f.readBackup(ref); !errors.Is(err, ErrNoBackup) {
			t.Errorf("%s: err = %v", ref, err)
		}
	}
	if _, _, _, err := f.readBackup("-4"); err == nil || !strings.Contains(err.Error(), "there are 3") {
		t.Errorf("-4: err = %v", err)
	}
}

// Every backup holds both files, whichever the save changed, each named by
// its role, so renaming the files keeps their backups.
func TestSave_BackupHoldsBothFilesByRole(t *testing.T) {
	dir := t.TempDir()
	f := Files{Config: filepath.Join(dir, "config.yaml"), Secrets: filepath.Join(dir, "secrets.yaml")}
	if err := os.WriteFile(f.Config, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Secrets, []byte(testSecrets), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	running, _, _, err := cfg.Source.Put(config.Path{"models", "assistant", "description"}, []byte(`"x"`), config.JSON)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := f.Save(running, cfg.Source, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Written) != 1 || saved.Written[0] != RoleConfig {
		t.Errorf("written: %v; want only the config", saved.Written)
	}
	for name, want := range map[string]string{"config.yaml": testConfig, "secrets.yaml": testSecrets} {
		if got, err := os.ReadFile(filepath.Join(dir, "backups", saved.Backup, name)); err != nil || string(got) != want {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "backups", saved.Backup, "secrets.yaml")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the secrets file's copy: %v, %v", info.Mode(), err)
	}

	renamed := Files{Config: filepath.Join(dir, "gateway.yaml"), Secrets: filepath.Join(dir, "keys.yaml")}
	if _, main, secrets, err := renamed.readBackup("-1"); err != nil || string(main) != testConfig || string(secrets) != testSecrets {
		t.Errorf("after a rename: %q %q %v", main, secrets, err)
	}
}

// mountinfo escapes a space, a tab, a newline and a backslash in a path as
// octal.
func TestUnescapeMount(t *testing.T) {
	if got := unescapeMount(`/srv/my\040config\011dir\012x\134y`); got != "/srv/my config\tdir\nx\\y" {
		t.Errorf("got %q", got)
	}
}

// A file whose folder can be written isn't saveable if the backups folder
// can't be, since a save backs up both files before it replaces either; the
// status says so, and a save is refused before it writes anything.
func TestStatus_NotSaveableWithoutBackups(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a folder whatever its mode")
	}
	dir := t.TempDir()
	f := Files{Config: filepath.Join(dir, "config.yaml"), Secrets: filepath.Join(dir, "secrets.yaml")}
	if err := os.WriteFile(f.Config, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Secrets, []byte(testSecrets), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "backups"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(dir, "backups"), 0o700) })
	cfg, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	files, backups := f.Status(cfg.Source, 10)
	if backups.Writable {
		t.Fatal("the backups folder counts as writable")
	}
	for _, s := range files {
		if !s.FolderWritable || s.Saveable || !strings.Contains(s.Reason, "backs both files up first") {
			t.Errorf("%s: %+v", s.Role, s)
		}
	}
	running, _, _, err := cfg.Source.Put(config.Path{"models", "assistant", "description"}, []byte(`"x"`), config.JSON)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Save(running, cfg.Source, false, 10)
	var saveErr *SaveError
	if !errors.As(err, &saveErr) || !saveErr.Conflict || !strings.Contains(err.Error(), "backups folder") {
		t.Errorf("err = %v", err)
	}
	if got, _ := os.ReadFile(f.Config); string(got) != testConfig {
		t.Errorf("the config changed:\n%s", got)
	}
}

// A save keeps the checksum of a file it didn't write, so a file laid out
// by hand, which re-encoding would lay out differently, doesn't then count
// as changed on disk, and a later save that writes it isn't refused.
func TestSave_FileNotWrittenStaysUnchanged(t *testing.T) {
	dir := t.TempDir()
	f := Files{Config: filepath.Join(dir, "config.yaml"), Secrets: filepath.Join(dir, "secrets.yaml")}
	byHand := "backends:\n\n    local:\n        apiKey:   sk-old\n"
	if err := os.WriteFile(f.Config, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Secrets, []byte(byHand), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	running, _, _, err := cfg.Source.Put(config.Path{"models", "assistant", "description"}, []byte(`"x"`), config.JSON)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := f.Save(running, cfg.Source, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Written) != 1 || saved.Written[0] != RoleConfig {
		t.Fatalf("written: %v", saved.Written)
	}
	for _, s := range fileStatuses(f.Status(saved.Document, 10)) {
		if s.ChangedOnDisk {
			t.Errorf("%s counts as changed on disk after the save", s.Role)
		}
	}
	running, _, _, err = saved.Document.Put(config.Path{"backends", "local", "apiKey"}, []byte(`"sk-new"`), config.JSON)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Save(running, saved.Document, false, 10); err != nil {
		t.Errorf("saving the secrets file next: %v", err)
	}
}

func fileStatuses(files []FileStatus, _ BackupsStatus) []FileStatus { return files }

// Reading the status writes nothing: the config's folder keeps its entries
// and its modification time, so a monitor polling the status doesn't churn
// a folder something else may watch. (Windows has no access(2), and there
// the status still creates and removes a file to find out.)
func TestStatus_WritesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows checks a folder by creating a file in it")
	}
	dir := t.TempDir()
	f := Files{Config: filepath.Join(dir, "config.yaml"), Secrets: filepath.Join(dir, "secrets.yaml")}
	if err := os.WriteFile(f.Config, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Secrets, []byte(testSecrets), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	// An hour back, since the clock that stamps the folder advances in
	// steps of a few milliseconds, and a change within the step since the
	// files were written wouldn't show.
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(dir, past, past); err != nil {
		t.Fatal(err)
	}
	files, backups := f.Status(cfg.Source, 10)
	if !files[0].Saveable || !files[0].FileWritable || !backups.Writable {
		t.Fatalf("status: %+v %+v", files, backups)
	}
	after, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if !after.ModTime().Equal(past) || len(entries) != 2 {
		t.Errorf("the status changed the folder: modified %v, was %v; %d entries", after.ModTime(), past, len(entries))
	}
}
