package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testFile is a file a test config can list, and testFiles that listing.
const testFile = "a.gguf"

var testFiles = "files:\n  - {path: " + testFile + ", sha256: " + strings.Repeat("0", 64) + ", url: http://x/a.gguf}\n"

// writeConfig writes a config.yaml of testFiles and these backend sections,
// and loads it.
func writeConfig(t *testing.T, backends string) (*config, error) {
	t.Helper()
	name := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(name, []byte(testFiles+"backends:\n"+backends), 0o644); err != nil {
		t.Fatal(err)
	}
	return loadConfig(name)
}

// wantError checks that err names want, or that there is none for an empty
// want.
func wantError(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
		t.Errorf("got %v, want an error naming %q", err, want)
	}
}

// The config is read strictly, so a misspelled key, a section for a backend
// the environment doesn't have, or a model on a file that isn't listed is an
// error naming it, rather than a setting that silently does nothing.
func TestLoadConfig_RejectsWhatCantWork(t *testing.T) {
	cases := []struct{ name, backends, want string }{
		{"valid", "  llamaSwap: {config: s.yaml}\n  ollama: {models: [{id: m, file: a.gguf}]}\n", ""},
		{"unknown backend", "  nope: {}\n", "'nope'"},
		{"unknown key", "  ollama: {keepalive: -1}\n", "keepalive"},
		{"unlisted file", "  ollama: {models: [{id: m, file: b.gguf}]}\n", "'b.gguf'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := writeConfig(t, c.backends)
			wantError(t, err, c.want)
		})
	}
}

// The override names a service only for a backend the config has a section
// for, so a backend left out of config.yaml is left as compose.yaml has it.
func TestConfig_OverrideHasOnlyConfiguredBackends(t *testing.T) {
	c, err := writeConfig(t, "  llamaSwap: {config: s.yaml}\n")
	if err != nil {
		t.Fatal(err)
	}
	override, err := c.override()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(override); !strings.Contains(got, "llama-swap:") || strings.Contains(got, "ollama:") {
		t.Errorf("override:\n%s", got)
	}
}
