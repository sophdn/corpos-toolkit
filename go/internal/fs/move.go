package fs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// MoveParams is the typed param struct for fs.move. Source and Dest are
// required. Source/Dest accept the common aliases src/from and destination/to
// (see UnmarshalJSON) so the action is forgiving of the obvious synonyms,
// mirroring the file_path/path alias the read/write/edit family carries.
//
// Record/Intent are the OPT-IN provenance mode: when Record is set the committed
// move is emitted as an ArtifactMoved event (Intent becomes the event
// rationale). They default off, leaving the plain rename untouched.
type MoveParams struct {
	Source string `json:"source"`
	Dest   string `json:"dest"`

	Record bool   `json:"record,omitempty"`
	Intent string `json:"intent,omitempty"`
}

// UnmarshalJSON accepts src/from as aliases for source and destination/to as
// aliases for dest when the canonical key is absent — the move analogue of the
// file_path/path alias on ReadParams/WriteParams.
func (p *MoveParams) UnmarshalJSON(data []byte) error {
	type alias MoveParams
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = MoveParams(a)
	if strings.TrimSpace(p.Source) == "" {
		var fb struct {
			Src  string `json:"src"`
			From string `json:"from"`
		}
		_ = json.Unmarshal(data, &fb)
		if fb.Src != "" {
			p.Source = fb.Src
		} else {
			p.Source = fb.From
		}
	}
	if strings.TrimSpace(p.Dest) == "" {
		var fb struct {
			Destination string `json:"destination"`
			To          string `json:"to"`
		}
		_ = json.Unmarshal(data, &fb)
		if fb.Destination != "" {
			p.Dest = fb.Destination
		} else {
			p.Dest = fb.To
		}
	}
	return nil
}

// MoveResult is the success shape for fs.move. Event is attached only in record
// mode (omitempty), so a default move marshals without it.
type MoveResult struct {
	Source      string `json:"source"`
	Dest        string `json:"dest"`         // the FINAL destination path (dir-into resolved)
	IsDir       bool   `json:"is_dir"`       // the moved entry was a directory
	CrossDevice bool   `json:"cross_device"` // os.Rename failed EXDEV; copy-then-remove fallback ran

	Event *ArtifactEvent `json:"event,omitempty"`
}

// HandleMove renames or relocates Source to Dest.
//
// CONTRACT:
//   - Source must exist (file or directory); otherwise a typed "does not exist"
//     error.
//   - When Dest is an existing directory the entry is moved INTO it (final path
//     = Dest/basename(Source)), matching `mv` semantics; otherwise Dest is the
//     literal target path and its missing parent directories are created.
//   - The final destination must NOT already exist — fs.move refuses to clobber
//     (move is mutating, not destructive; an overwrite would silently destroy
//     the target). Remove the target first (fs.remove) if a replace is intended.
//   - The rename is in-process via os.Rename (no shell). When source and dest
//     live on different filesystems os.Rename fails EXDEV; fs.move then falls
//     back to a recursive copy-then-remove so a cross-device move still works in
//     the distroless container. CrossDevice reports which path ran.
//
// Unlike fs.write/fs.edit, fs.move is NOT coupled to the read-state registry:
// it relocates bytes without inspecting or rewriting content, so a prior read is
// not a precondition.
func HandleMove(_ context.Context, params json.RawMessage) (MoveResult, error) {
	var p MoveParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return MoveResult{}, fmt.Errorf("fs.move: invalid params: %w", err)
		}
	}
	if strings.TrimSpace(p.Source) == "" {
		return MoveResult{}, errors.New("fs.move requires source")
	}
	if strings.TrimSpace(p.Dest) == "" {
		return MoveResult{}, errors.New("fs.move requires dest")
	}
	p.Source = expandUserPath(p.Source)
	p.Dest = expandUserPath(p.Dest)

	srcInfo, err := os.Stat(p.Source)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return MoveResult{}, fmt.Errorf("fs.move: source does not exist: %s", p.Source)
		}
		if errors.Is(err, fs.ErrPermission) {
			return MoveResult{}, fmt.Errorf("fs.move: permission denied: %s", p.Source)
		}
		return MoveResult{}, fmt.Errorf("fs.move: %w", err)
	}

	// Resolve the final destination: moving onto an existing directory places the
	// entry inside it (mv semantics); otherwise dest is the literal target.
	finalDest := p.Dest
	if di, derr := os.Stat(p.Dest); derr == nil && di.IsDir() {
		finalDest = filepath.Join(p.Dest, filepath.Base(p.Source))
	}

	// Refuse to clobber an existing final destination — move stays mutating, not
	// destructive.
	if _, derr := os.Stat(finalDest); derr == nil {
		return MoveResult{}, fmt.Errorf("fs.move: destination already exists: %s (remove it first if a replace is intended)", finalDest)
	} else if !errors.Is(derr, fs.ErrNotExist) {
		if errors.Is(derr, fs.ErrPermission) {
			return MoveResult{}, fmt.Errorf("fs.move: permission denied: %s", finalDest)
		}
		return MoveResult{}, fmt.Errorf("fs.move: %w", derr)
	}

	if dir := filepath.Dir(finalDest); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return MoveResult{}, fmt.Errorf("fs.move: create destination parent: %w", err)
		}
	}

	crossDevice := false
	if err := os.Rename(p.Source, finalDest); err != nil {
		if !isCrossDevice(err) {
			return MoveResult{}, fmt.Errorf("fs.move: %w", err)
		}
		// Cross-filesystem move: os.Rename can't span devices, so copy then
		// remove the source. Pure Go — no shell, works in the container.
		crossDevice = true
		if cerr := copyTree(p.Source, finalDest, srcInfo); cerr != nil {
			return MoveResult{}, fmt.Errorf("fs.move: cross-device copy: %w", cerr)
		}
		if rerr := os.RemoveAll(p.Source); rerr != nil {
			return MoveResult{}, fmt.Errorf("fs.move: cross-device remove source after copy: %w", rerr)
		}
	}

	return MoveResult{
		Source:      p.Source,
		Dest:        finalDest,
		IsDir:       srcInfo.IsDir(),
		CrossDevice: crossDevice,
	}, nil
}

// isCrossDevice reports whether err is the EXDEV ("cross-device link") failure
// os.Rename returns when source and dest live on different filesystems.
func isCrossDevice(err error) bool {
	return errors.Is(err, syscall.EXDEV)
}

// copyTree recursively copies src to dst, preserving file modes. info is src's
// already-fetched os.FileInfo. Used only by the cross-device fallback of
// fs.move; the same-filesystem path stays a single os.Rename.
func copyTree(src, dst string, info os.FileInfo) error {
	if info.IsDir() {
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			ei, err := e.Info()
			if err != nil {
				return err
			}
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), ei); err != nil {
				return err
			}
		}
		return nil
	}
	return copyFile(src, dst, info.Mode().Perm())
}

// copyFile copies a single regular file from src to dst with the given mode.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
