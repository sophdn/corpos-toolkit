package actiondocs

import "embed"

// corpusFS holds the per-action documentation corpus, baked into the
// toolkit-server binary at compile time. The corpus moved under the Go
// module tree (chain single-source-action-describe T6) precisely so
// go:embed can reach it: embed only sees files inside the module rooted
// at go/, and the corpus previously lived at repo-root
// blueprints/action-docs/ — outside the module.
//
// Embedding makes the corpus ALWAYS present: admin.action_describe and
// the dashboard action-docs browser no longer depend on a
// --action-docs-dir flag resolving a real path at startup. The flag
// survives only as a dev/hot-reload override (see LoadEmbedded vs Load).
//
// The `all:` prefix is load-bearing: go:embed's default rules skip
// underscore-prefixed names recursively, which would silently drop every
// surface's _general.toml (the GeneralAction cross-cutting chunk) from the
// embedded set. all: includes them, so the embedded corpus matches an
// on-disk Load chunk-for-chunk (pinned by TestLoadEmbedded_MatchesOnDiskCorpus).
// The top-level _schema.toml and README.md ride along too but Load ignores
// both (it only descends into surface subdirectories and filters to .toml).
//
//go:embed all:corpus
var corpusFS embed.FS
