package fs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"toolkit/internal/db"
)

// Deps is the shared dependency bundle for the fs surface. Pool is consumed only
// by the opt-in substrate-native upgrade modes (knowledge-aware grep, provenance
// read, projection-joined glob/ls) and may be nil. Reads is the read-state
// registry that couples read/write/edit (fs.read records, fs.write/fs.edit
// require); BuildTable initializes it when nil.
type Deps struct {
	Pool  *db.Pool
	Reads *ReadRegistry
}

// Read contract constants:
//   - MaxReadSizeBytes is the model-agnostic byte cap above which a whole-file
//     read fails (a ranged read bypasses it).
//   - DefaultReadOffset is the 1-based default start line.
//
// A model-coupled token cap is intentionally NOT part of this contract: this
// surface is model-agnostic, so the byte cap is the only size guard.
const (
	DefaultReadOffset = 1
	MaxReadSizeBytes  = 256 * 1024 // 0.25 MB
)

// ReadParams is the typed param struct for fs.read. FilePath is required;
// Offset (1-based start line) and Limit (max lines) are optional. The field
// types here are the source of truth for the generated action-doc param types.
//
// Outline/Symbol/Provenance/Oriented are the OPT-IN substrate-native upgrade
// modes; all default to the zero value, which selects the byte-parity read. A
// mode read is a partial/derived view and does NOT record full read-state (it
// cannot satisfy the fs.write / fs.edit precondition). At most one mode is
// honored per call, in the precedence order outline > symbol > provenance >
// oriented; the byte-parity default is unaffected by their presence.
type ReadParams struct {
	FilePath string `json:"file_path"`
	Offset   int64  `json:"offset"`
	Limit    int64  `json:"limit"`

	// Outline: return a go/ast structural summary (top-level signatures + doc
	// comments) instead of the full file — measurably smaller, for orientation.
	Outline bool `json:"outline,omitempty"`
	// Symbol: resolve a single named declaration (func / type / const / var, or
	// Type.Method) via go/ast and return just its source span.
	Symbol string `json:"symbol,omitempty"`
	// Provenance: attach intent-annotated mutation history for the read range
	// (git blame over the range + matching substrate events).
	Provenance bool `json:"provenance,omitempty"`
	// Oriented: attach the file's package intended-use block (doc.go) and any
	// related knowledge_pointers.
	Oriented bool `json:"oriented,omitempty"`
}

// UnmarshalJSON accepts `path` as an alias for the canonical `file_path` when
// the latter is absent. read/write/edit canonically take `file_path`; the
// directory/search actions take `path` — a caller (esp. a small local model)
// that guesses the other family's spelling still succeeds. Canonical-name
// behavior is unchanged (the alias only fills an empty FilePath). Bug
// fs-surface-param-name-inconsistency-read-filepath-vs-ls-path.
func (p *ReadParams) UnmarshalJSON(data []byte) error {
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err == nil {
		if val, exists := m["symbol"]; exists {
			if _, isString := val.(string); !isString {
				return fmt.Errorf("fs.read: invalid parameter 'symbol', expected string, got %T", val)
			}
		}
	}

	type alias ReadParams // avoids recursion into this method
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = ReadParams(a)
	if strings.TrimSpace(p.FilePath) == "" {
		var fb struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(data, &fb)
		p.FilePath = fb.Path
	}
	return nil
}

// readMode reports the single upgrade mode selected by p (or modeNone for the
// byte-parity default). Precedence is fixed so the selection is deterministic.
func (p ReadParams) readMode() readModeKind {
	switch {
	case p.Outline:
		return modeOutline
	case strings.TrimSpace(p.Symbol) != "":
		return modeSymbol
	case p.Provenance:
		return modeProvenance
	case p.Oriented:
		return modeOriented
	default:
		return modeNone
	}
}

// ReadResult is the success shape for fs.read. Content is the numbered-line text
// and is empty when no lines are selected; in that case Warning carries the
// system-reminder text. The line metadata lets a caller page.
//
// The substrate-native upgrade modes attach exactly one of the trailing pointer
// fields (Outline/Symbol/Provenance/Oriented); all are omitempty, so the
// byte-parity default read marshals to precisely the historical shape.
type ReadResult struct {
	FilePath   string `json:"file_path"`
	Content    string `json:"content"`
	Warning    string `json:"warning,omitempty"`
	StartLine  int    `json:"start_line"`
	LineCount  int    `json:"line_count"`
	TotalLines int    `json:"total_lines"`

	Outline    *OutlineView    `json:"outline,omitempty"`
	Symbol     *SymbolView     `json:"symbol,omitempty"`
	Provenance *ProvenanceView `json:"provenance,omitempty"`
	Oriented   *OrientedView   `json:"oriented,omitempty"`
}

// HandleRead reads a file (optionally a line range) and returns its contents as
// numbered lines.
//
// CONTRACT (see testdata/parity/OBSERVED_HARNESS_CONTRACT.md):
//   - reader: strip a leading UTF-8 BOM, split on "\n", strip a trailing "\r"
//     per line, select the range [offset-1, offset-1+limit), join with "\n"
//     (NO trailing newline). The final fragment after the last "\n" is always
//     counted, so a file ending in "\n" yields a trailing empty line, and
//     totalLines counts that fragment.
//   - format: "<n>\t<content>" with an UNPADDED line number, numbered from
//     `offset`, joined with "\n" (not cat -n's padded form).
//   - byte cap: a whole-file read (no limit) of a file larger than 256 KB throws
//     FileTooLargeError; a ranged read (limit set) skips the cap.
//   - empty output: offset past EOF (or an empty file) returns a system-reminder
//     Warning verbatim, not an error.
//   - intentional sane-divergence: offset < 1 is normalized to 1 (the source
//     allows offset 0, which numbers the first line "0"); documented, not faithful.
//
// It streams line by line rather than loading the whole file, so a huge file
// with a small window stays cheap.
func HandleRead(_ context.Context, params json.RawMessage) (ReadResult, error) {
	var p ReadParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return ReadResult{}, fmt.Errorf("fs.read: invalid params: %w", err)
		}
	}
	if strings.TrimSpace(p.FilePath) == "" {
		return ReadResult{}, errors.New("fs.read requires file_path")
	}
	p.FilePath = expandUserPath(p.FilePath)

	start := p.Offset
	if start < 1 {
		start = DefaultReadOffset
	}
	lineOffset := start - 1 // 0-based first line to select
	whole := p.Limit <= 0   // absent / non-positive == whole file
	endLine := int64(-1)    // -1 sentinel == unbounded
	if !whole {
		endLine = lineOffset + p.Limit
	}

	info, err := os.Stat(p.FilePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ReadResult{}, fmt.Errorf("fs.read: file does not exist: %s", p.FilePath)
		}
		if errors.Is(err, fs.ErrPermission) {
			return ReadResult{}, fmt.Errorf("fs.read: permission denied: %s", p.FilePath)
		}
		return ReadResult{}, fmt.Errorf("fs.read: %w", err)
	}
	if info.IsDir() {
		return ReadResult{}, fmt.Errorf("fs.read: %s is a directory, not a file", p.FilePath)
	}
	// Byte cap applies only to whole-file reads; a ranged read (limit set)
	// bypasses it.
	if whole && info.Size() > MaxReadSizeBytes {
		return ReadResult{}, fmt.Errorf(
			"fs.read: file content (%s) exceeds maximum allowed size (%s). Use offset and limit parameters to read specific portions of the file, or search for specific content instead of reading the whole file.",
			humanByteSize(info.Size()), humanByteSize(MaxReadSizeBytes),
		)
	}

	f, err := os.Open(p.FilePath)
	if err != nil {
		return ReadResult{}, fmt.Errorf("fs.read: %w", err)
	}
	defer f.Close()

	selected, totalLines, err := selectLines(bufio.NewReader(f), lineOffset, endLine)
	if err != nil {
		return ReadResult{}, fmt.Errorf("fs.read: %w", err)
	}

	content := strings.Join(selected, "\n")
	if content == "" {
		// No selectable content (empty file, or offset past EOF). The harness
		// returns a system-reminder as the result content rather than erroring.
		var warn string
		if totalLines == 0 {
			warn = "<system-reminder>Warning: the file exists but the contents are empty.</system-reminder>"
		} else {
			warn = fmt.Sprintf("<system-reminder>Warning: the file exists but is shorter than the provided offset (%d). The file has %d lines.</system-reminder>", start, totalLines)
		}
		return ReadResult{FilePath: p.FilePath, Warning: warn, StartLine: int(start), TotalLines: totalLines}, nil
	}

	var b strings.Builder
	for i, line := range selected {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d\t%s", start+int64(i), line)
	}

	return ReadResult{
		FilePath:   p.FilePath,
		Content:    b.String(),
		StartLine:  int(start),
		LineCount:  len(selected),
		TotalLines: totalLines,
	}, nil
}

// selectLines streams r per the contract's line model: strip a leading UTF-8
// BOM, split on "\n", strip a trailing "\r" per line, and collect the lines
// whose 0-based index falls in [lineOffset, endLine) (endLine < 0 == unbounded).
// It returns the selected lines and the TOTAL line count (counting the final
// fragment after the last "\n", so a file ending in "\n" counts a trailing
// empty line). Lines outside the window are counted, not retained.
func selectLines(r *bufio.Reader, lineOffset, endLine int64) (selected []string, totalLines int, err error) {
	// Strip a leading UTF-8 BOM (EF BB BF) if present.
	if bom, perr := r.Peek(3); perr == nil && bom[0] == 0xEF && bom[1] == 0xBB && bom[2] == 0xBF {
		_, _ = r.Discard(3)
	}

	idx := int64(0) // 0-based index of the current part
	keep := func(line string) {
		if idx >= lineOffset && (endLine < 0 || idx < endLine) {
			selected = append(selected, capLine(line))
		}
		idx++
	}

	for {
		chunk, readErr := r.ReadString('\n')
		if readErr == nil {
			keep(strings.TrimSuffix(chunk, "\n"))
			continue
		}
		if readErr == io.EOF {
			// The remainder after the last "\n" is always the final part
			// (strings.Split semantics), counted even when empty.
			keep(chunk)
			break
		}
		return nil, 0, readErr
	}
	return selected, int(idx), nil
}

// capLine strips a trailing "\r" (CRLF→LF) and truncates a pathological line so
// it cannot flood the window. (The harness has no per-line length cap; this is a
// model-agnostic safety guard, flagged in OBSERVED_HARNESS_CONTRACT.md.)
func capLine(line string) string {
	line = strings.TrimSuffix(line, "\r")
	if len(line) > maxReadLineLen {
		line = line[:maxReadLineLen] + "... [truncated]"
	}
	return line
}

const maxReadLineLen = 60000

// humanByteSize renders a byte count for the byte-cap message as
// "<n> bytes" / "<kb>KB" / "<mb>MB" / "<gb>GB" — one decimal with a trailing
// ".0" stripped, no space before the unit.
func humanByteSize(sizeInBytes int64) string {
	kb := float64(sizeInBytes) / 1024
	if kb < 1 {
		return fmt.Sprintf("%d bytes", sizeInBytes)
	}
	if kb < 1024 {
		return trimDotZero(kb) + "KB"
	}
	mb := kb / 1024
	if mb < 1024 {
		return trimDotZero(mb) + "MB"
	}
	return trimDotZero(mb/1024) + "GB"
}

func trimDotZero(v float64) string {
	s := fmt.Sprintf("%.1f", v)
	return strings.TrimSuffix(s, ".0")
}
