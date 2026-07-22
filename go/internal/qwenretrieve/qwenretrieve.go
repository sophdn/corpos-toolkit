// Package qwenretrieve hosts shape-aware prompt composition and orchestration
// for Qwen-driven retrieve calls. Mirrors mcp-servers/inference-clients/src/dispatcher
// on the Rust side, ported per PARITY_STANDARD.md. Named to disambiguate from
// internal/dispatch (the MCP meta-tool router).
//
// Retrieve call sites (vault_search, kiwix_search rerank, knowledge_search)
// route through DispatchTwoPassRetrieve.
package qwenretrieve
