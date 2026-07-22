package construct

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/forge/fieldvalue"
	"toolkit/internal/forge/registry"
	"toolkit/internal/work"
)

// ── Bucket 4: file-schema create paths ──────────────────────────────────────
//
// Unlike the event-sourced creates, these schemas' PRIMARY artifact is a FILE
// (markdown or SQL) the harness reads; the *Forged / *Written event is fail-open
// telemetry (memory's MemoryWritten DOES also fold proj_memories — it's the
// only event-folded file schema). So each builder does TWO things: write the
// file (re-homing forge's renderer via the exported forge helpers, byte-
// identical to forge's output) and return the event for submission through
// record. See docs/EMIT_SURFACE_PHASE2.md §15 for the bucket boundary.

// MemoryInput carries forge(memory)'s fields. Memory is cross-project: project
// is DB attribution (proj_memories.project_id + the event's entity_project_id);
// Kind drives the vault subdir routing (closed enum: user|feedback|project|
// reference). RecurrenceCount rides the FILE frontmatter only (absent from
// MemoryWrittenPayload), so it affects the artifact, not the projection.
type MemoryInput struct {
	Slug            string // == frontmatter name; must match ^[a-z0-9-]+$
	Kind            string
	Description     string
	Body            string
	Source          string
	ObservedFirst   string
	RecurrenceCount string
}

var memoryKinds = map[string]bool{"user": true, "feedback": true, "project": true, "reference": true}

// buildMemory writes the memory file (WriteMemoryArtifact — byte-
// identical to forge(memory)) and returns the MemoryWritten event. Package-
// private — construct.Create dispatches here for "memory".
func buildMemory(ctx context.Context, q db.Queryer, schema registry.Schema, project string, in MemoryInput) (work.RecordEvent, string, error) {
	if strings.TrimSpace(in.Slug) == "" {
		return work.RecordEvent{}, "", fmt.Errorf("memory: name is required")
	}
	if !memoryKinds[in.Kind] {
		return work.RecordEvent{}, "", fmt.Errorf("memory: memory_kind %q invalid (one of user|feedback|project|reference)", in.Kind)
	}
	if strings.TrimSpace(in.Description) == "" {
		return work.RecordEvent{}, "", fmt.Errorf("memory: description is required")
	}
	if strings.Contains(in.Description, "\n") {
		return work.RecordEvent{}, "", fmt.Errorf("memory: description must be a single line (no newline)")
	}
	if strings.TrimSpace(in.Body) == "" {
		return work.RecordEvent{}, "", fmt.Errorf("memory: body is required")
	}
	path, routingNote, err := WriteMemoryArtifact(ctx, q, schema, project, in.Slug,
		in.Kind, in.Description, in.Body, in.Source, in.ObservedFirst, in.RecurrenceCount)
	if err != nil {
		return work.RecordEvent{}, "", fmt.Errorf("write memory file: %w", err)
	}
	payload, err := json.Marshal(events.MemoryWrittenPayload{
		Name:            in.Slug,
		Kind:            in.Kind,
		Description:     in.Description,
		Source:          optionalStr(in.Source),
		ObservedFirst:   optionalStr(in.ObservedFirst),
		VaultPath:       path,
		BodyLengthBytes: len(in.Body),
	})
	if err != nil {
		return work.RecordEvent{}, "", fmt.Errorf("marshal MemoryWritten payload: %w", err)
	}
	pid := project
	return work.RecordEvent{
		Type:            "MemoryWritten",
		EntitySlug:      in.Slug,
		EntityProjectID: &pid,
		Payload:         payload,
	}, routingNote, nil
}

// ── Memory edit arm (Stage 3 Slice 3) ─────────────────────────────────────

// MemoryEditInput is the sparse-update form of MemoryInput. Slug identifies
// the existing memory note (matching its filename + frontmatter `name:`).
// All other fields are *string pointers; nil means "leave unchanged."
// Kind is the memory_kind alias — a non-nil Kind that differs from the
// stored value RELOCATES the file (memory/<kind>/<slug>.md) per the
// re-homed seam's logic.
type MemoryEditInput struct {
	Slug            string
	Kind            *string
	Description     *string
	Body            *string
	Source          *string
	ObservedFirst   *string
	RecurrenceCount *string
}

// fieldMap returns the partial-update map sent to EditMemoryArtifact.
// The keys match memory's TOML field names: memory_kind / description /
// body / source / observed_first / recurrence_count.
func (in MemoryEditInput) fieldMap() map[string]fieldvalue.FieldValue {
	out := map[string]fieldvalue.FieldValue{}
	if in.Kind != nil {
		out["memory_kind"] = fieldvalue.SingleValue(*in.Kind)
	}
	if in.Description != nil {
		out["description"] = fieldvalue.SingleValue(*in.Description)
	}
	if in.Body != nil {
		out["body"] = fieldvalue.SingleValue(*in.Body)
	}
	if in.Source != nil {
		out["source"] = fieldvalue.SingleValue(*in.Source)
	}
	if in.ObservedFirst != nil {
		out["observed_first"] = fieldvalue.SingleValue(*in.ObservedFirst)
	}
	if in.RecurrenceCount != nil {
		out["recurrence_count"] = fieldvalue.SingleValue(*in.RecurrenceCount)
	}
	return out
}

// buildEditMemory writes the memory file via the re-homed
// EditMemoryArtifact seam (file bytes byte-identical to forge_edit's),
// then emits a MemoryWritten event so the projection (proj_memories) and the
// knowledge index stay in sync with the file.
//
// Behavioral note (B-ED3 + record-construction-layer discipline): forge_edit
// on memory writes the file but emits NO event, leaving proj_memories stale
// vs. the file (a long-standing forge quirk). construct.Update closes that
// gap by emitting MemoryWritten with the merged post-edit content — the
// fold then upserts proj_memories. File bytes match forge_edit byte-for-byte;
// the projection convergence is a strict improvement that record-construction
// layer adopters get for free.
func buildEditMemory(ctx context.Context, q db.Queryer, schema registry.Schema, project, slug string, partial map[string]fieldvalue.FieldValue) (work.RecordEvent, EditResult, error) {
	res, merged, err := EditMemoryArtifact(ctx, q, schema, project, slug, partial)
	if err != nil {
		return work.RecordEvent{}, EditResult{}, err
	}
	if res.NotFound {
		return work.RecordEvent{}, res, &NotFoundError{Schema: "memory", Slug: slug, Project: project}
	}
	kind := stringFieldOrEmpty(merged, "memory_kind")
	description := stringFieldOrEmpty(merged, "description")
	body := stringFieldOrEmpty(merged, "body")
	source := stringFieldOrEmpty(merged, "source")
	observedFirst := stringFieldOrEmpty(merged, "observed_first")
	payload, err := json.Marshal(events.MemoryWrittenPayload{
		Name:            slug,
		Kind:            kind,
		Description:     description,
		Source:          optionalStr(source),
		ObservedFirst:   optionalStr(observedFirst),
		VaultPath:       res.ArtifactPath,
		BodyLengthBytes: len(body),
	})
	if err != nil {
		return work.RecordEvent{}, EditResult{}, fmt.Errorf("marshal MemoryWritten payload: %w", err)
	}
	pid := project
	return work.RecordEvent{
		Type:            "MemoryWritten",
		EntitySlug:      slug,
		EntityProjectID: &pid,
		Payload:         payload,
	}, res, nil
}

// stringFieldOrEmpty reads a string field from the merged map (the form
// EditMemoryArtifact returns). Empty when absent.
func stringFieldOrEmpty(merged map[string]fieldvalue.FieldValue, name string) string {
	v, ok := merged[name]
	if !ok {
		return ""
	}
	return v.AsJoined()
}

// ── Chain-anchored doc edit arm (Stage 3 Slice 4) ──────────────────────────

// ChainAnchoredDocEditInput is the sparse-update form of
// ChainAnchoredDocInput for retrospective + report-card edits. Slug
// identifies the existing doc; Title and Sections are optional partial
// updates. ChainSlug is NOT on the edit input — chain identity is
// preserved from the parsed file (a chain-anchored doc can't be re-anchored
// to a different chain without re-forging it).
type ChainAnchoredDocEditInput struct {
	Slug     string
	Title    *string
	Sections map[string]*string
}

// fieldMap converts the edit input into a fieldvalue.FieldValue partial. Only
// non-nil Title and non-nil section entries become entries; the chain_slug
// frontmatter is preserved by editMarkdown's existing+partial merge.
func (in ChainAnchoredDocEditInput) fieldMap() map[string]fieldvalue.FieldValue {
	out := map[string]fieldvalue.FieldValue{}
	if in.Title != nil {
		out["title"] = fieldvalue.SingleValue(*in.Title)
	}
	for k, v := range in.Sections {
		if v != nil {
			out[k] = fieldvalue.SingleValue(*v)
		}
	}
	return out
}

// buildEditRetrospective edits a retrospective file (via
// EditMarkdownArtifact) and returns a RetrospectiveForged event +
// EditResult. The event re-uses the create-side RetrospectiveForged shape
// (chain entity, repo-relative file_path, populated section count) so the
// fold + ledger stay consistent with create-time emission.
func buildEditRetrospective(ctx context.Context, pool *db.Pool, schema registry.Schema, project, slug string, partial map[string]fieldvalue.FieldValue) (work.RecordEvent, EditResult, error) {
	return buildEditChainAnchoredDoc(ctx, pool, schema, "retrospective", "RetrospectiveForged", project, slug, partial)
}

// buildEditReportCard is the sister of buildEditRetrospective.
func buildEditReportCard(ctx context.Context, pool *db.Pool, schema registry.Schema, project, slug string, partial map[string]fieldvalue.FieldValue) (work.RecordEvent, EditResult, error) {
	return buildEditChainAnchoredDoc(ctx, pool, schema, "report-card", "ReportCardForged", project, slug, partial)
}

// buildEditChainAnchoredDoc factors the retrospective + report-card
// edit body. Both schemas share the chain-anchored doc shape (chain entity,
// file_path + section_count payload, knowledge_pointer refresh).
//
// The merged map returned by EditMarkdownArtifact carries the post-edit
// state — including the preserved chain_slug from the existing frontmatter
// — which buildEditChainAnchoredDoc then uses to resolve the chain anchor,
// compute the section count, refresh the knowledge_pointer, and stamp
// chain_id on the event payload.
func buildEditChainAnchoredDoc(ctx context.Context, pool *db.Pool, schema registry.Schema, name, eventType, project, slug string, partial map[string]fieldvalue.FieldValue) (work.RecordEvent, EditResult, error) {
	res, merged, err := EditMarkdownArtifact(ctx, pool.DB(), schema, project, slug, partial, EditOpts{})
	if err != nil {
		return work.RecordEvent{}, EditResult{}, err
	}
	if res.NotFound {
		return work.RecordEvent{}, res, &NotFoundError{Schema: name, Slug: slug, Project: project}
	}
	chainSlug := stringFieldOrEmpty(merged, "chain_slug")
	if chainSlug == "" {
		return work.RecordEvent{}, res, fmt.Errorf("construct.Update(%s): %q has no chain_slug in frontmatter (file corrupt or pre-canonical)", name, slug)
	}
	anchor, err := ChainAnchor(ctx, pool.DB(), project, chainSlug)
	if err != nil {
		return work.RecordEvent{}, res, err
	}
	root := ResolveMarkdownRoot(ctx, pool.DB(), project, schema)
	repoRel := RepoRelativePath(res.ArtifactPath, root, schema)
	sectionCount := countPopulatedSections(merged, name)

	// B-F3: refresh knowledge_pointer with the post-edit content. Done HERE
	// (inside the buildX function) rather than via the umbrella's
	// IndexSyncFromProjection — chain-anchored docs are file-based, and
	// IndexSyncFromProjection.ReadCanonicalFields returns ok=false when
	// editedPath is empty.
	if err := UpsertChainAnchoredDocPointer(ctx, pool, project, slug, name, merged); err != nil {
		return work.RecordEvent{}, res, fmt.Errorf("construct.Update(%s): pointer upsert: %w", name, err)
	}

	chainID := anchor.ChainID
	payload, err := json.Marshal(struct {
		ChainSlug    string `json:"chain_slug"`
		ChainID      *int64 `json:"chain_id,omitempty"`
		FilePath     string `json:"file_path"`
		SectionCount int    `json:"section_count"`
	}{ChainSlug: chainSlug, ChainID: &chainID, FilePath: repoRel, SectionCount: sectionCount})
	if err != nil {
		return work.RecordEvent{}, res, fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	pid := project
	return work.RecordEvent{
		Type:            eventType,
		EntityKind:      "chain",
		EntitySlug:      chainSlug,
		EntityProjectID: &pid,
		Payload:         payload,
	}, res, nil
}

// countPopulatedSections counts the schema's skeleton sections that have a
// non-empty value in merged. Matches createChainAnchoredDoc /
// WriteChainAnchoredDoc's section_count computation exactly.
func countPopulatedSections(merged map[string]fieldvalue.FieldValue, schemaName string) int {
	var skeleton []string
	switch schemaName {
	case "retrospective":
		skeleton = RetrospectiveSkeletonFields()
	case "report-card":
		skeleton = ReportCardSkeletonFields()
	default:
		return 0
	}
	n := 0
	for _, fn := range skeleton {
		if v, ok := merged[fn]; ok && !v.IsEmpty() {
			n++
		}
	}
	return n
}

// ChainAnchoredDocInput carries forge(retrospective) / forge(report-card)
// inputs. Sections maps a schema section field name (what_landed, surprises,
// per_task_grades, …) → verbatim markdown; omitted sections are skipped.
type ChainAnchoredDocInput struct {
	Slug      string // doc slug (frontmatter slug:)
	ChainSlug string // the chain this doc anchors to (must exist)
	Title     string // optional; defaults per schema
	Sections  map[string]string
}

// buildRetrospective writes the retrospective file + upserts its
// knowledge_pointer (WriteChainAnchoredDoc) and returns the
// RetrospectiveForged event (entity = the anchored CHAIN — entity_kind is set
// explicitly since record can't infer "chain" from the RetrospectiveForged
// type). Rejects chain_not_found. Package-private — construct.Create
// dispatches here for "retrospective".
//
// Documented delta vs forge(retrospective): forge runs
// captureOrphanedFollowons (auto-files suggestions for uncaptured next-chain
// candidates) as a fail-open post-write side effect; the construct layer does
// NOT replicate it. §15 records this as an intentional Stage-2 boundary.
func buildRetrospective(ctx context.Context, pool *db.Pool, schema registry.Schema, project string, in ChainAnchoredDocInput) (work.RecordEvent, string, error) {
	return buildChainAnchoredDoc(ctx, pool, schema, "retrospective", "RetrospectiveForged", project, in)
}

// buildReportCard is the report-card arm (sister of buildRetrospective).
func buildReportCard(ctx context.Context, pool *db.Pool, schema registry.Schema, project string, in ChainAnchoredDocInput) (work.RecordEvent, string, error) {
	return buildChainAnchoredDoc(ctx, pool, schema, "report-card", "ReportCardForged", project, in)
}

func buildChainAnchoredDoc(ctx context.Context, pool *db.Pool, schema registry.Schema, name, eventType, project string, in ChainAnchoredDocInput) (work.RecordEvent, string, error) {
	res, err := WriteChainAnchoredDoc(ctx, pool, schema, name, project, in.Slug, in.ChainSlug, in.Title, in.Sections)
	if err != nil {
		return work.RecordEvent{}, "", err
	}
	chainID := res.ChainID
	// RetrospectiveForged / ReportCardForged share the same payload shape.
	payload, err := json.Marshal(struct {
		ChainSlug    string `json:"chain_slug"`
		ChainID      *int64 `json:"chain_id,omitempty"`
		FilePath     string `json:"file_path"`
		SectionCount int    `json:"section_count"`
	}{ChainSlug: in.ChainSlug, ChainID: &chainID, FilePath: res.FilePath, SectionCount: res.SectionCount})
	if err != nil {
		return work.RecordEvent{}, "", fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	pid := project
	return work.RecordEvent{
		Type:            eventType,
		EntityKind:      "chain", // explicit: record can't infer chain from the *Forged type
		EntitySlug:      in.ChainSlug,
		EntityProjectID: &pid,
		Payload:         payload,
	}, res.RoutingNote, nil
}

// MigrationInput carries forge(migration)'s fields. The .sql file (canonical +
// testutil mirror) is the artifact; MigrationForged is fail-open provenance
// (no projection folds it; no knowledge index). UpSQL is EXPLAIN-checked at
// build time — unparseable SQL rejects before any file lands.
type MigrationInput struct {
	Slug      string
	UpSQL     string
	Docstring string
}

// buildMigration writes a migration's canonical + testutil-mirror .sql files
// (byte-identical to forge(migration), via WriteMigrationArtifact) and
// returns the MigrationForged event. An existing slug is idempotent (returns
// the existing number, writes nothing) — the event then carries
// idempotent=true, matching  Package-private — construct.Create
// dispatches here for "migration".
func buildMigration(ctx context.Context, pool *db.Pool, schema registry.Schema, project string, in MigrationInput) (work.RecordEvent, string, error) {
	if strings.TrimSpace(in.Slug) == "" {
		return work.RecordEvent{}, "", fmt.Errorf("migration: slug is required")
	}
	res, err := WriteMigrationArtifact(ctx, pool, schema, project, in.Slug, in.UpSQL, in.Docstring)
	if err != nil {
		return work.RecordEvent{}, "", err
	}
	payload, err := json.Marshal(events.MigrationForgedPayload{
		MigrationNumber: res.MigrationNumber,
		Slug:            in.Slug,
		FilePaths:       res.FilePaths,
		DocstringLength: res.DocstringLength,
		SQLLength:       res.SQLLength,
		Idempotent:      res.Idempotent,
	})
	if err != nil {
		return work.RecordEvent{}, "", fmt.Errorf("marshal MigrationForged payload: %w", err)
	}
	pid := project
	return work.RecordEvent{
		Type:            "MigrationForged",
		EntitySlug:      in.Slug,
		EntityProjectID: &pid,
		Payload:         payload,
	}, res.RoutingNote, nil
}
