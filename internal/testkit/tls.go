package testkit

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Certificate is a self-signed certificate for 127.0.0.1, 127.0.0.2 and
// localhost that WriteCertificate wrote, with the paths of its files. Linux
// routes all of 127.0.0.0/8 to loopback, so a test can bind a second IP
// there; macOS routes only 127.0.0.1 by default.
type Certificate struct {
	*x509.Certificate
	CertPath, KeyPath string
}

// WriteCertificate writes a new self-signed certificate, valid for an hour,
// and its key, as PEM, to name.crt and name.key in dir.
func WriteCertificate(t testing.TB, dir, name string) Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.2")},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	c := Certificate{Certificate: cert, CertPath: filepath.Join(dir, name+".crt"), KeyPath: filepath.Join(dir, name+".key")}
	if err := os.WriteFile(c.CertPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.KeyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return c
}

// TLSClient trusts only certs, offers HTTP/2, and follows no redirect, so a
// test sees each 308 itself.
func TLSClient(certs ...Certificate) *http.Client {
	pool := x509.NewCertPool()
	for _, c := range certs {
		pool.AddCert(c.Certificate)
	}
	return &http.Client{
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
