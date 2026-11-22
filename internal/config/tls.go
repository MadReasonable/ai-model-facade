package config

import (
	"crypto/tls"
	"fmt"
	"path/filepath"
)

// The values of a tls block's serve.
const (
	ServeHTTP  = "http"
	ServeHTTPS = "https"
	ServeBoth  = "both"
)

// TLS is one side's HTTPS: server.tls for the client APIs, and for the admin
// side unless adminApi.listen moves it, and server.adminApi.tls for the
// admin side. Neither falls back to the other; a certificate both use is an
// anchor under vars.
type TLS struct {
	// Listen is where HTTPS is served; left out, this side's own listen,
	// shared with plain HTTP and told apart by each connection's first byte.
	Listen string `yaml:"listen" doc:"The address and port HTTPS is served on. Leave blank to use the same IP and port as plain HTTP. A change takes effect on restart."`
	Cert   string `yaml:"cert" doc:"The certificate file, in PEM, with any intermediate certificates after it, such as certbot's fullchain.pem. The path is relative from the config file's folder. Set cert and key together, or leave both blank to serve plain HTTP only."`
	Key    string `yaml:"key" doc:"The certificate's private key file, in PEM, such as certbot's privkey.pem. The path is relative from the config file's folder. Reload the config after renewing the certificate, to use it without a restart."`
	Serve  string `yaml:"serve" doc:"Choose https to serve HTTPS only, redirecting plain HTTP requests to it, http to serve plain HTTP only, or both. https needs cert and key." schema:"enum=http|https|both,default=both"`
	// certificate is the key pair LoadTLS read; nil until then, and for a
	// block without cert.
	certificate *tls.Certificate
}

// On reports whether t serves HTTPS: it has a certificate, and serve isn't
// http.
func (t TLS) On() bool {
	return t.Cert != "" && t.Serve != ServeHTTP
}

// Only reports whether t serves HTTPS alone, redirecting plain requests.
func (t TLS) Only() bool {
	return t.On() && t.Serve == ServeHTTPS
}

// Certificate is the key pair LoadTLS read, nil before it.
func (t TLS) Certificate() *tls.Certificate {
	return t.certificate
}

// checkServe fills in serve's default and checks its value; name is t's
// config path.
func (t *TLS) checkServe(name string) error {
	switch t.Serve {
	case "":
		t.Serve = ServeBoth
	case ServeHTTP, ServeHTTPS, ServeBoth:
	default:
		return fmt.Errorf("%s.serve: '%s' is not http, https or both", name, t.Serve)
	}
	return nil
}

// checkPair checks that cert and key come together, and that https has
// them. It is left to Parse, not the draft check, since the admin UI sends
// each field as an edit of its own, so a draft holds cert without key
// between the two.
func (t *TLS) checkPair(name string) error {
	switch {
	case t.Cert != "" && t.Key == "":
		return fmt.Errorf("%s: cert is set but key isn't; give both, or neither to serve plain HTTP only", name)
	case t.Key != "" && t.Cert == "":
		return fmt.Errorf("%s: key is set but cert isn't; give both, or neither to serve plain HTTP only", name)
	case t.Serve == ServeHTTPS && t.Cert == "":
		return fmt.Errorf("%s.serve: https needs cert and key", name)
	}
	return nil
}

// load reads t's key pair, resolving relative paths from dir.
func (t *TLS) load(name, dir string) error {
	t.certificate = nil
	if t.Cert == "" {
		return nil
	}
	cert, key := t.Cert, t.Key
	if !filepath.IsAbs(cert) {
		cert = filepath.Join(dir, cert)
	}
	if !filepath.IsAbs(key) {
		key = filepath.Join(dir, key)
	}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return fmt.Errorf("%s: cert '%s' and key '%s': %w", name, cert, key, err)
	}
	t.certificate = &pair
	return nil
}

// LoadTLS reads the key pairs of server.tls and server.adminApi.tls,
// resolving relative paths from dir, the config file's folder. Parse checks
// the blocks without reading files, so a draft is checked without them;
// what serves a config loads them first.
func (c *Config) LoadTLS(dir string) error {
	if err := c.Server.TLS.load("server.tls", dir); err != nil {
		return err
	}
	return c.Server.AdminAPI.TLS.load("server.adminApi.tls", dir)
}

// Addresses are where a config's sides listen: each side's plain address
// and, where it serves HTTPS, its HTTPS address, "" where it doesn't.
type Addresses struct {
	Main, MainTLS   string // server.listen, and server.tls.listen or server.listen
	Admin, AdminTLS string // server.adminApi.listen, and its tls.listen, or listen, or server.listen
}

// Addresses resolves where s's sides listen, each tls.listen falling back to
// its side's listen, and the admin side's listen to server.listen.
func (s Server) Addresses() Addresses {
	a := Addresses{Main: s.Listen, Admin: s.AdminAPI.Listen}
	if s.TLS.On() {
		a.MainTLS = firstSet(s.TLS.Listen, s.Listen)
	}
	if s.AdminAPI.TLS.On() {
		a.AdminTLS = firstSet(s.AdminAPI.TLS.Listen, s.AdminAPI.Listen, s.Listen)
	}
	return a
}

func firstSet(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// checkServe checks both tls blocks' serve, as part of the draft check.
func (s *Server) checkServe() error {
	if err := s.TLS.checkServe("server.tls"); err != nil {
		return err
	}
	return s.AdminAPI.TLS.checkServe("server.adminApi.tls")
}

// checkTLS checks both tls blocks' cert and key, and where they listen. An
// address holds one side's plain HTTP and one side's HTTPS at most: two
// plain sides would need one listener to serve two handlers, and two HTTPS
// sides one handshake to offer two certificates.
func (s *Server) checkTLS() error {
	if err := s.TLS.checkPair("server.tls"); err != nil {
		return err
	}
	if err := s.AdminAPI.TLS.checkPair("server.adminApi.tls"); err != nil {
		return err
	}
	a := s.Addresses()
	if a.MainTLS != "" && a.MainTLS == a.AdminTLS {
		return fmt.Errorf("server.tls and server.adminApi.tls both serve HTTPS on '%s'; one address can offer only one certificate, so give server.adminApi.tls a listen of its own", a.MainTLS)
	}
	return nil
}
