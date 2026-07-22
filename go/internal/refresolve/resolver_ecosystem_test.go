package refresolve_test

import (
	"context"
	"strings"
	"testing"

	"toolkit/internal/refresolve"
	"toolkit/internal/testutil"
)

// The end-to-end cold-agent path: with example-host learned, LoadCatalogs picks
// up the token, the detector flags it in an orient-time message, and the resolver
// returns the deterministic access answer — the chain that surfaces "yes, ssh as
// youruser" in the parse_context envelope before an agent can wrongly deny.
func TestEcosystemResolver_OrientTimeAccessAnswer(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	if _, err := pool.DB().ExecContext(ctx,
		`INSERT INTO hosts (slug, addr, ssh_user, ssh_key_path) VALUES ('example-host','203.0.113.10','youruser','~/.ssh/id_ed25519')`,
	); err != nil {
		t.Fatalf("seed host: %v", err)
	}

	cat, err := refresolve.LoadCatalogs(ctx, "", pool, "")
	if err != nil {
		t.Fatalf("LoadCatalogs: %v", err)
	}
	found := false
	for _, tok := range cat.EcosystemTokens {
		if tok == "example-host" {
			found = true
		}
	}
	if !found {
		t.Fatalf("EcosystemTokens missing example-host: %v", cat.EcosystemTokens)
	}

	det := refresolve.NewDetector(cat, nil)
	refs, err := det.Detect(ctx, "do I have access to example-host?")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	var ecoRef *refresolve.Reference
	for i := range refs {
		if refs[i].Shape == refresolve.ShapeEcosystemToken && refs[i].Token == "example-host" {
			ecoRef = &refs[i]
		}
	}
	if ecoRef == nil {
		t.Fatalf("no ShapeEcosystemToken reference detected in %+v", refs)
	}

	res := refresolve.NewEcosystemResolver(pool)
	hs, err := res.Resolve(ctx, *ecoRef)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(hs.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(hs.Candidates))
	}
	if !strings.Contains(hs.Candidates[0].Title, "ssh youruser@") {
		t.Errorf("answer = %q, want an 'ssh youruser@...' access answer", hs.Candidates[0].Title)
	}
}

// An un-learned token that is not in the catalog is never detected, and the
// resolver leaves an un-resolvable token unbound (TierNoHit) rather than
// asserting a stale answer.
func TestEcosystemResolver_UnlearnedIsNoHit(t *testing.T) {
	pool := testutil.NewTestDB(t)
	res := refresolve.NewEcosystemResolver(pool)
	hs, err := res.Resolve(context.Background(),
		refresolve.Reference{Token: "ghost-host", Shape: refresolve.ShapeEcosystemToken})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(hs.Candidates) != 0 || hs.ConfidenceTier != refresolve.TierNoHit {
		t.Errorf("want no-hit for un-learned token, got %+v", hs)
	}
}
