// Package auditemit is the data-driven replacement for the family of
// one-shot go/cmd/*-audit-emit emitters. Each historical emitter baked a
// chain retrospective's findings into Go literals and called events.Emit;
// this package reads the same content from a JSON findings spec and lands
// it through events.EmitRecord (the raw-payload twin of Emit), so one
// generic command (go/cmd/audit-emit) can re-emit any registered
// *AuditCompleted event from a spec file.
//
// ## Intended use
//
//   - Workflow served: collapsing the ~17 near-identical *-audit-emit
//     one-shots into one parameterized command + per-audit findings specs.
//     A chain retrospective authors a spec (or one is generated from a
//     prior emitter) and re-emits it; the emitted row is indistinguishable
//     from one the typed emitter produced, because EmitRecord and Emit
//     share the validator and the events table writer.
//   - Invocation pattern: Load(path) -> Spec, then Emit(ctx, pool, spec).
//     The generic command wraps these; tests drive them directly against a
//     hermetic pool.
//   - Success shape: every event in the spec lands as a schema-valid events
//     row of the spec's type/entity/payload, in spec order; Emit returns the
//     (type, event_id) pairs. A schema-invalid or unknown-type event is
//     rejected before its INSERT, leaving the ledger clean.
//   - Non-goals: this package does not author findings content, does not
//     own the event schemas (internal/events does), and does not dedupe
//     against already-emitted audits — re-emitting a spec emits a new row.
package auditemit
