package grounding

import (
	"encoding/json"
	"testing"

	"toolkit/internal/telemetry"
)

// TestNormalizeSlug pins the slug-form normalization that TT1.5 §7.1
// documented — assistant text references vault notes by the date-prefixed
// tail of the path, not the full vault-rooted form.
func TestNormalizeSlug(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"learnings/general/2026-05-12_foo-bar.md", "2026-05-12_foo-bar"},
		{"decisions/2026-05-09_baz-qux.md", "2026-05-09_baz-qux"},
		{"vault/learnings/mcp-servers/2026-05-04_x.md", "2026-05-04_x"},
		{"devdocs_en_rust::book/ch17-02-trait-objects", "devdocs_en_rust::book/ch17-02-trait-objects"},
		{"   ", ""},
		{"", ""},
		{"plain-file.md", "plain-file"},
		{"some.txt", "some.txt"},
	}
	for _, c := range cases {
		if got := normalizeSlug(c.in); got != c.want {
			t.Errorf("normalizeSlug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// buildScope returns a synthetic list of entries representing the
// records that follow a search call within the same prompt arc. The
// promptId is threaded onto every entry so scopeWithinPrompt accepts
// the whole slice.
func buildScope(t *testing.T, lines ...string) []jsonlEntry {
	t.Helper()
	out := make([]jsonlEntry, 0, len(lines))
	for _, l := range lines {
		out = append(out, mustEntry(t, l))
	}
	for i := range out {
		out[i].PromptID = "prompt-A"
	}
	return out
}

// mustEntry unmarshals a single JSONL line into a jsonlEntry. Shared by
// detect_test.go and resolve_test.go fixtures.
func mustEntry(t *testing.T, line string) jsonlEntry {
	t.Helper()
	var e jsonlEntry
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		t.Fatalf("mustEntry unmarshal: %v\nline: %s", err, line)
	}
	return e
}

// ── followed ─────────────────────────────────────────────────────────

func TestDetectFollowed_VaultRead(t *testing.T) {
	scope := buildScope(t,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u1","name":"mcp__toolkit-server__knowledge","input":{"action":"vault_read","params":{"path":"learnings/general/2026-05-12_foo.md"}}}]}}`,
	)
	ev := ProcessedEvent{
		SourceRefs: []string{
			"learnings/general/2026-05-12_foo.md",
			"reference/other.md",
		},
		PromptID: "prompt-A",
	}
	got := detectInteractions(ev, append(syntheticSearchPrelude(), scope...))
	want := map[string]telemetry.ClickKind{
		"learnings/general/2026-05-12_foo.md": telemetry.ClickFollowed,
	}
	assertOnlyKinds(t, got, want)
}

func TestDetectFollowed_NativeRead(t *testing.T) {
	scope := buildScope(t,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u1","name":"Read","input":{"file_path":"/home/user/dev/mcp-servers/learnings/general/2026-05-12_foo.md"}}]}}`,
	)
	ev := ProcessedEvent{
		SourceRefs: []string{"learnings/general/2026-05-12_foo.md"},
		PromptID:   "prompt-A",
	}
	got := detectInteractions(ev, append(syntheticSearchPrelude(), scope...))
	want := map[string]telemetry.ClickKind{
		"learnings/general/2026-05-12_foo.md": telemetry.ClickFollowed,
	}
	assertOnlyKinds(t, got, want)
}

func TestDetectFollowed_KiwixFetch(t *testing.T) {
	scope := buildScope(t,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u1","name":"mcp__toolkit-server__knowledge","input":{"action":"kiwix_fetch","params":{"zim_id":"devdocs_en_rust","slug":"book/ch17"}}}]}}`,
	)
	ev := ProcessedEvent{
		SourceRefs: []string{"devdocs_en_rust::book/ch17"},
		PromptID:   "prompt-A",
	}
	got := detectInteractions(ev, append(syntheticSearchPrelude(), scope...))
	if len(got) != 1 || got[0].ClickKind != telemetry.ClickFollowed {
		t.Fatalf("want one followed row, got %+v", got)
	}
}

func TestDetectFollowed_NoMatch(t *testing.T) {
	scope := buildScope(t,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u1","name":"mcp__toolkit-server__knowledge","input":{"action":"vault_read","params":{"path":"unrelated.md"}}}]}}`,
	)
	ev := ProcessedEvent{
		SourceRefs: []string{"learnings/general/2026-05-12_foo.md"},
		PromptID:   "prompt-A",
	}
	got := detectInteractions(ev, append(syntheticSearchPrelude(), scope...))
	if len(got) != 0 {
		t.Fatalf("want zero rows, got %+v", got)
	}
}

// TestDetectFollowed_ChainState is the canonical regression for bug
// follow-detector-only-credits-vault-kiwix-read: a knowledge_search
// surfaces a chain candidate (scheme-prefixed source_ref) and the agent
// follows it via work.chain_state on the same slug. Pre-fix this produced
// no interaction (isFollowAction rejected the work tool), so the chain
// candidate could only ever be labeled negative.
func TestDetectFollowed_ChainState(t *testing.T) {
	scope := buildScope(t,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u1","name":"mcp__toolkit-server__work","input":{"action":"chain_state","project":"mcp-servers","params":{"slug":"action-docs-corpus"}}}]}}`,
	)
	ev := ProcessedEvent{
		SourceRefs: []string{
			"chain:mcp-servers::action-docs-corpus",
			"chain:mcp-servers::unrelated-chain",
		},
		PromptID: "prompt-A",
	}
	got := detectInteractions(ev, append(syntheticSearchPrelude(), scope...))
	want := map[string]telemetry.ClickKind{
		"chain:mcp-servers::action-docs-corpus": telemetry.ClickFollowed,
	}
	assertOnlyKinds(t, got, want)
}

// followTU builds a single-tool_use assistant scope for a follow action.
func followTU(t *testing.T, toolUseJSON string) []jsonlEntry {
	t.Helper()
	return buildScope(t, `{"type":"assistant","message":{"content":[`+toolUseJSON+`]}}`)
}

// TestDetectFollowed_NonVaultSurfaces exercises every newly-credited
// follow verb against its canonical scheme-prefixed source_ref — the
// coverage that bug follow-detector-only-credits-vault-kiwix-read was
// missing. Each row asserts exactly one `followed` interaction on the
// matching ref and none on the decoy.
func TestDetectFollowed_NonVaultSurfaces(t *testing.T) {
	cases := []struct {
		name    string
		toolUse string
		match   string // the source_ref that should be followed
		decoy   string // a same-scheme ref that should NOT match
	}{
		{
			name:    "bug_read by slug",
			toolUse: `{"type":"tool_use","id":"u1","name":"mcp__toolkit-server__work","input":{"action":"bug_read","project":"mcp-servers","params":{"slug":"live-spans-empty"}}}`,
			match:   "bug:mcp-servers::live-spans-empty",
			decoy:   "bug:mcp-servers::other-bug",
		},
		{
			name:    "task_read by chain_slug+task_slug",
			toolUse: `{"type":"tool_use","id":"u1","name":"mcp__toolkit-server__work","input":{"action":"task_read","project":"mcp-servers","params":{"chain_slug":"action-docs-corpus-frontend","task_slug":"action-docs-browser-page"}}}`,
			match:   "task:mcp-servers::action-docs-corpus-frontend::action-docs-browser-page",
			decoy:   "task:mcp-servers::action-docs-corpus-frontend::other-task",
		},
		{
			name:    "suggestion_read by slug (no project component)",
			toolUse: `{"type":"tool_use","id":"u1","name":"mcp__toolkit-server__work","input":{"action":"suggestion_read","params":{"slug":"forge-bug"}}}`,
			match:   "suggestion:forge-bug",
			decoy:   "suggestion:other-suggestion",
		},
		{
			name:    "library_get by slug",
			toolUse: `{"type":"tool_use","id":"u1","name":"mcp__toolkit-server__knowledge","input":{"action":"library_get","project":"seed-packet","params":{"slug":"005.11"}}}`,
			match:   "library:seed-packet::005.11",
			decoy:   "library:seed-packet::006.22",
		},
		{
			name:    "Skill tool by name",
			toolUse: `{"type":"tool_use","id":"u1","name":"Skill","input":{"skill":"reference-resolution","args":"x"}}`,
			match:   "skill:skills/reference-resolution.toml",
			decoy:   "skill:skills/some-other-skill.toml",
		},
		{
			name:    "vault_read against knowledge_search vault: scheme ref",
			toolUse: `{"type":"tool_use","id":"u1","name":"mcp__toolkit-server__knowledge","input":{"action":"vault_read","params":{"path":"learnings/general/2026-05-12_foo.md"}}}`,
			match:   "vault:.claude/vault/learnings/general/2026-05-12_foo.md",
			decoy:   "vault:.claude/vault/learnings/general/2026-05-12_bar.md",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := ProcessedEvent{SourceRefs: []string{c.match, c.decoy}, PromptID: "prompt-A"}
			got := detectInteractions(ev, append(syntheticSearchPrelude(), followTU(t, c.toolUse)...))
			assertOnlyKinds(t, got, map[string]telemetry.ClickKind{c.match: telemetry.ClickFollowed})
		})
	}
}

// TestDetectFollowed_SlugGuards pins the false-positive guards on
// slug-style follows: wrong project, wrong scheme, and id-only reads
// (which carry no slug to match) must NOT fire.
func TestDetectFollowed_SlugGuards(t *testing.T) {
	cases := []struct {
		name    string
		toolUse string
		ref     string
	}{
		{
			name:    "wrong project",
			toolUse: `{"type":"tool_use","id":"u1","name":"mcp__toolkit-server__work","input":{"action":"chain_state","project":"mcp-servers","params":{"slug":"shared-slug"}}}`,
			ref:     "chain:seed-packet::shared-slug",
		},
		{
			name:    "right slug wrong scheme (bug ref, chain follow)",
			toolUse: `{"type":"tool_use","id":"u1","name":"mcp__toolkit-server__work","input":{"action":"chain_state","project":"mcp-servers","params":{"slug":"action-docs-corpus"}}}`,
			ref:     "bug:mcp-servers::action-docs-corpus",
		},
		{
			name:    "bug_read by numeric id carries no slug",
			toolUse: `{"type":"tool_use","id":"u1","name":"mcp__toolkit-server__work","input":{"action":"bug_read","params":{"id":1298}}}`,
			ref:     "bug:mcp-servers::live-spans-empty",
		},
		{
			name:    "unrelated work action (forge) is not a follow",
			toolUse: `{"type":"tool_use","id":"u1","name":"mcp__toolkit-server__work","input":{"action":"forge","project":"mcp-servers","params":{"slug":"action-docs-corpus"}}}`,
			ref:     "chain:mcp-servers::action-docs-corpus",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := ProcessedEvent{SourceRefs: []string{c.ref}, PromptID: "prompt-A"}
			got := detectInteractions(ev, append(syntheticSearchPrelude(), followTU(t, c.toolUse)...))
			if len(got) != 0 {
				t.Fatalf("want zero interactions, got %+v", got)
			}
		})
	}
}

// TestRefIdentity pins the scheme/project/terminal decomposition the
// slug-style follow matcher relies on, across every source_ref shape in
// the live corpus plus the bare-path and kiwix-"::"-form edge cases.
func TestRefIdentity(t *testing.T) {
	cases := []struct {
		ref      string
		scheme   string
		proj     string
		terminal string
	}{
		{"chain:mcp-servers::action-docs-corpus", "chain", "mcp-servers", "action-docs-corpus"},
		{"task:mcp-servers::chainslug::taskslug", "task", "mcp-servers", "taskslug"},
		{"bug:mcp-servers::live-spans-empty", "bug", "mcp-servers", "live-spans-empty"},
		{"suggestion:forge-bug", "suggestion", "", "forge-bug"},
		{"library:seed-packet::005.11", "library", "seed-packet", "005.11"},
		{"skill:skills/reference-resolution.toml", "skill", "", "reference-resolution"},
		{"schema:blueprints/forge-schemas/bug.toml", "schema", "", "bug"},
		{"tool:action-manifests/bug-resolve.toml", "tool", "", "bug-resolve"},
		{"project:mcp-servers", "project", "", "mcp-servers"},
		// no-scheme bare vault path → scheme "" (path-style handles these)
		{"learnings/general/2026-05-12_foo.md", "", "", "learnings/general/2026-05-12_foo.md"},
		// kiwix bare "<zim>::<slug>" has no lowercase-scheme prefix → unchanged
		{"devdocs_en_rust::book/ch17-02-trait-objects", "", "", "devdocs_en_rust::book/ch17-02-trait-objects"},
	}
	for _, c := range cases {
		scheme, proj, terminal := refIdentity(c.ref)
		if scheme != c.scheme || proj != c.proj || terminal != c.terminal {
			t.Errorf("refIdentity(%q) = (%q,%q,%q), want (%q,%q,%q)",
				c.ref, scheme, proj, terminal, c.scheme, c.proj, c.terminal)
		}
	}
}

// TestStripScheme pins the scheme-prefix stripping used to align
// knowledge_search "vault:"-prefixed refs with bare vault_read paths,
// while leaving bare paths and kiwix "::" refs untouched.
func TestStripScheme(t *testing.T) {
	cases := []struct{ in, want string }{
		{"vault:.claude/vault/learnings/general/foo.md", ".claude/vault/learnings/general/foo.md"},
		{"chain:mcp-servers::slug", "mcp-servers::slug"},
		{"learnings/general/foo.md", "learnings/general/foo.md"},
		{"devdocs_en_rust::book/ch17", "devdocs_en_rust::book/ch17"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripScheme(c.in); got != c.want {
			t.Errorf("stripScheme(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ── mentioned ────────────────────────────────────────────────────────

func TestDetectMentioned_SlugForm(t *testing.T) {
	scope := buildScope(t,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"The 2026-05-12_foo learning documents this pattern."}]}}`,
	)
	ev := ProcessedEvent{
		SourceRefs: []string{"learnings/general/2026-05-12_foo.md"},
		PromptID:   "prompt-A",
	}
	got := detectInteractions(ev, append(syntheticSearchPrelude(), scope...))
	if len(got) != 1 || got[0].ClickKind != telemetry.ClickMentioned {
		t.Fatalf("want one mentioned row, got %+v", got)
	}
}

func TestDetectMentioned_CaseInsensitive(t *testing.T) {
	scope := buildScope(t,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"see 2026-05-12_FOO for details"}]}}`,
	)
	ev := ProcessedEvent{
		SourceRefs: []string{"learnings/general/2026-05-12_foo.md"},
		PromptID:   "prompt-A",
	}
	got := detectInteractions(ev, append(syntheticSearchPrelude(), scope...))
	if len(got) != 1 || got[0].ClickKind != telemetry.ClickMentioned {
		t.Fatalf("want one mentioned row, got %+v", got)
	}
}

func TestDetectMentioned_NoMatch(t *testing.T) {
	scope := buildScope(t,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"unrelated work today"}]}}`,
	)
	ev := ProcessedEvent{
		SourceRefs: []string{"learnings/general/2026-05-12_foo.md"},
		PromptID:   "prompt-A",
	}
	got := detectInteractions(ev, append(syntheticSearchPrelude(), scope...))
	if len(got) != 0 {
		t.Fatalf("want zero rows, got %+v", got)
	}
}

func TestDetectMentioned_SchemePrefixedChainByBareSlug(t *testing.T) {
	// bug 958: a chain: ref mentioned by its bare terminal slug in prose must
	// fire mentioned. normalizeSlug leaves the "chain:...::action-docs-corpus"
	// literal intact, which agents never write — they write the bare slug.
	scope := buildScope(t,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"I reused the action-docs-corpus chain output here."}]}}`,
	)
	ev := ProcessedEvent{
		SourceRefs: []string{"chain:mcp-servers::action-docs-corpus"},
		PromptID:   "prompt-A",
	}
	got := detectInteractions(ev, append(syntheticSearchPrelude(), scope...))
	if len(got) != 1 || got[0].ClickKind != telemetry.ClickMentioned ||
		got[0].SourceRef != "chain:mcp-servers::action-docs-corpus" {
		t.Fatalf("want one mentioned row on the chain ref, got %+v", got)
	}
}

func TestDetectMentioned_SchemeTerminalWordBoundaryGuard(t *testing.T) {
	// The scheme-terminal token must match on word boundaries so a short
	// terminal ("init") does not over-fire inside a larger word.
	scope := buildScope(t,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"reinitialize the cache and move on"}]}}`,
	)
	ev := ProcessedEvent{
		SourceRefs: []string{"task:mcp-servers::some-chain::init"},
		PromptID:   "prompt-A",
	}
	got := detectInteractions(ev, append(syntheticSearchPrelude(), scope...))
	if len(got) != 0 {
		t.Fatalf("terminal 'init' inside 'reinitialize' must not fire, got %+v", got)
	}
}

// ── cited ────────────────────────────────────────────────────────────

func TestDetectCited_MarkdownLink(t *testing.T) {
	scope := buildScope(t,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"See the [floor-char learning](learnings/general/2026-05-12_foo.md) for the boundary handling."}]}}`,
	)
	ev := ProcessedEvent{
		SourceRefs: []string{"learnings/general/2026-05-12_foo.md"},
		PromptID:   "prompt-A",
	}
	got := detectInteractions(ev, append(syntheticSearchPrelude(), scope...))
	// markdown-link triggers `cited`; the assistant text also contains the
	// slug 2026-05-12_foo, so `mentioned` fires too. Both are valid per
	// TT1 §5.1 (multiple kinds per (span, source_ref)).
	wantKinds := map[telemetry.ClickKind]bool{
		telemetry.ClickCited:     true,
		telemetry.ClickMentioned: true,
	}
	for _, in := range got {
		if !wantKinds[in.ClickKind] {
			t.Errorf("unexpected kind: %s", in.ClickKind)
		}
		delete(wantKinds, in.ClickKind)
	}
	if len(wantKinds) != 0 {
		t.Errorf("missing kinds: %v in %+v", wantKinds, got)
	}
	for _, in := range got {
		if in.ClickKind == telemetry.ClickCited {
			if in.CitationKind == nil || *in.CitationKind != telemetry.CitationMarkdownLink {
				t.Errorf("cited citation_kind = %v, want markdown-link", in.CitationKind)
			}
		}
	}
}

func TestDetectCited_FileLine(t *testing.T) {
	scope := buildScope(t,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"The bug is in main.go:42 — see the helper for the fix."}]}}`,
	)
	ev := ProcessedEvent{
		SourceRefs: []string{"main.go"},
		PromptID:   "prompt-A",
	}
	got := detectInteractions(ev, append(syntheticSearchPrelude(), scope...))
	var citedSeen bool
	for _, in := range got {
		if in.ClickKind == telemetry.ClickCited {
			citedSeen = true
			if in.CitationKind == nil || *in.CitationKind != telemetry.CitationFileLine {
				t.Errorf("cited citation_kind = %v, want file-line", in.CitationKind)
			}
		}
	}
	if !citedSeen {
		t.Fatalf("want one cited row, got %+v", got)
	}
}

func TestDetectCited_QuotedBlock(t *testing.T) {
	// 50-char excerpt embedded in the result body AND quoted verbatim in
	// assistant text. The body lives under a vault_search "results[].excerpt"
	// shape that the extractor honors.
	excerpt := "this is a fifty-character excerpt right exactly!!!"
	if len(excerpt) < minCitationChars {
		t.Fatalf("test setup: excerpt < %d chars", minCitationChars)
	}
	result := `{"results":[{"path":"learnings/general/2026-05-12_foo.md","excerpt":"` + excerpt + `"}]}`
	scope := buildScope(t,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"As the note says: \"`+excerpt+`\" — that's the pattern."}]}}`,
	)
	ev := ProcessedEvent{
		SourceRefs: []string{"learnings/general/2026-05-12_foo.md"},
		PromptID:   "prompt-A",
		ResultText: result,
	}
	got := detectInteractions(ev, append(syntheticSearchPrelude(), scope...))
	var citedSeen bool
	for _, in := range got {
		if in.ClickKind == telemetry.ClickCited {
			citedSeen = true
			if in.CitationKind == nil || *in.CitationKind != telemetry.CitationQuotedBlock {
				t.Errorf("cited citation_kind = %v, want quoted-block", in.CitationKind)
			}
			if in.CitationQuoteChars == nil || *in.CitationQuoteChars < minCitationChars {
				t.Errorf("cited quote_chars = %v, want >= %d", in.CitationQuoteChars, minCitationChars)
			}
		}
	}
	if !citedSeen {
		t.Fatalf("want one cited row, got %+v", got)
	}
}

// ── prompt scope ─────────────────────────────────────────────────────

func TestDetectInteractions_PromptScopeStopsAtBoundary(t *testing.T) {
	prelude := syntheticSearchPrelude()
	// Mentioned-in-scope record (same promptId)
	inScope := buildScope(t,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"placeholder before boundary"}]}}`,
	)
	// Across-boundary record: a new user prompt with a different promptId.
	// detectInteractions should NOT scan past this entry.
	outOfScope := []jsonlEntry{
		{Type: "user", PromptID: "prompt-B"},
		{Type: "assistant", PromptID: "prompt-B", Message: &jsonlMessage{Content: json.RawMessage(
			`[{"type":"text","text":"new prompt mentions 2026-05-12_foo verbatim"}]`,
		)}},
	}
	ev := ProcessedEvent{
		SourceRefs: []string{"learnings/general/2026-05-12_foo.md"},
		PromptID:   "prompt-A",
	}
	all := append(prelude, append(inScope, outOfScope...)...)
	got := detectInteractions(ev, all)
	if len(got) != 0 {
		t.Fatalf("expected no interactions (text after boundary), got %+v", got)
	}
}

// ── helpers ──────────────────────────────────────────────────────────

// syntheticSearchPrelude returns the tool_use + tool_result records
// that precede the scope. detectInteractions trims from RecordIdx+1, so
// putting a single record before scope keeps RecordIdx=0 simple while
// the index slice still works without negative offsets.
func syntheticSearchPrelude() []jsonlEntry {
	return []jsonlEntry{
		{Type: "assistant", PromptID: "prompt-A"},
	}
}

func assertOnlyKinds(t *testing.T, got []Interaction, want map[string]telemetry.ClickKind) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d, want %d: %+v", len(got), len(want), got)
	}
	for _, in := range got {
		k, ok := want[in.SourceRef]
		if !ok {
			t.Errorf("unexpected source_ref %q", in.SourceRef)
			continue
		}
		if in.ClickKind != k {
			t.Errorf("source_ref %q kind = %s, want %s", in.SourceRef, in.ClickKind, k)
		}
	}
}
