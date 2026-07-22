// Package qwenretrieve assembles prompts for Qwen-based corpus retrieval
// — the shared shape used by both vault and Kiwix retrieval paths.
//
// ## Intended use
//
// **Workflow served:** the internal/knowledge handlers need consistent
// system prompts and user-prompt headers when ranking candidates
// through Qwen; centralizing the prompt-shape selection here means
// vocabulary / formatting changes land in one place rather than
// drifting across the two retrieval surfaces.
//
// **Invocation pattern:** `system, user := qwenretrieve.BuildPrompt(
// shape, query, candidates, withBody)` where `shape` is
// `CorpusShapeVault` or `CorpusShapeKiwix`; the knowledge handler
// passes these straight to `inference.Generate`.
//
// **Success shape:** returns two strings (system prompt, user prompt)
// shaped for the corpus's vocabulary — vault titles for `CorpusShapeVault`
// or `<zim_id>/<slug>` paths for `CorpusShapeKiwix`; the model's ranked
// output is parseable as one candidate index per line.
//
// **Non-goals:** not a model client (calls into internal/inference are
// the caller's responsibility), not a parser of the model's ranked
// output (internal/knowledge does the parsing), not the corpus index
// — purely the prompt-shape layer.
package qwenretrieve
