package openaihttp

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

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
