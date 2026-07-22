package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LSParams is the typed param struct for fs.ls. Path defaults to the working
// directory; All includes dotfiles. (fs.ls is self-defined — the harness has no
// LS tool; see testdata/LS_CONTRACT.md.)
type LSParams struct {
	Path string `json:"path,omitempty"`
	All  *bool  `json:"all,omitempty"`
}

// UnmarshalJSON accepts `file_path` as an alias for the canonical `path` when
// the latter is absent. ls/glob/grep canonically take `path`; read/write/edit
// take `file_path` — a caller that guesses the other family's spelling still
// succeeds. Bug fs-surface-param-name-inconsistency-read-filepath-vs-ls-path.
func (p *LSParams) UnmarshalJSON(data []byte) error {
	type alias LSParams
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = LSParams(a)
	if strings.TrimSpace(p.Path) == "" {
		var fb struct {
			FilePath string `json:"file_path"`
		}
		_ = json.Unmarshal(data, &fb)
		p.Path = fb.FilePath
	}
	return nil
}

// LSEntry is one listed directory entry.
type LSEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // dir | file | symlink
	Size int64  `json:"size"` // bytes; 0 for directories
}

// LSResult is the success shape for fs.ls.
type LSResult struct {
	Path    string    `json:"path"`
	Entries []LSEntry `json:"entries"`
	Count   int       `json:"count"`
}

// HandleLS lists the immediate entries of a directory, sorted by name, with
// dotfiles gated by `all`.
func HandleLS(_ context.Context, params json.RawMessage) (LSResult, error) {
	var p LSParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return LSResult{}, fmt.Errorf("fs.ls: invalid params: %w", err)
		}
	}
	dir := strings.TrimSpace(p.Path)
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return LSResult{}, fmt.Errorf("fs.ls: resolve working directory: %w", err)
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return LSResult{}, fmt.Errorf("fs.ls: absolutize %q: %w", dir, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return LSResult{}, fmt.Errorf("fs.ls: path does not exist: %s", dir)
	}
	if !fi.IsDir() {
		return LSResult{}, fmt.Errorf("fs.ls: %s is not a directory", dir)
	}

	read, err := os.ReadDir(abs) // sorted by filename
	if err != nil {
		return LSResult{}, fmt.Errorf("fs.ls: %w", err)
	}

	all := p.All != nil && *p.All
	entries := make([]LSEntry, 0, len(read))
	for _, de := range read {
		name := de.Name()
		if !all && strings.HasPrefix(name, ".") {
			continue
		}
		entries = append(entries, lsEntryFor(name, de))
	}
	return LSResult{Path: abs, Entries: entries, Count: len(entries)}, nil
}

func lsEntryFor(name string, de fs.DirEntry) LSEntry {
	e := LSEntry{Name: name, Type: "file"}
	switch {
	case de.Type()&fs.ModeSymlink != 0:
		e.Type = "symlink"
		if info, err := de.Info(); err == nil {
			e.Size = info.Size()
		}
	case de.IsDir():
		e.Type = "dir"
	default:
		if info, err := de.Info(); err == nil {
			e.Size = info.Size()
		}
	}
	return e
}
