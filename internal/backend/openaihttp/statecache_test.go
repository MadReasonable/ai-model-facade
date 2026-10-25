package openaihttp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// fetcher is a state fetch that counts its calls. If gate is set, each call
// waits until it is closed, so a test can hold a refresh in flight.
type fetcher struct {
	calls  atomic.Int32
	gate   chan struct{}
	models []backend.ModelInfo
	err    error

	mu      sync.Mutex
	started chan struct{} // closed once the first call has begun
	ctxErr  error         // the context's error when the last call returned
}

func newFetcher(models ...backend.ModelInfo) *fetcher {
	return &fetcher{models: models, started: make(chan struct{})}
}

func (f *fetcher) fetch(ctx context.Context) ([]backend.ModelInfo, error) {
	if f.calls.Add(1) == 1 {
		close(f.started)
	}
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	f.ctxErr = ctx.Err()
	f.mu.Unlock()
	return f.models, f.err
}

// readersWaiting makes s report each read that waits for a fetch, and
// returns a function that blocks until n reads have.
func readersWaiting(t *testing.T, s *StateCache, n int) func() {
	t.Helper()
	waiting := make(chan struct{}, 64)
	s.waiting = func() { waiting <- struct{}{} }
	return func() {
		t.Helper()
		for range n {
			select {
			case <-waiting:
			case <-time.After(5 * time.Second):
				t.Fatalf("fewer than %d reads waited for the fetch", n)
			}
		}
	}
}

func TestStateCache_CachesForStateCacheSeconds(t *testing.T) {
	f := newFetcher(backend.ModelInfo{ID: "m"})
	s := NewStateCache(Config{StateCacheSeconds: 60}, f.fetch)
	s.LoadedModels(context.Background())
	got, err := s.LoadedModels(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != "m" || f.calls.Load() != 1 {
		t.Errorf("got %+v, %v after %d fetches", got, err, f.calls.Load())
	}
}

func TestStateCache_FetchesOnEveryReadWithoutCache(t *testing.T) {
	f := newFetcher()
	s := NewStateCache(Config{}, f.fetch)
	s.LoadedModels(context.Background())
	s.LoadedModels(context.Background())
	if f.calls.Load() != 2 {
		t.Errorf("got %d fetches, want 2", f.calls.Load())
	}
}

func TestStateCache_DoesNotCacheAFailedFetch(t *testing.T) {
	f := newFetcher()
	f.err = errors.New("down")
	s := NewStateCache(Config{StateCacheSeconds: 60}, f.fetch)
	if _, err := s.LoadedModels(context.Background()); err == nil {
		t.Fatal("expected the fetch's error")
	}
	f.err = nil
	if _, err := s.LoadedModels(context.Background()); err != nil || f.calls.Load() != 2 {
		t.Errorf("after recovery: %v after %d fetches, want 2", err, f.calls.Load())
	}
}

// Concurrent reads wait for one fetch instead of each starting its own.
func TestStateCache_ConcurrentReadsShareOneFetch(t *testing.T) {
	f := newFetcher(backend.ModelInfo{ID: "m"})
	f.gate = make(chan struct{})
	s := NewStateCache(Config{}, f.fetch)
	allWaiting := readersWaiting(t, s, 8)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { _, errs[i] = s.LoadedModels(context.Background()) })
	}
	allWaiting()
	close(f.gate)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if f.calls.Load() != 1 {
		t.Errorf("got %d fetches, want 1", f.calls.Load())
	}
}

// A reader whose request ends stops waiting at once; the fetch, detached from
// any request, carries on for the others.
func TestStateCache_ReaderLeavingDoesNotStopTheFetch(t *testing.T) {
	f := newFetcher(backend.ModelInfo{ID: "m"})
	f.gate = make(chan struct{})
	s := NewStateCache(Config{}, f.fetch)
	bothWaiting := readersWaiting(t, s, 2)
	ctx, cancel := context.WithCancel(context.Background())
	left := make(chan error)
	go func() { _, err := s.LoadedModels(ctx); left <- err }()
	stayed := make(chan []backend.ModelInfo)
	go func() { got, _ := s.LoadedModels(context.Background()); stayed <- got }()
	bothWaiting()
	cancel()
	if err := <-left; !errors.Is(err, context.Canceled) {
		t.Errorf("leaving reader: got %v", err)
	}
	close(f.gate)
	if got := <-stayed; len(got) != 1 {
		t.Errorf("staying reader: got %+v", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ctxErr != nil {
		t.Errorf("the fetch's context ended: %v", f.ctxErr)
	}
}

// A read that may not wait (WithoutStateWait) gets ErrStateBusy before any
// state was fetched, or the last state while a refresh is in flight; with no
// refresh in flight it fetches like any read.
func TestStateCache_ReadThatMayNotWait(t *testing.T) {
	f := newFetcher(backend.ModelInfo{ID: "m"})
	f.gate = make(chan struct{})
	s := NewStateCache(Config{}, f.fetch)
	noWait := backend.WithoutStateWait(context.Background())
	firstWaiting := readersWaiting(t, s, 1)
	first := make(chan error)
	go func() { _, err := s.LoadedModels(context.Background()); first <- err }()
	firstWaiting()
	if _, err := s.LoadedModels(noWait); !errors.Is(err, backend.ErrStateBusy) {
		t.Errorf("before any state: got %v", err)
	}
	close(f.gate)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	f.gate = make(chan struct{})
	refreshing := readersWaiting(t, s, 1)
	refreshed := make(chan struct{})
	go func() { s.LoadedModels(context.Background()); close(refreshed) }()
	refreshing()
	if got, err := s.LoadedModels(noWait); err != nil || len(got) != 1 {
		t.Errorf("during a refresh: got %+v, %v", got, err)
	}
	close(f.gate)
	<-refreshed
	if got, err := s.LoadedModels(noWait); err != nil || len(got) != 1 || f.calls.Load() != 3 {
		t.Errorf("with no refresh under way: got %+v, %v after %d fetches, want 3", got, err, f.calls.Load())
	}
}

// A refresh that began before Invalidate isn't cached, and reads after
// Invalidate don't join it: it may predate what made the state stale.
func TestStateCache_IgnoresRefreshStartedBeforeInvalidate(t *testing.T) {
	stale := make(chan struct{})
	var calls atomic.Int32
	started := make(chan struct{})
	s := NewStateCache(Config{StateCacheSeconds: 60}, func(ctx context.Context) ([]backend.ModelInfo, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-stale
			return []backend.ModelInfo{{ID: "stale"}}, nil
		}
		return []backend.ModelInfo{{ID: "fresh"}}, nil
	})
	first := make(chan []backend.ModelInfo)
	go func() { got, _ := s.LoadedModels(context.Background()); first <- got }()
	<-started
	s.Invalidate()
	if got, _ := s.LoadedModels(context.Background()); len(got) != 1 || got[0].ID != "fresh" {
		t.Errorf("after Invalidate: %+v", got)
	}
	close(stale)
	if got := <-first; len(got) != 1 || got[0].ID != "stale" {
		t.Errorf("the read that started the old refresh: %+v", got)
	}
	if got, _ := s.LoadedModels(context.Background()); len(got) != 1 || got[0].ID != "fresh" || calls.Load() != 2 {
		t.Errorf("cached: %+v after %d fetches", got, calls.Load())
	}
}

// A read reuses a state fetched since its request arrived, even without a
// cache, but not one fetched before, nor one dropped by Invalidate; a read
// with no request start reuses nothing.
func TestStateCache_ReusesStateFetchedDuringRequest(t *testing.T) {
	f := newFetcher(backend.ModelInfo{ID: "m"})
	s := NewStateCache(Config{}, f.fetch)
	request := backend.WithRequestStart(context.Background(), time.Now())
	s.LoadedModels(context.Background()) // fetched after the request arrived
	steps := []struct {
		name  string
		ctx   context.Context
		calls int32
	}{
		{"fetched since the request arrived", request, 1},
		{"fetched before the request arrived", backend.WithRequestStart(context.Background(), time.Now()), 2},
		{"no request start", context.Background(), 3},
	}
	for _, step := range steps {
		if s.LoadedModels(step.ctx); f.calls.Load() != step.calls {
			t.Errorf("%s: %d fetches, want %d", step.name, f.calls.Load(), step.calls)
		}
	}
	request = backend.WithRequestStart(context.Background(), time.Now())
	s.LoadedModels(context.Background())
	s.Invalidate()
	if s.LoadedModels(request); f.calls.Load() != 5 {
		t.Errorf("invalidated: %d fetches, want 5", f.calls.Load())
	}
}

// Adjust changes the backend's own state on every read, before pending
// requests are added, and its error fails the read.
func TestStateCache_AdjustRunsBeforePending(t *testing.T) {
	f := newFetcher(backend.ModelInfo{ID: "own"})
	s := NewStateCache(Config{StateCacheSeconds: 60}, f.fetch)
	release := s.Pending("pending")
	defer release()
	var seen []backend.ModelInfo
	got, err := s.LoadedModelsAdjusted(context.Background(), func(ctx context.Context, models []backend.ModelInfo) ([]backend.ModelInfo, error) {
		seen = models
		return append(models, backend.ModelInfo{ID: "added"}), nil
	})
	if err != nil || len(seen) != 1 || len(got) != 3 || got[1].ID != "added" || got[2].ID != "pending" || !got[2].Starting {
		t.Errorf("adjust saw %+v; got %+v, %v", seen, got, err)
	}
	if _, err := s.LoadedModelsAdjusted(context.Background(), func(ctx context.Context, models []backend.ModelInfo) ([]backend.ModelInfo, error) {
		return nil, errors.New("adjust failed")
	}); err == nil {
		t.Error("adjust's error was dropped")
	}
}
