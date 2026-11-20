package config

import (
	"log/slog"
	"strings"
	"testing"
)

// server.adminApi.logs takes a level the gateway knows, logging.level's by
// default, and a size that isn't negative: 0 keeps no history, and left out
// it is 1 MiB.
func TestParse_Logs(t *testing.T) {
	parse := func(logs string) (*Config, error) {
		src := strings.Replace(minimal, "listen: 0.0.0.0:8090", "listen: x\n  adminApi: {logs: "+logs+"}", 1)
		return Parse([]byte(src+"logging: {level: warning}\n"), env(nil))
	}
	for logs, want := range map[string]string{
		"{level: loud}":   "server.adminApi.logs.level: unknown level 'loud' (debug | info | warning | error)",
		"{maxBytes: -1}":  "server.adminApi.logs.maxBytes: -1 is negative",
		"{maxBytes: big}": "cannot unmarshal",
	} {
		if _, err := parse(logs); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", logs, err, want)
		}
	}
	cfg, err := parse("{}")
	if err != nil {
		t.Fatal(err)
	}
	stderr, _ := cfg.Logging.SlogLevel()
	if l := cfg.Server.AdminAPI.Logs; l.KeepBytes() != 1<<20 || l.SlogLevel(stderr) != slog.LevelWarn {
		t.Errorf("defaults: %d bytes, level %v", l.KeepBytes(), l.SlogLevel(stderr))
	}
	if cfg, err = parse("{level: debug, maxBytes: 0}"); err != nil {
		t.Fatal(err)
	}
	if l := cfg.Server.AdminAPI.Logs; l.KeepBytes() != 0 || l.SlogLevel(stderr) != slog.LevelDebug {
		t.Errorf("set: %d bytes, level %v", l.KeepBytes(), l.SlogLevel(stderr))
	}
}
