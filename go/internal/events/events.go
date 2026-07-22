// Package events is the append-only audit-trail substrate for the toolkit-server
// work meta-tool. Every state mutation dual-writes — event row first, CRUD row
// second — inside a single SQLite transaction. The events table is the source
// of truth; CRUD tables are a denormalized projection cache. See
// docs/EVENT_SUBSTRATE.md for the full design.
//
// ## Intended use
//
// Workflow served: agent or human mutates work-meta-tool state (resolve a
// bug, complete a task, close a chain, record a benchmark run). The handler
// constructs an [EmitArgs] with a typed payload and calls [Emit] inside
// the WithWrite closure that performs the CRUD update.
//
// Invocation pattern: Emit(ctx, tx, EmitArgs{Payload: BugResolvedPayload{...}}).
// The Payload's concrete type identifies the event type via its
// EventType() method; the validator cross-checks the marshaled JSON
// shape against the embedded blueprint schema. Actor and span_id come
// from ctx via [WithActor] / [WithSpanID] (T3 wires the real values
// at the dispatch boundary; this package's defaults make the foundation
// testable in isolation).
//
// Success shape: returns the generated UUIDv7 event_id and nil error;
// the events table row is committed when the enclosing WithWrite closure
// commits. On schema-validation failure, returns *ErrInvalidInput before
// any DB write — the enclosing transaction stays clean.
//
// Non-goals: this package does not enforce that rationale is non-empty
// for agent actors (T3's dispatch-boundary middleware does); does not run
// projection folds (T4); does not own actor inference at the transport
// layer (T3 again). It is the substrate primitive that those tasks build on.
package events

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// SchemaVersion is the current event envelope version. Bumping requires a
// migration that backfills or maps every existing event. Per-type evolution
// is via type renaming (BugResolvedV2), not per-type version. See
// docs/EVENT_SUBSTRATE.md §2.
const SchemaVersion = 1

// Actor is who emitted the event. Kind is closed: agent (stdio MCP
// transport), human (portal HTTP write paths), or system (CLI subcommands,
// cron jobs). Inferred at dispatch from the transport that delivered the
// call — see [WithActor]. T3 populates real values at the dispatch
// boundary; until then, the default actor returned by [ActorFromContext]
// is a system-kind sentinel so the foundation works without coupling to
// dispatch.
type Actor struct {
	Kind string // "agent" | "human" | "system"
	ID   string // model name (agent), portal session (human), cli-<subcmd> (system)
}

// EntityRef points at the primary entity an event acts on. ProjectID is a
// pointer because some event types are cross-cutting (e.g. benchmark runs
// spanning projects); use NewEntityRef for project-scoped kinds and
// NewCrossCuttingEntityRef for the rest.
type EntityRef struct {
	Kind      string  `json:"kind"`
	Slug      string  `json:"slug"`
	ProjectID *string `json:"project_id"`
}

// NewEntityRef builds a project-scoped EntityRef. Use this for bug, task,
// and chain events.
func NewEntityRef(kind, slug, projectID string) EntityRef {
	pid := projectID
	return EntityRef{Kind: kind, Slug: slug, ProjectID: &pid}
}

// NewCrossCuttingEntityRef builds an EntityRef with no project scope. Use
// this for events that span projects (regression-suite benchmark runs,
// system-level events).
func NewCrossCuttingEntityRef(kind, slug string) EntityRef {
	return EntityRef{Kind: kind, Slug: slug, ProjectID: nil}
}

// Refs are causal and cross-entity links carried in the envelope.
// CausedByEventID points at a parent event for cascade emits or at a
// reversed event for compensating emits. RelatedEntities is the
// cross-cutting reference set — e.g. BugResolved with kind=routed puts
// the routed task here while the bug stays in Entity.
type Refs struct {
	CausedByEventID *string
	RelatedEntities []EntityRef
}

// ErrInvalidInput is returned by Emit when the constructed event fails
// schema validation. Wraps the underlying validator error with a Field
// pointer so the dispatch layer can surface a structured error to the
// caller. Mirrors the CONVENTIONS.md §Error Enum Shape InvalidInput
// concept on the Go side.
type ErrInvalidInput struct {
	Field  string
	Reason string
}

func (e *ErrInvalidInput) Error() string {
	if e.Field == "" {
		return "invalid input: " + e.Reason
	}
	return "invalid input on field " + e.Field + ": " + e.Reason
}

// uuidv7State carries the previous emission's bytes so same-millisecond
// generations can guarantee monotonicity per RFC 9562 §6.2 Method 1
// (random-LSB increment). Protected by the mutex; not exported.
var (
	uuidv7Mu      sync.Mutex
	uuidv7Last    [16]byte
	uuidv7HasLast bool
)

// newUUIDv7 returns a fresh UUIDv7 — 48-bit Unix-ms timestamp followed by
// 74 bits of randomness, with version (7) and variant (RFC 4122) markers
// in the standard positions. Time-ordered: sorting events by event_id
// gives wall-clock order; this is the load-bearing property the sibling
// chain query-telemetry-substrate's FK-array semantics depend on.
//
// Intra-millisecond monotonicity is preserved per RFC 9562 §6.2 Method 1:
// when two UUIDs are generated in the same millisecond, the second is
// constructed by incrementing the previous UUID's 74 random bits (rand_a
// + rand_b). This makes "sort by event_id" produce strict insertion
// order even under high emit rate. Cross-process generations within the
// same ms are independent (each process keeps its own counter) — that's
// acceptable because cross-process emits to the same toolkit.db are
// already serialized through the WithWrite mutex on the pool.
//
// Pure-Go, no dependency.
func newUUIDv7() (string, error) {
	uuidv7Mu.Lock()
	defer uuidv7Mu.Unlock()

	var b [16]byte
	ms := uint64(time.Now().UnixMilli())

	if uuidv7HasLast {
		lastMS := binary.BigEndian.Uint64(uuidv7Last[0:8]) >> 16
		if ms <= lastMS {
			// Same or earlier millisecond — increment the previous UUID's
			// trailing 74 bits to preserve monotonicity.
			b = uuidv7Last
			incrementUUIDv7Tail(&b)
			uuidv7Last = b
			return formatUUID(b), nil
		}
	}

	// New millisecond — fresh random tail.
	binary.BigEndian.PutUint64(b[0:8], ms<<16)
	if _, err := rand.Read(b[6:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	b[6] = (b[6] & 0x0F) | 0x70 // version 7
	b[8] = (b[8] & 0x3F) | 0x80 // RFC 4122 variant
	uuidv7Last = b
	uuidv7HasLast = true
	return formatUUID(b), nil
}

// incrementUUIDv7Tail bumps the 74 random bits of a UUIDv7 by 1, leaving
// the timestamp + version + variant nibbles untouched. The 74 bits split:
//   - low 4 bits of byte 6 (the rand_a remainder under the version nibble)
//   - low 6 bits of byte 8 (the rand_b leader under the variant bits)
//   - all 8 bits of byte 7
//   - all 8 bits of bytes 9..15
//
// Walk bytes 15→7 with carry, then handle the masked bits in byte 8
// (low 6) and byte 6 (low 4). On the astronomical wrap (~74 bits of
// headroom per millisecond), re-randomize the tail rather than roll
// into the version nibble.
func incrementUUIDv7Tail(b *[16]byte) {
	carry := uint16(1)
	for i := 15; i >= 9; i-- {
		s := uint16(b[i]) + carry
		b[i] = byte(s & 0xFF)
		carry = s >> 8
		if carry == 0 {
			return
		}
	}
	low := uint16(b[8] & 0x3F)
	s := low + carry
	b[8] = byte((b[8] & 0xC0) | byte(s&0x3F))
	carry = s >> 6
	if carry == 0 {
		return
	}
	s7 := uint16(b[7]) + carry
	b[7] = byte(s7 & 0xFF)
	carry = s7 >> 8
	if carry == 0 {
		return
	}
	low6 := uint16(b[6] & 0x0F)
	s6 := low6 + carry
	b[6] = byte((b[6] & 0xF0) | byte(s6&0x0F))
	if s6>>4 != 0 {
		_, _ = rand.Read(b[7:])
		b[6] = b[6] & 0xF0
		b[8] = (b[8] & 0x3F) | 0x80
	}
}

// newUUIDv4 returns a fresh random UUIDv4. Used for span_id where time
// ordering doesn't matter and we just want a unique identifier per
// MCP request. Pure-Go, no dependency.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	b[6] = (b[6] & 0x0F) | 0x40 // version 4
	b[8] = (b[8] & 0x3F) | 0x80 // RFC 4122 variant
	return formatUUID(b), nil
}

// formatUUID renders 16 bytes as the canonical 8-4-4-4-12 hex form.
func formatUUID(b [16]byte) string {
	hexStr := hex.EncodeToString(b[:])
	return hexStr[0:8] + "-" + hexStr[8:12] + "-" + hexStr[12:16] + "-" + hexStr[16:20] + "-" + hexStr[20:]
}
