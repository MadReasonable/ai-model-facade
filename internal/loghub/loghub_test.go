package loghub

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/MadReasonable/ai-model-facade/internal/config"
)

func configure(t *testing.T, h *Hub, server string) {
	t.Helper()
	cfg, err := config.Parse([]byte("server:\n  listen: :0\n  apiKeys: [{name: alice, key: k}]\n"+server+"models: {}\n"), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	h.Configure(cfg)
}

const readers = "  adminApi:\n    read: {allowedKeys: [alice]}\n"

func messages(entries []Entry) string {
	var out []string
	for _, e := range entries {
		out = append(out, strings.TrimSpace(string(e.Text)))
	}
	return strings.Join(out, "\n")
}

// The Hub keeps each record at its level as stderr's text line and as a JSON
// line, with the attributes With adds, and nothing below its level.
func TestHub_KeepsBothFormsAtItsLevel(t *testing.T) {
	h := New()
	configure(t, h, readers+"    logs: {level: debug}\n")
	log := slog.New(h.Handler()).With("backend", "local")
	log.Debug("probing", "n", 1)
	log.Info("listening")
	entries := h.Read(Filter{Level: slog.LevelDebug})
	if len(entries) != 2 || !strings.Contains(string(entries[0].Text), `level=DEBUG msg=probing backend=local n=1`) || !strings.Contains(string(entries[0].JSON), `"msg":"probing","backend":"local","n":1}`) {
		t.Fatalf("entries:\n%s", messages(entries))
	}
	if got := h.Read(Filter{Level: slog.LevelInfo}); len(got) != 1 {
		t.Errorf("info and above: %d entries", len(got))
	}
	configure(t, h, readers) // the level is logging.level's, info
	log.Debug("not kept")
	if got := h.Read(Filter{Level: slog.LevelDebug}); len(got) != 2 {
		t.Errorf("a debug line below the level was kept:\n%s", messages(got))
	}
}

// The ring drops the oldest records past maxBytes, but always keeps the
// newest, however large.
func TestHub_RingKeepsTheNewestPastItsSize(t *testing.T) {
	h := New()
	configure(t, h, readers+"    logs: {maxBytes: 700}\n")
	log := slog.New(h.Handler())
	for _, msg := range []string{"one", "two", "three", "four", "five"} {
		log.Info(msg)
	}
	got := messages(h.Read(Filter{}))
	if strings.Contains(got, "msg=one") || !strings.Contains(got, "msg=five") {
		t.Errorf("entries:\n%s", got)
	}
	log.Info(strings.Repeat("x", 5000))
	if entries := h.Read(Filter{}); len(entries) != 1 || len(entries[0].Text) < 5000 {
		t.Errorf("%d entries; want the large newest alone", len(entries))
	}
}

// With maxBytes 0 the Hub keeps no history, and takes records only while a
// stream wants them, at the lowest level an open stream asked for.
func TestHub_NoRingTakesOnlyForStreams(t *testing.T) {
	h := New()
	configure(t, h, readers+"    logs: {maxBytes: 0}\n")
	handler, ctx := h.Handler(), context.Background()
	if handler.Enabled(ctx, slog.LevelError) {
		t.Error("enabled with no stream open")
	}
	_, s := h.Follow(Filter{Level: slog.LevelWarn})
	if handler.Enabled(ctx, slog.LevelInfo) || !handler.Enabled(ctx, slog.LevelWarn) {
		t.Error("not enabled at the stream's level alone")
	}
	slog.New(handler).Warn("seen")
	if item := <-s.Items(); !strings.Contains(string(item.Text), "msg=seen") {
		t.Errorf("stream got %s", item.Text)
	}
	if got := h.Read(Filter{}); len(got) != 0 {
		t.Errorf("kept %d entries with no ring", len(got))
	}
	h.Stop(s)
	if handler.Enabled(ctx, slog.LevelError) {
		t.Error("enabled after the last stream stopped")
	}
}

// A stream starts with the ring's selection and goes on with each new record,
// none in both or in neither; a reader that falls behind loses records, and
// the next one it gets says how many.
func TestHub_FollowAndDrops(t *testing.T) {
	h := New()
	configure(t, h, readers)
	log := slog.New(h.Handler())
	log.Info("before")
	backlog, s := h.Follow(Filter{})
	if messages(backlog) == "" || !strings.Contains(messages(backlog), "msg=before") {
		t.Fatalf("backlog: %s", messages(backlog))
	}
	for i := 0; i < queueSize+3; i++ {
		log.Info("flood")
	}
	for i := 0; i < queueSize; i++ {
		if item := <-s.Items(); item.Dropped != 0 {
			t.Fatalf("item %d: dropped %d", i, item.Dropped)
		}
	}
	log.Info("after")
	if item := <-s.Items(); item.Dropped != 3 || !strings.Contains(string(item.Text), "msg=after") {
		t.Errorf("got %s, dropped %d; want 'after' with 3 dropped", item.Text, item.Dropped)
	}
}

// A config that lets no one read the log turns the Hub off: it drops what it
// holds, ends every stream and takes nothing.
func TestHub_OffWithoutReaders(t *testing.T) {
	h := New()
	configure(t, h, readers)
	slog.New(h.Handler()).Info("kept")
	_, s := h.Follow(Filter{})
	configure(t, h, "")
	select {
	case <-s.Ended():
	default:
		t.Error("stream not ended")
	}
	if len(h.Read(Filter{})) != 0 || h.Handler().Enabled(context.Background(), slog.LevelError) {
		t.Error("still keeping the log")
	}
	if _, s := h.Follow(Filter{}); s == nil {
		t.Fatal("no stream")
	} else {
		select {
		case <-s.Ended():
		default:
			t.Error("a stream of a Hub that is off isn't ended")
		}
	}
}

// Attributes in a group added with WithGroup come out as stderr shows them,
// in both forms.
func TestHub_KeepsGroups(t *testing.T) {
	h := New()
	configure(t, h, readers)
	slog.New(h.Handler()).WithGroup("req").With("id", 7).Info("grouped", "path", "/v1")
	entries := h.Read(Filter{})
	if len(entries) != 1 || !strings.Contains(string(entries[0].Text), "req.id=7 req.path=/v1") || !strings.Contains(string(entries[0].JSON), `"req":{"id":7,"path":"/v1"}`) {
		t.Errorf("entries:\n%s\n%s", messages(entries), entries[0].JSON)
	}
}

// A ring whose backing array is mostly room it no longer uses, after its
// front was dropped, is copied, so that memory is freed; one that uses more
// than half is kept as it is.
func TestClipFront(t *testing.T) {
	sparse := make([]Entry, 2, 10)
	sparse[0].Text, sparse[1].Text = []byte("a"), []byte("b")
	got := clipFront(sparse)
	if &got[0] == &sparse[0] || cap(got) != 2 || string(got[0].Text) != "a" || string(got[1].Text) != "b" {
		t.Errorf("mostly unused: cap %d, copied %v, %q %q", cap(got), &got[0] != &sparse[0], got[0].Text, got[1].Text)
	}
	dense := make([]Entry, 6, 10)
	if got := clipFront(dense); &got[0] != &dense[0] {
		t.Error("a ring using more than half its array was copied")
	}
}
