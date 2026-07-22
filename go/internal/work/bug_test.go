package work_test

import (
	"context"
	"encoding/json"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
	"toolkit/internal/work"
)

func seedBug(t *testing.T, pool *db.Pool, project, slug, status string) {
	t.Helper()
	testutil.SeedBug(t, pool, project, slug, status, testutil.SeedBugOpts{
		ProblemStatement: "detail",
	})
}

// TestBugList_EmptyScopeReturnsCrossProject pins bug 1310's cross-project
// fallback: empty args.Project + no filter + no all=true now returns the
// full cross-project list (bounded by the default limit of 50). Replaces
// the prior RequiresProjectOrFilter gate.
func TestBugList_EmptyScopeReturnsCrossProject(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "a", "open")
	seedBug(t, pool, "seed-packet", "b", "open")

	resp, err := work.HandleBugList(context.Background(), pool, "", mustJSON(t, map[string]any{}))
	if err != nil {
		t.Fatalf("HandleBugList: %v", err)
	}
	if resp.Err != nil {
		t.Fatalf("expected cross-project list, got error envelope: %+v", resp.Err)
	}
	if len(resp.DefaultItems) != 2 {
		t.Errorf("expected 2 items across both projects, got %+v", resp.DefaultItems)
	}
}

// TestBugList_AllAcceptedAsLegacyNoOp pins that the legacy `all=true`
// field is accepted without changing behavior — same as omitting it.
func TestBugList_AllAcceptedAsLegacyNoOp(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "x", "open")
	resp, err := work.HandleBugList(context.Background(), pool, "", mustJSON(t, map[string]any{"all": true}))
	if err != nil {
		t.Fatalf("HandleBugList: %v", err)
	}
	if len(resp.DefaultItems) != 1 {
		t.Errorf("expected one item, got %+v", resp.DefaultItems)
	}
}

func TestBugList_StatusFilter(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "a", "open")
	seedBug(t, pool, "mcp-servers", "b", "fixed")
	resp, _ := work.HandleBugList(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"status": "open"}))
	if len(resp.DefaultItems) != 1 || resp.DefaultItems[0].Slug != "a" {
		t.Errorf("filter: %+v", resp.DefaultItems)
	}
}

// TestBugList_StatusFilterAliases pins the three accepted filter-key
// spellings: canonical `status`, Rust-dispatch alias `state`, and
// schema-header alias `resolve_state`. All three must narrow identically.
func TestBugList_StatusFilterAliases(t *testing.T) {
	for _, key := range []string{"status", "state", "resolve_state"} {
		t.Run(key, func(t *testing.T) {
			pool := openTestPool(t)
			seedBug(t, pool, "mcp-servers", "a", "open")
			seedBug(t, pool, "mcp-servers", "b", "fixed")
			resp, _ := work.HandleBugList(context.Background(), pool, "mcp-servers",
				mustJSON(t, map[string]any{key: "open"}))
			if len(resp.DefaultItems) != 1 || resp.DefaultItems[0].Slug != "a" {
				t.Errorf("alias %q: expected one open row, got %+v", key, resp.DefaultItems)
			}
		})
	}
}

func TestBugList_VerboseIncludesProblemStatement(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "a", "open")
	resp, _ := work.HandleBugList(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"verbose": true}))
	if !resp.Verbose || len(resp.VerboseItems) == 0 {
		t.Fatalf("expected verbose items, got %+v", resp)
	}
	if resp.VerboseItems[0].ProblemStatement == "" {
		t.Error("verbose list omitted problem_statement")
	}
}

// TestBugList_EmptyResultMarshalsAsArrayNotNull locks in the JSON-shape
// distinction between "no matches" and "tool error". A nil slice marshals
// as `null` — indistinguishable from an error — so the scan helpers
// return a zero-length non-nil slice.
func TestBugList_EmptyResultMarshalsAsArrayNotNull(t *testing.T) {
	pool := openTestPool(t)
	// Filter that matches nothing.
	for _, params := range []map[string]any{
		{"status": "open"},                      // default projection
		{"status": "open", "verbose": true},     // verbose projection
		{"status": "open", "titles_only": true}, // titles-only projection
	} {
		resp, err := work.HandleBugList(context.Background(), pool, "mcp-servers", mustJSON(t, params))
		if err != nil {
			t.Fatalf("HandleBugList(%+v): %v", params, err)
		}
		b, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("marshal(%+v): %v", params, err)
		}
		if string(b) != "[]" {
			t.Errorf("params %+v: empty result marshalled as %s, want []", params, b)
		}
	}
}

// (TestMigration027NormalizesBugSurfaceAndTagsDelimiters was deleted in
// chain agent-substrate-crud-retirement T6 — the `bugs` CRUD table that
// migration 027 transformed has been dropped by migration 060, so the
// migration runs against historical DBs only; there is no live `bugs`
// table for the test fixture to seed into or assert against.)

func TestBugRead_BySlug(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "x", "open")
	resp, _ := work.HandleBugRead(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"slug": "x"}))
	if resp.Bug == nil {
		t.Fatalf("expected Bug, got err=%+v", resp.Err)
	}
	if resp.Bug.Slug != "x" {
		t.Errorf("slug: %q", resp.Bug.Slug)
	}
}

func TestBugResolve_NormalisesAliases(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"fix", "fixed"},
		{"route", "routed"},
		{"wont_fix", "wontfix"},
		{"duplicate", "dup"},
		{"fixed", "fixed"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			pool := openTestPool(t)
			seedBug(t, pool, "mcp-servers", "x", "open")

			resp, _ := work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
				"slug":            "x",
				"resolution_kind": c.in,
			}))
			if !resp.OK {
				t.Fatalf("resp: %+v", resp)
			}
			var kind string
			pool.DB().QueryRow(`SELECT resolution_kind FROM proj_current_bugs WHERE slug = 'x'`).Scan(&kind)
			if kind != c.want {
				t.Errorf("alias %q stored as %q, want %q", c.in, kind, c.want)
			}
		})
	}
}

func TestBugResolve_RoutedReroutePreservesResolvedAt(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "x", "open")
	// Initial route.
	work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":              "x",
		"resolution_kind":   "routed",
		"routed_chain_slug": "c1",
		"routed_task_slug":  "t1",
	}))
	var resolvedAt1 string
	pool.DB().QueryRow(`SELECT resolved_at FROM proj_current_bugs WHERE slug = 'x'`).Scan(&resolvedAt1)
	if resolvedAt1 == "" {
		t.Fatal("first route did not set resolved_at")
	}

	// Re-route. resolved_at must stay equal.
	work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":              "x",
		"resolution_kind":   "routed",
		"routed_chain_slug": "c2",
		"routed_task_slug":  "t2",
	}))
	var resolvedAt2, chain string
	pool.DB().QueryRow(`SELECT resolved_at, routed_chain_slug FROM proj_current_bugs WHERE slug = 'x'`).Scan(&resolvedAt2, &chain)
	if resolvedAt1 != resolvedAt2 {
		t.Errorf("re-route changed resolved_at: %q → %q", resolvedAt1, resolvedAt2)
	}
	if chain != "c2" {
		t.Errorf("re-route did not update routed_chain_slug: %q", chain)
	}
}

func TestBugResolve_RoutedToFixedRequiresSHA(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "x", "open")
	work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "x", "resolution_kind": "routed", "routed_chain_slug": "c", "routed_task_slug": "t",
	}))
	// No commit_sha → reject.
	resp, _ := work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "x", "resolution_kind": "fixed",
	}))
	if resp.Error == "" || !contains(resp.Error, "commit_sha") {
		t.Errorf("expected commit_sha gate error, got %q", resp.Error)
	}

	// With sha → allowed; resolved_at preserved.
	var preResolvedAt string
	pool.DB().QueryRow(`SELECT resolved_at FROM proj_current_bugs WHERE slug = 'x'`).Scan(&preResolvedAt)

	work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "x", "resolution_kind": "fixed", "commit_sha": "abc1234",
	}))
	var postResolvedAt, status, sha string
	pool.DB().QueryRow(`SELECT resolved_at, status, resolved_commit_sha FROM proj_current_bugs WHERE slug = 'x'`).Scan(&postResolvedAt, &status, &sha)
	if preResolvedAt != postResolvedAt {
		t.Errorf("routed→fixed reset resolved_at: %q → %q", preResolvedAt, postResolvedAt)
	}
	if status != "fixed" || sha != "abc1234" {
		t.Errorf("post-update: status=%q sha=%q", status, sha)
	}
}

func TestBugResolve_AcceptsShaAlias(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "x", "open")
	work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "x", "resolution_kind": "fixed", "sha": "deadbeef",
	}))
	var sha string
	pool.DB().QueryRow(`SELECT resolved_commit_sha FROM proj_current_bugs WHERE slug = 'x'`).Scan(&sha)
	if sha != "deadbeef" {
		t.Errorf("sha alias did not persist: %q", sha)
	}
}

// TestBugResolve_AcceptsResolutionNoteAliases — table-driven contract
// pin: every recognized alias for resolution_note lands at the
// canonical column (BugResolved.payload.resolution_note). Started as
// bug 549's `notes` alias regression; extended by bug 858 to cover
// `resolution_summary` and `summary` paraphrases agents have hit.
// Consistent with the existing `sha`/`commit_sha` and `kind`/
// `resolution_kind` alias pairs. Extending this single test instead
// of writing per-alias tests pins the contract ("any alias lands at
// the canonical column") not just the instance, per bug-fixing-
// discipline reflex 2 / dial-in 2.
func TestBugResolve_AcceptsResolutionNoteAliases(t *testing.T) {
	aliases := []struct {
		key, slug string
	}{
		{"notes", "alias-notes"},
		{"resolution_summary", "alias-resolution-summary"},
		{"summary", "alias-summary"},
	}
	for _, c := range aliases {
		t.Run(c.key, func(t *testing.T) {
			pool := openTestPool(t)
			seedBug(t, pool, "mcp-servers", c.slug, "open")
			value := "fixed via " + c.key + " alias"
			resp, err := work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
				"slug": c.slug, "resolution_kind": "fixed", "commit_sha": "abc1234",
				c.key: value,
			}))
			if err != nil {
				t.Fatalf("HandleBugResolve: %v", err)
			}
			if !resp.OK {
				t.Fatalf("resolve rejected: %+v", resp)
			}
			var note string
			if err := pool.DB().QueryRow(
				`SELECT json_extract(payload, '$.resolution_note') FROM events WHERE type='BugResolved' AND entity_slug=?`,
				c.slug,
			).Scan(&note); err != nil {
				t.Fatalf("read event payload: %v", err)
			}
			if note != value {
				t.Errorf("alias %q did not persist to resolution_note: got %q, want %q", c.key, note, value)
			}
		})
	}
}

// Bug 1381: when a commit_sha (or sha alias) is supplied and
// resolution_kind is omitted, the handler defaults the kind to 'fixed'
// — the dominant 'fix landed in commit X' shape. Other kinds remain
// explicit-by-necessity because sha+wontfix/dup/routed is incoherent.
func TestBugResolve_DefaultsKindFixedWhenShaSet(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "x", "open")
	resp, _ := work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "x", "commit_sha": "abc1234", "resolution_note": "fixed by adding X",
	}))
	if resp.Error != "" {
		t.Fatalf("expected ok, got error: %q", resp.Error)
	}
	var status, kind, sha string
	pool.DB().QueryRow(`SELECT status, resolution_kind, resolved_commit_sha FROM proj_current_bugs WHERE slug = 'x'`).Scan(&status, &kind, &sha)
	if status != "fixed" || kind != "fixed" || sha != "abc1234" {
		t.Errorf("defaults didn't take: status=%q kind=%q sha=%q", status, kind, sha)
	}
}

// Bug 1381: when no SHA is supplied, the kind requirement still
// applies — disambiguation only matters for sha-less resolutions
// (wontfix vs dup vs routed). The error wording mentions the
// commit_sha default so callers can discover the shortcut.
func TestBugResolve_StillRequiresKindWithoutSha(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "x", "open")
	resp, _ := work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "x", "resolution_note": "abandoned",
	}))
	if resp.Error == "" {
		t.Fatalf("expected missing-kind rejection, got success")
	}
	if !contains(resp.Error, "resolution_kind") {
		t.Errorf("error should name resolution_kind: %q", resp.Error)
	}
	if !contains(resp.Error, "commit_sha") {
		t.Errorf("error should mention the commit_sha default shortcut: %q", resp.Error)
	}
}

func TestBugResolve_RejectsResolvedBugBeingReResolved(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "x", "open")
	work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "x", "resolution_kind": "fixed", "commit_sha": "abc1234",
	}))
	resp, _ := work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "x", "resolution_kind": "wontfix",
	}))
	if resp.Error == "" || !contains(resp.Error, "non-open") {
		t.Errorf("expected non-open rejection, got %q", resp.Error)
	}
}

func TestBugReopen_ResolvedToOpen(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "x", "open")
	work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "x", "resolution_kind": "fixed", "commit_sha": "abc1234",
	}))
	resp, _ := work.HandleBugReopen(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"slug": "x"}))
	if !resp.OK {
		t.Fatalf("resp: %+v", resp)
	}
	// resolution_note retired from the projection in migration 065
	// (Phase 4 F2); the reopen check no longer asserts on it.
	var status, kind string
	pool.DB().QueryRow(`SELECT status, COALESCE(resolution_kind, '') FROM proj_current_bugs WHERE slug = 'x'`).Scan(&status, &kind)
	if status != "open" {
		t.Errorf("status: %q", status)
	}
	if kind != "" {
		t.Errorf("resolution_kind: %q (want empty/null)", kind)
	}
}

func TestBugReopen_AlreadyOpenErrors(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "x", "open")
	resp, _ := work.HandleBugReopen(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"slug": "x"}))
	if resp.Error == "" {
		t.Errorf("expected already-open error, got %+v", resp)
	}
}

func TestBugStampSHA_RequiresResolved(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "x", "open")
	resp, _ := work.HandleBugStampSHA(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "x", "commit_sha": "abc1234",
	}))
	if resp.Error == "" || !contains(resp.Error, "resolve it before stamping") {
		t.Errorf("stamp on open: %q", resp.Error)
	}
}

func TestBugStampSHA_OnResolvedBug(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "x", "open")
	work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "x", "resolution_kind": "fixed", "commit_sha": "abc1234",
	}))
	resp, _ := work.HandleBugStampSHA(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "x", "commit_sha": "abcd1234",
	}))
	if !resp.OK {
		t.Fatalf("resp: %+v", resp)
	}
	var sha string
	pool.DB().QueryRow(`SELECT resolved_commit_sha FROM proj_current_bugs WHERE slug = 'x'`).Scan(&sha)
	if sha != "abcd1234" {
		t.Errorf("sha: %q", sha)
	}
}

// TestBugResolve_AcceptsIDAlias pins bug 1329: bug_resolve accepts {id: N}
// as a slug alias so the bug_list → bug_resolve flow can stay id-keyed
// end-to-end (bug_list's compact projection already surfaces id first).
// The handler resolves id→slug internally, then runs the same write path.
func TestBugResolve_AcceptsIDAlias(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "x", "open")
	var id int64
	if err := pool.DB().QueryRow(`SELECT id FROM proj_current_bugs WHERE slug = 'x'`).Scan(&id); err != nil {
		t.Fatalf("fetch id: %v", err)
	}
	resp, err := work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"id":              id,
		"resolution_kind": "fixed",
		"commit_sha":      "abc1234",
	}))
	if err != nil {
		t.Fatalf("HandleBugResolve: %v", err)
	}
	if !resp.OK || resp.Slug != "x" {
		t.Fatalf("expected ok+slug=x, got %+v", resp)
	}
	var status, sha string
	pool.DB().QueryRow(`SELECT status, resolved_commit_sha FROM proj_current_bugs WHERE slug = 'x'`).Scan(&status, &sha)
	if status != "fixed" || sha != "abc1234" {
		t.Errorf("post-update: status=%q sha=%q", status, sha)
	}
}

// TestBugResolve_IDNotFoundErrors locks in the error path: an id that
// doesn't resolve surfaces 'bug id N not found' (parity with bug_read's
// missing-id message).
func TestBugResolve_IDNotFoundErrors(t *testing.T) {
	pool := openTestPool(t)
	resp, _ := work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"id": 9999, "resolution_kind": "fixed", "commit_sha": "abc1234",
	}))
	if resp.Error == "" || !contains(resp.Error, "9999") || !contains(resp.Error, "not found") {
		t.Errorf("expected not-found error citing id 9999, got %q", resp.Error)
	}
}

// TestBugResolve_NeitherSlugNorIDErrors keeps the missing-identifier path
// honest: with the id alias added, the error wording must name both
// fields so the caller knows either is accepted.
func TestBugResolve_NeitherSlugNorIDErrors(t *testing.T) {
	pool := openTestPool(t)
	resp, _ := work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"resolution_kind": "fixed",
	}))
	if resp.Error == "" || !contains(resp.Error, "slug") || !contains(resp.Error, "id") {
		t.Errorf("expected error naming slug AND id, got %q", resp.Error)
	}
}

// TestBugStampSHA_AcceptsIDAlias mirrors bug 1329 for the second
// SHA-stamping action.
func TestBugStampSHA_AcceptsIDAlias(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "x", "open")
	work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "x", "resolution_kind": "fixed", "commit_sha": "abc1234",
	}))
	var id int64
	pool.DB().QueryRow(`SELECT id FROM proj_current_bugs WHERE slug = 'x'`).Scan(&id)

	resp, _ := work.HandleBugStampSHA(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"id": id, "commit_sha": "deadbeef",
	}))
	if !resp.OK || resp.Slug != "x" {
		t.Fatalf("expected ok+slug=x, got %+v", resp)
	}
	var sha string
	pool.DB().QueryRow(`SELECT resolved_commit_sha FROM proj_current_bugs WHERE slug = 'x'`).Scan(&sha)
	if sha != "deadbeef" {
		t.Errorf("sha alias on id: %q", sha)
	}
}

// TestBugResolve_UpstreamKind pins bug 1330: the resolution_kind enum
// includes a sibling for 'real, reproducible, traceable to a dependency
// we don't author; not fixed locally for that reason'. Distinct from
// wontfix so downstream filters can separate the two without parsing the
// resolution_note free text.
func TestBugResolve_UpstreamKind(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "x", "open")
	resp, _ := work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":            "x",
		"resolution_kind": "upstream",
		"resolution_note": "Confirmed upstream Claude Code CLI",
	}))
	if !resp.OK {
		t.Fatalf("resp: %+v", resp)
	}
	var status, kind string
	pool.DB().QueryRow(`SELECT status, resolution_kind FROM proj_current_bugs WHERE slug = 'x'`).Scan(&status, &kind)
	if status != "upstream" || kind != "upstream" {
		t.Errorf("expected status=upstream kind=upstream, got status=%q kind=%q", status, kind)
	}
}

// TestBugResolve_UpstreamAliases pins the verb/adjective-form aliases
// the bug-filing discipline uses naturally: external, externalized,
// upstreamed all normalize to 'upstream'.
func TestBugResolve_UpstreamAliases(t *testing.T) {
	for _, alias := range []string{"upstream", "external", "externalized", "upstreamed"} {
		t.Run(alias, func(t *testing.T) {
			pool := openTestPool(t)
			seedBug(t, pool, "mcp-servers", "x", "open")
			resp, _ := work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
				"slug": "x", "resolution_kind": alias,
			}))
			if !resp.OK {
				t.Fatalf("alias %q: %+v", alias, resp)
			}
			var kind string
			pool.DB().QueryRow(`SELECT resolution_kind FROM proj_current_bugs WHERE slug = 'x'`).Scan(&kind)
			if kind != "upstream" {
				t.Errorf("alias %q stored as %q, want upstream", alias, kind)
			}
		})
	}
}

// TestBugResolve_RejectsUnknownKind keeps the validator's reject path
// honest after the upstream addition. The error message must enumerate
// every accepted kind so a typo is self-correcting.
func TestBugResolve_RejectsUnknownKind(t *testing.T) {
	pool := openTestPool(t)
	seedBug(t, pool, "mcp-servers", "x", "open")
	resp, _ := work.HandleBugResolve(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "x", "resolution_kind": "obsolete",
	}))
	if resp.Error == "" {
		t.Fatalf("expected error envelope, got %+v", resp)
	}
	for _, expected := range []string{"fixed", "wontfix", "upstream", "dup", "routed"} {
		if !contains(resp.Error, expected) {
			t.Errorf("error message %q missing kind %q", resp.Error, expected)
		}
	}
}
