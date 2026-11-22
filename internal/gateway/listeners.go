package gateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// tlsRecordHandshake is the first byte of every TLS connection: the content
// type of the ClientHello's record (RFC 8446, 5.1). No HTTP request starts
// with it, since a request line starts with a method name.
const tlsRecordHandshake = 0x16

// firstByteTimeout bounds the wait for a connection's first byte, as
// ReadHeaderTimeout bounds the wait for a request's headers, so a
// connection that sends nothing doesn't hold a goroutine forever.
const firstByteTimeout = 10 * time.Second

// Split hands each connection ln accepts to plain or secure by its first
// byte, so one address serves plain HTTP and HTTPS. Closing both closes ln.
func Split(ln net.Listener) (plain, secure net.Listener) {
	s := &split{ln: ln, open: 2, closed: make(chan struct{})}
	p := &splitListener{split: s, conns: make(chan net.Conn), closed: make(chan struct{})}
	t := &splitListener{split: s, conns: make(chan net.Conn), closed: make(chan struct{})}
	go s.run(p, t)
	return p, t
}

// split is the listener that Split reads from.
type split struct {
	ln       net.Listener
	mu       sync.Mutex
	open     int // of the two splitListeners
	closed   chan struct{}
	closeErr error
}

func (s *split) run(plain, secure *splitListener) {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			close(s.closed)
			return
		}
		go s.route(c, plain, secure)
	}
}

// route waits for c's first byte and gives c to the listener it belongs to,
// or closes it if that listener is closed.
func (s *split) route(c net.Conn, plain, secure *splitListener) {
	r := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(firstByteTimeout))
	first, err := r.Peek(1)
	c.SetReadDeadline(time.Time{})
	if err != nil {
		c.Close()
		return
	}
	to := plain
	if first[0] == tlsRecordHandshake {
		to = secure
	}
	select {
	case to.conns <- &peekedConn{Conn: c, r: r}:
	case <-to.closed:
		c.Close()
	case <-s.closed:
		c.Close()
	}
}

// release closes ln once both splitListeners are closed.
func (s *split) release() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open--; s.open == 0 {
		s.closeErr = s.ln.Close()
	}
	return s.closeErr
}

// splitListener is one of the two listeners Split returns.
type splitListener struct {
	*split
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

func (l *splitListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-l.split.closed:
		return nil, net.ErrClosed
	}
}

func (l *splitListener) Close() error {
	err := net.ErrClosed
	l.closeOnce.Do(func() {
		close(l.closed)
		err = l.release()
	})
	return err
}

func (l *splitListener) Addr() net.Addr { return l.ln.Addr() }

// peekedConn is a connection whose first byte route has read, which Read
// returns again.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// redirectToTLS answers every request with 308 to the same host, path and
// query under https://, on the port of addr, the HTTPS address it belongs
// to. 308 keeps the method and body, so a client's POST isn't turned into a
// GET.
func redirectToTLS(addr string) http.Handler {
	_, port, _ := net.SplitHostPort(addr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		http.Redirect(w, r, "https://"+net.JoinHostPort(host, port)+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
}

// errNoHTTPS refuses a handshake on an address the running config serves no
// HTTPS on, as after a reload that turns a side's TLS off.
var errNoHTTPS = errors.New("no HTTPS is served on this address")

// TLSConfig is the TLS configuration of the HTTPS served on addr, one of
// Addresses. Each handshake takes the certificate of the config running
// then, so a reload's new certificate reaches new connections.
func (l *Live) TLSConfig(addr string) *tls.Config {
	return &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		if slot := l.current.Load().slots[addr]; slot != nil && slot.cert != nil {
			return slot.cert, nil
		}
		return nil, fmt.Errorf("%s: %w", addr, errNoHTTPS)
	}}
}

// Servers serve every address a Live listens on, plain HTTP and HTTPS on
// each, told apart by each connection's first byte (Split). What each
// serves, and with which certificate, follows the running config.
type Servers struct {
	servers []*http.Server
	errs    chan error
}

// Listen listens on every address l listens on and serves them until
// Shutdown, each server calling onShutdown as it shuts down, for streams
// that never end by themselves. It returns once every address is open, so
// a caller that logs it listens only after the ports are.
func (l *Live) Listen(onShutdown ...func()) (*Servers, error) {
	addrs := l.Addresses()
	s := &Servers{errs: make(chan error, 2*len(addrs))}
	var listeners []net.Listener
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			for _, open := range listeners {
				open.Close()
			}
			return nil, err
		}
		listeners = append(listeners, ln)
	}
	for i, addr := range addrs {
		plain, secure := Split(listeners[i])
		plainServer := newServer(addr, l.Plain(addr))
		tlsServer := newServer(addr, l.Secure(addr))
		tlsServer.TLSConfig = l.TLSConfig(addr)
		for _, server := range []*http.Server{plainServer, tlsServer} {
			for _, f := range onShutdown {
				server.RegisterOnShutdown(f)
			}
		}
		s.servers = append(s.servers, plainServer, tlsServer)
		go func() {
			s.errs <- plainServer.Serve(plain)
		}()
		go func() {
			s.errs <- tlsServer.ServeTLS(secure, "", "")
		}()
	}
	return s, nil
}

// newServer is the HTTP server of one address's plain HTTP or HTTPS.
func newServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

// Errs receives the error of a server that stopped serving by itself.
func (s *Servers) Errs() <-chan error {
	return s.errs
}

// Shutdown shuts every server down, as http.Server.Shutdown does.
func (s *Servers) Shutdown(ctx context.Context) error {
	var errs []error
	for _, server := range s.servers {
		if err := server.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
