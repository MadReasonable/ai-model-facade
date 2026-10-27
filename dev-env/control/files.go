package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
)

// files keeps the models store filled with the files the config lists. A
// file is downloaded under a .part name and given its final name only once
// its sha256 matches, so a file that exists under its final name is complete,
// and only its existence is checked afterwards. What it downloads is recorded
// in the manifest, so a prune removes only those files.
type files struct {
	config   func() (*config, error)
	store    string // the models store's mount point
	jobs     *jobs
	manifest *manifest
}

func (f *files) listed() ([]modelFile, error) {
	c, err := f.config()
	if err != nil {
		return nil, err
	}
	return c.Files, nil
}

// fileStatus is one file as GET /api/files reports it: a listed one, or one
// in the store the config doesn't list. Managed says whether control
// downloaded it and it is unchanged since, false for a file that doesn't
// exist; a prune removes the files that are unlisted and managed. Detail says
// why a download failed, for people.
type fileStatus struct {
	Path     string    `json:"path"`
	State    string    `json:"state"` // done, pending (missing), running (downloading), failed or unlisted
	Detail   string    `json:"detail"`
	Managed  bool      `json:"managed"`
	Progress *byteDone `json:"progress,omitempty"`
}

const stateUnlisted = "unlisted"

type byteDone struct {
	Done  int64 `json:"done"`
	Total int64 `json:"total"`
}

const filesJob = "files"

// status reports every listed file, then every file in the store the
// config doesn't list. A file counts as downloading while the files job runs
// and the file isn't done yet; the job downloads one file at a time, so the
// job's progress belongs to the file being written.
func (f *files) status() ([]fileStatus, error) {
	listed, err := f.listed()
	if err != nil {
		return nil, err
	}
	run := f.jobs.last(filesJob)
	var statuses []fileStatus
	var writing bool
	known := map[string]bool{manifestName: true, manifestName + ".tmp": true}
	for _, m := range listed {
		known[m.Path], known[m.Path+".part"] = true, true
		s := fileStatus{Path: m.Path, State: statePending}
		switch {
		case exists(filepath.Join(f.store, m.Path)):
			s.State, s.Managed = stateDone, f.managed(m.Path)
		case run != nil && !run.finished() && !writing:
			writing = true
			s.State, s.Managed = stateRunning, f.managed(m.Path+".part")
			s.Progress = &byteDone{Done: run.progress.done.Load(), Total: run.progress.total.Load()}
		case run != nil && run.finished() && run.err != nil:
			s.State, s.Detail = stateFailed, run.err.Error()
		}
		statuses = append(statuses, s)
	}
	unlisted, err := f.unlisted(known)
	if err != nil {
		return nil, err
	}
	for _, path := range unlisted {
		statuses = append(statuses, fileStatus{Path: path, State: stateUnlisted, Managed: f.managed(path)})
	}
	return statuses, nil
}

func (f *files) managed(path string) bool {
	return f.manifest.managed(path)
}

// unlisted returns the regular files in the store that aren't in known, in
// lexical order.
func (f *files) unlisted(known map[string]bool) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(f.store, func(name string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		path, err := filepath.Rel(f.store, name)
		if err != nil {
			return err
		}
		if path = filepath.ToSlash(path); !known[path] {
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
}

// prune removes the files the config doesn't list that control downloaded
// and that are unchanged since, .part files included, and the folders that
// leaves empty, and returns the paths it removed. The API refuses it while a
// download runs or a backend reading the files does.
func (f *files) prune() ([]string, error) {
	statuses, err := f.status()
	if err != nil {
		return nil, err
	}
	removed := []string{}
	for _, s := range statuses {
		if s.State != stateUnlisted || !s.Managed {
			continue
		}
		name := filepath.Join(f.store, s.Path)
		if err := os.Remove(name); err != nil {
			return removed, err
		}
		removed = append(removed, s.Path)
		// os.Remove fails on a folder that isn't empty, which ends the climb.
		for dir := filepath.Dir(name); dir != f.store; dir = filepath.Dir(dir) {
			if os.Remove(dir) != nil {
				break
			}
		}
	}
	return removed, f.manifest.forget(removed...)
}

// ready reports whether every listed file is in the store.
func (f *files) ready() (bool, error) {
	statuses, err := f.status()
	if err != nil {
		return false, err
	}
	for _, s := range statuses {
		if s.State != stateDone && s.State != stateUnlisted {
			return false, nil
		}
	}
	return true, nil
}

// download starts, or joins, the job that downloads every missing file.
func (f *files) download() *job {
	return f.jobs.start(filesJob, func(ctx context.Context, p *progress) error {
		listed, err := f.listed()
		if err != nil {
			return err
		}
		for _, m := range listed {
			if exists(filepath.Join(f.store, m.Path)) {
				continue
			}
			if err := f.fetch(ctx, m, p); err != nil {
				return fmt.Errorf("file '%s': %w", m.Path, err)
			}
		}
		return nil
	})
}

// fetch downloads one file, resuming a .part file left by an earlier run,
// and moves it into place once its sha256 matches.
func (f *files) fetch(ctx context.Context, m modelFile, p *progress) error {
	final := filepath.Join(f.store, m.Path)
	part := final + ".part"
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return err
	}
	if err := f.manifest.recordPart(m.Path + ".part"); err != nil {
		return err
	}
	out, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	have, err := out.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
	if err != nil {
		return err
	}
	if have > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusPartialContent:
	case resp.StatusCode == http.StatusOK:
		// The server ignored the range, so the download starts over.
		if err := out.Truncate(0); err != nil {
			return err
		}
		have = 0
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		// The .part file already holds every byte; the hash decides.
	default:
		return fmt.Errorf("GET '%s': %s", m.URL, resp.Status)
	}
	p.done.Store(have)
	p.total.Store(have + max(resp.ContentLength, 0))
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		if _, err := io.Copy(out, &counting{r: resp.Body, n: &p.done}); err != nil {
			return err
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := checkSHA256(part, m.SHA256); err != nil {
		// A .part file with the wrong bytes can't be resumed into the
		// right ones, so the next run starts from nothing.
		os.Remove(part)
		f.manifest.forget(m.Path + ".part")
		return err
	}
	if err := os.Rename(part, final); err != nil {
		return err
	}
	return f.manifest.recordDone(m.Path+".part", m.Path)
}

func checkSHA256(name, want string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("sha256 is %s, config.yaml says %s", got, want)
	}
	return nil
}

// counting adds the bytes read through it to n.
type counting struct {
	r io.Reader
	n *atomic.Int64
}

func (c *counting) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n.Add(int64(n))
	return n, err
}

func exists(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}
