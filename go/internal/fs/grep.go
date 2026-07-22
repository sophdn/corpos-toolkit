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
	"sort"
	"strconv"
	"strings"
)

// DefaultGrepHeadLimit caps grep output when head_limit is unset, so an
// unbounded content-mode search cannot flood the context. A head_limit of 0
// means unlimited.
const DefaultGrepHeadLimit = 250

// grepMaxColumns clamps matched-line length so minified/base64 content does not
// flood output.
const grepMaxColumns = "500"

// vcsExcludeDirs are version-control metadata directories excluded from every
// search — they are noise, never search targets.
var vcsExcludeDirs = []string{".git", ".svn", ".hg", ".bzr", ".jj", ".sl"}

// GrepParams is the typed param struct for fs.grep. Pattern is required; the
// rest default per the contract (output_mode=files_with_matches, show_line_numbers
// true in content mode, head_limit 250). The tri-state flags are pointers so an
// absent value is distinguishable from an explicit false / 0.
type GrepParams struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path,omitempty"`
	Glob            string `json:"glob,omitempty"`
	Type            string `json:"type,omitempty"`
	OutputMode      string `json:"output_mode,omitempty"`
	ContextBefore   *int   `json:"context_before,omitempty"`
	ContextAfter    *int   `json:"context_after,omitempty"`
	Context         *int   `json:"context,omitempty"`
	ShowLineNumbers *bool  `json:"show_line_numbers,omitempty"`
	CaseInsensitive *bool  `json:"case_insensitive,omitempty"`
	HeadLimit       *int   `json:"head_limit,omitempty"`
	Offset          int    `json:"offset,omitempty"`
	Multiline       bool   `json:"multiline,omitempty"`
}

// UnmarshalJSON accepts `file_path` as an alias for the canonical `path` when
// the latter is absent — see LSParams.UnmarshalJSON. Bug
// fs-surface-param-name-inconsistency-read-filepath-vs-ls-path.
func (p *GrepParams) UnmarshalJSON(data []byte) error {
	type alias GrepParams
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = GrepParams(a)
	if strings.TrimSpace(p.Path) == "" {
		var fb struct {
			FilePath string `json:"file_path"`
		}
		_ = json.Unmarshal(data, &fb)
		p.Path = fb.FilePath
	}
	return nil
}

// GrepResult is the success shape for fs.grep. Filenames is non-nil even when
// empty. Content/NumLines apply to content mode; NumMatches to count mode; the
// applied_* paging markers are set only when they took effect.
type GrepResult struct {
	Mode          string   `json:"mode"`
	NumFiles      int      `json:"num_files"`
	Filenames     []string `json:"filenames"`
	Content       string   `json:"content,omitempty"`
	NumLines      int      `json:"num_lines,omitempty"`
	NumMatches    int      `json:"num_matches,omitempty"`
	AppliedLimit  int      `json:"applied_limit,omitempty"`
	AppliedOffset int      `json:"applied_offset,omitempty"`
}

// HandleGrep searches file contents with ripgrep (see
// testdata/parity/GREP_GLOB_CONTRACT.md). The owned, substrate-native
// replacement for the harness Grep tool; the default output is faithful to it.
func HandleGrep(ctx context.Context, params json.RawMessage) (GrepResult, error) {
	var p GrepParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return GrepResult{}, fmt.Errorf("fs.grep: invalid params: %w", err)
		}
	}
	if strings.TrimSpace(p.Pattern) == "" {
		return GrepResult{}, errors.New("fs.grep requires pattern")
	}

	mode := p.OutputMode
	if mode == "" {
		mode = "files_with_matches"
	}
	if mode != "content" && mode != "files_with_matches" && mode != "count" {
		return GrepResult{}, fmt.Errorf("fs.grep: invalid output_mode %q (want content|files_with_matches|count)", mode)
	}

	searchRoot, err := resolveSearchRoot(p.Path)
	if err != nil {
		return GrepResult{}, err
	}
	relBase := relBaseFor(searchRoot)

	args := buildGrepArgs(p, mode, searchRoot)
	stdout, err := runRipgrep(ctx, args)
	if err != nil {
		return GrepResult{}, fmt.Errorf("fs.grep: %w", err)
	}

	res := GrepResult{Mode: mode, Filenames: []string{}}
	if p.Offset > 0 {
		res.AppliedOffset = p.Offset
	}
	lines := nonEmptyLines(stdout)

	switch mode {
	case "content":
		limited, applied := applyGrepHeadLimit(lines, p.HeadLimit, p.Offset)
		final := make([]string, len(limited))
		for i, ln := range limited {
			final[i] = stripRootPrefix(ln, relBase)
		}
		res.Content = strings.Join(final, "\n")
		res.NumLines = len(final)
		res.AppliedLimit = applied
	case "count":
		limited, applied := applyGrepHeadLimit(lines, p.HeadLimit, p.Offset)
		final := make([]string, 0, len(limited))
		total, files := 0, 0
		for _, ln := range limited {
			rel := stripRootPrefix(ln, relBase)
			final = append(final, rel)
			if count, ok := parseTrailingCount(rel); ok {
				total += count
				files++
			}
		}
		res.Content = strings.Join(final, "\n")
		res.NumMatches = total
		res.NumFiles = files
		res.AppliedLimit = applied
	default: // files_with_matches
		sortByMtimeDesc(lines)
		limited, applied := applyGrepHeadLimit(lines, p.HeadLimit, p.Offset)
		rel := make([]string, 0, len(limited))
		for _, f := range limited {
			rel = append(rel, stripRootPrefix(f, relBase))
		}
		res.Filenames = rel
		res.NumFiles = len(rel)
		res.AppliedLimit = applied
	}
	return res, nil
}

// buildGrepArgs assembles the ripgrep argv from the params, mirroring the
// harness Grep's flag mapping. The search root is the final positional arg.
func buildGrepArgs(p GrepParams, mode, searchRoot string) []string {
	args := []string{"--hidden"}
	for _, d := range vcsExcludeDirs {
		args = append(args, "--glob", "!"+d)
	}
	args = append(args, "--max-columns", grepMaxColumns)

	if p.Multiline {
		args = append(args, "-U", "--multiline-dotall")
	}
	if p.CaseInsensitive != nil && *p.CaseInsensitive {
		args = append(args, "-i")
	}
	switch mode {
	case "files_with_matches":
		args = append(args, "-l")
	case "count":
		args = append(args, "-c")
	}
	if mode == "content" {
		// Always emit the filename (-H) so a single-file search is still
		// addressable (path:line:text); rg otherwise omits it for one file.
		args = append(args, "-H")
		if p.ShowLineNumbers == nil || *p.ShowLineNumbers {
			args = append(args, "-n")
		}
		// -C / context takes precedence over -B / -A.
		switch {
		case p.Context != nil:
			args = append(args, "-C", strconv.Itoa(*p.Context))
		default:
			if p.ContextBefore != nil {
				args = append(args, "-B", strconv.Itoa(*p.ContextBefore))
			}
			if p.ContextAfter != nil {
				args = append(args, "-A", strconv.Itoa(*p.ContextAfter))
			}
		}
	}
	// A pattern beginning with '-' is passed explicitly so it is not parsed as a flag.
	if strings.HasPrefix(p.Pattern, "-") {
		args = append(args, "-e", p.Pattern)
	} else {
		args = append(args, p.Pattern)
	}
	if p.Type != "" {
		args = append(args, "--type", p.Type)
	}
	for _, g := range splitGlobPatterns(p.Glob) {
		args = append(args, "--glob", g)
	}
	return append(args, searchRoot)
}

// runRipgrep runs rg and returns its stdout. Exit 1 (no matches) is not an
// error; exit 2 (rg error) is.
func runRipgrep(ctx context.Context, args []string) (string, error) {
	bin, err := exec.LookPath("rg")
	if err != nil {
		return "", fmt.Errorf("ripgrep (rg) not found on PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return "", nil // no matches
		}
		return "", fmt.Errorf("ripgrep: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// splitGlobPatterns splits a glob filter on whitespace and commas, preserving
// brace groups (which contain commas that must not be split).
func splitGlobPatterns(glob string) []string {
	glob = strings.TrimSpace(glob)
	if glob == "" {
		return nil
	}
	var out []string
	for _, raw := range strings.Fields(glob) {
		if strings.Contains(raw, "{") && strings.Contains(raw, "}") {
			out = append(out, raw)
			continue
		}
		for _, piece := range strings.Split(raw, ",") {
			if piece != "" {
				out = append(out, piece)
			}
		}
	}
	return out
}

// applyGrepHeadLimit pages items: skip offset, then cap at head_limit (default
// DefaultGrepHeadLimit, 0 = unlimited). The returned appliedLimit is non-zero
// only when truncation actually occurred.
func applyGrepHeadLimit(items []string, limit *int, offset int) ([]string, int) {
	if offset < 0 {
		offset = 0
	}
	if offset > len(items) {
		offset = len(items)
	}
	rest := items[offset:]
	if limit != nil && *limit == 0 {
		return rest, 0 // unlimited
	}
	eff := DefaultGrepHeadLimit
	if limit != nil {
		eff = *limit
	}
	if len(rest) > eff {
		return rest[:eff], eff
	}
	return rest, 0
}

// resolveSearchRoot resolves the search path (default working dir) to an
// absolute path and verifies it exists.
func resolveSearchRoot(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("fs.grep: resolve working directory: %w", err)
		}
		path = wd
	}
	path = expandUserPath(path)
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("fs.grep: absolutize %q: %w", path, err)
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("fs.grep: path does not exist: %s", path)
	}
	return abs, nil
}

// relBaseFor returns the directory results are reported relative to: the search
// root when it is a directory, its parent when it is a file.
func relBaseFor(searchRoot string) string {
	if fi, err := os.Stat(searchRoot); err == nil && !fi.IsDir() {
		return filepath.Dir(searchRoot)
	}
	return searchRoot
}

func relativize(abs, base string) string {
	if rel, err := filepath.Rel(base, abs); err == nil {
		return rel
	}
	return abs
}

// stripRootPrefix removes a leading "<base>/" from a ripgrep output line so the
// path renders relative to the search root. It is separator-agnostic, so it
// works for match lines ("path:line:text"), context lines ("path-line-text"),
// and count lines ("path:count") alike.
func stripRootPrefix(line, base string) string {
	prefix := base + string(os.PathSeparator)
	if strings.HasPrefix(line, prefix) {
		return line[len(prefix):]
	}
	return line
}

// parseTrailingCount reads the integer after the last colon of a "path:count"
// line.
func parseTrailingCount(line string) (int, bool) {
	i := strings.LastIndexByte(line, ':')
	if i < 0 {
		return 0, false
	}
	count, err := strconv.Atoi(line[i+1:])
	return count, err == nil
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if ln != "" {
			out = append(out, ln)
		}
	}
	return out
}

// sortByMtimeDesc sorts file paths by modification time, newest first, with the
// path as a tiebreaker. Unstattable paths sort as mtime 0.
func sortByMtimeDesc(paths []string) {
	mtime := make(map[string]int64, len(paths))
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			mtime[p] = fi.ModTime().UnixNano()
		}
	}
	sort.SliceStable(paths, func(i, j int) bool {
		if mtime[paths[i]] != mtime[paths[j]] {
			return mtime[paths[i]] > mtime[paths[j]]
		}
		return paths[i] < paths[j]
	})
}
