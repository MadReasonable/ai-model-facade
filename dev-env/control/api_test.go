package main

import (
	"errors"
	"testing"
)

// A backend's state comes from its start job, its ready check and its
// container, in that order: a start under way is starting; a ready backend
// is ready however it was started, even after an earlier start failed; a
// failed start that left it unready is failed; a running container that
// isn't ready yet is starting; anything else is stopped. Detail says what a
// start is doing or why it failed.
func TestBackendState(t *testing.T) {
	failed := errors.New("backend 'b' wasn't ready within 5m0s of starting")
	cases := []struct {
		name     string
		starting bool
		failed   error
		running  bool
		waiting  string
		state    string
		detail   string
	}{
		{"a start under way", true, nil, true, "", stateStarting, "starting its container"},
		{"ready", false, nil, true, "", stateReady, ""},
		{"ready after a failed start", false, failed, true, "", stateReady, ""},
		{"failed and not ready", false, failed, true, "waiting for it to answer /health", stateFailed, failed.Error()},
		{"failed and stopped", false, failed, false, "", stateFailed, failed.Error()},
		{"started by hand, not ready yet", false, nil, true, "waiting for it to answer /health", stateStarting, "waiting for it to answer /health"},
		{"stopped", false, nil, false, "", stateStopped, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state, detail := backendState(c.starting, "starting its container", c.failed, c.running, c.waiting)
			if state != c.state || detail != c.detail {
				t.Errorf("got %q, %q; want %q, %q", state, detail, c.state, c.detail)
			}
		})
	}
}
