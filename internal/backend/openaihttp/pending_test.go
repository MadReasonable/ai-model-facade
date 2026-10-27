package openaihttp

import (
	"context"
	"sync"
	"testing"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// A pending model shows as starting unless the backend reports it ready or
// starting; one the backend can't report becomes starting, and a pending
// alias counts as its base. Every pending model is marked Pending.
func TestPending_ShowsAsStartingWhereTheBackendIsSilent(t *testing.T) {
	f := newFetcher(
		backend.ModelInfo{ID: "ready", Aliases: []string{"ready:alias"}},
		backend.ModelInfo{ID: "starting", Starting: true},
		backend.ModelInfo{ID: "unknown", Unknown: true},
	)
	s := NewStateCache(Config{StateCacheSeconds: 60}, f.fetch)
	for _, id := range []string{"ready", "ready:alias", "starting", "unknown", "unlisted", "unlisted"} {
		defer s.Pending(id)()
	}
	got, _ := s.LoadedModels(context.Background())
	want := []backend.ModelInfo{{ID: "ready", Pending: true}, {ID: "starting", Starting: true, Pending: true}, {ID: "unknown", Starting: true, Pending: true}, {ID: "unlisted", Starting: true, Pending: true}}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Starting != want[i].Starting || got[i].Unknown != want[i].Unknown || got[i].Pending != want[i].Pending {
			t.Errorf("model %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Releasing a model the cached state listed as ready keeps the cache;
// releasing any other drops it, since a load has just finished or failed and
// may have evicted other models.
func TestPending_ReleaseDropsTheCacheUnlessTheModelWasReady(t *testing.T) {
	cases := []struct {
		model string
		calls int32
	}{
		{"ready", 1},
		{"ready:alias", 1},
		{"starting", 2},
		{"unlisted", 2},
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			f := newFetcher(backend.ModelInfo{ID: "ready", Aliases: []string{"ready:alias"}}, backend.ModelInfo{ID: "starting", Starting: true})
			s := NewStateCache(Config{StateCacheSeconds: 60}, f.fetch)
			s.LoadedModels(context.Background())
			s.Pending(c.model)()
			if s.LoadedModels(context.Background()); f.calls.Load() != c.calls {
				t.Errorf("%d fetches, want %d", f.calls.Load(), c.calls)
			}
		})
	}
}

// Each request is released once, however often its release is called.
func TestPending_SecondReleaseDoesNothing(t *testing.T) {
	s := NewStateCache(Config{StateCacheSeconds: 60}, newFetcher().fetch)
	release := s.Pending("m")
	defer s.Pending("m")()
	release()
	release()
	if got, _ := s.LoadedModels(context.Background()); len(got) != 1 || !got[0].Starting {
		t.Errorf("with one of two requests left: %+v", got)
	}
}

// A request that starts while a read is fetching still shows in that read.
func TestPending_RequestStartedDuringAFetchShows(t *testing.T) {
	f := newFetcher()
	f.gate = make(chan struct{})
	s := NewStateCache(Config{}, f.fetch)
	result := make(chan []backend.ModelInfo)
	go func() { got, _ := s.LoadedModels(context.Background()); result <- got }()
	<-f.started
	defer s.Pending("late")()
	close(f.gate)
	if got := <-result; len(got) != 1 || got[0].ID != "late" || !got[0].Starting {
		t.Errorf("got %+v", got)
	}
}

// Many requests counting and releasing at once leave nothing pending.
func TestPending_ConcurrentRequestsBalance(t *testing.T) {
	s := NewStateCache(Config{StateCacheSeconds: 60}, newFetcher().fetch)
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			release := s.Pending("m")
			s.LoadedModels(context.Background())
			release()
		})
	}
	wg.Wait()
	if got, _ := s.LoadedModels(context.Background()); len(got) != 0 {
		t.Errorf("still pending: %+v", got)
	}
}

// Without a state cache, counting does nothing, for an adapter whose client
// doesn't track pending requests.
func TestPending_NilCacheDoesNothing(t *testing.T) {
	var s *StateCache
	s.Pending("m")()
}
