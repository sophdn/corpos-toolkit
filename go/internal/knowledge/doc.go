// Package knowledge serves the knowledge meta-tool — vault + Kiwix
// retrieval and grounding-event capture.
//
// ## Intended use
//
// **Workflow served:** agents searching the local knowledge vault or
// the offline Kiwix archives hit this meta-tool; the handler runs FTS5
// retrieval, reranks through Qwen via internal/inference, records the
// search trajectory in `grounding_events`, and returns a ranked path
// list the agent can `knowledge_read`.
//
// **Invocation pattern:** `knowledge_search` with
// `{query, corpus, k}` (corpus = `"vault"` | `"kiwix"`);
// `knowledge_read` to fetch a specific path; `kiwix_*` actions to query
// the offline encyclopedic corpus addressed by `<zim_id>/<slug>`.
//
// **Success shape:** a slice of `KnowledgeHit{Path, Title, Score, Excerpt}`
// rows; one `grounding_events` row gets inserted per call so the
// query-telemetry-substrate (sibling chain) can reconstruct retrieval
// trajectories by span_id.
//
// **Non-goals:** does not host the corpora (the vault lives under
// `~/.claude/vault/`; Kiwix zim files live on local disk), does not
// own the rerank model (calls into internal/inference), does not
// enforce vault filing discipline — that lives in the agent's
// `vault-filing-discipline` skill, not in this handler.
package knowledge
