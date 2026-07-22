package refresolve

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// SkillManifestEntry mirrors one [[skill]] block from
// mcp-servers/skills/_manifest.toml. Only fields the parse_context
// resolvers consume are unmarshalled; other fields stay in the file
// without producing an unmarshal error (BurntSushi/toml ignores
// unknown keys when the target is a struct).
//
// Authored by reference-resolution-migration T3 (self-containment
// migration). reference-resolution-migration T5 reads it here to
// power the skill_trigger + discipline_skill resolvers.
type SkillManifestEntry struct {
	Name            string   `toml:"name"`
	BodyPath        string   `toml:"body_path"`
	InstallTarget   string   `toml:"install_target"`
	Bucket          string   `toml:"bucket"`
	TriggerKeywords []string `toml:"trigger_keywords"`
	Description     string   `toml:"description"`
	Origin          string   `toml:"origin"`
}

// SkillManifest is the parsed representation of skills/_manifest.toml
// the parse_context resolvers read at handler startup. Tests build
// one inline; production code calls LoadSkillManifest.
type SkillManifest struct {
	Skills []SkillManifestEntry `toml:"skill"`
}

// LoadSkillManifest parses skills/_manifest.toml under repoRoot.
// Returns nil + nil if the file is absent (the manifest is
// optional — handlers degrade to "no skill_trigger results"
// rather than failing). Returns nil + error on parse failure so
// startup surfaces the misconfiguration loudly.
func LoadSkillManifest(repoRoot string) (*SkillManifest, error) {
	if repoRoot == "" {
		return nil, nil
	}
	manifestPath := filepath.Join(repoRoot, "skills", "_manifest.toml")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var m SkillManifest
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// TriggerIndex returns a keyword → []entry map for fast lookup.
// Built once per LoadCatalogs call. Keys preserve the manifest's
// case (trigger keywords are matched case-sensitively, mirroring
// the rest of the detector); values are slices because two skills
// may share a keyword (e.g. "review" → artifact-review +
// github-code-review).
func (m *SkillManifest) TriggerIndex() map[string][]SkillManifestEntry {
	if m == nil {
		return nil
	}
	idx := make(map[string][]SkillManifestEntry)
	for _, entry := range m.Skills {
		for _, kw := range entry.TriggerKeywords {
			kw = strings.TrimSpace(kw)
			if kw == "" {
				continue
			}
			idx[kw] = append(idx[kw], entry)
		}
	}
	return idx
}

// TriggerKeywords returns the sorted, deduplicated list of every
// trigger keyword across all manifest entries. The detector uses
// this as the catalog for detectSkillTrigger.
func (m *SkillManifest) TriggerKeywords() []string {
	if m == nil {
		return nil
	}
	seen := make(map[string]struct{})
	for _, entry := range m.Skills {
		for _, kw := range entry.TriggerKeywords {
			kw = strings.TrimSpace(kw)
			if kw == "" {
				continue
			}
			seen[kw] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for kw := range seen {
		out = append(out, kw)
	}
	sort.Strings(out)
	return out
}
