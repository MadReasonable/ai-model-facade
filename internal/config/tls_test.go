package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

func noEnv(string) (string, bool) { return "", false }

// withServer is a config whose server block is server.
func withServer(server string) string {
	return "server:\n" + server + `
backends:
  llamaSwap: {type: llamaSwap, baseUrl: "http://x"}
models:
  task: {backend: llamaSwap}
`
}

// A tls block is checked by itself without its files: cert and key
// together, serve one of its three values, https only with a certificate,
// and two blocks never serving HTTPS on one address.
func TestTLS_Checks(t *testing.T) {
	pair := "cert: c.pem, key: k.pem"
	cases := []struct{ name, server, want string }{
		{"cert alone", "  listen: :8088\n  tls: {cert: c.pem}", "server.tls: cert is set but key isn't"},
		{"key alone", "  listen: :8088\n  tls: {key: k.pem}", "server.tls: key is set but cert isn't"},
		{"unknown serve", "  listen: :8088\n  tls: {serve: tls}", "server.tls.serve: 'tls' is not http, https or both"},
		{"https without a certificate", "  listen: :8088\n  tls: {serve: https}", "server.tls.serve: https needs cert and key"},
		{"admin cert alone", "  listen: :8088\n  adminApi: {tls: {cert: c.pem}}", "server.adminApi.tls: cert is set but key isn't"},
		{"both on server.listen", "  listen: :8088\n  tls: {" + pair + "}\n  adminApi: {tls: {" + pair + "}}", "server.tls and server.adminApi.tls both serve HTTPS on ':8088'"},
		{"both on one tls.listen", "  listen: :8088\n  tls: {" + pair + ", listen: ':8443'}\n  adminApi: {listen: ':8089', tls: {" + pair + ", listen: ':8443'}}", "both serve HTTPS on ':8443'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := config.Parse([]byte(withServer(c.server)), noEnv)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want %q", err, c.want)
			}
		})
	}
	ok := []struct{ name, server string }{
		{"no tls", "  listen: :8088"},
		{"one port", "  listen: :8088\n  tls: {" + pair + "}"},
		{"admin on a port of its own", "  listen: :8088\n  tls: {" + pair + "}\n  adminApi: {listen: ':8089', tls: {" + pair + "}}"},
		{"admin HTTPS on server.listen's plain port", "  listen: :8088\n  adminApi: {tls: {" + pair + "}}"},
		{"certificate turned off", "  listen: :8088\n  tls: {" + pair + ", serve: http}\n  adminApi: {tls: {" + pair + "}}"},
	}
	for _, c := range ok {
		t.Run(c.name, func(t *testing.T) {
			if _, err := config.Parse([]byte(withServer(c.server)), noEnv); err != nil {
				t.Error(err)
			}
		})
	}
}

// Each side's HTTPS address falls back to its own listen, and the admin
// side's to server.listen; a side that serves no HTTPS has none.
func TestTLS_Addresses(t *testing.T) {
	pair := "cert: c.pem, key: k.pem"
	cases := []struct {
		server string
		want   config.Addresses
	}{
		{"  listen: :8088", config.Addresses{Main: ":8088"}},
		{"  listen: :8088\n  tls: {" + pair + "}", config.Addresses{Main: ":8088", MainTLS: ":8088"}},
		{"  listen: :8088\n  tls: {" + pair + ", listen: ':8443'}", config.Addresses{Main: ":8088", MainTLS: ":8443"}},
		{"  listen: :8088\n  tls: {" + pair + ", serve: http}", config.Addresses{Main: ":8088"}},
		{"  listen: :8088\n  adminApi: {tls: {" + pair + "}}", config.Addresses{Main: ":8088", AdminTLS: ":8088"}},
		{"  listen: :8088\n  adminApi: {listen: ':8089', tls: {" + pair + "}}", config.Addresses{Main: ":8088", Admin: ":8089", AdminTLS: ":8089"}},
		{"  listen: :8088\n  adminApi: {listen: ':8089', tls: {" + pair + ", listen: ':8444'}}", config.Addresses{Main: ":8088", Admin: ":8089", AdminTLS: ":8444"}},
	}
	for _, c := range cases {
		cfg, err := config.Parse([]byte(withServer(c.server)), noEnv)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Server.Addresses(); got != c.want {
			t.Errorf("%s:\ngot  %+v\nwant %+v", c.server, got, c.want)
		}
	}
}

// Load reads the key pairs from the config file's folder for a relative
// path, and names both files when they can't make one.
func TestTLS_LoadReadsTheFilesFromTheConfigsFolder(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "tls"), 0o755)
	cert := testkit.WriteCertificate(t, filepath.Join(dir, "tls"), "gateway")
	other := testkit.WriteCertificate(t, filepath.Join(dir, "tls"), "other")
	path := filepath.Join(dir, "config.yaml")
	write := func(server string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(withServer(server)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("  listen: :8088\n  tls: {cert: tls/gateway.crt, key: tls/gateway.key}")
	t.Chdir(t.TempDir()) // a relative path must not depend on the working directory
	cfg, err := config.Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Server.TLS.Certificate()
	if got == nil || len(got.Certificate) == 0 || string(got.Certificate[0]) != string(cert.Raw) {
		t.Fatal("the certificate read isn't tls/gateway.crt")
	}

	write("  listen: :8088\n  tls: {cert: tls/gateway.crt, key: tls/missing.key}")
	if _, err := config.Load(path, ""); err == nil || !strings.Contains(err.Error(), "server.tls: cert '"+cert.CertPath+"' and key '"+filepath.Join(dir, "tls", "missing.key")+"'") {
		t.Errorf("a missing key: %v", err)
	}
	write("  listen: :8088\n  tls: {cert: tls/gateway.crt, key: " + other.KeyPath + "}")
	if _, err := config.Load(path, ""); err == nil || !strings.Contains(err.Error(), "server.tls: cert") {
		t.Errorf("a key of another certificate: %v", err)
	}
}

// health reaches the gateway over plain HTTP on server.listen, unless
// server.tls serves only HTTPS, on its own address or server.listen.
func TestTLS_HealthAddress(t *testing.T) {
	cases := []struct {
		server string
		addr   string
		https  bool
	}{
		{"  listen: :8088", ":8088", false},
		{"  listen: :8088\n  tls: {cert: c.pem, key: k.pem}", ":8088", false},
		{"  listen: :8088\n  tls: {cert: c.pem, key: k.pem, serve: https}", ":8088", true},
		{"  listen: :8088\n  tls: {cert: c.pem, key: k.pem, serve: https, listen: ':8443'}", ":8443", true},
		{"  listen: :8088\n  adminApi: {tls: {cert: c.pem, key: k.pem, serve: https}}", ":8088", false},
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, c := range cases {
		if err := os.WriteFile(path, []byte(withServer(c.server)), 0o644); err != nil {
			t.Fatal(err)
		}
		addr, https, err := config.HealthAddress(path)
		if err != nil || addr != c.addr || https != c.https {
			t.Errorf("%s: got %s, https %t, %v; want %s, https %t", c.server, addr, https, err, c.addr, c.https)
		}
	}
}

// The draft check takes a tls block with cert and no key yet, since the
// admin UI sends the two fields as edits of their own; Parse, which an apply
// runs, still refuses it. A wrong serve is refused by both.
func TestTLS_DraftTakesACertBeforeItsKey(t *testing.T) {
	check := func(server string) error {
		d, err := config.ReadDocument([]byte(withServer(server)), nil)
		if err != nil {
			t.Fatal(err)
		}
		return d.CheckSchema(noEnv, func(string, config.Entry) error { return nil })
	}
	for _, server := range []string{"  listen: :8088\n  tls: {cert: c.pem}", "  listen: :8088\n  tls: {serve: https}"} {
		if err := check(server); err != nil {
			t.Errorf("the draft check refused %q: %v", server, err)
		}
		if _, err := config.Parse([]byte(withServer(server)), noEnv); err == nil {
			t.Errorf("Parse took %q", server)
		}
	}
	if err := check("  listen: :8088\n  tls: {serve: tls}"); err == nil || !strings.Contains(err.Error(), "'tls' is not http, https or both") {
		t.Errorf("a wrong serve: %v", err)
	}
}
