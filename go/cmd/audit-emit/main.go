// Command audit-emit emits one-or-more registered *AuditCompleted events
// from a JSON findings spec, through events.EmitRecord. It is the
// data-driven replacement for the family of one-shot go/cmd/*-audit-emit
// binaries that each baked a chain retrospective's findings into Go literals
// and called events.Emit: the findings now live in spec files under specs/,
// and any of them can be (re-)emitted with this single command.
//
// Usage:
//
//	audit-emit --db data/toolkit.db --spec specs/memory-substrate.json
//	audit-emit --db data/toolkit.db --spec specs/          # every *.json in dir
//	audit-emit --spec specs/memory-substrate.json --check  # validate only, no DB
//
// A spec carries the actor + chain entity an audit's events are emitted
// under, plus one-or-more typed events (the bare agent-first-substrate audit
// emitted two — Architecture + Convention — under one entity, which the
// events array preserves). See go/internal/auditemit for the spec shape and
// the parity guarantee with the typed emit path.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"

	"toolkit/internal/auditemit"
	"toolkit/internal/db"
)

func main() {
	dbPath := flag.String("db", "", "path to toolkit.db (required unless --check)")
	specPath := flag.String("spec", "", "spec file or directory of *.json specs (required)")
	check := flag.Bool("check", false, "validate the spec(s) against schemas and exit without emitting")
	flag.Parse()

	if *specPath == "" {
		log.Fatal("audit-emit: --spec is required")
	}
	specFiles, err := resolveSpecs(*specPath)
	if err != nil {
		log.Fatalf("audit-emit: %v", err)
	}
	if len(specFiles) == 0 {
		log.Fatalf("audit-emit: no *.json specs found at %s", *specPath)
	}

	ctx := context.Background()

	if *check {
		for _, f := range specFiles {
			spec, err := auditemit.Load(f)
			if err != nil {
				log.Fatalf("audit-emit: %v", err)
			}
			if err := auditemit.CheckSchema(ctx, spec); err != nil {
				log.Fatalf("audit-emit: %s: %v", f, err)
			}
			fmt.Printf("ok   %s (%d events)\n", f, len(spec.Events))
		}
		return
	}

	if *dbPath == "" {
		log.Fatal("audit-emit: --db is required (or pass --check to validate only)")
	}
	if _, err := os.Stat(*dbPath); err != nil {
		log.Fatalf("audit-emit: stat %s: %v", *dbPath, err)
	}
	pool, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("audit-emit: open db: %v", err)
	}
	defer pool.Close()

	for _, f := range specFiles {
		spec, err := auditemit.Load(f)
		if err != nil {
			log.Fatalf("audit-emit: %v", err)
		}
		emitted, err := auditemit.Emit(ctx, pool, spec)
		if err != nil {
			log.Fatalf("audit-emit: %s: %v", f, err)
		}
		for _, e := range emitted {
			fmt.Printf("%s: %s\n", e.Type, e.EventID)
		}
	}
}

// resolveSpecs returns the spec files named by path: the single file as-is,
// or every *.json in the directory sorted for a deterministic emit order.
func resolveSpecs(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	matches, err := filepath.Glob(filepath.Join(path, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}
