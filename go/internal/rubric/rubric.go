// Package rubric loads TOML rubric definitions and exposes a registry for
// classify handlers.
package rubric

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

// Example is one worked example grounding a rubric.
type Example struct {
	Text      string `toml:"text"`
	Label     string `toml:"label"`
	Reasoning string `toml:"reasoning"`
}

// RubricDef is one typed rubric loaded from a TOML file.
type RubricDef struct {
	Name           string    `toml:"name"`
	Description    string    `toml:"description"`
	IsDeployed     bool      `toml:"is_deployed"`
	OutputEnum     []string  `toml:"output_enum"`
	Criteria       []string  `toml:"criteria"`
	PromptTemplate string    `toml:"prompt_template"`
	Examples       []Example `toml:"examples"`
}

// Registry holds loaded rubrics and allows hot-reload without restart.
type Registry struct {
	mu      sync.RWMutex
	dir     string
	rubrics map[string]RubricDef
}

// NewRegistry loads all .toml files from dir and returns a Registry.
func NewRegistry(dir string) (*Registry, error) {
	r := &Registry{dir: dir}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

// Get returns the RubricDef for the given name, or (zero, false) if not found.
func (r *Registry) Get(name string) (RubricDef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.rubrics[name]
	return def, ok
}

// Reload re-reads all .toml files from the registry directory without restart.
func (r *Registry) Reload() error {
	return r.load()
}

func (r *Registry) load() error {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return fmt.Errorf("rubric registry: read dir %s: %w", r.dir, err)
	}

	rubrics := make(map[string]RubricDef)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		def, err := loadFile(filepath.Join(r.dir, e.Name()))
		if err != nil {
			return fmt.Errorf("rubric registry: %w", err)
		}
		if def.Name == "" {
			return fmt.Errorf("rubric registry: %s: name field is required", e.Name())
		}
		if len(def.OutputEnum) == 0 {
			return fmt.Errorf("rubric registry: %s: output_enum must not be empty", e.Name())
		}
		rubrics[def.Name] = def
	}

	r.mu.Lock()
	r.rubrics = rubrics
	r.mu.Unlock()
	return nil
}

func loadFile(path string) (RubricDef, error) {
	data, err := fs.ReadFile(os.DirFS(filepath.Dir(path)), filepath.Base(path))
	if err != nil {
		return RubricDef{}, fmt.Errorf("load %s: %w", filepath.Base(path), err)
	}
	var def RubricDef
	if _, err := toml.Decode(string(data), &def); err != nil {
		return RubricDef{}, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return def, nil
}
