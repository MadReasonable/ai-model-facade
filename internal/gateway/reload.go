package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/MadReasonable/ai-model-facade/internal/config"
)

// Live serves the current gateway and can replace it while running.
// Requests already in flight, including streams, finish on the gateway
// that accepted them; new requests use the replacement.
type Live struct {
	log     *slog.Logger
	mu      sync.Mutex // serializes Reload
	current atomic.Pointer[live]
}

type live struct {
	gateway *Gateway
	handler http.Handler
}

func NewLive(g *Gateway, log *slog.Logger) *Live {
	l := &Live{log: log}
	l.current.Store(&live{gateway: g, handler: g.Handler(log)})
	return l
}

func (l *Live) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.current.Load().handler.ServeHTTP(w, r)
}

// Gateway returns the gateway currently serving requests.
func (l *Live) Gateway() *Gateway {
	return l.current.Load().gateway
}

// Reload builds a gateway from cfg and, only if that fully succeeds, swaps
// it in. On any error the running gateway is untouched. server.listen can't
// change without a restart, because the port is already bound.
func (l *Live) Reload(ctx context.Context, cfg *config.Config) (*Gateway, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	old := l.current.Load().gateway
	if cfg.Server.Listen != old.Config.Server.Listen {
		return nil, fmt.Errorf("server.listen changed from %q to %q; restart to apply", old.Config.Server.Listen, cfg.Server.Listen)
	}
	g, err := Build(ctx, cfg, l.log)
	if err != nil {
		return nil, err
	}
	l.current.Store(&live{gateway: g, handler: g.Handler(l.log)})
	old.closeIdleConnections()
	return g, nil
}

// closeIdleConnections releases pooled connections a replaced gateway no
// longer needs. Connections still serving in-flight requests are untouched.
func (g *Gateway) closeIdleConnections() {
	for _, b := range g.backends {
		if c, ok := b.(interface{ CloseIdleConnections() }); ok {
			c.CloseIdleConnections()
		}
	}
}
