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
)

// WriteParams is the typed param struct for fs.write. FilePath and Content are
// required (Content may be the empty string — that writes an empty file).
//
// Record/Intent are the OPT-IN provenance mode: when Record is set the committed
// write is emitted as an ArtifactWritten event (Intent becomes the event
// rationale). They default off, leaving the byte-parity write untouched.
type WriteParams struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`

	Record bool   `json:"record,omitempty"`
	Intent string `json:"intent,omitempty"`
}

// UnmarshalJSON accepts `path` as an alias for the canonical `file_path` when
// the latter is absent — see ReadParams.UnmarshalJSON. Bug
// fs-surface-param-name-inconsistency-read-filepath-vs-ls-path.
func (p *WriteParams) UnmarshalJSON(data []byte) error {
	type alias WriteParams
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = WriteParams(a)
	if strings.TrimSpace(p.FilePath) == "" {
		var fb struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(data, &fb)
		p.FilePath = fb.Path
	}
	return nil
}

// WriteResult is the success shape for fs.write. Event is attached only in
// record mode (omitempty), so a default write marshals to the historical shape.
type WriteResult struct {
	FilePath     string `json:"file_path"`
	Created      bool   `json:"created"` // the file did not exist before the write
	BytesWritten int    `json:"bytes_written"`
	LineCount    int    `json:"line_count"`

	Event *ArtifactEvent `json:"event,omitempty"`
}

// HandleWrite writes Content to FilePath, replacing the whole file.
//
// CONTRACT:
//   - Content is written verbatim (UTF-8); parent directories are created.
//   - Overwriting an EXISTING file requires that it was fully read first and has
//     not changed since (the read/write/edit family precondition via
//     ReadRegistry) — otherwise a typed "read it first" / "modified since read"
//     error. Creating a NEW file needs no prior read.
//   - After a successful write the new state is recorded as read, so an
//     immediate follow-up write/edit does not require a re-read.
func HandleWrite(_ context.Context, params json.RawMessage, reg *ReadRegistry) (WriteResult, error) {
	var p WriteParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return WriteResult{}, fmt.Errorf("fs.write: invalid params: %w", err)
		}
	}
	if strings.TrimSpace(p.FilePath) == "" {
		return WriteResult{}, errors.New("fs.write requires file_path")
	}
	p.FilePath = expandUserPath(p.FilePath)

	info, statErr := os.Stat(p.FilePath)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		if errors.Is(statErr, fs.ErrPermission) {
			return WriteResult{}, fmt.Errorf("fs.write: permission denied: %s", p.FilePath)
		}
		return WriteResult{}, fmt.Errorf("fs.write: %w", statErr)
	}
	if exists && info.IsDir() {
		return WriteResult{}, fmt.Errorf("fs.write: %s is a directory, not a file", p.FilePath)
	}
	if exists {
		if ok, reason := reg.checkWritable(p.FilePath, info.ModTime().UnixMilli()); !ok {
			return WriteResult{}, fmt.Errorf("fs.write: %s", reason)
		}
	}

	if err := writeFileMkdir(p.FilePath, p.Content); err != nil {
		return WriteResult{}, fmt.Errorf("fs.write: %w", err)
	}
	markWritten(reg, p.FilePath)

	return WriteResult{
		FilePath:     p.FilePath,
		Created:      !exists,
		BytesWritten: len(p.Content),
		LineCount:    lineCount(p.Content),
	}, nil
}

// writeFileMkdir writes content to path, creating parent directories as needed.
// Shared by fs.write and fs.edit's file-creation paths.
func writeFileMkdir(path, content string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create parent dir: %w", err)
		}
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// markWritten records the just-written file's state as read, so the family
// precondition is satisfied for an immediate follow-up write/edit.
func markWritten(reg *ReadRegistry, path string) {
	if ni, err := os.Stat(path); err == nil {
		reg.MarkRead(path, ni.ModTime().UnixMilli(), true)
	}
}

// lineCount counts newline-terminated lines plus a trailing unterminated line.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}
