package vault

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fixtureVault builds a temp vault mirroring the Rust fixture_vault helper.
// Returns the canonicalized root path.
func fixtureVault(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, sub := range []string{"decisions", "learnings/general", "learnings/llama-server", "reference"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	must := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, p), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	must("decisions/2026-05-05_toolkit-canonical.md",
		"---\ntitle: Toolkit-server canonical core\ntags: [toolkit, decisions]\n---\n\n# Body heading\n\nDecision body here.\n")
	must("learnings/llama-server/2026-05-04_flash-attn.md",
		"# llama-server flash-attn flag\n\nNotes about the flag.\n")
	must("learnings/general/malformed.md",
		"---\ntitle: \"oh no\nbroken: yaml\n---\n\nBody despite broken YAML.\n")
	must(".hidden.md", "# Hidden\n")
	if err := os.MkdirAll(filepath.Join(root, ".obsidian"), 0o755); err != nil {
		t.Fatalf("mkdir .obsidian: %v", err)
	}
	must(".obsidian/workspace.json", "{}")
	must("README.txt", "not a note")
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	return canonical
}

func TestWalk_ReturnsOnlyMarkdownSkipsHiddenAndObsidian(t *testing.T) {
	root := fixtureVault(t)
	entries, err := Walk(root)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.Path
	}
	want := []string{
		"decisions/2026-05-05_toolkit-canonical.md",
		"learnings/general/malformed.md",
		"learnings/llama-server/2026-05-04_flash-attn.md",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths mismatch:\n got %v\nwant %v", paths, want)
	}
}

func TestWalk_ExtractsTitleFromFrontmatterThenH1ThenFilename(t *testing.T) {
	root := fixtureVault(t)
	entries, err := Walk(root)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	byPath := map[string]Entry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}
	if got := byPath["decisions/2026-05-05_toolkit-canonical.md"].Title; got != "Toolkit-server canonical core" {
		t.Errorf("frontmatter title must win; got %q", got)
	}
	if got := byPath["learnings/llama-server/2026-05-04_flash-attn.md"].Title; got != "llama-server flash-attn flag" {
		t.Errorf("H1 must be used when no frontmatter title; got %q", got)
	}
	if got := byPath["learnings/general/malformed.md"].Title; got != "malformed" {
		t.Errorf("malformed frontmatter must degrade to filename; got %q", got)
	}
}

func TestWalk_ExtractsSummaryFromFirstBodyParagraph(t *testing.T) {
	root := fixtureVault(t)
	entries, _ := Walk(root)
	byPath := map[string]Entry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}
	if got := byPath["decisions/2026-05-05_toolkit-canonical.md"].Summary; got != "Decision body here." {
		t.Errorf("frontmatter+H1 case: summary = %q", got)
	}
	if got := byPath["learnings/llama-server/2026-05-04_flash-attn.md"].Summary; got != "Notes about the flag." {
		t.Errorf("H1+body case: summary = %q", got)
	}
}

func TestWalk_ExtractsTagsLowercasedDeduped(t *testing.T) {
	root := fixtureVault(t)
	entries, _ := Walk(root)
	var canonical Entry
	for _, e := range entries {
		if e.Path == "decisions/2026-05-05_toolkit-canonical.md" {
			canonical = e
			break
		}
	}
	want := []string{"decisions", "toolkit"}
	if !reflect.DeepEqual(canonical.Tags, want) {
		t.Errorf("tags mismatch: got %v want %v", canonical.Tags, want)
	}
}

func TestFirstBodyExcerpt_TruncatesLongLinesAtWordBoundary(t *testing.T) {
	body := "First very long opening line that exceeds the max char cap and should be cut at a word boundary then suffixed with an ellipsis to indicate truncation."
	excerpt := FirstBodyExcerpt(body, 60)
	if !endsWithEllipsis(excerpt) {
		t.Errorf("expected trailing ellipsis, got %q", excerpt)
	}
	if got := []rune(excerpt); len(got) > 61 {
		t.Errorf("char count > 61 (60+ellipsis): got %d (%q)", len(got), excerpt)
	}
}

func TestFirstBodyExcerpt_SkipsHeadingsBulletsBlockquotes(t *testing.T) {
	body := "# Heading\n\n- bullet item\n* another bullet\n> quote\n\nReal first paragraph here."
	if got := FirstBodyExcerpt(body, 100); got != "Real first paragraph here." {
		t.Errorf("got %q", got)
	}
}

func TestFirstBodyExcerpt_EmptyOnNoBody(t *testing.T) {
	for _, in := range []string{"", "\n\n\n", "# Only a heading\n"} {
		if got := FirstBodyExcerpt(in, 100); got != "" {
			t.Errorf("expected empty for %q, got %q", in, got)
		}
	}
}

// ── ReadNote ──────────────────────────────────────────────────────────

func TestReadNote_ReturnsFrontmatterAsStructuredValue(t *testing.T) {
	root := fixtureVault(t)
	note, err := ReadNote(root, "decisions/2026-05-05_toolkit-canonical.md")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if note.Path != "decisions/2026-05-05_toolkit-canonical.md" {
		t.Errorf("path wrong: %s", note.Path)
	}
	if note.Frontmatter == nil {
		t.Fatalf("frontmatter unexpectedly nil")
	}
	if note.Frontmatter.Title != "Toolkit-server canonical core" {
		t.Errorf("title: %q", note.Frontmatter.Title)
	}
	if note.FrontmatterWarning != "" {
		t.Errorf("happy path must have no warning: %q", note.FrontmatterWarning)
	}
}

func TestReadNote_NoFrontmatterReturnsFullContent(t *testing.T) {
	root := fixtureVault(t)
	note, err := ReadNote(root, "learnings/llama-server/2026-05-04_flash-attn.md")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if note.Frontmatter != nil {
		t.Errorf("expected nil frontmatter")
	}
	if note.Content == "" || note.Content[:1] != "#" {
		t.Errorf("content must start with H1: %q", note.Content)
	}
}

func TestReadNote_MalformedFrontmatterDegradesGracefully(t *testing.T) {
	root := fixtureVault(t)
	note, err := ReadNote(root, "learnings/general/malformed.md")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if note.FrontmatterWarning == "" {
		t.Errorf("warning must be set when frontmatter parse fails")
	}
}

func TestReadNote_RejectsPathTraversal(t *testing.T) {
	root := fixtureVault(t)
	// Create a sibling temp file outside the vault.
	sibling := filepath.Join(filepath.Dir(root), "escape-target.txt")
	if err := os.WriteFile(sibling, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write sibling: %v", err)
	}
	defer os.Remove(sibling)
	_, err := ReadNote(root, "../escape-target.txt")
	if err == nil {
		t.Fatal("expected traversal error, got nil")
	}
	if !errors.Is(err, ErrPathTraversal) && !errors.Is(err, ErrNoteNotFound) {
		t.Errorf("expected traversal or not-found, got %v", err)
	}
}

func TestReadNote_RejectsAbsolutePath(t *testing.T) {
	root := fixtureVault(t)
	_, err := ReadNote(root, "/etc/passwd")
	if !errors.Is(err, ErrPathTraversal) {
		t.Errorf("expected traversal, got %v", err)
	}
}

func TestReadNote_RejectsEmptyPath(t *testing.T) {
	root := fixtureVault(t)
	_, err := ReadNote(root, "")
	if !errors.Is(err, ErrPathTraversal) {
		t.Errorf("expected traversal, got %v", err)
	}
}

func TestReadNote_UnknownPathIsNotFoundNotTraversal(t *testing.T) {
	root := fixtureVault(t)
	_, err := ReadNote(root, "decisions/no-such-file.md")
	if !errors.Is(err, ErrNoteNotFound) {
		t.Errorf("expected not-found, got %v", err)
	}
}

// ── ReadNoteBodyExcerpt ───────────────────────────────────────────────

func TestReadNoteBodyExcerpt_UnderCap(t *testing.T) {
	root := fixtureVault(t)
	body, err := ReadNoteBodyExcerpt(root, "decisions/2026-05-05_toolkit-canonical.md", 500)
	if err != nil {
		t.Fatalf("excerpt: %v", err)
	}
	if endsWithEllipsis(body) {
		t.Errorf("no ellipsis under cap, got %q", body)
	}
	if want := "# Body heading"; len(body) < len(want) || body[:len(want)] != want {
		t.Errorf("body must start at first post-frontmatter line; got %q", body)
	}
}

func TestReadNoteBodyExcerpt_TruncatesLongBody(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "learnings/general"), 0o755); err != nil {
		t.Fatal(err)
	}
	long := ""
	for i := 0; i < 50; i++ {
		long += "lorem ipsum dolor sit amet "
	}
	body := "---\ntitle: long\n---\n\n" + long
	if err := os.WriteFile(filepath.Join(root, "learnings/general/long.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.EvalSymlinks(root)
	excerpt, err := ReadNoteBodyExcerpt(root, "learnings/general/long.md", 80)
	if err != nil {
		t.Fatalf("excerpt: %v", err)
	}
	if !endsWithEllipsis(excerpt) {
		t.Errorf("expected ellipsis on truncation, got %q", excerpt)
	}
}

func TestReadNoteBodyExcerpt_RejectsPathTraversal(t *testing.T) {
	root := fixtureVault(t)
	_, err := ReadNoteBodyExcerpt(root, "../etc/passwd", 100)
	if !errors.Is(err, ErrPathTraversal) && !errors.Is(err, ErrNoteNotFound) {
		t.Errorf("expected rejection, got %v", err)
	}
}

func TestValidatePath_RejectsSymlinkEscape(t *testing.T) {
	root := fixtureVault(t)
	outside := filepath.Join(filepath.Dir(root), "outside.md")
	if err := os.WriteFile(outside, []byte("# outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)
	link := filepath.Join(root, "escape.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	defer os.Remove(link)
	_, err := ReadNote(root, "escape.md")
	if !errors.Is(err, ErrPathTraversal) {
		t.Errorf("expected traversal, got %v", err)
	}
}

func TestResolveRoot_RejectsMissingRoot(t *testing.T) {
	_, err := ResolveRoot("/tmp/no-such-vault-root-xyz-12345")
	if !errors.Is(err, ErrRootMissing) {
		t.Errorf("expected ErrRootMissing, got %v", err)
	}
}

func TestResolveRoot_RejectsNonDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveRoot(file)
	if !errors.Is(err, ErrRootNotDir) {
		t.Errorf("expected ErrRootNotDir, got %v", err)
	}
}

// TestResolveRoot_ForgeMarkdownRoot exercises the symmetry-with-forge
// precedence step: FORGE_MARKDOWN_ROOT set, TOOLKIT_VAULT_ROOT unset.
// vault_search resolves to "<FORGE_MARKDOWN_ROOT>/vault" so forge writes
// and vault reads agree without callers having to set two env vars.
func TestResolveRoot_ForgeMarkdownRoot(t *testing.T) {
	parent := t.TempDir()
	vaultDir := filepath.Join(parent, "vault")
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(VaultRootEnv, "")
	t.Setenv(ForgeMarkdownRootEnv, parent)
	got, err := ResolveRoot("")
	if err != nil {
		t.Fatalf("ResolveRoot: %v", err)
	}
	// Canonicalize the expected path for the symlink-resolved comparison
	// (macOS temp dirs land under /var/folders → /private/var/folders).
	want, err := filepath.EvalSymlinks(vaultDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestResolveRoot_VaultRootBeatsForgeMarkdownRoot pins the precedence so a
// caller that intentionally separates the read root from the forge write
// root (e.g. read-only sandbox against a fixture vault, fresh write target
// per test) sees TOOLKIT_VAULT_ROOT win.
func TestResolveRoot_VaultRootBeatsForgeMarkdownRoot(t *testing.T) {
	readRoot := t.TempDir()
	writeRoot := t.TempDir()
	// Make both candidate paths exist as dirs so the assertion is about
	// precedence, not about a missing-root short-circuit.
	if err := os.MkdirAll(filepath.Join(writeRoot, "vault"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(VaultRootEnv, readRoot)
	t.Setenv(ForgeMarkdownRootEnv, writeRoot)
	got, err := ResolveRoot("")
	if err != nil {
		t.Fatalf("ResolveRoot: %v", err)
	}
	want, _ := filepath.EvalSymlinks(readRoot)
	if got != want {
		t.Errorf("TOOLKIT_VAULT_ROOT should win: got %q, want %q", got, want)
	}
}

// ── KeywordScore / KeywordPrefilter ───────────────────────────────────

func entry(p, title string, tags []string, summary string) Entry {
	return Entry{Path: p, Title: title, Tags: tags, Summary: summary}
}

func TestKeywordScore_MatchesTitleAndTags(t *testing.T) {
	e := entry("decisions/2026-05-12_build-rs-auto-embed.md",
		"build.rs auto-embed file registry",
		[]string{"build", "rust", "migrations"},
		"Replace manual include_str list with build.rs scanning.")
	if got := KeywordScore(e, "build.rs embedded file list"); got <= 0 {
		t.Errorf("expected positive score, got %f", got)
	}
}

func TestKeywordScore_ZeroOnNoOverlap(t *testing.T) {
	e := entry("reference/kubernetes.md", "Kubernetes cluster setup",
		[]string{"k8s", "docker"}, "Pod scheduling.")
	if got := KeywordScore(e, "rust migrations build script"); got != 0 {
		t.Errorf("expected zero, got %f", got)
	}
}

func TestKeywordPrefilter_RanksRelevantFirst(t *testing.T) {
	relevant := entry("learnings/general/build-rs.md", "build.rs migration registry",
		[]string{"rust", "build"}, "Auto-discover migrations using build.rs.")
	irrelevant := entry("reference/k8s.md", "Kubernetes networking",
		[]string{"k8s", "docker"}, "Pod scheduling.")
	got := KeywordPrefilter([]Entry{irrelevant, relevant}, "build.rs rust", 2)
	if got[0].Path != relevant.Path {
		t.Errorf("relevant must rank first; got %v", got)
	}
}

func TestKeywordPrefilter_FallsBackToOriginalOrderOnZeroScores(t *testing.T) {
	a := entry("a/note.md", "Alpha note", nil, "First entry.")
	b := entry("b/note.md", "Beta note", nil, "Second entry.")
	got := KeywordPrefilter([]Entry{a, b}, "kubernetes docker", 2)
	if got[0].Path != a.Path || got[1].Path != b.Path {
		t.Errorf("expected original order, got %v", got)
	}
}

// Bug 1324 regression: KeywordScore considers body content, not just
// path / title / tags / summary. Before this fix, an older note whose
// only query-matching tokens lived past the 160-char Summary cap scored
// 0 and lost ground to newer non-matching notes that happened to have
// matching titles.
func TestKeywordScore_MatchesBodyForScoring(t *testing.T) {
	bodyOnly := Entry{
		Path:    "learnings/general/2025-12-01_polymorphic-refs.md",
		Title:   "Generic schema notes",
		Tags:    []string{"sql"},
		Summary: "A note that covers several adjacent topics in passing.",
		BodyForScoring: "When you write a polymorphic_reference table that holds " +
			"both chain and task targets, the ref_kind + ref_slug pattern …",
	}
	titleHasNoMatch := Entry{
		Path:           "learnings/general/2026-05-14_unrelated.md",
		Title:          "Unrelated note",
		Tags:           nil,
		Summary:        "Nothing to see here.",
		BodyForScoring: "Nothing to see here either.",
	}
	if KeywordScore(titleHasNoMatch, "polymorphic_reference ref_kind") > 0 {
		t.Errorf("unrelated entry should score zero")
	}
	if KeywordScore(bodyOnly, "polymorphic_reference ref_kind") == 0 {
		t.Errorf("body-keyword-only entry should score above zero")
	}
}

// Bug 1324 regression: an older note whose body matches the query
// outranks a newer note whose only signal is recency. Before the body
// term was added to KeywordScore, this case fell back to walk-order
// (which path-sorts older dates first within a directory, so the
// "newer non-matching" entry would still appear first when both
// scored zero — masking the failure mode the bug describes).
//
// With the body term, the older keyword-matching note rises above the
// newer non-matching one regardless of walk position.
func TestKeywordPrefilter_OlderBodyMatchBeatsNewerNonMatch(t *testing.T) {
	olderMatching := Entry{
		Path:           "learnings/general/2025-09-01_old-but-relevant.md",
		Title:          "Generic title",
		Tags:           []string{"general"},
		Summary:        "Generic summary.",
		BodyForScoring: "Detailed treatment of the dependency-injection container shape we settled on.",
	}
	newerIrrelevant := Entry{
		Path:           "decisions/2026-05-15_new-but-unrelated.md",
		Title:          "Storage selection",
		Tags:           []string{"storage"},
		Summary:        "Comparing key-value stores.",
		BodyForScoring: "We compared etcd, consul, and zookeeper for the leader-election path.",
	}
	got := KeywordPrefilter(
		[]Entry{newerIrrelevant, olderMatching},
		"dependency-injection container",
		2,
	)
	if got[0].Path != olderMatching.Path {
		t.Errorf("older body-matching entry must rank first; got order %v",
			[]string{got[0].Path, got[1].Path})
	}
}

func TestKeywordPrefilter_RespectsLimit(t *testing.T) {
	var entries []Entry
	for i := 0; i < 20; i++ {
		entries = append(entries, entry("notes/note.md", "Note", nil, ""))
	}
	if got := KeywordPrefilter(entries, "note", 5); len(got) != 5 {
		t.Errorf("expected 5, got %d", len(got))
	}
}

func endsWithEllipsis(s string) bool {
	return len(s) >= 3 && s[len(s)-3:] == "…"
}
