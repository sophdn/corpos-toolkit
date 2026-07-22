package actiondocs_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"toolkit/internal/actiondocs"
)

// TestCorpus_NoUndecodedKeys is the structural guard against the
// silent-drop class found in bug
// action-doc-toml-notes-after-table-array-silently-dropped-on-non-work-
// surfaces: when a hand-authored chunk places a top-level scalar (most
// often `notes`) AFTER a [[params]]/[[errors]]/[[examples]] table array,
// BurntSushi/toml attaches that scalar to the LAST table element instead
// of the ActionDoc top level. The target struct (Param/ErrorCondition/
// Example/…) has no such field, so the value is silently dropped on
// decode — no ParseError, just missing prose at action_describe time.
//
// The guard re-decodes every corpus chunk and asserts MetaData.Undecoded()
// is empty. Any key present in the TOML that did not bind to an ActionDoc
// field is exactly the silent-drop signal — whether it's a misplaced
// top-level scalar, a typo'd key, or a struct field that was never wired.
// This catches the whole class across ALL surfaces (work/knowledge/
// measure/admin) and any future hand-authored chunk, not just the eight
// instances found at filing time.
//
// The test walks the on-disk corpus (the source the embedded set is built
// from — embed==disk is independently pinned by
// TestLoadEmbedded_MatchesOnDiskCorpus), so a fix is incomplete until this
// is green over the whole tree.
func TestCorpus_NoUndecodedKeys(t *testing.T) {
	const root = "corpus"

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".toml") {
			return nil
		}
		// corpus/_schema.toml describes the corpus; it is not an
		// ActionDoc chunk and Load skips it. Skip it here too.
		if filepath.Dir(path) == root {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Errorf("%s: read: %v", path, readErr)
			return nil
		}

		var doc actiondocs.ActionDoc
		md, decErr := toml.Decode(string(data), &doc)
		if decErr != nil {
			t.Errorf("%s: decode: %v", path, decErr)
			return nil
		}

		if undecoded := md.Undecoded(); len(undecoded) > 0 {
			keys := make([]string, 0, len(undecoded))
			for _, k := range undecoded {
				keys = append(keys, k.String())
			}
			t.Errorf("%s: %d undecoded TOML key(s) — silent-drop risk "+
				"(a top-level scalar such as `notes` placed AFTER a [[table]] "+
				"array binds to the last table element and is dropped; move all "+
				"top-level scalars ABOVE the first [[table]]): %s",
				path, len(undecoded), strings.Join(keys, ", "))
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", root, walkErr)
	}
}
