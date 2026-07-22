package fs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// EditParams is the typed param struct for fs.edit. FilePath, OldString, and
// NewString are required; ReplaceAll defaults false.
//
// Record/Intent are the OPT-IN provenance mode: when Record is set the committed
// edit is emitted as an ArtifactEdited event (Intent becomes the event
// rationale). They default off, leaving the byte-parity edit untouched.
type EditParams struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`

	Record bool   `json:"record,omitempty"`
	Intent string `json:"intent,omitempty"`
}

// UnmarshalJSON accepts `path` as an alias for the canonical `file_path` when
// the latter is absent — see ReadParams.UnmarshalJSON. Bug
// fs-surface-param-name-inconsistency-read-filepath-vs-ls-path.
func (p *EditParams) UnmarshalJSON(data []byte) error {
	type alias EditParams
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = EditParams(a)
	if strings.TrimSpace(p.FilePath) == "" {
		var fb struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(data, &fb)
		p.FilePath = fb.Path
	}
	return nil
}

// EditResult is the success shape for fs.edit. Event is attached only in record
// mode (omitempty), so a default edit marshals to the historical shape.
type EditResult struct {
	FilePath     string `json:"file_path"`
	Created      bool   `json:"created"` // an empty old_string created a new file
	Replacements int    `json:"replacements"`

	Event *ArtifactEvent `json:"event,omitempty"`
}

// HandleEdit replaces OldString with NewString in FilePath.
//
// CONTRACT:
//   - old_string == new_string → a "no changes" error.
//   - File content is read and CRLF is normalized to LF before matching; the
//     result is written with LF endings.
//   - old_string must occur exactly once unless replace_all is set (0 matches →
//     not-found error; >1 without replace_all → an ambiguity error naming the
//     count). replace_all replaces every occurrence; otherwise the first.
//   - Empty old_string creates a new file from new_string (nonexistent path) or
//     fills an empty existing file; an empty old_string on a non-empty file errs.
//   - Editing an EXISTING file requires it was fully read first and unchanged
//     since (the read/write/edit family precondition via ReadRegistry).
func HandleEdit(_ context.Context, params json.RawMessage, reg *ReadRegistry) (EditResult, error) {
	var p EditParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return EditResult{}, fmt.Errorf("fs.edit: invalid params: %w", err)
		}
	}
	if strings.TrimSpace(p.FilePath) == "" {
		return EditResult{}, errors.New("fs.edit requires file_path")
	}
	if p.OldString == p.NewString {
		return EditResult{}, errors.New("fs.edit: No changes to make: old_string and new_string are exactly the same.")
	}
	p.FilePath = expandUserPath(p.FilePath)

	info, statErr := os.Stat(p.FilePath)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		if errors.Is(statErr, fs.ErrPermission) {
			return EditResult{}, fmt.Errorf("fs.edit: permission denied: %s", p.FilePath)
		}
		return EditResult{}, fmt.Errorf("fs.edit: %w", statErr)
	}
	if exists && info.IsDir() {
		return EditResult{}, fmt.Errorf("fs.edit: %s is a directory, not a file", p.FilePath)
	}

	// Nonexistent file: only an empty old_string is valid (new file creation).
	if !exists {
		if p.OldString != "" {
			return EditResult{}, fmt.Errorf("fs.edit: file does not exist: %s", p.FilePath)
		}
		if err := writeFileMkdir(p.FilePath, p.NewString); err != nil {
			return EditResult{}, fmt.Errorf("fs.edit: %w", err)
		}
		markWritten(reg, p.FilePath)
		return EditResult{FilePath: p.FilePath, Created: true, Replacements: 1}, nil
	}

	// Existing file: require a prior full read (and no change since).
	if ok, reason := reg.checkWritable(p.FilePath, info.ModTime().UnixMilli()); !ok {
		return EditResult{}, fmt.Errorf("fs.edit: %s", reason)
	}
	raw, err := os.ReadFile(p.FilePath)
	if err != nil {
		return EditResult{}, fmt.Errorf("fs.edit: %w", err)
	}
	content := strings.ReplaceAll(string(raw), "\r\n", "\n")

	// Empty old_string on an existing file: valid only when the file is empty.
	if p.OldString == "" {
		if strings.TrimSpace(content) != "" {
			return EditResult{}, errors.New("fs.edit: Cannot create new file - file already exists.")
		}
		if err := writeFileMkdir(p.FilePath, p.NewString); err != nil {
			return EditResult{}, fmt.Errorf("fs.edit: %w", err)
		}
		markWritten(reg, p.FilePath)
		return EditResult{FilePath: p.FilePath, Replacements: 1}, nil
	}

	count := strings.Count(content, p.OldString)
	if count == 0 {
		return EditResult{}, fmt.Errorf("fs.edit: String to replace not found in file.\nString: %s", p.OldString)
	}
	if count > 1 && !p.ReplaceAll {
		return EditResult{}, fmt.Errorf("fs.edit: Found %d matches of the string to replace, but replace_all is false. To replace all occurrences, set replace_all to true. To replace only one occurrence, please provide more context to uniquely identify the instance.\nString: %s", count, p.OldString)
	}

	var out string
	var n int
	if p.ReplaceAll {
		out, n = strings.ReplaceAll(content, p.OldString, p.NewString), count
	} else {
		out, n = strings.Replace(content, p.OldString, p.NewString, 1), 1
	}
	if err := writeFileMkdir(p.FilePath, out); err != nil {
		return EditResult{}, fmt.Errorf("fs.edit: %w", err)
	}
	markWritten(reg, p.FilePath)
	return EditResult{FilePath: p.FilePath, Replacements: n}, nil
}
