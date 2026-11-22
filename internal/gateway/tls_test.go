package gateway

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

// freeAddr is a loopback address whose port nothing listens on now, on
// 127.0.0.1, or on ip if one is given.
func freeAddr(t *testing.T, ip ...string) string {
	t.Helper()
	ln, err := net.Listen("tcp", firstSet(append(ip, "127.0.0.1")...)+":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// servedTLSGateway is the admin test config served as serve serves it, on
// real ports, with a certificate for 127.0.0.1.
type servedTLSGateway struct {
	*adminLive
	cert  testkit.Certificate
	https *http.Client // trusts cert, and follows no redirect
	plain *http.Client // follows no redirect
}

// startTLS serves adminMain with server's lines in place of its listen, and
// any further edits, old and new text in pairs as edit takes them, ${CERT}
// and ${KEY} in them standing for a new certificate's files, through
// Live.Listen.
func startTLS(t *testing.T, server string, edits ...string) *servedTLSGateway {
	t.Helper()
	cert := testkit.WriteCertificate(t, t.TempDir(), "gateway")
	files := strings.NewReplacer("${CERT}", cert.CertPath, "${KEY}", cert.KeyPath)
	edits = append([]string{"  listen: :0\n", server}, edits...)
	for i := range edits {
		edits[i] = files.Replace(edits[i])
	}
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, edit(t, adminMain, edits...), adminSecrets)
	servers, err := a.live.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		servers.Shutdown(ctx)
	})
	g := &servedTLSGateway{adminLive: a, cert: cert, https: testkit.TLSClient(cert), plain: testkit.TLSClient()}
	t.Cleanup(func() { // before Shutdown, which waits for open connections
		g.https.CloseIdleConnections()
		g.plain.CloseIdleConnections()
	})
	return g
}

// get sends a GET through client, with key as a bearer key if it isn't "".
func (g *servedTLSGateway) get(t *testing.T, client *http.Client, url, key string) response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return response{resp, string(body)}
}

// handshake reports whether a TLS handshake on addr succeeds, and the serial
// number of the certificate it got, trusting any certificate.
func handshake(addr string) (string, error) {
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].SerialNumber.String(), nil
}

// With serve left at both, one port serves plain HTTP and HTTPS, the
// HTTPS with HTTP/2, both serving every endpoint.
func TestTLS_OnePortServesBoth(t *testing.T) {
	addr := freeAddr(t)
	g := startTLS(t, "  listen: "+addr+"\n  tls: {cert: ${CERT}, key: ${KEY}}\n")
	if r := g.get(t, g.plain, "http://"+addr+"/health", ""); r.StatusCode != http.StatusOK || r.ProtoMajor != 1 || r.TLS != nil {
		t.Errorf("plain: %d %s", r.StatusCode, r.Proto)
	}
	if r := g.get(t, g.https, "https://"+addr+"/health", ""); r.StatusCode != http.StatusOK || r.ProtoMajor != 2 {
		t.Errorf("HTTPS: %d %s", r.StatusCode, r.Proto)
	}
	if r := g.get(t, g.https, "https://"+addr+"/v1/models", "secret"); r.StatusCode != http.StatusOK || !strings.Contains(r.body, `"task"`) {
		t.Errorf("a client API over HTTPS: %d %s", r.StatusCode, r.body)
	}
	if r := g.get(t, g.https, "https://"+addr+"/amf/config-status", "bobs-key"); r.StatusCode != http.StatusOK {
		t.Errorf("the admin API over HTTPS: %d %s", r.StatusCode, r.body)
	}
}

// With serve: https, a plain request gets 308 to the same host, path and
// query under https://, which keeps a POST a POST.
func TestTLS_HTTPSOnlyRedirectsPlainRequests(t *testing.T) {
	addr := freeAddr(t)
	g := startTLS(t, "  listen: "+addr+"\n  tls: {cert: ${CERT}, key: ${KEY}, serve: https}\n")
	resp, err := g.plain.Post("http://"+addr+"/v1/chat/completions?x=1", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != "https://"+addr+"/v1/chat/completions?x=1" {
		t.Errorf("got %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if r := g.get(t, g.https, "https://"+addr+"/health", ""); r.StatusCode != http.StatusOK {
		t.Errorf("HTTPS: %d", r.StatusCode)
	}
}

// A tls.listen of its own serves HTTPS there, and redirects a plain request
// there to itself; listen goes on serving plain HTTP, and no HTTPS.
func TestTLS_ASeparateHTTPSAddress(t *testing.T) {
	addr, httpsAddr := freeAddr(t), freeAddr(t)
	g := startTLS(t, "  listen: "+addr+"\n  tls: {cert: ${CERT}, key: ${KEY}, listen: '"+httpsAddr+"'}\n")
	if r := g.get(t, g.https, "https://"+httpsAddr+"/health", ""); r.StatusCode != http.StatusOK {
		t.Errorf("HTTPS on tls.listen: %d", r.StatusCode)
	}
	if r := g.get(t, g.plain, "http://"+httpsAddr+"/health?a=b", ""); r.StatusCode != http.StatusPermanentRedirect || r.Header.Get("Location") != "https://"+httpsAddr+"/health?a=b" {
		t.Errorf("plain on tls.listen: %d to %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r := g.get(t, g.plain, "http://"+addr+"/health", ""); r.StatusCode != http.StatusOK {
		t.Errorf("plain on listen: %d", r.StatusCode)
	}
	if _, err := handshake(addr); err == nil {
		t.Error("listen completed a TLS handshake it has no certificate for")
	}
}

// An admin side with HTTPS only and no listen of its own: on server.listen's
// plain HTTP, admin paths redirect to the admin side's HTTPS, client paths
// are served, and that HTTPS serves the admin side alone.
func TestTLS_AdminHTTPSOnlyBesidePlainClients(t *testing.T) {
	addr, adminHTTPS := freeAddr(t), freeAddr(t)
	g := startTLS(t, "  listen: "+addr+"\n", "  adminApi:\n", "  adminApi:\n    tls: {cert: ${CERT}, key: ${KEY}, serve: https, listen: '"+adminHTTPS+"'}\n")
	if r := g.get(t, g.plain, "http://"+addr+"/amf/config-status", "bobs-key"); r.StatusCode != http.StatusPermanentRedirect || r.Header.Get("Location") != "https://"+adminHTTPS+"/amf/config-status" {
		t.Errorf("an admin path over plain HTTP: %d to %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r := g.get(t, g.plain, "http://"+addr+"/amf/ui/", ""); r.StatusCode != http.StatusPermanentRedirect {
		t.Errorf("the admin UI over plain HTTP: %d", r.StatusCode)
	}
	if r := g.get(t, g.plain, "http://"+addr+"/v1/models", "secret"); r.StatusCode != http.StatusOK {
		t.Errorf("a client path over plain HTTP: %d", r.StatusCode)
	}
	if r := g.get(t, g.https, "https://"+adminHTTPS+"/amf/config-status", "bobs-key"); r.StatusCode != http.StatusOK {
		t.Errorf("the admin side's HTTPS: %d %s", r.StatusCode, r.body)
	}
	if r := g.get(t, g.https, "https://"+adminHTTPS+"/v1/models", "secret"); r.StatusCode != http.StatusNotFound {
		t.Errorf("a client path on the admin side's HTTPS: %d", r.StatusCode)
	}
}

// A reload reads the files again: a renewed certificate reaches the next
// handshake, and a config without one turns HTTPS off, while plain HTTP goes
// on.
func TestTLS_ReloadReadsTheCertificateAgain(t *testing.T) {
	addr := freeAddr(t)
	g := startTLS(t, "  listen: "+addr+"\n  tls: {cert: ${CERT}, key: ${KEY}}\n")
	if serial, err := handshake(addr); err != nil || serial != g.cert.SerialNumber.String() {
		t.Fatalf("before the reload: %s, %v", serial, err)
	}
	renewed := testkit.WriteCertificate(t, t.TempDir(), "renewed")
	for from, to := range map[string]string{renewed.CertPath: g.cert.CertPath, renewed.KeyPath: g.cert.KeyPath} {
		data, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, to, string(data), 0o600)
	}
	g.expect(t, g.send(t, "POST", "/amf/reload", "alices-key", ""), http.StatusOK, "reload")
	if serial, err := handshake(addr); err != nil || serial != renewed.SerialNumber.String() {
		t.Errorf("after the reload: %s, %v; want the renewed certificate's %s", serial, err, renewed.SerialNumber)
	}

	writeFile(t, g.main, strings.Replace(readFile(t, g.main), "  tls: {cert: "+g.cert.CertPath+", key: "+g.cert.KeyPath+"}\n", "", 1), 0o644)
	g.expect(t, g.send(t, "POST", "/amf/reload", "alices-key", ""), http.StatusOK, "reload")
	if _, err := handshake(addr); err == nil {
		t.Error("a handshake succeeded after a reload turned HTTPS off")
	}
	if r := g.get(t, g.plain, "http://"+addr+"/health", ""); r.StatusCode != http.StatusOK {
		t.Errorf("plain after the reload: %d", r.StatusCode)
	}
}

// A reload that moves HTTPS to an address the gateway didn't bind at start
// waits for a restart: HTTPS goes on where it was, and restart says where it
// will be.
func TestTLS_ANewHTTPSAddressWaitsForARestart(t *testing.T) {
	addr, later := freeAddr(t), freeAddr(t)
	g := startTLS(t, "  listen: "+addr+"\n  tls: {cert: ${CERT}, key: ${KEY}}\n")
	writeFile(t, g.main, strings.Replace(readFile(t, g.main), ", key: "+g.cert.KeyPath+"}", ", key: "+g.cert.KeyPath+", listen: '"+later+"'}", 1), 0o644)
	g.expect(t, g.send(t, "POST", "/amf/reload", "alices-key", ""), http.StatusOK, "reload")
	if _, err := handshake(addr); err != nil {
		t.Errorf("HTTPS stopped where it was: %v", err)
	}
	status := g.send(t, "GET", "/amf/config-status", "bobs-key", "")
	restart := gjson.Get(status.body, `restart.#(key=="server.tls.listen")`)
	if restart.Get("listening").String() != addr || restart.Get("running").String() != later || !restart.Get("saved").Bool() {
		t.Errorf("restart: %s", gjson.Get(status.body, "restart"))
	}
}

// curl reaches both protocols on one port, HTTP/2 over TLS included.
func TestTLS_CurlOnOnePort(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl on the PATH")
	}
	addr := freeAddr(t)
	g := startTLS(t, "  listen: "+addr+"\n  tls: {cert: ${CERT}, key: ${KEY}}\n")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"http://" + addr + "/health"}, "1.1"},
		{[]string{"--http2", "--cacert", g.cert.CertPath, "https://" + addr + "/health"}, "2"},
		{[]string{"--http1.1", "--cacert", g.cert.CertPath, "https://" + addr + "/health"}, "1.1"},
	} {
		out, err := exec.Command(curl, append([]string{"-sS", "-o", "/dev/null", "-w", "%{http_code} %{http_version}"}, c.args...)...).CombinedOutput()
		if err != nil || string(out) != "200 "+c.want {
			t.Errorf("curl %s: %s, %v; want 200 %s", strings.Join(c.args, " "), out, err, c.want)
		}
	}
}

// Each half of a split listener closes by itself, and the listener it reads
// from closes with the second.
func TestTLS_SplitClosesWithBothHalves(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	plain, secure := Split(ln)
	plain.Close()
	if _, err := plain.Accept(); err == nil {
		t.Error("a closed half accepted")
	}
	accepted := make(chan error, 1)
	go func() {
		c, err := secure.Accept()
		if err == nil {
			c.Close()
		}
		accepted <- err
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("the listener closed with one half: %v", err)
	}
	conn.Write([]byte{tlsRecordHandshake})
	if err := <-accepted; err != nil {
		t.Errorf("the open half: %v", err)
	}
	conn.Close()
	secure.Close()
	if c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		c.Close()
		t.Error("the listener stayed open after both halves closed")
	}
}

// health reaches a gateway that serves only HTTPS without checking its
// certificate, which a loopback address isn't on.
func TestTLS_HealthOverHTTPS(t *testing.T) {
	addr := freeAddr(t)
	startTLS(t, "  listen: "+addr+"\n  tls: {cert: ${CERT}, key: ${KEY}, serve: https}\n")
	url, err := HealthURL(addr, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckHealth(context.Background(), url); err != nil {
		t.Error(err)
	}
}

// An admin tls block with no address of its own shares server.listen, the
// main API's IP and port: plain HTTP there serves everything, and HTTPS
// there serves the admin side alone.
func TestTLS_AdminHTTPSOnTheMainAPIsPort(t *testing.T) {
	addr := freeAddr(t)
	g := startTLS(t, "  listen: "+addr+"\n", "  adminApi:\n", "  adminApi:\n    tls: {cert: ${CERT}, key: ${KEY}}\n")
	if r := g.get(t, g.https, "https://"+addr+"/amf/config-status", "bobs-key"); r.StatusCode != http.StatusOK {
		t.Errorf("the admin API over HTTPS: %d %s", r.StatusCode, r.body)
	}
	if r := g.get(t, g.https, "https://"+addr+"/v1/models", "secret"); r.StatusCode != http.StatusNotFound {
		t.Errorf("a client path over the admin side's HTTPS: %d", r.StatusCode)
	}
	if r := g.get(t, g.plain, "http://"+addr+"/v1/models", "secret"); r.StatusCode != http.StatusOK {
		t.Errorf("a client path over plain HTTP: %d", r.StatusCode)
	}
	if r := g.get(t, g.plain, "http://"+addr+"/amf/config-status", "bobs-key"); r.StatusCode != http.StatusOK {
		t.Errorf("the admin API over plain HTTP, with serve left at both: %d", r.StatusCode)
	}
}

// An admin side on an IP and port of its own serves its plain HTTP and its
// HTTPS there, and only there; the main API stays on its own.
func TestTLS_AdminOnAnIPAndPortOfItsOwn(t *testing.T) {
	if ln, err := net.Listen("tcp", "127.0.0.2:0"); err != nil {
		t.Skip("127.0.0.2 isn't a loopback address here, as on macOS by default")
	} else {
		ln.Close()
	}
	addr, admin := freeAddr(t), freeAddr(t, "127.0.0.2")
	g := startTLS(t, "  listen: "+addr+"\n", "  adminApi:\n", "  adminApi:\n    listen: '"+admin+"'\n    tls: {cert: ${CERT}, key: ${KEY}}\n")
	if r := g.get(t, g.https, "https://"+admin+"/amf/config-status", "bobs-key"); r.StatusCode != http.StatusOK || r.ProtoMajor != 2 {
		t.Errorf("the admin API over HTTPS: %d %s %s", r.StatusCode, r.Proto, r.body)
	}
	if r := g.get(t, g.plain, "http://"+admin+"/amf/config-status", "bobs-key"); r.StatusCode != http.StatusOK {
		t.Errorf("the admin API over plain HTTP: %d", r.StatusCode)
	}
	if r := g.get(t, g.https, "https://"+admin+"/v1/models", "secret"); r.StatusCode != http.StatusNotFound {
		t.Errorf("a client path on the admin side's HTTPS: %d", r.StatusCode)
	}
	if r := g.get(t, g.plain, "http://"+addr+"/amf/config-status", "bobs-key"); r.StatusCode != http.StatusNotFound {
		t.Errorf("the admin API on the main API's address: %d", r.StatusCode)
	}
	if r := g.get(t, g.plain, "http://"+addr+"/v1/models", "secret"); r.StatusCode != http.StatusOK {
		t.Errorf("the main API: %d", r.StatusCode)
	}
	if _, err := handshake(addr); err == nil {
		t.Error("the main API's address completed a TLS handshake with no server.tls")
	}
}

// The admin UI sends cert and key as edits of their own, so the draft takes
// cert alone, and only an apply of it is refused.
func TestTLS_ADraftTakesACertBeforeItsKey(t *testing.T) {
	swap := newSwap(t)
	a := startAdmin(t, swap.URL, adminMain, adminSecrets)
	a.expect(t, a.send(t, "PUT", "/amf/config/server/tls", "alices-key", `{"cert": "tls/gateway.pem"}`), http.StatusCreated, "cert alone")
	if r := a.send(t, "POST", "/amf/config-apply", "alices-key", ""); r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(r.body, "cert is set but key isn't") {
		t.Errorf("apply: %d %s", r.StatusCode, r.body)
	}
	a.expect(t, a.send(t, "PUT", "/amf/config/server/tls/key", "alices-key", `"tls/gateway.key"`), http.StatusCreated, "then its key")
}
