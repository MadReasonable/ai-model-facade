package virtualmodel

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// A preset listed without its base model gets the same override note as an
// alias, exactly or through a pattern, and only when params are in play.
func TestCandidates_NotesThatAPresetMayOverrideParams(t *testing.T) {
	catalog := backend.NewCatalog([]backend.ModelInfo{
		{ID: "a", Aliases: []string{"a:Dreamer"}},
		{ID: "b:Slacker", Preset: true},
		{ID: "c"},
	})
	cases := []struct {
		candidate string
		hasParams bool
		want      string
	}{
		{"b:Slacker", true, "preset listed without its base model|" + aliasOverrideNote},
		{"b:Slacker", false, "preset listed without its base model"},
		{"*:Slacker", true, "base models: b:Slacker|aliases: none|presets listed without their base model: b:Slacker|" + aliasOverrideNote},
		{"a:Dreamer", true, "alias of a|" + aliasOverrideNote},
		{"c", true, ""},
	}
	for _, c := range cases {
		if got := strings.Join(describeAliases(c.candidate, "openWebUi", catalog, c.hasParams), "|"); got != c.want {
			t.Errorf("check, %s (params %v): got %q, want %q", c.candidate, c.hasParams, got, c.want)
		}
		var logged bytes.Buffer
		noteAliases(slog.New(slog.NewTextHandler(&logged, nil)), "m", c.candidate, catalog, c.hasParams)
		if noted := strings.Contains(logged.String(), "can resolve to a backend alias"); noted != (c.hasParams && c.want != "") {
			t.Errorf("startup log, %s (params %v): noted %v; log %q", c.candidate, c.hasParams, noted, logged.String())
		}
	}
}

// A plain alias, such as llama-server's, is only another name for its model,
// so check still names its base but neither check nor the startup log notes
// an override.
func TestCandidates_NoOverrideNoteForAPlainAlias(t *testing.T) {
	catalog := backend.NewCatalog([]backend.ModelInfo{{ID: "a", Aliases: []string{"a-alt"}, PlainAliases: true}})
	cases := []struct{ candidate, want string }{
		{"a-alt", "alias of a"},
		{"*-alt", "base models: none|aliases: a-alt"},
	}
	for _, c := range cases {
		if got := strings.Join(describeAliases(c.candidate, "llamaServer", catalog, true), "|"); got != c.want {
			t.Errorf("check, %s: got %q, want %q", c.candidate, got, c.want)
		}
		var logged bytes.Buffer
		noteAliases(slog.New(slog.NewTextHandler(&logged, nil)), "m", c.candidate, catalog, true)
		if logged.Len() > 0 {
			t.Errorf("startup log, %s: %q", c.candidate, logged.String())
		}
	}
}
