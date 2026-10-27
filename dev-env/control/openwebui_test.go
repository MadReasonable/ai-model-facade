package main

import "testing"

// Open WebUI's section needs the admin account control signs in as,
// connects only to backends it can reach, by name, and gives each
// workspace model one ID and a base.
func TestOpenWebUi_ChecksItsAdminAndConnections(t *testing.T) {
	cases := []struct{ name, section, want string }{
		{"valid", "{admin: {email: a@example.com, password: p}, connections: [ollama]}", ""},
		{"no admin", "{connections: [ollama]}", "needs admin"},
		{"bad connection", "{admin: {email: a@example.com, password: p}, connections: [nope]}", "'nope'"},
		{"model without a base", "{admin: {email: a@example.com, password: p}, models: [{id: m}]}", "needs a base"},
		{"model twice", "{admin: {email: a@example.com, password: p}, models: [{id: m, base: b}, {id: m, base: b}]}", "twice"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := writeConfig(t, "  openWebUi: "+c.section+"\n")
			wantError(t, err, c.want)
		})
	}
}
