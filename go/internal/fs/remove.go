package fs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// RemoveParams is the typed param struct for fs.remove. FilePath is required
// (path is accepted as an alias, like the rest of the family). Recursive must be
// set explicitly to delete a non-empty directory — the guard that keeps a single
// remove from wiping a tree by accident.
//
// Record/Intent are the OPT-IN provenance mode: when Record is set the committed
// deletion is emitted as an ArtifactRemoved event (Intent becomes the event
// rationale). They default off.
type RemoveParams struct {
	FilePath  string `json:"file_path"`
	Recursive bool   `json:"recursive"`

	Record bool   `json:"record,omitempty"`
	Intent string `json:"intent,omitempty"`
}

// UnmarshalJSON accepts `path` as an alias for the canonical `file_path` when
// the latter is absent — the same alias the read/write/edit family carries.
func (p *RemoveParams) UnmarshalJSON(data []byte) error {
	type alias RemoveParams
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = RemoveParams(a)
	if strings.TrimSpace(p.FilePath) == "" {
		var fb struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(data, &fb)
		p.FilePath = fb.Path
	}
	return nil
}

// RemoveResult is the success shape for fs.remove. Event is attached only in
// record mode (omitempty).
type RemoveResult struct {
	FilePath string `json:"file_path"`
	WasDir   bool   `json:"was_dir"` // the removed entry was a directory

	Event *ArtifactEvent `json:"event,omitempty"`
}

// dangerousRemoveTargets is the closed set of absolute paths fs.remove refuses
// outright — the filesystem roots whose recursive deletion is never a legitimate
// owned-surface operation. The check is a backstop, not a sandbox: real
// confinement is the process's filesystem permissions (and, in deployment, the
// container's mount set).
var dangerousRemoveTargets = map[string]struct{}{
	"/":     {},
	"/home": {},
	"/usr":  {},
	"/etc":  {},
	"/var":  {},
	"/bin":  {},
	"/lib":  {},
	"/boot": {},
	"/root": {},
}

// HandleRemove deletes FilePath.
//
// CONTRACT:
//   - The target must exist; otherwise a typed "does not exist" error.
//   - A regular file (or empty directory) is removed via os.Remove.
//   - A NON-EMPTY directory is removed only when Recursive is true (os.RemoveAll);
//     without the explicit flag a typed error is returned and nothing is deleted.
//   - A small set of obviously dangerous absolute targets (filesystem roots like
//     /, /home, /etc) is refused outright — a backstop against a catastrophic
//     recursive remove, not a substitute for the process's filesystem
//     permissions.
//
// fs.remove is destructive: there is no read-state precondition (you remove a
// path, not its content), but it IS rationale-gated for agent actors at the
// dispatch boundary and risk-classified ClassDestructive in the corpos gate.
func HandleRemove(_ context.Context, params json.RawMessage) (RemoveResult, error) {
	var p RemoveParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return RemoveResult{}, fmt.Errorf("fs.remove: invalid params: %w", err)
		}
	}
	if strings.TrimSpace(p.FilePath) == "" {
		return RemoveResult{}, errors.New("fs.remove requires file_path")
	}
	p.FilePath = expandUserPath(p.FilePath)

	cleaned := filepath.Clean(absPath(p.FilePath))
	if _, bad := dangerousRemoveTargets[cleaned]; bad {
		return RemoveResult{}, fmt.Errorf("fs.remove: refusing to remove a protected filesystem root: %s", cleaned)
	}

	info, err := os.Lstat(p.FilePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return RemoveResult{}, fmt.Errorf("fs.remove: path does not exist: %s", p.FilePath)
		}
		if errors.Is(err, fs.ErrPermission) {
			return RemoveResult{}, fmt.Errorf("fs.remove: permission denied: %s", p.FilePath)
		}
		return RemoveResult{}, fmt.Errorf("fs.remove: %w", err)
	}

	isDir := info.IsDir()
	if isDir && !p.Recursive {
		// A non-empty directory needs the explicit recursive flag. An empty
		// directory removes fine via os.Remove, so distinguish the two by
		// attempting the non-recursive remove and surfacing a clear error when
		// it is non-empty.
		if rerr := os.Remove(p.FilePath); rerr != nil {
			if isNotEmpty(rerr) {
				return RemoveResult{}, fmt.Errorf("fs.remove: %s is a non-empty directory; pass recursive=true to delete it and its contents", p.FilePath)
			}
			return RemoveResult{}, fmt.Errorf("fs.remove: %w", rerr)
		}
		return RemoveResult{FilePath: p.FilePath, WasDir: true}, nil
	}

	if isDir {
		if err := os.RemoveAll(p.FilePath); err != nil {
			return RemoveResult{}, fmt.Errorf("fs.remove: %w", err)
		}
		return RemoveResult{FilePath: p.FilePath, WasDir: true}, nil
	}

	if err := os.Remove(p.FilePath); err != nil {
		return RemoveResult{}, fmt.Errorf("fs.remove: %w", err)
	}
	return RemoveResult{FilePath: p.FilePath, WasDir: false}, nil
}

// isNotEmpty reports whether err is the "directory not empty" failure os.Remove
// returns for a populated directory (ENOTEMPTY, or EEXIST on some platforms).
func isNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}
