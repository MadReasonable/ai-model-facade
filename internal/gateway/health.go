package gateway

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"
)

// HealthURL returns the URL a local health check should call for listen,
// over HTTPS with https set. A wildcard host (empty, 0.0.0.0 or ::) is
// reached through the loopback address of the same family.
func HealthURL(listen string, https bool) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("server.listen '%s': %w", listen, err)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	scheme := "http://"
	if https {
		scheme = "https://"
	}
	return scheme + net.JoinHostPort(host, port) + "/health", nil
}

// healthClient is CheckHealth's client. It doesn't check an HTTPS
// certificate: a local check reaches the gateway by a loopback address,
// which a certificate for the gateway's name doesn't cover, and it sends no
// key or other secret.
var healthClient = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

// CheckHealth calls url and reports an error unless it answers 200.
func CheckHealth(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := healthClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return nil
}
