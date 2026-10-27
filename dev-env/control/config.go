package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// config is dev-env/config.yaml: the model files the environment downloads
// and what each backend runs. control reads it on every request that needs
// it, so an edit applies without restarting control: a backend's settings
// at its next start, and its models at the next setup or reload where the
// backend allows it.
// Each backend's section is read by its own kind, strictly, and a backend
// without a section isn't configured, so it is listed but does nothing;
// settings a container takes at start reach it through a Compose override
// that control writes from this config for every Compose run (see
// override).
type config struct {
	Files      []modelFile
	configured map[string]configured // backend name -> its configured backend, for those with a section
}

// modelFile is a file the backends' models load, with where to get it and
// what its bytes must hash to.
type modelFile struct {
	Path   string `yaml:"path"` // under the models store, e.g. Qwen/Qwen3-0.6B-GGUF/Qwen3-0.6B-Q8_0.gguf
	SHA256 string `yaml:"sha256"`
	URL    string `yaml:"url"`
}

// fileIndex is the listed files by path, for a kind's checks.
type fileIndex map[string]modelFile

func (f fileIndex) has(path string) bool {
	_, ok := f[path]
	return ok
}

// loadConfig reads and checks the config. Unknown keys are errors, so a
// misspelled one doesn't silently do nothing.
func loadConfig(name string) (*config, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Files    []modelFile          `yaml:"files"`
		Backends map[string]yaml.Node `yaml:"backends"`
	}
	if err := decodeStrict(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	files, problems := checkFiles(raw.Files)
	for section := range raw.Backends {
		if _, ok := findBackend(section); !ok {
			problems = append(problems, fmt.Sprintf("backends has '%s', which isn't a backend of the environment; it has %s", section, joinQuoted(backendNames())))
		}
	}
	c := &config{Files: raw.Files, configured: map[string]configured{}}
	for _, b := range backends {
		node, ok := raw.Backends[b.Name]
		if !ok {
			continue // not configured; see api.withBackend
		}
		configured, err := b.kind.configure(&node, files)
		if err != nil {
			problems = append(problems, fmt.Sprintf("backends.%s: %v", b.Name, err))
			continue
		}
		c.configured[b.Name] = configured
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s: %s", name, strings.Join(problems, "; "))
	}
	return c, nil
}

func checkFiles(listed []modelFile) (fileIndex, []string) {
	var problems []string
	files := fileIndex{}
	for i, f := range listed {
		switch {
		case f.Path == "" || f.URL == "":
			problems = append(problems, fmt.Sprintf("files[%d] needs a path and a url", i))
		case len(f.SHA256) != 64:
			problems = append(problems, fmt.Sprintf("file '%s' needs a 64-character sha256", f.Path))
		case files.has(f.Path):
			problems = append(problems, fmt.Sprintf("file '%s' is listed twice", f.Path))
		}
		files[f.Path] = f
	}
	return files, problems
}

// decodeStrict decodes YAML into out, refusing keys out has no field for.
// An empty document decodes to nothing.
func decodeStrict(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// decodeSection decodes a backend's section of config.yaml strictly.
// yaml.v3 applies KnownFields only to a decoder, not to Node.Decode, so the
// section is encoded again and decoded through one.
func decodeSection(section *yaml.Node, out any) error {
	if section == nil {
		return nil
	}
	data, err := yaml.Marshal(section)
	if err != nil {
		return err
	}
	return decodeStrict(data, out)
}

// problems joins what a kind found wrong with its section into one error,
// or nil if nothing is.
func problems(found []string) error {
	if len(found) == 0 {
		return nil
	}
	return errors.New(strings.Join(found, "; "))
}

// override is the Compose file, merged over compose.yaml on every Compose
// run, that hands the config's settings to the containers that take them
// at start. It names only the services of the backends registered, so it
// never defines a service compose.yaml doesn't have.
func (c *config) override() ([]byte, error) {
	services := map[string]*service{}
	for _, b := range backends {
		configured, ok := c.configured[b.Name]
		if !ok {
			continue
		}
		if s := configured.service(); s != nil {
			services[b.Service] = s
		}
	}
	return yaml.Marshal(map[string]any{"services": services})
}
