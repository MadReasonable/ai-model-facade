package main

import "testing"

// An alias must name one of the backend's models and not reuse a model's
// ID, or LocalAI refuses its config.
func TestLocalAI_RejectsAWrongAlias(t *testing.T) {
	cases := []struct{ name, aliases, want string }{
		{"valid", "[{id: m:alias, model: m}]", ""},
		{"unknown model", "[{id: m:alias, model: other}]", "'other'"},
		{"a model's ID", "[{id: m, model: m}]", "both a model and an alias"},
		{"no id", "[{model: m}]", "has no id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := writeConfig(t, "  localAi: {models: [{id: m, file: a.gguf}], aliases: "+c.aliases+"}\n")
			wantError(t, err, c.want)
		})
	}
}
