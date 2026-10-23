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
