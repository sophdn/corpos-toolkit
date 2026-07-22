// Package projections is the denormalised read-path substrate for the
// toolkit-server work meta-tool. Each [Projection] folds events (and,
// transitionally, reads CRUD) into a `proj_*` table that the dashboard
// and observe-http endpoints query directly. See docs/PROJECTIONS.md
// for the full design and per-projection contracts.
//
// ## Intended use
//
// Workflow served: handler emits an event via [events.Emit]; the
// emit's fold hook (registered at server startup via [events.SetFoldHook])
// invokes [FoldAll] inside the same SQLite transaction. Each registered
// projection inspects the event and refreshes its row(s) from CRUD —
// the projection table converges on the post-write state in lockstep
// with the event INSERT.
//
// Invocation pattern: handlers DO NOT call into this package directly.
// Projection rebuilds run via `toolkit-server rebuild-projections`,
// which iterates [All] and invokes [Projection.RebuildFromEmpty] for
// each. Tests register projections at init() time exactly like
// production wiring; the projection-fold path is the same code in
// both contexts.
//
// Success shape: a successful [FoldAll] returns nil and leaves the
// projection table + watermark advanced. Fold failure returns an error
// that propagates through [events.Emit] → handler's WithWrite closure
// → caller; the entire transaction (event INSERT + CRUD UPDATE +
// projection refresh) rolls back as a unit. Eventual consistency is
// rejected per chain design_decisions item 4.
//
// Non-goals: projections do not perform schema validation (that's
// [events.Validate]'s job at emit time); do not rate-limit fold work
// (synchronous-by-design); do not own actor / rationale propagation
// (those are envelope concerns on the events row itself).
//
// Cross-projection dependencies: a projection whose Fold READS the
// table of another registered projection within the same fold tx
// MUST implement [DependentProjection] and name its dependencies via
// DependsOn. [All] performs a topological sort honoring these
// declarations so cross-reads always see post-fold state from the
// dependency. Without the declaration, alphabetical sort decides
// fold order and silently miscomputes when the reader sorts earlier
// than the dependency (see bug
// proj-chain-status-counters-always-one-task-transition-behind for
// the canonical instance). Today three projections declare deps:
// chain_status → current_tasks; task_blockers → chain_status,
// current_tasks; roadmap_view → chain_status, current_tasks.
package projections

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// RawEvent mirrors one row of the events table — the shape projection
// Fold methods receive. Pointer fields are nullable; JSON columns
// (payload, related_entities) arrive as raw bytes so each projection
// can decode against its own typed schema.
//
// Field-by-field identical to [events.RawEvent]; [FromEventsRaw] bridges
// the two without importing events into projection consumers and vice
// versa.
type RawEvent struct {
	EventID         string
	Ts              string
	ActorKind       string
	ActorID         string
	Type            string
	EntityKind      string
	EntitySlug      string
	EntityProjectID *string
	Payload         json.RawMessage
	Rationale       *string
	CausedByEventID *string
	RelatedEntities json.RawMessage
	SpanID          string
	SchemaVersion   int
}

// FromEventsRaw rebuilds a projection-side RawEvent from a row scanned
// out of the events table (or constructed by the events package's
// post-INSERT hook). Lets callers convert without the projections
// package depending on internal/events for typed-shape import.
func FromEventsRaw(
	eventID, ts, actorKind, actorID, typ, entityKind, entitySlug string,
	entityProjectID *string,
	payload []byte,
	rationale *string,
	causedBy *string,
	related []byte,
	spanID string,
	schemaVersion int,
) RawEvent {
	return RawEvent{
		EventID: eventID, Ts: ts,
		ActorKind: actorKind, ActorID: actorID,
		Type:            typ,
		EntityKind:      entityKind,
		EntitySlug:      entitySlug,
		EntityProjectID: entityProjectID,
		Payload:         payload,
		Rationale:       rationale,
		CausedByEventID: causedBy,
		RelatedEntities: related,
		SpanID:          spanID,
		SchemaVersion:   schemaVersion,
	}
}

// Projection is the contract every projection implements. Auto-registered
// via [Register] from each projection file's init(); the rebuild CLI
// iterates [All] so sibling-chain projections show up without code
// change here.
//
// Method contracts:
//
//   - Name: stable identifier used as the watermark key in
//     `projections_watermark`. Match the `proj_<name>` table name's
//     suffix (e.g. proj_current_bugs → "current_bugs").
//
//   - TableName: the SQL table the projection writes to. Used by
//     RebuildFromEmpty's TRUNCATE step and by tests.
//
//   - Fold: invoked synchronously by [FoldAll] after every event INSERT
//     and during incremental rebuild. The projection inspects evt and
//     refreshes whatever rows the event affects. Idempotent: folding
//     the same event twice produces the same final state. Returns
//     nil on no-op (e.g. event entity kind unrelated to this projection).
//
//   - RebuildFromEmpty: TRUNCATEs and re-snapshots from CRUD. Used by
//     the rebuild CLI's default mode. Caller is responsible for
//     resetting the watermark afterward.
type Projection interface {
	Name() string
	TableName() string
	Fold(ctx context.Context, tx *sql.Tx, evt RawEvent) error
	RebuildFromEmpty(ctx context.Context, tx *sql.Tx) error
}

// DependentProjection is the optional contract a projection implements
// when its Fold or RebuildFromEmpty READS the table of another
// registered projection within the same fold transaction. The names
// returned MUST be the Name()s of projections that this one reads
// from — every one of them runs BEFORE this projection in [All],
// [FoldAll], and [RebuildAll]. Cycles are rejected at the first call
// to [All] with a panic naming the cycle members.
//
// Why this matters: without explicit ordering, projections iterate
// alphabetically and a read from a later-sorted projection captures
// pre-event state — silent off-by-one drift in the reader's
// projection until the next event in the affected entity triggers
// another fold pass (bug
// `proj-chain-status-counters-always-one-task-transition-behind`).
// Today three projections cross-read:
//
//   - chain_status reads proj_current_tasks (the bug above).
//   - task_blockers reads proj_chain_status + proj_current_tasks
//     (worked only by alphabetical accident).
//   - roadmap_view reads proj_chain_status + proj_current_tasks
//     (worked only by alphabetical accident).
//
// All three now declare DependsOn explicitly so the agreement
// invariant (denormalised counter == live COUNT(*)) holds on every
// fold pass and survives any future Name() change.
//
// Projections that read another projection's table OUTSIDE the fold
// transaction (e.g. current_tasks looking up a chain_id row that was
// committed by a previous emit's tx) do NOT need to declare a
// dependency — the read happens across tx boundaries, and the prior
// projection state is already durable. DependsOn is purely about
// within-fold ordering.
type DependentProjection interface {
	DependsOn() []string
}

// dependenciesOf returns the projection's declared in-fold
// dependencies, or empty if it doesn't implement DependentProjection.
// Pure-function helper so the topological sort doesn't have to repeat
// the type-assertion ceremony.
func dependenciesOf(p Projection) []string {
	if dep, ok := p.(DependentProjection); ok {
		return dep.DependsOn()
	}
	return nil
}

// Registry state. registerMu guards Register-time appends; reads happen
// post-init() so the lock is dropped before the server starts taking
// traffic, but the mutex still makes test parallelism safe.
var (
	registerMu sync.Mutex
	registered []Projection
)

// Register adds a projection to the package-level registry. Call from
// each projection's init() so the registration order is deterministic
// (Go runs init() in file-name order within a package; the registry's
// sort-by-name on [All] makes ordering predictable across builds).
func Register(p Projection) {
	registerMu.Lock()
	defer registerMu.Unlock()
	for _, existing := range registered {
		if existing.Name() == p.Name() {
			panic(fmt.Sprintf("projections: duplicate registration for %q", p.Name()))
		}
	}
	registered = append(registered, p)
}

// All returns a stable-ordered slice of every registered projection.
// Ordering is topological per the optional [DependentProjection]
// contract: a projection's declared DependsOn list always appears
// earlier in the returned slice. Ties between independent projections
// are broken alphabetically so the order is fully deterministic across
// builds. The rebuild CLI iterates this list; tests assert against
// expected names; [FoldAll] and [RebuildAll] both consume it so
// in-fold ordering is single-sourced.
//
// Panics with a clear error when projections declare a dependency
// cycle or name an unregistered projection. Both conditions are
// authoring errors, not runtime conditions — better to fail loudly at
// the first All() call (which init-time tests will trigger) than to
// silently miscompute fold order.
func All() []Projection {
	registerMu.Lock()
	defer registerMu.Unlock()
	return topoSort(registered)
}

// topoSort returns a topological ordering of projections honoring
// every DependsOn declaration. Independent projections (no declared
// dependency between them) appear in alphabetical order, preserving
// the pre-DependsOn ordering for migration friendliness. Panics on
// unknown-dep or cycle; the panic message names the offending
// projections so the failure is actionable.
//
// Algorithm: Kahn's BFS over the dependency graph with
// alphabetical-name tie-breaking on the ready-queue. Stable across
// reruns; complexity O(V + E) where V ≤ low-dozen projections.
func topoSort(in []Projection) []Projection {
	// Sort input alphabetically once so tie-breaking among independent
	// nodes is deterministic.
	nodes := make([]Projection, len(in))
	copy(nodes, in)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name() < nodes[j].Name() })

	byName := make(map[string]Projection, len(nodes))
	for _, p := range nodes {
		byName[p.Name()] = p
	}

	// inDegree counts unresolved dependencies per node. dependents[x]
	// lists every projection that depends on x — when x is emitted,
	// its dependents' in-degrees drop.
	inDegree := make(map[string]int, len(nodes))
	dependents := make(map[string][]string, len(nodes))
	for _, p := range nodes {
		for _, dep := range dependenciesOf(p) {
			if _, ok := byName[dep]; !ok {
				panic(fmt.Sprintf("projections: %q declares DependsOn %q which is not registered", p.Name(), dep))
			}
			if dep == p.Name() {
				panic(fmt.Sprintf("projections: %q declares DependsOn on itself", p.Name()))
			}
			inDegree[p.Name()]++
			dependents[dep] = append(dependents[dep], p.Name())
		}
	}

	// Seed the ready queue with every zero-in-degree node, alphabetical.
	ready := make([]string, 0, len(nodes))
	for _, p := range nodes {
		if inDegree[p.Name()] == 0 {
			ready = append(ready, p.Name())
		}
	}
	sort.Strings(ready)

	out := make([]Projection, 0, len(nodes))
	emitted := make(map[string]bool, len(nodes))
	for len(ready) > 0 {
		// Pop the alphabetically-smallest ready node so independent
		// projections preserve the pre-DependsOn alphabetical order.
		name := ready[0]
		ready = ready[1:]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		out = append(out, byName[name])

		// Drop the in-degree of each dependent; queue any that reach
		// zero, re-sorting to keep the alphabetical tie-break.
		newlyReady := []string{}
		for _, child := range dependents[name] {
			inDegree[child]--
			if inDegree[child] == 0 {
				newlyReady = append(newlyReady, child)
			}
		}
		if len(newlyReady) > 0 {
			ready = append(ready, newlyReady...)
			sort.Strings(ready)
		}
	}

	if len(out) != len(nodes) {
		// Unemitted nodes form a cycle. Name them in the panic so the
		// offender is obvious without re-running with a debugger.
		stuck := []string{}
		for _, p := range nodes {
			if !emitted[p.Name()] {
				stuck = append(stuck, p.Name())
			}
		}
		panic(fmt.Sprintf("projections: DependsOn cycle detected among %v", stuck))
	}
	return out
}

// Get returns the named projection or (nil, false). Used by the
// rebuild CLI when --projection=<name> targets one projection.
func Get(name string) (Projection, bool) {
	for _, p := range All() {
		if p.Name() == name {
			return p, true
		}
	}
	return nil, false
}

// resetRegistryForTests is a test-only seam — clears the registered
// slice so a test can drive a known set of projections without picking
// up the production init() registrations. Not exported; only the test
// file in this package can reach it.
func resetRegistryForTests() {
	registerMu.Lock()
	defer registerMu.Unlock()
	registered = nil
}

// FoldAll invokes every registered projection's Fold against evt and
// advances each projection's watermark. Run inside the same tx as the
// event INSERT that produced evt — fold failure rolls the tx back.
//
// Order: projections iterate in [All]-order (sorted by name). Folds are
// independent at the SQL level (each projection touches its own table),
// so order is observable only via watermark advance — and the watermark
// advance happens for every projection regardless of order, so the
// per-projection final state is order-independent.
func FoldAll(ctx context.Context, tx *sql.Tx, evt RawEvent) error {
	for _, p := range All() {
		if err := p.Fold(ctx, tx, evt); err != nil {
			return fmt.Errorf("projection %s fold: %w", p.Name(), err)
		}
		if err := WriteWatermark(ctx, tx, p.Name(), evt.EventID, evt.Ts); err != nil {
			return fmt.Errorf("projection %s watermark: %w", p.Name(), err)
		}
	}
	return nil
}

// readSidePrefixes names the projection-name prefixes that fold from
// the read-side telemetry substrate (grounding_events / query_interactions
// / query_resolutions) instead of the write-side events ledger. Members:
//   - "query_":      query-telemetry-substrate volume rollup
//   - "retrieval_":  query-telemetry-substrate per-query success
//   - "training_":   query-telemetry-substrate ML training data
//   - "injection_":  proactive-injection follow-on chain (reserved)
//   - "offload_":    Qwen/ML offload future chains (reserved)
//   - "inference_":  per-call inference telemetry (chain per-tool-per-model-
//     observability): proj_inference_tool_model_performance
//     re-snapshots from inference_invocations
//
// docs/TELEMETRY_SUBSTRATE.md §7.3 documents the namespace contract.
// query-telemetry-substrate owns query_/retrieval_/training_ collectively;
// TT3 §AC names the per-table prefix differently from the chain-level
// "query_* namespace" label, so all three are read-side prefixes.
var readSidePrefixes = []string{"query_", "retrieval_", "training_", "injection_", "offload_", "inference_"}

// isReadSideName reports whether name carries one of the read-side
// namespace prefixes. Used by FoldAllReadSide to filter All() to the
// telemetry-emit-triggered projections.
func isReadSideName(name string) bool {
	for _, p := range readSidePrefixes {
		if len(name) >= len(p) && name[:len(p)] == p {
			return true
		}
	}
	return false
}

// rebuildProjection is the shared DELETE-then-INSERT workhorse for the
// read-side query_* projections. Each projection's RebuildFromEmpty
// passes its TableName + a per-projection INSERT…SELECT…GROUP BY SQL
// string; the error-wrapping ceremony lives here once. Used only by
// read-side projections that snapshot from CRUD on every Fold; the
// write-side projections (current_bugs / chain_status / roadmap_view)
// don't truncate per fold and don't need this helper.
func rebuildProjection(ctx context.Context, tx *sql.Tx, tableName, insertSQL string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM "+tableName); err != nil {
		return fmt.Errorf("truncate %s: %w", tableName, err)
	}
	if _, err := tx.ExecContext(ctx, insertSQL); err != nil {
		return fmt.Errorf("rebuild %s: %w", tableName, err)
	}
	return nil
}

// FoldAllReadSide invokes the read-side projections' Fold methods —
// the ones whose Name() starts with a read-side namespace prefix.
// Trigger is a telemetry emit (telemetry.EmitInteraction /
// telemetry.EmitResolution); the bootstrap wires this via
// telemetry.SetFoldHook so emits propagate fold failures back through
// the same tx (per TT3 AC, fold failure aborts the emit).
//
// Read-side Folds ignore the RawEvent argument and re-snapshot their
// table from CRUD. The RawEvent passed here is the zero value — a
// placeholder so the shared Projection interface stays single-shape.
func FoldAllReadSide(ctx context.Context, tx *sql.Tx) error {
	for _, p := range All() {
		if !isReadSideName(p.Name()) {
			continue
		}
		if err := p.Fold(ctx, tx, RawEvent{}); err != nil {
			return fmt.Errorf("read-side projection %s fold: %w", p.Name(), err)
		}
	}
	return nil
}

// ReadWatermark returns the per-projection watermark (highest event_id
// folded) or empty strings when no row exists for the projection. Reads
// through the supplied tx so callers can run inside a snapshot.
func ReadWatermark(ctx context.Context, tx *sql.Tx, name string) (eventID, ts string, err error) {
	var eid, et sql.NullString
	row := tx.QueryRowContext(ctx,
		`SELECT last_event_id, last_folded_ts FROM projections_watermark WHERE projection_name = ?`,
		name)
	if err := row.Scan(&eid, &et); err != nil {
		if err == sql.ErrNoRows {
			return "", "", nil
		}
		return "", "", err
	}
	return eid.String, et.String, nil
}

// WriteWatermark upserts the watermark for the supplied projection.
// Idempotent (multiple folds of the same event leave the column
// unchanged); the INSERT-OR-REPLACE shape keeps the watermark row
// present whether it was seeded by migration 033 or first touched
// during fold.
func WriteWatermark(ctx context.Context, tx *sql.Tx, name, eventID, ts string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO projections_watermark (projection_name, last_event_id, last_folded_ts)
		 VALUES (?, ?, ?)
		 ON CONFLICT(projection_name) DO UPDATE SET last_event_id = excluded.last_event_id,
		   last_folded_ts = excluded.last_folded_ts`,
		name, eventID, ts)
	return err
}

// ResetWatermark clears the watermark for the supplied projection.
// Called by RebuildAll after a from-empty snapshot — the snapshot
// represents post-CRUD truth, so future folds start at the highest
// event_id present at rebuild time (set by RebuildAll separately).
func ResetWatermark(ctx context.Context, tx *sql.Tx, name string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO projections_watermark (projection_name, last_event_id, last_folded_ts)
		 VALUES (?, NULL, NULL)
		 ON CONFLICT(projection_name) DO UPDATE SET last_event_id = NULL,
		   last_folded_ts = NULL`,
		name)
	return err
}

// RebuildResult is the per-projection summary returned by RebuildAll.
// Used by the rebuild CLI's stdout formatting; structured so tests can
// assert against it without parsing log lines.
type RebuildResult struct {
	Name      string
	TableName string
	Rows      int64
	Watermark string
}

// RebuildAll executes a from-empty rebuild for every projection in
// `names` (or every registered write-side projection when names is
// empty). Runs entirely inside one write transaction so a mid-rebuild
// crash rolls back to the pre-rebuild state.
//
// Full-rebuild path (names is empty or nil): TRUNCATE every target's
// table, then walk the events log in event_id order and pass each
// event through FoldAll. This is the only way to satisfy
// cross-projection rebuild dependencies cleanly — when chain_status
// reads proj_current_tasks during a task event's fold, current_tasks
// has already folded that same event because FoldAll respects the
// DependsOn topology. Per-projection RebuildFromEmpty methods walk
// the events table independently, which leaves them blind to the
// fresh state their dependencies have not yet rebuilt; the unified
// driver here side-steps that by sharing one event walk across all
// targets.
//
// Targeted path (names non-empty): TRUNCATE each named projection
// and call its RebuildFromEmpty. Sibling projection tables remain
// populated from prior rebuilds, so the targeted projection's
// in-fold reads against sibling tables (current_tasks reading
// chain_status, etc.) still hit live data. Use this path only when
// you intentionally want to rebuild one projection without disturbing
// the others.
func RebuildAll(ctx context.Context, tx *sql.Tx, names []string) ([]RebuildResult, error) {
	var maxEventID, maxTs sql.NullString
	if err := tx.QueryRowContext(ctx,
		// ts is the chronological authority; event_id only tiebreaks
		// same-tx emits. See go/internal/events/doc.go §Invariant.
		`SELECT event_id, ts FROM events ORDER BY ts DESC, event_id DESC LIMIT 1`,
	).Scan(&maxEventID, &maxTs); err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("read current max event_id: %w", err)
	}

	if len(names) == 0 {
		return rebuildAllFullViaFoldAll(ctx, tx, maxEventID, maxTs)
	}

	targets := make([]Projection, 0, len(names))
	for _, n := range names {
		p, ok := Get(n)
		if !ok {
			return nil, fmt.Errorf("unknown projection: %q", n)
		}
		targets = append(targets, p)
	}

	out := make([]RebuildResult, 0, len(targets))
	for _, p := range targets {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+p.TableName()); err != nil {
			return nil, fmt.Errorf("truncate %s: %w", p.TableName(), err)
		}
		if err := p.RebuildFromEmpty(ctx, tx); err != nil {
			return nil, fmt.Errorf("rebuild %s: %w", p.Name(), err)
		}
		var rows int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+p.TableName()).Scan(&rows); err != nil {
			return nil, fmt.Errorf("count %s: %w", p.TableName(), err)
		}
		if maxEventID.Valid {
			if err := WriteWatermark(ctx, tx, p.Name(), maxEventID.String, maxTs.String); err != nil {
				return nil, fmt.Errorf("watermark %s: %w", p.Name(), err)
			}
		} else {
			if err := ResetWatermark(ctx, tx, p.Name()); err != nil {
				return nil, fmt.Errorf("reset watermark %s: %w", p.Name(), err)
			}
		}
		out = append(out, RebuildResult{
			Name:      p.Name(),
			TableName: p.TableName(),
			Rows:      rows,
			Watermark: maxEventID.String,
		})
	}
	return out, nil
}

// rebuildAllFullViaFoldAll is the full-rebuild path: TRUNCATE every
// write-side target, then walk events once and pass each through
// FoldAll so projections see events in DependsOn-respecting order.
// Read-side projections (query_*, retrieval_*, training_*) snapshot
// from the read-side telemetry substrate, not the events ledger, so
// they're excluded from the event-walk and rebuilt via their own
// RebuildFromEmpty after the walk.
//
// This is the single-source-of-truth ordering: same FoldAll the
// production fold hook uses, same topology, no per-projection event
// walks competing for state. Resolves the genuine bidirectional
// rebuild dependency between chain_status and current_tasks that the
// pre-fix "two warmup rebuilds" hack worked around.
func rebuildAllFullViaFoldAll(ctx context.Context, tx *sql.Tx, maxEventID, maxTs sql.NullString) ([]RebuildResult, error) {
	targets := All()
	for _, p := range targets {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+p.TableName()); err != nil {
			return nil, fmt.Errorf("truncate %s: %w", p.TableName(), err)
		}
	}

	// Read-side projections (query_*, retrieval_*, training_*) fold
	// off the telemetry substrate, not the events ledger. The
	// shared-event walk below feeds them empty events for entity_kinds
	// they ignore — but to keep their snapshot fresh after rebuild,
	// run their RebuildFromEmpty after the walk completes.
	writeSide := []Projection{}
	readSide := []Projection{}
	for _, p := range targets {
		if isReadSideName(p.Name()) {
			readSide = append(readSide, p)
		} else {
			writeSide = append(writeSide, p)
		}
	}

	// Walk events in event_id order; pass each through FoldAll so
	// projections see them in the topological dependency order
	// every other emit path uses.
	rows, err := tx.QueryContext(ctx, `
		SELECT event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug,
		       entity_project_id, payload, rationale, caused_by_event_id,
		       related_entities, span_id, schema_version
		FROM events
		ORDER BY ts ASC, event_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("read events for replay: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var evt RawEvent
		var entityProjectID, rationale, causedBy sql.NullString
		var payloadStr, relatedStr string
		if err := rows.Scan(&evt.EventID, &evt.Ts, &evt.ActorKind, &evt.ActorID,
			&evt.Type, &evt.EntityKind, &evt.EntitySlug, &entityProjectID,
			&payloadStr, &rationale, &causedBy, &relatedStr,
			&evt.SpanID, &evt.SchemaVersion); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		evt.Payload = json.RawMessage(payloadStr)
		evt.RelatedEntities = json.RawMessage(relatedStr)
		if entityProjectID.Valid {
			s := entityProjectID.String
			evt.EntityProjectID = &s
		}
		if rationale.Valid {
			s := rationale.String
			evt.Rationale = &s
		}
		if causedBy.Valid {
			s := causedBy.String
			evt.CausedByEventID = &s
		}
		// Drive through every write-side projection's Fold in topo
		// order. Skip read-side here — they're rebuilt below.
		for _, p := range writeSide {
			if err := p.Fold(ctx, tx, evt); err != nil {
				return nil, fmt.Errorf("replay event %s through %s: %w", evt.EventID, p.Name(), err)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("event-walk rows.Err: %w", err)
	}

	// Read-side projections snapshot from telemetry tables, not the
	// events ledger, so re-run their RebuildFromEmpty now.
	for _, p := range readSide {
		if err := p.RebuildFromEmpty(ctx, tx); err != nil {
			return nil, fmt.Errorf("read-side rebuild %s: %w", p.Name(), err)
		}
	}

	// Per-projection watermark + row-count + result construction.
	out := make([]RebuildResult, 0, len(targets))
	for _, p := range targets {
		var rowCount int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+p.TableName()).Scan(&rowCount); err != nil {
			return nil, fmt.Errorf("count %s: %w", p.TableName(), err)
		}
		if maxEventID.Valid {
			if err := WriteWatermark(ctx, tx, p.Name(), maxEventID.String, maxTs.String); err != nil {
				return nil, fmt.Errorf("watermark %s: %w", p.Name(), err)
			}
		} else {
			if err := ResetWatermark(ctx, tx, p.Name()); err != nil {
				return nil, fmt.Errorf("reset watermark %s: %w", p.Name(), err)
			}
		}
		out = append(out, RebuildResult{
			Name:      p.Name(),
			TableName: p.TableName(),
			Rows:      rowCount,
			Watermark: maxEventID.String,
		})
	}
	return out, nil
}
