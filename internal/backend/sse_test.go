package backend

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func readAll(s *SSEStream) ([]string, error) {
	var events []string
	for {
		data, err := s.Next()
		if err != nil {
			return events, err
		}
		events = append(events, string(data))
	}
}

func stream(src string) *SSEStream {
	return NewSSEStream(io.NopCloser(strings.NewReader(src)))
}

func TestSSEStream(t *testing.T) {
	src := ": keepalive\n\ndata: {\"a\":1}\n\nevent: x\r\ndata:{\"b\":2}\r\n\r\ndata: line1\ndata: line2\n\ndata: [DONE]\n\n"
	events, err := readAll(stream(src))
	if err != io.EOF {
		t.Fatalf("got %v", err)
	}
	want := []string{`{"a":1}`, `{"b":2}`, "line1\nline2"}
	if strings.Join(events, "|") != strings.Join(want, "|") {
		t.Errorf("got %q", events)
	}
}

func TestSSEStreamTruncated(t *testing.T) {
	events, err := readAll(stream("data: 1\n\ndata: 2\n\n"))
	if !errors.Is(err, io.ErrUnexpectedEOF) || len(events) != 2 {
		t.Errorf("got %q, %v", events, err)
	}
}

// An event not followed by a blank line is incomplete and is discarded, as
// the SSE spec requires, even when its data is [DONE].
func TestSSEStreamUnterminatedFinalEventIsDiscarded(t *testing.T) {
	for _, src := range []string{"data: 1\n\ndata: [DONE]", "data: 1\n\ndata: [DONE]\n", "data: 1\n\ndata: {\"truncat"} {
		events, err := readAll(stream(src))
		if !errors.Is(err, io.ErrUnexpectedEOF) || len(events) != 1 || events[0] != "1" {
			t.Errorf("%q: got %q, %v", src, events, err)
		}
	}
}

func TestSSEStreamErrorEvent(t *testing.T) {
	s := stream("data: ok\n\nevent: error\ndata: {\"error\":{\"message\":\"boom\"}}\n\n")
	data, err := s.Next()
	if err != nil || string(data) != "ok" {
		t.Fatalf("got %q, %v", data, err)
	}
	_, err = s.Next()
	var streamErr *StreamError
	if !errors.As(err, &streamErr) || string(streamErr.Data) != `{"error":{"message":"boom"}}` {
		t.Errorf("got %v", err)
	}
}

// A bare line that isn't an SSE field (llama-server and some proxies write a
// raw JSON error before closing) is surfaced in the truncation error.
func TestSSEStreamTruncationKeepsLastLineContext(t *testing.T) {
	for _, src := range []string{"data: 1\n\n{\"error\":{\"code\":500}}\n", "data: 1\n\n{\"error\":{\"code\":500}}"} {
		events, err := readAll(stream(src))
		if !errors.Is(err, io.ErrUnexpectedEOF) || len(events) != 1 {
			t.Fatalf("%q: got %q, %v", src, events, err)
		}
		if !strings.Contains(err.Error(), `{"error":{"code":500}}`) {
			t.Errorf("%q: error lacks context: %v", src, err)
		}
	}
}

func TestSSEStreamLineTooLong(t *testing.T) {
	src := "data: " + strings.Repeat("x", maxSSELineBytes+1024) + "\n\ndata: [DONE]\n\n"
	_, err := readAll(stream(src))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("got %v", err)
	}
}

func TestSSEStreamEventTooLarge(t *testing.T) {
	line := "data: " + strings.Repeat("y", maxSSELineBytes-1024) + "\n"
	src := strings.Repeat(line, maxSSEEventBytes/maxSSELineBytes+2) + "\n"
	_, err := readAll(stream(src))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("got %v", err)
	}
}
