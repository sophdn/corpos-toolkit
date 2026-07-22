package fs

// read_modes_test.go is the per-mode integration net for the OPT-IN
// substrate-native fs.read upgrades. The load-bearing invariant every test here
// protects is that the byte-parity default is untouched: TestReadModes_*Default*
// pins that a no-mode read marshals to exactly the historical shape, and each
// mode test exercises one opt-in param end to end.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

const sampleGo = `package sample

// Foo does a thing.
// More detail.
func Foo(x int) (int, error) {
	return x + 1, nil
}

// T is a sample type.
type T struct {
	A int
	B string
}

// Method greets.
func (t T) Method() string {
	return "hi"
}

const Answer = 42

var Global = "g"
`

// TestReadModes_DefaultMarshalUnchanged proves the opt-in mode fields never
// touch the default read: a no-mode read leaves every mode view nil and the
// marshaled JSON carries none of the new keys.
func TestReadModes_DefaultMarshalUnchanged(t *testing.T) {
	path := writeTemp(t, "f.txt", "alpha\nbeta\ngamma\n")
	got, err := HandleRead(context.Background(), mustJSON(t, ReadParams{FilePath: path}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Outline != nil || got.Symbol != nil || got.Provenance != nil || got.Oriented != nil {
		t.Fatalf("default read populated a mode view: %+v", got)
	}
	b, _ := json.Marshal(got)
	for _, key := range []string{"outline", "symbol", "provenance", "oriented"} {
		if strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("default read JSON leaked mode key %q: %s", key, b)
		}
	}
	// Router agrees: no-mode params resolve to the parity path.
	if readModeOf(mustJSON(t, ReadParams{FilePath: path})) != modeNone {
		t.Error("no-mode params should resolve to modeNone")
	}
}

func TestReadMode_Outline(t *testing.T) {
	path := writeTemp(t, "sample.go", sampleGo)
	got, err := handleReadMode(context.Background(), Deps{}, mustJSON(t, ReadParams{FilePath: path, Outline: true}))
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	if got.Outline == nil {
		t.Fatal("outline view missing")
	}
	o := got.Outline
	if o.Package != "sample" {
		t.Errorf("package = %q", o.Package)
	}
	if o.DeclCount != 5 {
		t.Errorf("decl_count = %d, want 5 (Foo, T, Method, Answer, Global)", o.DeclCount)
	}
	// The whole point: the outline is measurably smaller than the source.
	if o.OutlineBytes >= o.FullBytes {
		t.Errorf("outline (%d bytes) not smaller than full file (%d bytes)", o.OutlineBytes, o.FullBytes)
	}
	if o.FullBytes != len(sampleGo) {
		t.Errorf("full_bytes = %d, want %d", o.FullBytes, len(sampleGo))
	}
	// Signatures present; struct body omitted (that is where the shrink comes from).
	if !strings.Contains(got.Content, "func Foo(x int) (int, error)") {
		t.Errorf("outline missing Foo signature:\n%s", got.Content)
	}
	if !strings.Contains(got.Content, "type T struct") || strings.Contains(got.Content, "A int") {
		t.Errorf("type signature should collapse struct fields:\n%s", got.Content)
	}
	if !strings.Contains(got.Content, "Foo does a thing.") {
		t.Errorf("outline missing doc line:\n%s", got.Content)
	}
	// Kinds resolved for the receiver method and the value decls.
	kinds := map[string]string{}
	for _, d := range o.Decls {
		kinds[d.Name] = d.Kind
	}
	if kinds["T.Method"] != "method" || kinds["Foo"] != "func" || kinds["Answer"] != "const" || kinds["Global"] != "var" {
		t.Errorf("decl kinds wrong: %+v", kinds)
	}
}

func TestReadMode_OutlineRequiresGo(t *testing.T) {
	path := writeTemp(t, "f.txt", "not go\n")
	_, err := handleReadMode(context.Background(), Deps{}, mustJSON(t, ReadParams{FilePath: path, Outline: true}))
	if err == nil || !strings.Contains(err.Error(), "requires a Go source file") {
		t.Fatalf("expected go-only error, got %v", err)
	}
}

func TestReadMode_Symbol(t *testing.T) {
	path := writeTemp(t, "sample.go", sampleGo)
	cases := []struct {
		symbol         string
		wantKind       string
		wantContains   string
		wantFirstStart int
	}{
		{"Foo", "func", "func Foo(x int) (int, error)", 3}, // span begins at the doc comment
		{"T", "type", "type T struct", 9},                  // doc at line 9
		{"T.Method", "method", "func (t T) Method()", 15},  // method via Recv.Type
		{"Answer", "const", "const Answer = 42", 20},
	}
	for _, c := range cases {
		t.Run(c.symbol, func(t *testing.T) {
			got, err := handleReadMode(context.Background(), Deps{}, mustJSON(t, ReadParams{FilePath: path, Symbol: c.symbol}))
			if err != nil {
				t.Fatalf("symbol %q: %v", c.symbol, err)
			}
			if got.Symbol == nil {
				t.Fatalf("symbol view missing for %q", c.symbol)
			}
			if got.Symbol.Kind != c.wantKind {
				t.Errorf("kind = %q, want %q", got.Symbol.Kind, c.wantKind)
			}
			if got.Symbol.StartLine != c.wantFirstStart {
				t.Errorf("start_line = %d, want %d", got.Symbol.StartLine, c.wantFirstStart)
			}
			if !strings.Contains(got.Content, c.wantContains) {
				t.Errorf("span missing %q:\n%s", c.wantContains, got.Content)
			}
			// Content is numbered from the span start (parity format).
			wantPrefix := itoa(c.wantFirstStart) + "\t"
			if !strings.HasPrefix(got.Content, wantPrefix) {
				t.Errorf("content should start with %q, got %q", wantPrefix, firstLine(got.Content))
			}
		})
	}
}

func TestReadMode_SymbolNotFound(t *testing.T) {
	path := writeTemp(t, "sample.go", sampleGo)
	_, err := handleReadMode(context.Background(), Deps{}, mustJSON(t, ReadParams{FilePath: path, Symbol: "Nope"}))
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found, got %v", err)
	}
}

func TestReadMode_ProvenanceGitBlame(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	path := filepath.Join(dir, "code.txt")
	if err := os.WriteFile(path, []byte("line one\nline two\nline three\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommit(t, dir, "code.txt", "add the seed file")

	got, err := handleReadMode(context.Background(), Deps{}, mustJSON(t, ReadParams{FilePath: path, Provenance: true}))
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if got.Provenance == nil {
		t.Fatal("provenance view missing")
	}
	if len(got.Provenance.Commits) != 1 {
		t.Fatalf("expected 1 blame commit, got %+v (note=%q)", got.Provenance.Commits, got.Provenance.Note)
	}
	c := got.Provenance.Commits[0]
	if c.Summary != "add the seed file" {
		t.Errorf("summary (intent) = %q, want the commit subject", c.Summary)
	}
	if c.Lines != 3 {
		t.Errorf("attributed lines = %d, want 3", c.Lines)
	}
}

func TestReadMode_ProvenanceUntrackedIsSoft(t *testing.T) {
	// A file outside any git repo must not error — provenance is fail-soft.
	path := writeTemp(t, "loose.txt", "x\n")
	got, err := handleReadMode(context.Background(), Deps{}, mustJSON(t, ReadParams{FilePath: path, Provenance: true}))
	if err != nil {
		t.Fatalf("provenance should be fail-soft, got %v", err)
	}
	if got.Provenance == nil || got.Provenance.Note == "" {
		t.Errorf("expected an explanatory note for an untracked file, got %+v", got.Provenance)
	}
}

// TestReadMode_ProvenanceEvents proves the read half of the closed loop: an
// owned artifact event for the path surfaces in provenance. (The write half —
// fs.write/fs.edit emitting these — lands in the upgrade-write/edit tasks; here
// we insert the row directly to test the read-side fold in isolation.)
func TestReadMode_ProvenanceEvents(t *testing.T) {
	pool := testutil.NewTestDB(t)
	path := writeTemp(t, "tracked.txt", "x\n")
	insertArtifactEvent(t, pool, "ArtifactWritten", absPath(path), "wrote it deliberately")

	got, err := handleReadMode(context.Background(), Deps{Pool: pool}, mustJSON(t, ReadParams{FilePath: path, Provenance: true}))
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if got.Provenance == nil || len(got.Provenance.Events) != 1 {
		t.Fatalf("expected 1 substrate event, got %+v", got.Provenance)
	}
	e := got.Provenance.Events[0]
	if e.Type != "ArtifactWritten" || e.Rationale != "wrote it deliberately" {
		t.Errorf("event mismatch: %+v", e)
	}
}

func TestReadMode_Oriented(t *testing.T) {
	dir := t.TempDir()
	docGo := "// Package sample does sample things.\n//\n// Workflow served: testing oriented read.\npackage sample\n"
	if err := os.WriteFile(filepath.Join(dir, "doc.go"), []byte(docGo), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "thing.go")
	if err := os.WriteFile(target, []byte("package sample\n\nfunc Thing() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := handleReadMode(context.Background(), Deps{}, mustJSON(t, ReadParams{FilePath: target, Oriented: true}))
	if err != nil {
		t.Fatalf("oriented: %v", err)
	}
	if got.Oriented == nil {
		t.Fatal("oriented view missing")
	}
	if got.Oriented.Package != "sample" {
		t.Errorf("package = %q", got.Oriented.Package)
	}
	if !strings.Contains(got.Oriented.IntendedUse, "Workflow served: testing oriented read.") {
		t.Errorf("intended-use block not extracted from doc.go:\n%q", got.Oriented.IntendedUse)
	}
}

func TestReadMode_OrientedPointers(t *testing.T) {
	pool := testutil.NewTestDB(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "thing.go")
	if err := os.WriteFile(target, []byte("package sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	term := filepath.Base(dir) // the matcher keys off the package directory name
	insertPointer(t, pool, "vault-note", "decisions/"+term+"-note.md", "When do I X?", "during X")

	got, err := handleReadMode(context.Background(), Deps{Pool: pool}, mustJSON(t, ReadParams{FilePath: target, Oriented: true}))
	if err != nil {
		t.Fatalf("oriented: %v", err)
	}
	if got.Oriented == nil || len(got.Oriented.Pointers) != 1 {
		t.Fatalf("expected 1 related pointer, got %+v", got.Oriented)
	}
	if got.Oriented.Pointers[0].Question != "When do I X?" {
		t.Errorf("pointer mismatch: %+v", got.Oriented.Pointers[0])
	}
}

// ── test helpers ─────────────────────────────────────────────────────────────

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func gitCommit(t *testing.T, dir, file, msg string) {
	t.Helper()
	for _, args := range [][]string{
		{"add", file},
		{"commit", "-q", "-m", msg},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func insertArtifactEvent(t *testing.T, pool *db.Pool, typ, filePath, rationale string) {
	t.Helper()
	payload := `{"file_path":` + jsonString(filePath) + `}`
	_, err := pool.DB().Exec(
		`INSERT INTO events (event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug, payload, rationale, related_entities, span_id, schema_version)
		 VALUES (?, ?, 'system', 'test', ?, 'artifact', ?, ?, ?, '[]', ?, 1)`,
		"ev-"+typ+"-1", "2026-05-30T00:00:00.000Z", typ, filePath, payload, rationale, "span-1",
	)
	if err != nil {
		t.Fatalf("insert artifact event: %v", err)
	}
}

func insertPointer(t *testing.T, pool *db.Pool, sourceType, sourceRef, question, invokeWhen string) {
	t.Helper()
	_, err := pool.DB().Exec(
		`INSERT INTO knowledge_pointers (project_id, source_type, source_ref, question, invoke_when, status)
		 VALUES ('mcp-servers', ?, ?, ?, ?, 'active')`,
		sourceType, sourceRef, question, invokeWhen,
	)
	if err != nil {
		t.Fatalf("insert pointer: %v", err)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
