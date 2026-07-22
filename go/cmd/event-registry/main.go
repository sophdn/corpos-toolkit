// Command event-registry is the local CLI for the canonical event registry
// (chain emit-surface-forge-v2 T2) — the CI-gated, fast-forward-only,
// git-backed source of truth the forge-v2 `record` surface mirrors to.
//
// Subcommands:
//
//	export     --db PATH --dest DIR
//	    Serialize the local events ledger to a registry checkout (one JSON
//	    file per event under DIR/events/). Idempotent: re-exporting an
//	    unchanged ledger writes byte-identical files (a git no-op).
//
//	validate   --dir DIR
//	    The CI validity-stamp tier. Re-validates every event file with the
//	    full closed-enum schema check PLUS the cross-event causal and
//	    projection-coherence tiers. Exit 1 on any failure. This is the
//	    command the registry repo's Gitea Actions workflow runs.
//
//	verify-dr  --db PATH --dir DIR
//	    Disaster-recovery proof: reconstruct the ledger from the registry
//	    checkout, rebuild projections from empty, and assert byte-identity
//	    with a from-empty rebuild of the source ledger. Exit 1 on any
//	    divergence.
//
// All three require the sqlite_fts5 build tag (db.Open runs FTS5
// migrations); run via `make -C go build` or `go run -tags sqlite_fts5`.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/registry"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "export":
		return runExport(args[1:])
	case "validate":
		return runValidate(args[1:])
	case "verify-dr":
		return runVerifyDR(args[1:])
	case "mirror":
		return runMirror(args[1:])
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `event-registry — local CLI for the canonical event registry

Usage:
  event-registry export    --db PATH --dest DIR
  event-registry validate  --dir DIR
  event-registry verify-dr --db PATH --dir DIR
  event-registry mirror    --db PATH --registry DIR [--remote origin] [--branch main]
                           [--insecure] [--await-ci --api-base URL --owner O --repo R --token T]
`)
}

func runExport(args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	dbPath := fs.String("db", "", "path to toolkit.db (required)")
	dest := fs.String("dest", "", "registry checkout directory to write into (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dbPath == "" || *dest == "" {
		fmt.Fprintln(os.Stderr, "export: --db and --dest are required")
		return 2
	}
	pool, err := db.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		return 1
	}
	defer pool.Close()

	n, err := registry.ExportFromDB(context.Background(), pool, *dest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export: %v\n", err)
		return 1
	}
	fmt.Printf("exported %d event(s) to %s/events/\n", n, *dest)
	return 0
}

func runValidate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	dir := fs.String("dir", "", "registry checkout directory to validate (required)")
	strictList := fs.String("strict-list", "", "path to a file listing the newly-pushed event files/ids (one per line) to schema-check strictly; the rest of the ledger is the grandfathered immutable baseline. Omit for a full audit (every event schema-checked). The Gitea CI passes the ff-only delta here.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "validate: --dir is required")
		return 2
	}

	var opts registry.ValidateOptions
	if *strictList != "" {
		ids, err := readStrictList(*strictList)
		if err != nil {
			fmt.Fprintf(os.Stderr, "validate: read --strict-list: %v\n", err)
			return 1
		}
		opts.StrictSchemaEventIDs = ids
	}

	rep, err := registry.Validate(context.Background(), *dir, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "validate: %v\n", err)
		return 1
	}
	if rep.OK() {
		if rep.Grandfathered > 0 {
			fmt.Printf("OK: %d event(s) valid (%d strict-checked, %d grandfathered baseline; causal + projection-coherence over all)\n",
				rep.Total, rep.Total-rep.Grandfathered, rep.Grandfathered)
		} else {
			fmt.Printf("OK: %d event(s) valid (schema + causal + projection-coherence)\n", rep.Total)
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "FAIL: %d failure(s) over %d event(s):\n", len(rep.Failures), rep.Total)
	for _, f := range rep.Failures {
		fmt.Fprintln(os.Stderr, "  "+f.String())
	}
	return 1
}

// readStrictList parses a newline-delimited file of event files or ids into
// a set of event_ids. Each line may be a bare event_id, a filename
// (<event_id>.json), or a path ending in <event_id>.json (what
// `git diff --name-only` emits); the event_id is the basename minus .json.
// Blank lines and lines outside events/ are ignored.
func readStrictList(path string) (map[string]bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool)
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		base := filepath.Base(line)
		id := strings.TrimSuffix(base, ".json")
		if id != "" {
			out[id] = true
		}
	}
	return out, nil
}

func runVerifyDR(args []string) int {
	fs := flag.NewFlagSet("verify-dr", flag.ContinueOnError)
	dbPath := fs.String("db", "", "path to the source toolkit.db (required)")
	dir := fs.String("dir", "", "registry checkout directory to reconstruct from (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dbPath == "" || *dir == "" {
		fmt.Fprintln(os.Stderr, "verify-dr: --db and --dir are required")
		return 2
	}
	pool, err := db.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		return 1
	}
	defer pool.Close()

	if err := registry.VerifyDR(context.Background(), pool, *dir); err != nil {
		fmt.Fprintf(os.Stderr, "DR verification FAILED: %v\n", err)
		return 1
	}
	fmt.Println("OK: registry is a faithful DR source — clone → replay → rebuild is byte-identical to source")
	return 0
}

func runMirror(args []string) int {
	fs := flag.NewFlagSet("mirror", flag.ContinueOnError)
	dbPath := fs.String("db", "", "path to the local toolkit.db (required)")
	regDir := fs.String("registry", "", "path to the registry checkout to mirror into + push from (required)")
	remote := fs.String("remote", "origin", "git remote to push to")
	branch := fs.String("branch", "main", "branch to push to")
	insecure := fs.Bool("insecure", false, "skip TLS verification (homelab self-signed cert)")
	awaitCI := fs.Bool("await-ci", false, "after push, poll the CI verdict for the mirrored commit (the completion ping)")
	apiBase := fs.String("api-base", "", "Gitea API base for --await-ci (e.g. https://host/git/api/v1)")
	owner := fs.String("owner", "", "registry repo owner for --await-ci")
	repo := fs.String("repo", "", "registry repo name for --await-ci")
	token := fs.String("token", "", "Gitea token for --await-ci (Authorization: token)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dbPath == "" || *regDir == "" {
		fmt.Fprintln(os.Stderr, "mirror: --db and --registry are required")
		return 2
	}
	pool, err := db.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		return 1
	}
	defer pool.Close()

	ctx := context.Background()
	res, err := registry.Mirror(ctx, pool, *regDir, registry.MirrorOptions{
		Remote: *remote, Branch: *branch, InsecureTLS: *insecure,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mirror: %v\n", err)
		return 1
	}
	if !res.Pushed {
		fmt.Printf("nothing to mirror: %s (%d events total)\n", res.Message, res.TotalEvents)
		return 0
	}
	fmt.Printf("%s\n", res.Message)

	if !*awaitCI {
		return 0
	}
	if *apiBase == "" || *owner == "" || *repo == "" {
		fmt.Fprintln(os.Stderr, "--await-ci needs --api-base, --owner, --repo (and usually --token)")
		return 2
	}
	fmt.Println("awaiting CI verdict (the completion ping)…")
	fetcher := &registry.GiteaStatusFetcher{
		APIBase: *apiBase, Owner: *owner, Repo: *repo, Token: *token, Insecure: *insecure,
	}
	verdict, err := registry.PollVerdict(ctx, fetcher, res.CommitSHA, 60, 15*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "poll verdict: %v\n", err)
		return 1
	}
	switch verdict.State {
	case "success":
		fmt.Printf("CI verdict: BLESSED ✓ (%s) — %s\n", verdict.SHA[:min12(verdict.SHA)], verdict.Description)
		return 0
	case "pending":
		fmt.Printf("CI verdict: still pending after the poll window for %s — re-ping later\n", verdict.SHA[:min12(verdict.SHA)])
		return 0
	default:
		fmt.Fprintf(os.Stderr, "CI verdict: %s ✗ — %s\n", strings.ToUpper(verdict.State), verdict.Description)
		return 1
	}
}

func min12(s string) int {
	if len(s) < 12 {
		return len(s)
	}
	return 12
}
