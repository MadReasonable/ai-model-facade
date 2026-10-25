package openaihttp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// A caller that may not wait gets the last state while a refresh is in
// flight, or ErrStateBusy before any state was fetched. With no refresh in
// flight it fetches and waits like anyone else.
func TestWithoutStateWait(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{}, 1)
	s := NewStateCache(Config{}, func(ctx context.Context) ([]backend.ModelInfo, error) {
		calls.Add(1)
		<-release
		return []backend.ModelInfo{{ID: "m"}}, nil
	})
	noWait := backend.WithoutStateWait(context.Background())
	first := make(chan error)
	go func() {
		_, err := s.LoadedModels(context.Background())
		first <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if _, err := s.LoadedModels(noWait); err != backend.ErrStateBusy {
		t.Errorf("before any state: got %v", err)
	}
	release <- struct{}{}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	go s.LoadedModels(context.Background()) // stateCacheSeconds 0: refetches
	time.Sleep(20 * time.Millisecond)
	if got, err := s.LoadedModels(noWait); err != nil || len(got) != 1 || got[0].ID != "m" {
		t.Errorf("during refresh: got %+v, %v", got, err)
	}
	release <- struct{}{}
	time.Sleep(20 * time.Millisecond)
	release <- struct{}{}
	if got, err := s.LoadedModels(noWait); err != nil || len(got) != 1 || calls.Load() != 3 {
		t.Errorf("idle: got %+v, %v after %d fetches", got, err, calls.Load())
	}
}

// A refresh that started before Invalidate isn't cached, and callers after
// Invalidate don't join it.
func TestInvalidateDuringRefresh(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	s := NewStateCache(Config{StateCacheSeconds: 60}, func(ctx context.Context) ([]backend.ModelInfo, error) {
		if calls.Add(1) == 1 {
			<-release
			return []backend.ModelInfo{{ID: "stale"}}, nil
		}
		return []backend.ModelInfo{{ID: "fresh"}}, nil
	})
	first := make(chan []backend.ModelInfo)
	go func() {
		models, _ := s.LoadedModels(context.Background())
		first <- models
	}()
	time.Sleep(20 * time.Millisecond) // let the first refresh start
	s.Invalidate()
	if got, _ := s.LoadedModels(context.Background()); len(got) != 1 || got[0].ID != "fresh" {
		t.Errorf("after invalidate: %+v", got)
	}
	close(release)
	if got := <-first; len(got) != 1 || got[0].ID != "stale" {
		t.Errorf("first caller: %+v", got)
	}
	if got, _ := s.LoadedModels(context.Background()); len(got) != 1 || got[0].ID != "fresh" {
		t.Errorf("cached: %+v", got)
	}
	if calls.Load() != 2 {
		t.Errorf("got %d fetches", calls.Load())
	}
}

// A read reuses a state fetched since its request arrived even with
// stateCacheSeconds 0, but not one fetched before, nor one dropped by
// Invalidate, and a read with no request start reuses nothing.
func TestRequestStartReuse(t *testing.T) {
	var calls atomic.Int32
	s := NewStateCache(Config{}, func(ctx context.Context) ([]backend.ModelInfo, error) {
		calls.Add(1)
		return []backend.ModelInfo{{ID: "m"}}, nil
	})
	request := backend.WithRequestStart(context.Background(), time.Now())
	s.LoadedModels(context.Background()) // fetched after the request arrived
	if s.LoadedModels(request); calls.Load() != 1 {
		t.Errorf("fetched since the request arrived: got %d fetches, want 1", calls.Load())
	}
	if s.LoadedModels(backend.WithRequestStart(context.Background(), time.Now())); calls.Load() != 2 {
		t.Errorf("fetched before the request arrived: got %d fetches, want 2", calls.Load())
	}
	if s.LoadedModels(context.Background()); calls.Load() != 3 {
		t.Errorf("no request start: got %d fetches, want 3", calls.Load())
	}
	request = backend.WithRequestStart(context.Background(), time.Now())
	s.LoadedModels(context.Background())
	s.Invalidate()
	if s.LoadedModels(request); calls.Load() != 5 {
		t.Errorf("invalidated: got %d fetches, want 5", calls.Load())
	}
}

// A pending model is reported as starting unless the backend reports it ready
// or starting; an unknown one becomes starting, and a pending alias counts as
// its base. Releasing a model the cached state listed as ready keeps the
// cache; releasing any other drops it. A second release does nothing.
func TestPending(t *testing.T) {
	var calls atomic.Int32
	s := NewStateCache(Config{StateCacheSeconds: 60}, func(ctx context.Context) ([]backend.ModelInfo, error) {
		calls.Add(1)
		return []backend.ModelInfo{{ID: "r", Aliases: []string{"r:x"}}, {ID: "s", Starting: true}, {ID: "u", Unknown: true}}, nil
	})
	var releases []func()
	for _, id := range []string{"r", "r:x", "s", "u", "new", "new"} {
		releases = append(releases, s.Pending(id))
	}
	got, _ := s.LoadedModels(context.Background())
	want := []backend.ModelInfo{{ID: "r", Aliases: []string{"r:x"}}, {ID: "s", Starting: true}, {ID: "u", Starting: true}, {ID: "new", Starting: true}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Starting != want[i].Starting || got[i].Unknown != want[i].Unknown {
			t.Errorf("model %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	releases[0]() // r was ready: cache kept
	if s.LoadedModels(context.Background()); calls.Load() != 1 {
		t.Errorf("after releasing a ready model: %d fetches, want 1", calls.Load())
	}
	releases[4]()
	releases[4]() // a second release of the same request does nothing
	got, _ = s.LoadedModels(context.Background())
	if calls.Load() != 2 || got[len(got)-1].ID != "new" || !got[len(got)-1].Starting {
		t.Errorf("after releasing one of two 'new' requests: %d fetches, got %+v", calls.Load(), got)
	}
}

// A request that becomes pending while a read is fetching still shows in
// that read.
func TestPendingDuringFetch(t *testing.T) {
	fetching, release := make(chan struct{}), make(chan struct{})
	s := NewStateCache(Config{}, func(ctx context.Context) ([]backend.ModelInfo, error) {
		close(fetching)
		<-release
		return nil, nil
	})
	result := make(chan []backend.ModelInfo)
	go func() {
		models, _ := s.LoadedModels(context.Background())
		result <- models
	}()
	<-fetching
	s.Pending("late")
	close(release)
	if got := <-result; len(got) != 1 || got[0].ID != "late" || !got[0].Starting {
		t.Errorf("got %+v", got)
	}
}

// HTTP client errors name the URL in single quotes, not Go's double quotes,
// which logs and JSON bodies would escape; the cause stays inspectable.
func TestURLErrorWording(t *testing.T) {
	c, err := New(Config{BaseURL: "http://127.0.0.1:1", StateTimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	err = c.GetJSON(context.Background(), "/v1/models", &struct{}{})
	if err == nil || !strings.HasPrefix(err.Error(), "GET 'http://127.0.0.1:1/v1/models': ") || strings.Contains(err.Error(), `"`) {
		t.Errorf("got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Do(ctx, http.MethodPost, "/v1/chat/completions", []byte(`{}`), "", backend.Client{})
	if !errors.Is(err, context.Canceled) || !strings.HasPrefix(err.Error(), "POST '") {
		t.Errorf("cancelled: got %v", err)
	}
}
