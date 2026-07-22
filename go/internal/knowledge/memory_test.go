package knowledge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMemEntry writes a memory entry file under <root>/memory/<kind>/<name>.md
// with the given name/description/project frontmatter.
func writeMemEntry(t *testing.T, root, kind, name, desc, project string) {
	t.Helper()
	dir := filepath.Join(root, "memory", kind)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	proj := ""
	if project != "" {
		proj = "  project: " + project + "\n"
	}
	body := "---\nname: " + name + "\ndescription: " + desc + "\nmetadata:\n  type: " + kind + "\n" + proj + "---\n\nbody text\n"
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func memReadParams(t *testing.T, project, vaultRoot string) json.RawMessage {
	t.Helper()
	m := map[string]string{}
	if project != "" {
		m["project"] = project
	}
	if vaultRoot != "" {
		m["vault_root"] = vaultRoot
	}
	b, _ := json.Marshal(m)
	return b
}

func TestMemoryRead_Routing(t *testing.T) {
	root := t.TempDir()
	writeMemEntry(t, root, "user", "user-fact", "about Sophi", "mcp-servers")         // user → always
	writeMemEntry(t, root, "feedback", "fb-here", "scoped feedback", "mcp-servers")   // project match
	writeMemEntry(t, root, "project", "proj-other", "other project", "seed-packet")   // excluded
	writeMemEntry(t, root, "reference", "ref-fallback", "no project — fallback", "")  // empty → fallback include
	writeMemEntry(t, root, "feedback", "fb-other", "different project", "dm-toolkit") // excluded

	res, err := HandleMemoryRead(context.Background(), Deps{}, memReadParams(t, "mcp-servers", root))
	if err != nil {
		t.Fatalf("HandleMemoryRead: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	got := map[string]bool{}
	for _, e := range res.Entries {
		got[e.Name] = true
	}
	wantIn := []string{"user-fact", "fb-here", "ref-fallback"}
	for _, w := range wantIn {
		if !got[w] {
			t.Errorf("expected entry %q in result; entries=%v", w, got)
		}
	}
	for _, w := range []string{"proj-other", "fb-other"} {
		if got[w] {
			t.Errorf("entry %q should have been excluded", w)
		}
	}
	if res.EntryCount != 3 {
		t.Errorf("EntryCount = %d, want 3", res.EntryCount)
	}
	// Digest is one bullet per entry, sorted by name, with the description.
	if !strings.Contains(res.MemoryMarkdown, "- [fb-here](memory/feedback/fb-here.md) — scoped feedback\n") {
		t.Errorf("digest missing fb-here line:\n%s", res.MemoryMarkdown)
	}
	// Sorted by name: fb-here < ref-fallback < user-fact.
	if i, j, k := strings.Index(res.MemoryMarkdown, "fb-here"), strings.Index(res.MemoryMarkdown, "ref-fallback"), strings.Index(res.MemoryMarkdown, "user-fact"); !(i < j && j < k) {
		t.Errorf("entries not name-sorted:\n%s", res.MemoryMarkdown)
	}
}

func TestMemoryRead_MissingProject(t *testing.T) {
	res, err := HandleMemoryRead(context.Background(), Deps{}, memReadParams(t, "", t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Error == "" {
		t.Error("expected error for missing project")
	}
}

func TestMemoryRead_NoMemoryDir(t *testing.T) {
	// A vault root with no memory/ dir yields an empty (non-error) digest.
	res, err := HandleMemoryRead(context.Background(), Deps{}, memReadParams(t, "mcp-servers", t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Error != "" || res.EntryCount != 0 || res.MemoryMarkdown != "" {
		t.Errorf("want empty result, got %+v", res)
	}
}

func TestMemoryRead_SkipsMalformedAndNonMarkdown(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "memory", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// no frontmatter
	_ = os.WriteFile(filepath.Join(dir, "nofm.md"), []byte("just a body, no frontmatter\n"), 0o644)
	// unterminated frontmatter
	_ = os.WriteFile(filepath.Join(dir, "unterm.md"), []byte("---\nname: x\n"), 0o644)
	// non-markdown
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("---\nname: y\n---\n"), 0o644)
	// a nested dir (must be ignored)
	_ = os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	// valid one with no name → falls back to filename stem
	writeMemEntry(t, root, "user", "valid-entry", "", "")
	_ = os.WriteFile(filepath.Join(dir, "noname.md"), []byte("---\ndescription: d\nmetadata:\n  type: user\n---\n"), 0o644)

	res, err := HandleMemoryRead(context.Background(), Deps{}, memReadParams(t, "mcp-servers", root))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range res.Entries {
		names[e.Name] = true
	}
	if !names["valid-entry"] || !names["noname"] {
		t.Errorf("expected valid-entry + filename-fallback noname; got %v", names)
	}
	if names["x"] || names["y"] {
		t.Errorf("malformed/non-md entries leaked: %v", names)
	}
}

func TestMemoryRead_BadVaultRoot(t *testing.T) {
	// A vault_root that points at a file (not a dir) should surface a root error
	// or simply yield no entries — never panic.
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := HandleMemoryRead(context.Background(), Deps{}, memReadParams(t, "mcp-servers", f))
	if err != nil {
		t.Fatalf("should not hard-error: %v", err)
	}
	if res.EntryCount != 0 {
		t.Errorf("file-as-root should yield no entries, got %d", res.EntryCount)
	}
}

func TestRenderMemoryDigest_Empty(t *testing.T) {
	if got := renderMemoryDigest(nil); got != "" {
		t.Errorf("empty digest = %q, want empty", got)
	}
}
