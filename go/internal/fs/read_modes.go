package fs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"toolkit/internal/db"
)

// read_modes.go implements the OPT-IN substrate-native upgrade modes for
// fs.read. None of them changes the byte-parity default: the dispatcher here is
// reached only when ReadParams selects a mode, and each mode populates exactly
// one of ReadResult's trailing omitempty pointer fields. The modes are:
//
//   - outline:    a go/ast structural summary (signatures + doc), much smaller
//                 than the full file, for orientation.
//   - symbol:     resolve one named declaration via go/ast and return its span.
//   - provenance: intent-annotated mutation history for the read range (git
//                 blame + matching substrate events).
//   - oriented:   the file's package intended-use block (doc.go) plus related
//                 knowledge_pointers.
//
// There is no LSP in-repo, so the symbol/outline work is done with go/ast
// (stdlib). Provenance leans on git blame plus the owned event log.

type readModeKind int

const (
	modeNone readModeKind = iota
	modeOutline
	modeSymbol
	modeProvenance
	modeOriented
)

// readModeOf parses raw params just far enough to decide whether an upgrade
// mode is requested, so the table router can branch without re-implementing the
// parity path. Malformed params resolve to modeNone and fall through to the
// parity read, which produces the canonical parse error.
func readModeOf(params json.RawMessage) readModeKind {
	var p ReadParams
	if len(params) == 0 {
		return modeNone
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return modeNone
	}
	return p.readMode()
}

// handleReadMode is the upgrade-mode dispatcher. It is invoked only when
// ReadParams.readMode() != modeNone; the byte-parity default never reaches it.
func handleReadMode(ctx context.Context, deps Deps, params json.RawMessage) (ReadResult, error) {
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
	switch p.readMode() {
	case modeOutline:
		return readOutline(p)
	case modeSymbol:
		return readSymbol(p)
	case modeProvenance:
		return readProvenance(ctx, deps, p)
	case modeOriented:
		return readOriented(ctx, deps, p)
	default:
		// Unreachable via the table router, but stay safe: fall back to parity.
		return HandleRead(ctx, params)
	}
}

// statFileForMode runs the same existence/kind checks as the parity read and
// returns the typed fs.read-prefixed errors so a mode read fails identically to
// a default read on a missing path or a directory.
func statFileForMode(path string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("fs.read: file does not exist: %s", path)
		}
		if errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("fs.read: permission denied: %s", path)
		}
		return nil, fmt.Errorf("fs.read: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("fs.read: %s is a directory, not a file", path)
	}
	return info, nil
}

// ── outline mode ─────────────────────────────────────────────────────────────

// OutlineView is the structural summary attached by outline mode. It carries
// the per-declaration list plus the byte sizes that prove the outline is
// smaller than the source it summarizes.
type OutlineView struct {
	Language     string        `json:"language"`
	Package      string        `json:"package,omitempty"`
	Decls        []OutlineDecl `json:"decls"`
	DeclCount    int           `json:"decl_count"`
	FullBytes    int           `json:"full_bytes"`
	OutlineBytes int           `json:"outline_bytes"`
}

// OutlineDecl is one top-level declaration in the outline: its kind, name, a
// body-stripped signature, the leading doc comment, and the 1-based line.
type OutlineDecl struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Signature string `json:"signature"`
	Doc       string `json:"doc,omitempty"`
	Line      int    `json:"line"`
}

func readOutline(p ReadParams) (ReadResult, error) {
	if _, err := statFileForMode(p.FilePath); err != nil {
		return ReadResult{}, err
	}
	if !strings.HasSuffix(p.FilePath, ".go") {
		return ReadResult{}, fmt.Errorf("fs.read: outline mode requires a Go source file (.go): %s", p.FilePath)
	}
	src, err := os.ReadFile(p.FilePath)
	if err != nil {
		return ReadResult{}, fmt.Errorf("fs.read: %w", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, p.FilePath, src, parser.ParseComments)
	if err != nil {
		return ReadResult{}, fmt.Errorf("fs.read: outline parse failed: %w", err)
	}

	var decls []OutlineDecl
	for _, d := range file.Decls {
		decls = append(decls, outlineDecls(fset, d)...)
	}

	content := renderOutline(file.Name.Name, decls)
	view := &OutlineView{
		Language:     "go",
		Package:      file.Name.Name,
		Decls:        decls,
		DeclCount:    len(decls),
		FullBytes:    len(src),
		OutlineBytes: len(content),
	}
	return ReadResult{
		FilePath: p.FilePath,
		Content:  content,
		Outline:  view,
	}, nil
}

// outlineDecls flattens one top-level ast.Decl into outline rows (a GenDecl can
// hold several specs).
func outlineDecls(fset *token.FileSet, d ast.Decl) []OutlineDecl {
	switch decl := d.(type) {
	case *ast.FuncDecl:
		kind := "func"
		if decl.Recv != nil {
			kind = "method"
		}
		return []OutlineDecl{{
			Kind:      kind,
			Name:      funcName(decl),
			Signature: funcSignature(fset, decl),
			Doc:       firstDocLine(decl.Doc),
			Line:      fset.Position(decl.Pos()).Line,
		}}
	case *ast.GenDecl:
		var out []OutlineDecl
		for _, spec := range decl.Specs {
			if od, ok := specOutline(fset, decl, spec); ok {
				out = append(out, od)
			}
		}
		return out
	default:
		return nil
	}
}

func specOutline(fset *token.FileSet, decl *ast.GenDecl, spec ast.Spec) (OutlineDecl, bool) {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		return OutlineDecl{
			Kind:      "type",
			Name:      s.Name.Name,
			Signature: typeSignature(fset, s),
			Doc:       firstDocLine(orDoc(s.Doc, decl.Doc)),
			Line:      fset.Position(s.Pos()).Line,
		}, true
	case *ast.ValueSpec:
		kind := "var"
		if decl.Tok == token.CONST {
			kind = "const"
		}
		names := make([]string, 0, len(s.Names))
		for _, n := range s.Names {
			names = append(names, n.Name)
		}
		sig := kind + " " + strings.Join(names, ", ")
		if s.Type != nil {
			sig += " " + exprString(fset, s.Type)
		}
		return OutlineDecl{
			Kind:      kind,
			Name:      strings.Join(names, ", "),
			Signature: sig,
			Doc:       firstDocLine(orDoc(s.Doc, decl.Doc)),
			Line:      fset.Position(s.Pos()).Line,
		}, true
	default:
		return OutlineDecl{}, false
	}
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	recv := strings.TrimPrefix(exprStringType(fd.Recv.List[0].Type), "*")
	return recv + "." + fd.Name.Name
}

// funcSignature renders a func declaration header with the body stripped.
func funcSignature(fset *token.FileSet, fd *ast.FuncDecl) string {
	cp := *fd
	cp.Body = nil
	cp.Doc = nil
	var b strings.Builder
	if err := printer.Fprint(&b, fset, &cp); err != nil {
		return "func " + fd.Name.Name
	}
	return strings.TrimSpace(strings.ReplaceAll(b.String(), "\n", " "))
}

// typeSignature renders a compact type header: aliases keep their target,
// struct/interface collapse to the keyword (fields omitted), everything else
// shows its underlying expression.
func typeSignature(fset *token.FileSet, ts *ast.TypeSpec) string {
	if ts.Assign.IsValid() {
		return "type " + ts.Name.Name + " = " + exprString(fset, ts.Type)
	}
	switch ts.Type.(type) {
	case *ast.StructType:
		return "type " + ts.Name.Name + " struct"
	case *ast.InterfaceType:
		return "type " + ts.Name.Name + " interface"
	default:
		return "type " + ts.Name.Name + " " + exprString(fset, ts.Type)
	}
}

func exprString(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, e); err != nil {
		return ""
	}
	return strings.ReplaceAll(b.String(), "\n", " ")
}

// exprStringType renders a receiver type without a FileSet (used for method
// names where positions are irrelevant).
func exprStringType(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprStringType(t.X)
	case *ast.IndexExpr: // generic receiver T[P]
		return exprStringType(t.X)
	case *ast.IndexListExpr:
		return exprStringType(t.X)
	default:
		return ""
	}
}

func orDoc(primary, fallback *ast.CommentGroup) *ast.CommentGroup {
	if primary != nil {
		return primary
	}
	return fallback
}

// firstDocLine returns the first non-empty line of a doc comment, trimmed.
func firstDocLine(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	for _, line := range strings.Split(cg.Text(), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}

func renderOutline(pkg string, decls []OutlineDecl) string {
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n", pkg)
	for _, d := range decls {
		fmt.Fprintf(&b, "\nL%d\t%s", d.Line, d.Signature)
		if d.Doc != "" {
			fmt.Fprintf(&b, "\n\t// %s", d.Doc)
		}
	}
	return b.String()
}

// ── symbol mode ──────────────────────────────────────────────────────────────

// SymbolView identifies the resolved declaration and its source span; the
// numbered source text lands in ReadResult.Content.
type SymbolView struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Doc       string `json:"doc,omitempty"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

func readSymbol(p ReadParams) (ReadResult, error) {
	if _, err := statFileForMode(p.FilePath); err != nil {
		return ReadResult{}, err
	}
	if !strings.HasSuffix(p.FilePath, ".go") {
		return ReadResult{}, fmt.Errorf("fs.read: symbol mode requires a Go source file (.go): %s", p.FilePath)
	}
	src, err := os.ReadFile(p.FilePath)
	if err != nil {
		return ReadResult{}, fmt.Errorf("fs.read: %w", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, p.FilePath, src, parser.ParseComments)
	if err != nil {
		return ReadResult{}, fmt.Errorf("fs.read: symbol parse failed: %w", err)
	}

	want := strings.TrimSpace(p.Symbol)
	decl, kind, doc := findSymbol(file, want)
	if decl == nil {
		return ReadResult{}, fmt.Errorf("fs.read: symbol %q not found in %s", want, p.FilePath)
	}

	// Span starts at the doc comment when present, so the returned slice reads
	// as the symbol does in the file.
	startPos := decl.Pos()
	if doc != nil {
		startPos = doc.Pos()
	}
	startLine := fset.Position(startPos).Line
	endLine := fset.Position(decl.End()).Line

	lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")
	if startLine < 1 {
		startLine = 1
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}
	span := lines[startLine-1 : endLine]

	return ReadResult{
		FilePath:  p.FilePath,
		Content:   numberLines(span, startLine),
		StartLine: startLine,
		LineCount: len(span),
		Symbol: &SymbolView{
			Name:      want,
			Kind:      kind,
			Doc:       firstDocLine(doc),
			StartLine: startLine,
			EndLine:   endLine,
		},
	}, nil
}

// findSymbol resolves want (a top-level name, or "Type.Method") to its decl,
// returning the node, a kind string, and the associated doc comment group.
func findSymbol(file *ast.File, want string) (ast.Node, string, *ast.CommentGroup) {
	recvWant, methodWant, isMethod := strings.Cut(want, ".")
	for _, d := range file.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			if isMethod {
				if decl.Recv != nil && exprStringType(decl.Recv.List[0].Type) == recvWant && decl.Name.Name == methodWant {
					return decl, "method", decl.Doc
				}
				continue
			}
			if decl.Recv == nil && decl.Name.Name == want {
				return decl, "func", decl.Doc
			}
		case *ast.GenDecl:
			if isMethod {
				continue
			}
			for _, spec := range decl.Specs {
				if node, kind := matchSpec(decl, spec, want); node != nil {
					return node, kind, orDoc(specDoc(spec), decl.Doc)
				}
			}
		}
	}
	return nil, "", nil
}

func matchSpec(decl *ast.GenDecl, spec ast.Spec, want string) (ast.Node, string) {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		if s.Name.Name == want {
			return s, "type"
		}
	case *ast.ValueSpec:
		for _, n := range s.Names {
			if n.Name == want {
				kind := "var"
				if decl.Tok == token.CONST {
					kind = "const"
				}
				return s, kind
			}
		}
	}
	return nil, ""
}

func specDoc(spec ast.Spec) *ast.CommentGroup {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		return s.Doc
	case *ast.ValueSpec:
		return s.Doc
	default:
		return nil
	}
}

// numberLines renders lines in the parity read's "<n>\t<content>" form (unpadded
// line numbers from startLine, joined with newline, no trailing newline).
func numberLines(lines []string, startLine int) string {
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d\t%s", startLine+i, line)
	}
	return b.String()
}

// ── provenance mode ──────────────────────────────────────────────────────────

// ProvenanceView is the intent-annotated mutation history for the read range:
// the git-blame commits over the range (their summaries are the intent
// annotation) plus matching substrate events (CommitLanded subjects and the
// owned artifact-write/edit events with their rationales).
type ProvenanceView struct {
	FilePath   string             `json:"file_path"`
	RangeStart int                `json:"range_start"`
	RangeEnd   int                `json:"range_end"`
	Commits    []ProvenanceCommit `json:"commits"`
	Events     []ProvenanceEvent  `json:"events,omitempty"`
	Note       string             `json:"note,omitempty"`
}

// ProvenanceCommit is one commit attributed to the range by git blame; Summary
// is its subject line — the human intent for the change.
type ProvenanceCommit struct {
	Commit  string `json:"commit"`
	Author  string `json:"author"`
	Summary string `json:"summary"`
	Lines   int    `json:"lines"`
}

// ProvenanceEvent is one substrate event tied to the file/commit — the
// substrate-native intent annotation the harness Read cannot see.
type ProvenanceEvent struct {
	Type       string `json:"type"`
	Ts         string `json:"ts"`
	Rationale  string `json:"rationale,omitempty"`
	EntitySlug string `json:"entity_slug,omitempty"`
}

func readProvenance(ctx context.Context, deps Deps, p ReadParams) (ReadResult, error) {
	if _, err := statFileForMode(p.FilePath); err != nil {
		return ReadResult{}, err
	}
	start, end := provenanceRange(p)
	view := &ProvenanceView{FilePath: p.FilePath, RangeStart: start, RangeEnd: end}

	// Full SHAs drive the event join (events.entity_slug stores the full SHA);
	// the view shows the short form for readability.
	blamed, note := gitBlameRange(ctx, p.FilePath, start, end)
	shas := make([]string, 0, len(blamed))
	for _, bc := range blamed {
		view.Commits = append(view.Commits, ProvenanceCommit{
			Commit:  shortSHA(bc.sha),
			Author:  bc.author,
			Summary: bc.summary,
			Lines:   bc.lines,
		})
		shas = append(shas, bc.sha)
	}
	if note != "" {
		view.Note = note
	}

	if deps.Pool != nil {
		if evs, err := queryProvenanceEvents(ctx, deps.Pool, absPath(p.FilePath), shas); err == nil {
			view.Events = evs
		}
	}

	return ReadResult{FilePath: p.FilePath, Provenance: view}, nil
}

// provenanceRange returns the 1-based inclusive line range to attribute. With
// no offset/limit it is the whole file (0,0 sentinel → blame the whole file).
func provenanceRange(p ReadParams) (int, int) {
	if p.Offset <= 0 && p.Limit <= 0 {
		return 0, 0
	}
	start := int(p.Offset)
	if start < 1 {
		start = 1
	}
	end := 0
	if p.Limit > 0 {
		end = start + int(p.Limit) - 1
	}
	return start, end
}

// blameCommit is the internal per-commit fold of git blame output, carrying the
// FULL SHA (needed to join the event log) before the view shortens it.
type blameCommit struct {
	sha, author, summary string
	lines                int
}

// gitBlameRange runs `git blame --porcelain` over the range and groups the
// result by commit. It is fail-soft: a missing git, an untracked file, or any
// blame error returns no commits plus an explanatory note rather than an error.
func gitBlameRange(ctx context.Context, path string, start, end int) ([]blameCommit, string) {
	abs := absPath(path)
	dir := filepath.Dir(abs)
	args := []string{"blame", "--porcelain"}
	if start > 0 {
		rng := fmt.Sprintf("%d", start)
		if end >= start {
			rng = fmt.Sprintf("%d,%d", start, end)
		} else {
			rng = fmt.Sprintf("%d,%d", start, start)
		}
		args = append(args, "-L", rng)
	}
	args = append(args, "--", filepath.Base(abs))

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, "git blame unavailable for this path (untracked, outside a repo, or git missing)"
	}
	return parseBlamePorcelain(string(out)), ""
}

// parseBlamePorcelain folds git blame --porcelain output into per-commit rows,
// preserving first-seen order and counting attributed lines. SHAs are kept full.
func parseBlamePorcelain(out string) []blameCommit {
	order := []string{}
	byCommit := map[string]*blameCommit{}
	var cur string

	for _, line := range strings.Split(out, "\n") {
		switch {
		case len(line) >= 40 && isHexLine(line[:40]):
			cur = line[:40]
			if _, ok := byCommit[cur]; !ok {
				byCommit[cur] = &blameCommit{sha: cur}
				order = append(order, cur)
			}
			byCommit[cur].lines++
		case strings.HasPrefix(line, "author ") && cur != "":
			byCommit[cur].author = strings.TrimPrefix(line, "author ")
		case strings.HasPrefix(line, "summary ") && cur != "":
			byCommit[cur].summary = strings.TrimPrefix(line, "summary ")
		}
	}

	commits := make([]blameCommit, 0, len(order))
	for _, sha := range order {
		commits = append(commits, *byCommit[sha])
	}
	return commits
}

func isHexLine(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return len(s) == 40
}

func shortSHA(sha string) string {
	if len(sha) >= 12 {
		return sha[:12]
	}
	return sha
}

// queryProvenanceEvents folds two substrate sources into the provenance view:
// the owned artifact-write/edit events for this exact path, and the CommitLanded
// events for the commits git blame attributed to the range.
func queryProvenanceEvents(ctx context.Context, pool *db.Pool, absFile string, shas []string) ([]ProvenanceEvent, error) {
	var events []ProvenanceEvent

	rows, err := pool.DB().QueryContext(ctx,
		`SELECT type, ts, COALESCE(rationale,''), entity_slug
		   FROM events
		  WHERE (type IN ('ArtifactWritten','ArtifactEdited')
		         AND json_extract(payload,'$.file_path') = ?)
		     OR (type = 'ArtifactMoved'
		         AND json_extract(payload,'$.dest') = ?)
		  ORDER BY ts DESC
		  LIMIT 20`, absFile, absFile)
	if err != nil {
		return nil, err
	}
	if err := scanProvenanceRows(rows, &events); err != nil {
		return nil, err
	}

	// One parameterized query per blamed commit. The blame set for a single
	// range is small, and a per-SHA single-arg query keeps the call off the
	// dynamic []any args slice the surface's lint forbids outside db/dispatch.
	for _, sha := range shas {
		crows, err := pool.DB().QueryContext(ctx,
			`SELECT type, ts, COALESCE(rationale,''), entity_slug
			   FROM events
			  WHERE entity_kind='commit' AND entity_slug = ?
			  ORDER BY ts DESC
			  LIMIT 20`, sha)
		if err != nil {
			return nil, err
		}
		if err := scanProvenanceRows(crows, &events); err != nil {
			return nil, err
		}
	}
	return events, nil
}

func scanProvenanceRows(rows *sql.Rows, out *[]ProvenanceEvent) error {
	defer rows.Close()
	for rows.Next() {
		var e ProvenanceEvent
		if err := rows.Scan(&e.Type, &e.Ts, &e.Rationale, &e.EntitySlug); err != nil {
			return err
		}
		*out = append(*out, e)
	}
	return rows.Err()
}

// ── oriented mode ────────────────────────────────────────────────────────────

// OrientedView attaches the file's package intended-use block (from doc.go) and
// any related knowledge_pointers — the orientation the substrate-blind harness
// Read cannot supply.
type OrientedView struct {
	Package     string            `json:"package,omitempty"`
	IntendedUse string            `json:"intended_use,omitempty"`
	Pointers    []OrientedPointer `json:"pointers,omitempty"`
	Note        string            `json:"note,omitempty"`
}

// OrientedPointer is one related knowledge_pointers row.
type OrientedPointer struct {
	SourceType string `json:"source_type"`
	SourceRef  string `json:"source_ref"`
	Question   string `json:"question"`
	InvokeWhen string `json:"invoke_when"`
}

func readOriented(ctx context.Context, deps Deps, p ReadParams) (ReadResult, error) {
	if _, err := statFileForMode(p.FilePath); err != nil {
		return ReadResult{}, err
	}
	view := &OrientedView{}
	pkg, intended, note := packageIntendedUse(p.FilePath)
	view.Package = pkg
	view.IntendedUse = intended
	if note != "" {
		view.Note = note
	}

	if deps.Pool != nil {
		term := filepath.Base(filepath.Dir(absPath(p.FilePath)))
		if ptrs, err := queryRelatedPointers(ctx, deps.Pool, term); err == nil {
			view.Pointers = ptrs
		}
	}

	return ReadResult{FilePath: p.FilePath, Oriented: view}, nil
}

// packageIntendedUse extracts the package doc comment (the intended-use block)
// from the directory's doc.go, falling back to the file itself.
func packageIntendedUse(path string) (pkg, intended, note string) {
	dir := filepath.Dir(path)
	candidates := []string{filepath.Join(dir, "doc.go"), path}
	for _, c := range candidates {
		src, err := os.ReadFile(c)
		if err != nil {
			continue
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, c, src, parser.ParseComments|parser.PackageClauseOnly)
		if perr != nil {
			// PackageClauseOnly drops the doc comment; re-parse with comments.
			file, perr = parser.ParseFile(fset, c, src, parser.ParseComments)
			if perr != nil {
				continue
			}
		}
		if file.Doc != nil {
			return file.Name.Name, strings.TrimRight(file.Doc.Text(), "\n"), ""
		}
		pkg = file.Name.Name
	}
	if pkg == "" {
		return "", "", "no package intended-use block (no doc.go with a package comment in this directory)"
	}
	return pkg, "", "no package doc comment found in doc.go for package " + pkg
}

// queryRelatedPointers returns active knowledge_pointers whose source_ref,
// description, or question mentions term (the file's package directory name).
func queryRelatedPointers(ctx context.Context, pool *db.Pool, term string) ([]OrientedPointer, error) {
	like := "%" + term + "%"
	rows, err := pool.DB().QueryContext(ctx,
		`SELECT source_type, source_ref, question, invoke_when
		   FROM knowledge_pointers
		  WHERE status='active'
		    AND (source_ref LIKE ? OR description LIKE ? OR question LIKE ?)
		  ORDER BY COALESCE(quality_score, 0) DESC
		  LIMIT 10`, like, like, like)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ptrs []OrientedPointer
	for rows.Next() {
		var pt OrientedPointer
		if err := rows.Scan(&pt.SourceType, &pt.SourceRef, &pt.Question, &pt.InvokeWhen); err != nil {
			return nil, err
		}
		ptrs = append(ptrs, pt)
	}
	return ptrs, rows.Err()
}

// absPath resolves path to an absolute path, falling back to the input when the
// working directory is unavailable.
func absPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}
