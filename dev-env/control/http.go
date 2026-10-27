package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The HTTP calls control makes to backends: JSON in and out, a status other
// than 2xx as a statusError, and a 404 or a "not found" as errNoModel.

var client = &http.Client{Timeout: 10 * time.Minute} // a load can take minutes on a slow machine

// statusError is a backend's answer with a status other than 2xx.
type statusError struct {
	url  string
	code int
	body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("'%s' answered %d: %s", e.url, e.code, e.body)
}

func getJSON(ctx context.Context, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	return do(req, out)
}

func postJSON(ctx context.Context, target string, body any) error {
	return postJSONOut(ctx, target, body, nil)
}

// postJSONOut is postJSON that decodes the answer into out, if out isn't nil.
func postJSONOut(ctx context.Context, target string, body, out any) error {
	return sendJSON(ctx, http.MethodPost, target, body, out)
}

// sendJSON sends body as JSON with method, and decodes the answer into out,
// if out isn't nil.
func sendJSON(ctx context.Context, method, target string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return do(req, out)
}

// authJSON sends a request with a bearer token and decodes the answer into
// out.
func authJSON(ctx context.Context, method, target, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, target, strings.NewReader(""))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return do(req, out)
}

// do sends req and decodes a 2xx JSON answer into out, if out isn't nil. A
// 404, or an answer saying "not found", which is how backends refuse a model
// they don't have, is errNoModel.
func do(req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		text := strings.TrimSpace(string(body))
		if resp.StatusCode == http.StatusNotFound || strings.Contains(text, "not found") {
			return fmt.Errorf("%w: %s", errNoModel, text)
		}
		return &statusError{url: req.URL.String(), code: resp.StatusCode, body: text}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}
