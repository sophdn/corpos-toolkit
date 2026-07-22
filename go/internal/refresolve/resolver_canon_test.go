package refresolve_test

import (
	"context"
	"strings"
	"testing"

	"toolkit/internal/refresolve"
	"toolkit/internal/testutil"
)

// End-to-end: with the canon map learned, a retired token in an orient-time
// message is detected and resolved to its current canonical form — the chain that
// surfaces "mcp-servers is retired -> corpos-toolkit" in the parse_context
// envelope before stale-canon propagates.
func TestCanonResolver_OrientTimeStaleTokenAnswer(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	if _, err := pool.DB().ExecContext(ctx,
		`INSERT INTO canon_entries (slug, kind, canonical, status, gitea_owner, local_path)
		 VALUES ('corpos-toolkit','repo','sophdn/corpos-toolkit','current','shared','~/dev/corpos-toolkit')`,
	); err != nil {
		t.Fatalf("seed entry: %v", err)
	}
	if _, err := pool.DB().ExecContext(ctx,
		`INSERT INTO canon_aliases (alias, entry_slug, dimension) VALUES ('mcp-servers','corpos-toolkit','old-name')`,
	); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	cat, err := refresolve.LoadCatalogs(ctx, "", pool, "")
	if err != nil {
		t.Fatalf("LoadCatalogs: %v", err)
	}
	found := false
	for _, tok := range cat.CanonTokens {
		if tok == "mcp-servers" {
			found = true
		}
	}
	if !found {
		t.Fatalf("CanonTokens missing mcp-servers: %v", cat.CanonTokens)
	}

	det := refresolve.NewDetector(cat, nil)
	refs, err := det.Detect(ctx, "does the mcp-servers repo still build?")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	var canonRef *refresolve.Reference
	for i := range refs {
		if refs[i].Shape == refresolve.ShapeCanonToken && refs[i].Token == "mcp-servers" {
			canonRef = &refs[i]
		}
	}
	if canonRef == nil {
		t.Fatalf("no ShapeCanonToken reference detected in %+v", refs)
	}

	res := refresolve.NewCanonResolver(pool)
	hs, err := res.Resolve(ctx, *canonRef)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(hs.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(hs.Candidates))
	}
	if !strings.Contains(hs.Candidates[0].Title, "corpos-toolkit") {
		t.Errorf("answer=%q, want it to name the canonical corpos-toolkit", hs.Candidates[0].Title)
	}
}
