// Package ecosystem is the local-ecosystem service: a tenant-agnostic,
// deterministic map of the agent-host's world — which hosts exist, what services
// run on them, and how (and whether) the agent can reach each one.
//
// It exists to kill a recurring retrieval-miss friction. Crystallized ecosystem
// facts ("do I have access to example-host?", "where is gitea?", "what's the ssh
// user?") historically lived only as soft vault/memory prose retrieved
// probabilistically, so a cold agent would miss them, wrongly deny access, and
// need a human correction. This surface answers those questions DETERMINISTICALLY
// from a small direct-write store, with the same answer every time.
//
// ## Intended use
//
// **Workflow served:** an agent (or a human) needs a crystallized ecosystem/access
// fact — "do I have access to X", "where does service Y run", "what's the ssh
// user for host Z" — answered the same way every time, with no RAG round-trip and
// no correction loop. The operator seeds the map once via the learn actions;
// every later query is a deterministic lookup.
//
// **Invocation pattern:** BuildTable(Deps{Pool}) → the `ecosystem` MCP meta-tool.
// Reads: access_check / describe / list (ungated). Writes: host_learn /
// service_learn / access_learn (idempotent upsert, rationale-gated). ResolveAccess
// is the shared pure resolver both the access_check action and the refresolve
// ecosystem resolver call, so the explicit query and the parse_context orient-time
// answer can never diverge.
//
// **Success shape:** access_check returns an AccessSummary with a status of
// yes / no / unknown, the usable access methods (method + principal + address +
// credential_pointer), addresses, service endpoint, soft_refs, and a composed
// one-line answer.
//
// **Non-goals:** NOT a secret store (holds pointers only), NOT a live-state
// prober (that is the `sys` surface's ports/units/containers introspection), and
// NOT a home for procedural how-to prose (that stays soft in the vault, reached
// via soft_ref). It records declarative facts, not lifecycle history — hence
// direct-write, not event-sourced.
//
// # Model (chain 435 local-ecosystem-service-and-extraction-pattern)
//
//   - hosts (REUSED from migration 003 — the shared-infra host registry; carries
//     the inline SSH access method: ssh_user + ssh_key_path pointer).
//   - ecosystem_host_addresses — a host's alternate reachable addresses
//     (tailnet / lan / magicdns), with a preferred flag.
//   - ecosystem_services — something running on a host (endpoint, port,
//     live|retired status, soft_ref to the vault how-to).
//   - ecosystem_access_methods — how you authenticate to a host or service
//     (method, principal, credential_pointer, scope_note, enabled). Polymorphic
//     target (target_kind + target_slug), no back-pointer.
//
// # Tenant-agnostic
//
// The surface ships EMPTY. Nothing here hardcodes any host or service name; a new
// adopter's ecosystem is LEARNED via the *_learn actions (data, not code), exactly
// as the reused `hosts` table fills via registration. An un-learned target returns
// status "unknown" — never a hallucinated "no".
//
// # Determinism-vs-RAG boundary
//
// This surface answers WHETHER / WHERE / HOW-TO-REACH (the crystallized, exactly-
// one-right-answer facts). The procedural HOW-TO prose (transfer methods, deploy
// flows, gotchas, decisions) legitimately stays soft in the vault; a record's
// soft_ref points back at it. The service is the deterministic index; the vault is
// the narrative.
//
// # Credential invariant
//
// credential_pointer stores a POINTER to where a secret lives (a path or env name),
// NEVER a secret value. access_learn rejects pointer values that look like inline
// secrets. Real secrets stay in the filesystem/agent, untouched by this store.
package ecosystem
