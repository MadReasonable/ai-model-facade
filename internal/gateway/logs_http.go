package gateway

import (
	"bytes"
	"fmt"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/loghub"
)

// logsHeader marks a reply from a gateway whose copy of the log keeps no
// history, server.adminApi.logs.maxBytes 0, so an empty reply isn't read as a quiet
// gateway.
const logsHeader = "X-Gateway-Logs"

// keepAliveInterval is how often a log stream with nothing to send sends a
// comment, so a proxy that closes idle connections keeps it open, and the
// gateway checks the caller may still read the log.
var keepAliveInterval = 30 * time.Second

// getLogs serves GET /amf/logs: the gateway's copy of its log, as JSON
// lines, or as stderr's text lines with ?format=text or Accept: text/plain,
// filtered by ?level=, ?since= and ?limit=. With ?follow, it streams the
// selected lines and then each new one, as server-sent events.
func (s *server) getLogs(w http.ResponseWriter, r *http.Request, client backend.Client) {
	q := r.URL.Query()
	filter, err := logFilter(q)
	if err != nil {
		s.reject(amfFamily, w, r, client, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	text := false
	switch q.Get("format") {
	case "text":
		text = true
	case "json":
	case "":
		text = acceptsText(r)
	default:
		s.reject(amfFamily, w, r, client, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("format: '%s' is neither json nor text", q.Get("format")))
		return
	}
	logs := s.live.logs
	if !logs.KeepsHistory() {
		w.Header().Set(logsHeader, "streaming-only")
	}
	if q.Has("follow") {
		s.followLogs(w, r, client, filter, text)
		return
	}
	if text {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/x-ndjson")
	}
	var body bytes.Buffer
	for _, e := range logs.Read(filter) {
		body.Write(line(e, text))
	}
	w.Write(body.Bytes())
}

// logFilter reads ?level=, ?since= and ?limit=. since is a time in RFC 3339,
// or a duration back from now, such as 10m.
func logFilter(q url.Values) (loghub.Filter, error) {
	f := loghub.Filter{Level: slog.Level(math.MinInt32)}
	if name := q.Get("level"); name != "" {
		level, ok := map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warning": slog.LevelWarn, "error": slog.LevelError}[name]
		if !ok {
			return f, fmt.Errorf("level: unknown level '%s' (debug | info | warning | error)", name)
		}
		f.Level = level
	}
	if since := q.Get("since"); since != "" {
		if d, err := time.ParseDuration(since); err == nil && d >= 0 {
			f.Since = time.Now().Add(-d)
		} else if t, err := time.Parse(time.RFC3339Nano, since); err == nil {
			f.Since = t
		} else {
			return f, fmt.Errorf("since: '%s' is neither a time such as 2026-11-20T09:30:00Z nor a duration such as 10m", since)
		}
	}
	if limit := q.Get("limit"); limit != "" {
		n, err := strconv.Atoi(limit)
		if err != nil || n < 1 {
			return f, fmt.Errorf("limit: '%s' is not a number of lines above 0", limit)
		}
		f.Limit = n
	}
	return f, nil
}

func acceptsText(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		if mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(part)); err == nil && mediaType == "text/plain" {
			return true
		}
	}
	return false
}

func line(e loghub.Entry, text bool) []byte {
	if text {
		return e.Text
	}
	return e.JSON
}

// followLogs streams the lines filter selects, then each new one at its
// level or above, until the client leaves or the gateway ends the stream. A
// stream outlives a reload, so it ends once the running config no longer
// lets the caller read the log, checked with each line and each keep-alive.
func (s *server) followLogs(w http.ResponseWriter, r *http.Request, client backend.Client, filter loghub.Filter, text bool) {
	logs := s.live.logs
	backlog, stream := logs.Follow(filter)
	defer logs.Stop(stream)
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // so nginx passes each event on as it comes
	w.WriteHeader(http.StatusOK)
	for _, e := range backlog {
		writeSSE(w, "", line(e, text))
	}
	rc.Flush()
	keepAlive := time.NewTicker(keepAliveInterval)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-stream.Ended():
			writeSSE(w, "end", []byte(`{"reason":"the gateway ended the stream: it is shutting down, or no admin list lets anyone read the log"}`))
			rc.Flush()
			return
		case item := <-stream.Items():
			if !s.mayStillRead(client) {
				s.endRevoked(w, rc)
				return
			}
			if item.Dropped > 0 {
				writeSSE(w, "dropped", fmt.Appendf(nil, `{"dropped":%d}`, item.Dropped))
			}
			writeSSE(w, "", line(item.Entry, text))
			rc.Flush()
		case <-keepAlive.C:
			if !s.mayStillRead(client) {
				s.endRevoked(w, rc)
				return
			}
			fmt.Fprint(w, ": keep-alive\n\n")
			rc.Flush()
		}
	}
}

// mayStillRead reports whether the running config, which may have replaced
// the one the stream started under, lets client read the log, or the
// config's status, which server.adminApi.read allows together.
func (s *server) mayStillRead(client backend.Client) bool {
	cfg := s.live.Gateway().Config.Server
	return cfg.AdminAPI.CanRead(client.Name, cfg.GroupsOf(client.Name))
}

func (s *server) endRevoked(w http.ResponseWriter, rc *http.ResponseController) {
	writeSSE(w, "end", []byte(`{"reason":"server.adminApi no longer lets this client read the log"}`))
	rc.Flush()
}

// writeSSE writes one server-sent event: a named event, or with event ""
// the default one, holding data without its trailing newline.
func writeSSE(w http.ResponseWriter, event string, data []byte) {
	if event != "" {
		fmt.Fprintf(w, "event: %s\n", event)
	}
	fmt.Fprintf(w, "data: %s\n\n", bytes.TrimSuffix(data, []byte("\n")))
}
