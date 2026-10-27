package main

import "testing"

// The router's section must name its presets file in backend-config/, and
// the single-model server's must give it a model on a listed file.
func TestLlamaServer_ChecksBothModes(t *testing.T) {
	cases := []struct{ name, backends, want string }{
		{"valid", "  llamaRouter: {config: r.ini}\n  llamaServer: {model: {id: s, file: a.gguf}}\n", ""},
		{"router without config", "  llamaRouter: {modelsMax: 2}\n", "needs config"},
		{"single without a model", "  llamaServer: {ctxSize: 4096}\n", "has no id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := writeConfig(t, c.backends)
			wantError(t, err, c.want)
		})
	}
}
