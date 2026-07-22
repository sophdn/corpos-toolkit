package refresolve

import (
	"time"

	"toolkit/internal/stdiodrift"
)

// ExportShouldSurface exposes the unexported shouldSurface method to
// tests in refresolve_test. Lives in an *_test.go file so it never
// compiles into the production binary.
func ExportShouldSurface(t *DriftFireTracker, sessionID string, drift stdiodrift.State, intent string) (surface, bootstrap, suppressed bool) {
	return t.shouldSurface(sessionID, drift, intent)
}

// WorkStateCacheTestEntry builds a workStateCacheEntry from counts —
// the entry struct itself is unexported so tests can't construct it
// directly. The Refs field is left nil; cache invalidation tests only
// observe Len() / Get-bool, never read back the refs.
func WorkStateCacheTestEntry(bugs, tasks, chains int) workStateCacheEntry {
	return workStateCacheEntry{
		BugCount:   bugs,
		TaskCount:  tasks,
		ChainCount: chains,
	}
}

// BuildTestSkillManifest constructs an in-memory SkillManifest from
// the supplied entries. The SkillManifest type's fields are exported
// but the loader path through skills/_manifest.toml is the production
// constructor; tests use this helper to assemble a manifest without
// touching the disk. T7 (chain parse-context-lean-orienting) added it
// for the intent → discipline mapping tests.
func BuildTestSkillManifest(entries []SkillManifestEntry) *SkillManifest {
	return &SkillManifest{Skills: entries}
}

// SetClockForTest overrides the tracker's clock source so the recent-fire
// TTL boundary and expiry branches are reachable without sleeping real
// wall-clock minutes. Added in chain harden-go-deps T5 after mutation
// testing surfaced the TTL value + strict-`<` boundary as unpinned
// survivors (DisciplineFireTracker reads its clock through t.clock()).
func (t *DisciplineFireTracker) SetClockForTest(now func() time.Time) {
	t.now = now
}

// SeedFireForTest stamps a fire timestamp directly, bypassing markFired's
// guards. Lets tests construct entries the guarded public path can't
// produce — aged entries (for TTL/expiry) and empty-sessionID entries
// (to exercise recentlyFired's empty-session guard, which markFired's own
// guard would otherwise mask).
func (t *DisciplineFireTracker) SeedFireForTest(sessionID string, intent IntentShape, discipline string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[disciplineFireKey{sessionID, intent, discipline}] = at
}

// EntryCountForTest returns the number of tracked fire entries, so tests
// can observe whether a guarded write (markFired) was suppressed without
// going through recentlyFired (whose own guard would mask it).
func (t *DisciplineFireTracker) EntryCountForTest() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}
