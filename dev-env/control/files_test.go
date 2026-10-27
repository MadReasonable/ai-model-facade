package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fileTime = time.Unix(0, 0) // for http.ServeContent, which needs a modification time

// filesFor returns a files that downloads these into store, with the
// config holding nothing else.
func filesFor(store string, listed ...modelFile) *files {
	c := &config{Files: listed}
	return &files{config: func() (*config, error) { return c, nil }, store: store, jobs: newJobs(context.Background()), manifest: &manifest{store: store}}
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// A download resumes a .part file left by an earlier run, asking only for
// the bytes it lacks, and the file gets its final name once its sha256
// matches.
func TestFiles_DownloadResumesAndChecks(t *testing.T) {
	const content = "0123456789"
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		http.ServeContent(w, r, "f", fileTime, strings.NewReader(content))
	}))
	defer srv.Close()
	store := t.TempDir()
	os.MkdirAll(filepath.Join(store, "a"), 0o755)
	os.WriteFile(filepath.Join(store, "a", "f.part"), []byte(content[:4]), 0o644)
	f := filesFor(store, modelFile{Path: "a/f", SHA256: hash(content), URL: srv.URL + "/f"})
	if err := f.download().wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(store, "a", "f"))
	if string(got) != content || len(ranges) != 1 || ranges[0] != "bytes=4-" || exists(filepath.Join(store, "a", "f.part")) {
		t.Errorf("file %q after ranges %q", got, ranges)
	}
	if ready, _ := f.ready(); !ready {
		t.Error("not ready after the download")
	}
}

// A file whose bytes don't match its sha256 fails the step and leaves
// nothing behind, so the next run starts from the first byte.
func TestFiles_WrongHashFailsAndRemovesThePart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("wrong bytes"))
	}))
	defer srv.Close()
	store := t.TempDir()
	f := filesFor(store, modelFile{Path: "f", SHA256: hash("right bytes"), URL: srv.URL})
	err := f.download().wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sha256") || exists(filepath.Join(store, "f")) || exists(filepath.Join(store, "f.part")) {
		t.Fatalf("got %v", err)
	}
	statuses, _ := f.status()
	if len(statuses) != 1 || statuses[0].State != stateFailed {
		t.Errorf("status %+v", statuses)
	}
}

// serveContent serves content at every path, for a file to download.
func serveContent(t *testing.T, content string) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f", fileTime, strings.NewReader(content))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// byPath finds a file's status.
func byPath(statuses []fileStatus, path string) (fileStatus, bool) {
	for _, s := range statuses {
		if s.Path == path {
			return s, true
		}
	}
	return fileStatus{}, false
}

// A file control downloaded is managed, and a file put in the store by hand
// isn't. Once the config no longer lists the download, it is unlisted, and
// a prune removes it and the folder it leaves empty, but never the file
// control didn't make.
func TestFiles_PruneRemovesOnlyWhatItDownloaded(t *testing.T) {
	store := t.TempDir()
	listed := modelFile{Path: "a/f", SHA256: hash("bytes"), URL: serveContent(t, "bytes")}
	if err := filesFor(store, listed).download().wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(store, "mine"), 0o755)
	os.WriteFile(filepath.Join(store, "mine", "x.gguf"), []byte("own"), 0o644)
	statuses, _ := filesFor(store, listed).status()
	if s, _ := byPath(statuses, "a/f"); s.State != stateDone || !s.Managed {
		t.Errorf("listed download: %+v", s)
	}
	if s, _ := byPath(statuses, "mine/x.gguf"); s.State != stateUnlisted || s.Managed {
		t.Errorf("own file: %+v", s)
	}
	if _, ok := byPath(statuses, manifestName); ok {
		t.Error("the manifest is listed as a file")
	}
	unlisted := filesFor(store)
	removed, err := unlisted.prune()
	if err != nil || len(removed) != 1 || removed[0] != "a/f" {
		t.Fatalf("removed %q, %v", removed, err)
	}
	if exists(filepath.Join(store, "a")) || !exists(filepath.Join(store, "mine", "x.gguf")) {
		t.Error("the emptied folder stayed, or the own file went")
	}
	if unlisted.manifest.managed("a/f") {
		t.Error("the manifest still has the pruned file")
	}
}

// A managed file replaced since control wrote it, and a .part file control
// didn't start, are no longer control's, so a prune keeps them; a .part
// left by control's own interrupted download goes.
func TestFiles_PruneKeepsWhatChanged(t *testing.T) {
	store := t.TempDir()
	listed := modelFile{Path: "f", SHA256: hash("bytes"), URL: serveContent(t, "bytes")}
	f := filesFor(store, listed)
	if err := f.download().wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(store, "f"), []byte("other bytes"), 0o644)
	f.manifest.recordPart("g.part")
	os.WriteFile(filepath.Join(store, "g.part"), []byte("half"), 0o644)
	os.WriteFile(filepath.Join(store, "h.part"), []byte("half"), 0o644)
	removed, err := filesFor(store).prune()
	if err != nil || len(removed) != 1 || removed[0] != "g.part" {
		t.Errorf("removed %q, %v", removed, err)
	}
	if !exists(filepath.Join(store, "f")) || !exists(filepath.Join(store, "h.part")) {
		t.Error("removed a file that wasn't control's")
	}
}

// A file the config doesn't list doesn't keep the listed ones from being
// ready.
func TestFiles_ReadyIgnoresUnlistedFiles(t *testing.T) {
	store := t.TempDir()
	listed := modelFile{Path: "f", SHA256: hash("bytes"), URL: serveContent(t, "bytes")}
	f := filesFor(store, listed)
	if err := f.download().wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(store, "stray"), []byte("x"), 0o644)
	if ready, err := f.ready(); !ready || err != nil {
		t.Errorf("ready %v, %v", ready, err)
	}
}
