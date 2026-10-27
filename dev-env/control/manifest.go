package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// manifestName is the manifest's file in the models store's root. It lives
// with the files it describes, so a MODELS_DIR folder that outlives control's
// volumes, or is shared between machines, keeps its record.
const manifestName = ".control-downloads.json"

// manifest records the files control downloaded into the models store, so a
// prune removes only those: a MODELS_DIR folder can hold files of its owner's
// that control must never delete. A finished file is recorded with its size
// and modification time, and counts as control's only while both still
// match, so a file put in its place since is left alone; a .part file is
// recorded when its download starts and matched by path alone, since it
// grows while control writes it.
type manifest struct {
	store string

	mu      sync.Mutex
	entries map[string]manifestEntry // path under the store -> entry
}

type manifestEntry struct {
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
	Part    bool      `json:"part,omitempty"`
}

// load reads the manifest, or starts an empty one if the store has none.
func (m *manifest) load() error {
	m.entries = map[string]manifestEntry{}
	data, err := os.ReadFile(filepath.Join(m.store, manifestName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &m.entries)
}

// save writes the manifest to a temporary file and renames it into place,
// so an interrupted write leaves the previous one.
func (m *manifest) save() error {
	data, err := json.MarshalIndent(m.entries, "", "  ")
	if err != nil {
		return err
	}
	name := filepath.Join(m.store, manifestName)
	if err := os.WriteFile(name+".tmp", data, 0o644); err != nil {
		return err
	}
	return os.Rename(name+".tmp", name)
}

// recordPart records a .part file whose download is starting.
func (m *manifest) recordPart(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.load(); err != nil {
		return err
	}
	m.entries[path] = manifestEntry{Part: true}
	return m.save()
}

// recordDone replaces a .part file's entry with its finished file's.
func (m *manifest) recordDone(part, path string) error {
	info, err := os.Stat(filepath.Join(m.store, path))
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.load(); err != nil {
		return err
	}
	delete(m.entries, part)
	m.entries[path] = manifestEntry{Size: info.Size(), ModTime: info.ModTime().UTC()}
	return m.save()
}

// forget drops paths from the manifest.
func (m *manifest) forget(paths ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.load(); err != nil {
		return err
	}
	for _, p := range paths {
		delete(m.entries, p)
	}
	return m.save()
}

// managed reports whether control downloaded the file at path and it is
// still the file control wrote.
func (m *manifest) managed(path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.load(); err != nil {
		return false
	}
	e, ok := m.entries[path]
	if !ok {
		return false
	}
	if e.Part {
		return true
	}
	info, err := os.Stat(filepath.Join(m.store, path))
	return err == nil && info.Size() == e.Size && info.ModTime().UTC().Equal(e.ModTime)
}
