package fs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ReadRegistry is the owned read-state that makes the read/write/edit family
// self-consistent without leaning on any harness-internal state: fs.read records
// a full read of a path, and fs.write / fs.edit require that an EXISTING file was
// fully read — and not modified since — before it may be overwritten or edited.
// A new file (one that does not yet exist) needs no prior read.
//
// State is keyed by absolute path and is process-global (this is a single-user
// agent OS). The modified-since-read check is the backstop against a path read
// in one session being written from another after it changed on disk.
type ReadRegistry struct {
	mu sync.Mutex
	m  map[string]readMark
}

type readMark struct {
	mtimeMs int64 // file mtime (ms) observed at read time
	full    bool  // whole-file read (a ranged read is a partial view)
}

// NewReadRegistry returns an empty registry.
func NewReadRegistry() *ReadRegistry {
	return &ReadRegistry{m: make(map[string]readMark)}
}

func absKey(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// MarkRead records that path was observed at file mtime mtimeMs. full is false
// for a ranged/partial read (which does not satisfy the write/edit precondition).
func (r *ReadRegistry) MarkRead(path string, mtimeMs int64, full bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.m[absKey(path)] = readMark{mtimeMs: mtimeMs, full: full}
	r.mu.Unlock()
}

// checkWritable reports whether an existing path may be overwritten/edited given
// its current mtime, returning a caller-facing reason when not. A nil registry
// imposes no guard.
func (r *ReadRegistry) checkWritable(path string, currentMtimeMs int64) (bool, string) {
	if r == nil {
		return true, ""
	}
	r.mu.Lock()
	mark, ok := r.m[absKey(path)]
	r.mu.Unlock()
	if !ok || !mark.full {
		return false, "File has not been read yet. Read it first before writing to it."
	}
	if currentMtimeMs > mark.mtimeMs {
		return false, "File has been modified since read, either by the user or by a linter. Read it again before attempting to write it."
	}
	return true, ""
}

// noteRead records a successful fs.read into the registry (called by the read
// action). A ranged read (offset past line 1, or an explicit limit) is a partial
// view and does not satisfy the write/edit precondition.
func noteRead(reg *ReadRegistry, params json.RawMessage) {
	if reg == nil {
		return
	}
	var p ReadParams
	if json.Unmarshal(params, &p) != nil || strings.TrimSpace(p.FilePath) == "" {
		return
	}
	p.FilePath = expandUserPath(p.FilePath)
	info, err := os.Stat(p.FilePath)
	if err != nil || info.IsDir() {
		return
	}
	full := p.Offset <= 1 && p.Limit <= 0
	reg.MarkRead(p.FilePath, info.ModTime().UnixMilli(), full)
}
