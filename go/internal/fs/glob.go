package fs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// globMaxResults caps the number of files fs.glob returns; beyond it the result
// is marked truncated.
const globMaxResults = 100

// GlobParams is the typed param struct for fs.glob. Pattern is required.
type GlobParams struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

// UnmarshalJSON accepts `file_path` as an alias for the canonical `path` when
// the latter is absent — see LSParams.UnmarshalJSON. Bug
// fs-surface-param-name-inconsistency-read-filepath-vs-ls-path.
func (p *GlobParams) UnmarshalJSON(data []byte) error {
	type alias GlobParams
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = GlobParams(a)
	if strings.TrimSpace(p.Path) == "" {
		var fb struct {
			FilePath string `json:"file_path"`
		}
		_ = json.Unmarshal(data, &fb)
		p.Path = fb.FilePath
	}
	return nil
}

// GlobResult is the success shape for fs.glob. Filenames is non-nil even when
// empty, reported relative to the search root, sorted by modification time
// (newest first).
type GlobResult struct {
	Filenames  []string `json:"filenames"`
	NumFiles   int      `json:"num_files"`
	Truncated  bool     `json:"truncated"`
	DurationMS int64    `json:"duration_ms"`
}

// HandleGlob matches files by glob pattern via ripgrep's file enumeration (see
// testdata/parity/GREP_GLOB_CONTRACT.md). The owned replacement for the harness
// Glob tool; results are mtime-sorted and capped at globMaxResults.
func HandleGlob(ctx context.Context, params json.RawMessage) (GlobResult, error) {
	start := time.Now()
	var p GlobParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return GlobResult{}, fmt.Errorf("fs.glob: invalid params: %w", err)
		}
	}
	if strings.TrimSpace(p.Pattern) == "" {
		return GlobResult{}, errors.New("fs.glob requires pattern")
	}

	searchRoot, err := resolveGlobRoot(p.Path)
	if err != nil {
		return GlobResult{}, err
	}

	args := []string{"--files", "--hidden"}
	for _, d := range vcsExcludeDirs {
		args = append(args, "--glob", "!"+d)
	}
	args = append(args, "--glob", p.Pattern)

	stdout, err := runRipgrepIn(ctx, searchRoot, args)
	if err != nil {
		return GlobResult{}, fmt.Errorf("fs.glob: %w", err)
	}

	// Paths are printed relative to searchRoot (rg ran with it as cwd).
	rel := nonEmptyLines(stdout)
	abs := make([]string, len(rel))
	for i, r := range rel {
		abs[i] = filepath.Join(searchRoot, r)
	}
	sortByMtimeDesc(abs)

	truncated := false
	if len(abs) > globMaxResults {
		abs = abs[:globMaxResults]
		truncated = true
	}

	out := make([]string, len(abs))
	for i, a := range abs {
		out[i] = relativize(a, searchRoot)
	}
	return GlobResult{
		Filenames:  out,
		NumFiles:   len(out),
		Truncated:  truncated,
		DurationMS: time.Since(start).Milliseconds(),
	}, nil
}

// runRipgrepIn runs rg with cwd set to dir, so --files paths come out relative
// to dir. Exit 1 (no files) is not an error.
func runRipgrepIn(ctx context.Context, dir string, args []string) (string, error) {
	bin, err := exec.LookPath("rg")
	if err != nil {
		return "", fmt.Errorf("ripgrep (rg) not found on PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return "", nil
		}
		return "", fmt.Errorf("ripgrep: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func resolveGlobRoot(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("fs.glob: resolve working directory: %w", err)
		}
		path = wd
	}
	path = expandUserPath(path)
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("fs.glob: absolutize %q: %w", path, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("fs.glob: path does not exist: %s", path)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("fs.glob: %s is not a directory", path)
	}
	return abs, nil
}
