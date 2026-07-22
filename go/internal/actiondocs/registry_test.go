package actiondocs_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"toolkit/internal/actiondocs"
)

// copyFile assembles a corpus inside a t.TempDir() by copying one
// testdata chunk into a chosen target path. Mirrors the forge/registry
// test pattern.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(dst), err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func mustLoad(t *testing.T, dir string) *actiondocs.Registry {
	t.Helper()
	r, err := actiondocs.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return r
}

func TestRegistry_HappyPath(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, "testdata/bug_resolve.toml", filepath.Join(dir, "work", "bug_resolve.toml"))
	copyFile(t, "testdata/forge_schemas.toml", filepath.Join(dir, "work", "forge_schemas.toml"))
	copyFile(t, "testdata/_general.toml", filepath.Join(dir, "work", "_general.toml"))

	r := mustLoad(t, dir)
	if got, want := r.Len(), 3; got != want {
		t.Errorf("Len: want %d, got %d", want, got)
	}
	if errs := r.ParseErrors(); len(errs) != 0 {
		t.Errorf("ParseErrors: want empty, got %+v", errs)
	}

	doc, ok := r.Get("work", "bug_resolve")
	if !ok {
		t.Fatal("bug_resolve not found")
	}
	if doc.Purpose == "" {
		t.Error("bug_resolve.purpose empty after load")
	}
	if len(doc.Params) != 3 {
		t.Errorf("bug_resolve.params: want 3, got %d", len(doc.Params))
	}
	if len(doc.ParamAliases) != 3 {
		t.Errorf("bug_resolve.param_aliases: want 3, got %d", len(doc.ParamAliases))
	}
	if len(doc.ValueAliases) != 5 {
		t.Errorf("bug_resolve.value_aliases: want 5, got %d", len(doc.ValueAliases))
	}

	thin, ok := r.Get("work", "forge_schemas")
	if !ok {
		t.Fatal("forge_schemas not found")
	}
	if len(thin.Params) != 0 {
		t.Errorf("forge_schemas.params: want 0, got %d", len(thin.Params))
	}
}

func TestRegistry_GeneralChunkExcludedFromList(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, "testdata/bug_resolve.toml", filepath.Join(dir, "work", "bug_resolve.toml"))
	copyFile(t, "testdata/forge_schemas.toml", filepath.Join(dir, "work", "forge_schemas.toml"))
	copyFile(t, "testdata/_general.toml", filepath.Join(dir, "work", "_general.toml"))

	r := mustLoad(t, dir)

	general, ok := r.Get("work", actiondocs.GeneralAction)
	if !ok {
		t.Fatal("_general chunk should be findable via Get")
	}
	if general.Purpose == "" {
		t.Error("_general.purpose empty after load")
	}

	listed := r.List("work")
	if len(listed) != 2 {
		t.Fatalf("List(work): want 2 (bug_resolve + forge_schemas, _general excluded), got %d", len(listed))
	}
	for _, d := range listed {
		if d.Action == actiondocs.GeneralAction {
			t.Errorf("List(work) returned reserved _general chunk: %+v", d)
		}
	}

	names := r.Names("work")
	if got, want := strings.Join(names, ","), "bug_resolve,forge_schemas"; got != want {
		t.Errorf("Names(work): want %q, got %q", want, got)
	}
}

func TestRegistry_GetMiss(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, "testdata/bug_resolve.toml", filepath.Join(dir, "work", "bug_resolve.toml"))

	r := mustLoad(t, dir)
	if _, ok := r.Get("knowledge", "vault_search"); ok {
		t.Error("expected miss on unknown surface")
	}
	if _, ok := r.Get("work", "no_such_action"); ok {
		t.Error("expected miss on unknown action under known surface")
	}
}

func TestRegistry_MissingRequiredFieldRejected(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, "testdata/bug_resolve.toml", filepath.Join(dir, "work", "bug_resolve.toml"))
	copyFile(t, "testdata/missing-purpose.toml", filepath.Join(dir, "work", "missing-purpose.toml"))

	r := mustLoad(t, dir)
	if _, ok := r.Get("work", "missing-purpose"); ok {
		t.Error("missing-purpose chunk should be excluded after required-field rejection")
	}

	// The good chunk should still load — one bad chunk does not abort
	// the whole registration.
	if _, ok := r.Get("work", "bug_resolve"); !ok {
		t.Error("bug_resolve should still load alongside a rejected sibling chunk")
	}

	errs := r.ParseErrors()
	if len(errs) != 1 {
		t.Fatalf("ParseErrors: want 1, got %d (%+v)", len(errs), errs)
	}
	if !strings.Contains(errs[0].Err, "purpose is required") {
		t.Errorf("ParseError.Err: want substring 'purpose is required', got %q", errs[0].Err)
	}
	if !strings.HasSuffix(errs[0].SourceFile, "missing-purpose.toml") {
		t.Errorf("ParseError.SourceFile: want suffix 'missing-purpose.toml', got %q", errs[0].SourceFile)
	}
}

func TestRegistry_SurfaceMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, "testdata/surface_mismatch.toml", filepath.Join(dir, "work", "surface_mismatch.toml"))

	r := mustLoad(t, dir)
	if _, ok := r.Get("work", "surface_mismatch"); ok {
		t.Error("surface-mismatch chunk should be rejected")
	}
	errs := r.ParseErrors()
	if len(errs) != 1 {
		t.Fatalf("ParseErrors: want 1, got %d", len(errs))
	}
	if !strings.Contains(errs[0].Err, "surface mismatch") {
		t.Errorf("ParseError.Err: want substring 'surface mismatch', got %q", errs[0].Err)
	}
}

func TestRegistry_ActionMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, "testdata/action_mismatch.toml", filepath.Join(dir, "work", "action_mismatch.toml"))

	r := mustLoad(t, dir)
	if _, ok := r.Get("work", "action_mismatch"); ok {
		t.Error("action-mismatch chunk should be rejected (by file name)")
	}
	if _, ok := r.Get("work", "totally-different-name"); ok {
		t.Error("action-mismatch chunk should be rejected (by declared action)")
	}
	errs := r.ParseErrors()
	if len(errs) != 1 {
		t.Fatalf("ParseErrors: want 1, got %d", len(errs))
	}
	if !strings.Contains(errs[0].Err, "action mismatch") {
		t.Errorf("ParseError.Err: want substring 'action mismatch', got %q", errs[0].Err)
	}
}

// Regression test for bug `action-docs-returns-and-envelope-requirements-
// not-rendered`. Pre-fix the ActionDoc struct had no Returns field, so
// the TOML parser silently dropped any [returns] block — authors thought
// they were documenting return shape; the wire response carried nothing.
// Now [returns] survives load + carries through to JSON.
func TestRegistry_ReturnsBlockParsesAndSurvives(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, "testdata/returns_doc.toml", filepath.Join(dir, "work", "returns_doc.toml"))

	r := mustLoad(t, dir)
	doc, ok := r.Get("work", "returns_doc")
	if !ok {
		t.Fatal("returns_doc not found")
	}
	if doc.Returns == nil {
		t.Fatal("doc.Returns nil — [returns] block was dropped at load (regression)")
	}
	if got, want := doc.Returns.Shape, "ReturnsDocResult"; got != want {
		t.Errorf("Returns.Shape = %q, want %q", got, want)
	}
	if !strings.Contains(doc.Returns.Description, "ok") {
		t.Errorf("Returns.Description = %q, want to mention 'ok'", doc.Returns.Description)
	}
}

func TestRegistry_EmptyDirReturnsEmpty(t *testing.T) {
	dir := t.TempDir()

	r := mustLoad(t, dir)
	if r.Len() != 0 {
		t.Errorf("Len: want 0 for empty corpus, got %d", r.Len())
	}
	if errs := r.ParseErrors(); len(errs) != 0 {
		t.Errorf("ParseErrors: want empty, got %+v", errs)
	}
	if got := r.Surfaces(); len(got) != 0 {
		t.Errorf("Surfaces: want empty, got %+v", got)
	}
	if got := r.List("work"); got != nil {
		t.Errorf("List(unknown-surface): want nil, got %+v", got)
	}
}

func TestRegistry_NonexistentDirReturnsEmpty(t *testing.T) {
	r, err := actiondocs.Load(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("Load(missing dir): want nil error, got %v", err)
	}
	if r.Len() != 0 {
		t.Errorf("Len: want 0 for missing dir, got %d", r.Len())
	}
}

// TestLoadEmbedded_MatchesOnDiskCorpus pins the production invariant
// introduced in chain single-source-action-describe T6: the corpus baked
// into the binary via go:embed is identical to loading the on-disk source
// dir. This is what lets flagless stdio serve admin.action_describe with
// full docs — no --action-docs-dir needed. It also guards the //go:embed
// directive against drifting from the source tree (a newly added surface
// dir the directive silently fails to pick up would show as count/surface
// drift here).
func TestLoadEmbedded_MatchesOnDiskCorpus(t *testing.T) {
	embedded, err := actiondocs.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	if embedded.Len() == 0 {
		t.Fatal("embedded corpus empty — go:embed directive is not picking up corpus/")
	}
	// go test runs with cwd at the package dir, so the on-disk source is ./corpus.
	onDisk, err := actiondocs.Load(filepath.Clean("corpus"))
	if err != nil {
		t.Fatalf("Load(on-disk corpus): %v", err)
	}
	if embedded.Len() != onDisk.Len() {
		t.Fatalf("chunk-count drift: embedded=%d on-disk=%d", embedded.Len(), onDisk.Len())
	}
	if got, want := embedded.Surfaces(), onDisk.Surfaces(); !slices.Equal(got, want) {
		t.Fatalf("surface drift: embedded=%v on-disk=%v", got, want)
	}
	for _, surf := range onDisk.Surfaces() {
		if got, want := embedded.Names(surf), onDisk.Names(surf); !slices.Equal(got, want) {
			t.Errorf("surface %q action drift: embedded=%v on-disk=%v", surf, got, want)
		}
	}
}

func TestRegistry_TopLevelFilesIgnored(t *testing.T) {
	// _schema.toml and README.md live at the corpus root, not inside a
	// surface dir. The loader must skip them — they describe the corpus,
	// they are not chunks.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "_schema.toml"), []byte("schema_version = 1\n"), 0o600); err != nil {
		t.Fatalf("write _schema.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# corpus\n"), 0o600); err != nil {
		t.Fatalf("write README: %v", err)
	}
	copyFile(t, "testdata/forge_schemas.toml", filepath.Join(dir, "work", "forge_schemas.toml"))

	r := mustLoad(t, dir)
	if r.Len() != 1 {
		t.Errorf("Len: want 1 (forge_schemas only), got %d", r.Len())
	}
	if errs := r.ParseErrors(); len(errs) != 0 {
		t.Errorf("ParseErrors: want empty (top-level files skipped), got %+v", errs)
	}
}
