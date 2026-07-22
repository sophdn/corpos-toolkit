package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"toolkit/internal/db"
	"toolkit/internal/events"
)

// RoadmapEntry is one row of the live roadmap.
type RoadmapEntry struct {
	Position  int64  `json:"position"`
	RefKind   string `json:"ref_kind"`
	RefSlug   string `json:"ref_slug"`
	ChainSlug string `json:"chain_slug,omitempty"`
	Note      string `json:"note,omitempty"`
	Status    string `json:"status,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// RoadmapSetInput is one caller-supplied item for roadmap_set /
// preview_set / insert. When Position is nil, the row's effective
// position is its 1-based index in the items array (the historical
// behavior). When Position is set, the explicit value is honored —
// allowing gaps between items (e.g. positions 1, 5, 10) so callers
// can express cross-project priority bands. Duplicate positions
// within one items array are rejected.
type RoadmapSetInput struct {
	RefKind  string `json:"ref_kind"`
	RefSlug  string `json:"ref_slug"`
	Note     string `json:"note,omitempty"`
	Position *int64 `json:"position,omitempty"`
}

// effectivePositions returns the per-item position used by Set /
// preview_set, honoring explicit Position values and falling back to
// 1-based array index for nil entries. Returns a non-nil error if any
// two items collide on the same position.
func effectivePositions(items []RoadmapSetInput) ([]int64, error) {
	out := make([]int64, len(items))
	seen := make(map[int64]int, len(items))
	for i, it := range items {
		var pos int64
		if it.Position != nil {
			pos = *it.Position
		} else {
			pos = int64(i + 1)
		}
		if prev, dup := seen[pos]; dup {
			return nil, fmt.Errorf("duplicate position %d on items[%d] (%s '%s') and items[%d] (%s '%s')",
				pos, prev, items[prev].RefKind, items[prev].RefSlug,
				i, it.RefKind, it.RefSlug)
		}
		seen[pos] = i
		out[i] = pos
	}
	return out, nil
}

// RoadmapListEntry is the in-memory shape for roadmap_list rows;
// renamed so the result type stays the canonical wire shape.
type RoadmapListEntry = RoadmapEntry

// HandleRoadmapList implements work.roadmap_list. Cross-project by
// default (matches Rust list()); pass project to scope.
func HandleRoadmapList(ctx context.Context, pool *db.Pool, project string, _ json.RawMessage) (RoadmapListEntries, error) {
	const listSQL = `SELECT r.position, r.ref_kind, r.ref_slug, r.chain_slug, r.note,
		CASE WHEN r.ref_kind = 'chain' THEN c.status
		     WHEN r.ref_kind = 'task'  THEN t.status END AS status,
		CASE WHEN r.ref_kind = 'chain' THEN c.updated_at
		     WHEN r.ref_kind = 'task'  THEN t.updated_at END AS updated_at
		FROM proj_roadmap_view r
		LEFT JOIN proj_chain_status c ON r.ref_kind = 'chain' AND c.slug = r.ref_slug
		LEFT JOIN proj_current_tasks  t ON r.ref_kind = 'task'  AND t.slug = r.ref_slug`

	var rows *sql.Rows
	var err error
	if project != "" {
		rows, err = pool.DB().QueryContext(ctx, listSQL+` WHERE r.project_id = ? ORDER BY r.position ASC`, project)
	} else {
		rows, err = pool.DB().QueryContext(ctx, listSQL+` ORDER BY r.position ASC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := RoadmapListEntries{}
	for rows.Next() {
		var e RoadmapEntry
		var chain, note, status, updated sql.NullString
		if err := rows.Scan(&e.Position, &e.RefKind, &e.RefSlug, &chain, &note, &status, &updated); err != nil {
			return nil, err
		}
		e.ChainSlug = chain.String
		e.Note = note.String
		e.Status = status.String
		e.UpdatedAt = updated.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// RoadmapListEntries is a typed slice that marshals as `[]` not `null`
// when empty (json.Marshal on a nil-typed-slice still emits `null`;
// the wrapper makes the typed return non-nil).
type RoadmapListEntries []RoadmapEntry

// MarshalJSON ensures empty slices marshal as `[]` (callers distinguish
// "no items" from "error" by shape).
func (r RoadmapListEntries) MarshalJSON() ([]byte, error) {
	if r == nil {
		return []byte("[]"), nil
	}
	// Cast to a plain slice via the underlying type so json.Marshal
	// uses the default array encoder and skips this MarshalJSON.
	return json.Marshal([]RoadmapEntry(r))
}

// resolveRef validates one ref against chains / tasks and returns the
// pair (project_id, chain_slug_for_task). Mirrors Rust resolve_ref.
func resolveRef(ctx context.Context, pool *db.Pool, item RoadmapSetInput) (string, string, error) {
	switch item.RefKind {
	case "chain":
		var status, projectID string
		err := pool.DB().QueryRowContext(ctx,
			`SELECT status, project_id FROM proj_chain_status WHERE slug = ?`, item.RefSlug).
			Scan(&status, &projectID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", "", fmt.Errorf("chain '%s' not found", item.RefSlug)
			}
			return "", "", err
		}
		if status != "open" {
			return "", "", fmt.Errorf("chain '%s' is in status '%s'; only open chains can be roadmapped", item.RefSlug, status)
		}
		return projectID, "", nil
	case "task":
		var status, projectID, chainSlug string
		err := pool.DB().QueryRowContext(ctx,
			`SELECT t.status, c.project_id, c.slug
			 FROM proj_current_tasks t JOIN proj_chain_status c ON t.chain_id = c.id
			 WHERE t.slug = ?`, item.RefSlug).
			Scan(&status, &projectID, &chainSlug)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", "", fmt.Errorf("task '%s' not found", item.RefSlug)
			}
			return "", "", err
		}
		if status != "pending" && status != "active" {
			return "", "", fmt.Errorf("task '%s' is in status '%s'; only pending/active tasks can be roadmapped", item.RefSlug, status)
		}
		return projectID, chainSlug, nil
	}
	return "", "", fmt.Errorf("ref_kind must be 'chain' or 'task', got '%s'", item.RefKind)
}

// resolveProjectScope mirrors Rust roadmap::resolve_project_scope_for_items.
func resolveProjectScope(hint string, resolvedProjects []string) (string, error) {
	if len(resolvedProjects) > 0 {
		scope := resolvedProjects[0]
		for i := 1; i < len(resolvedProjects); i++ {
			if resolvedProjects[i] != scope {
				return "", fmt.Errorf("mixed projects in roadmap items: '%s' and '%s' — roadmap_set / preview_set scope to one project per call", scope, resolvedProjects[i])
			}
		}
		if hint != "" && hint != scope {
			return "", fmt.Errorf("project hint '%s' doesn't match items' resolved project '%s'", hint, scope)
		}
		return scope, nil
	}
	if hint == "" {
		return "", fmt.Errorf("roadmap_set / preview_set with no items requires an explicit `project` to scope the operation; otherwise the call has no way to know what to clear")
	}
	return hint, nil
}

// roadmapSetParams + HandleRoadmapSet replaces the project's roadmap
// with the supplied items atomically.
type roadmapSetParams struct {
	Items []RoadmapSetInput `json:"items"`
}

// RoadmapSetResult is the response shape for roadmap_set.
type RoadmapSetResult struct {
	OK      bool   `json:"ok,omitempty"`
	Count   int    `json:"count,omitempty"`
	Project string `json:"project,omitempty"`
	Error   string `json:"error,omitempty"`
}

func HandleRoadmapSet(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (RoadmapSetResult, error) {
	var p roadmapSetParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return RoadmapSetResult{Error: err.Error()}, nil
		}
	}
	resolved := make([][2]string, 0, len(p.Items))
	projects := make([]string, 0, len(p.Items))
	for _, it := range p.Items {
		proj, chain, err := resolveRef(ctx, pool, it)
		if err != nil {
			return RoadmapSetResult{Error: err.Error()}, nil
		}
		resolved = append(resolved, [2]string{proj, chain})
		projects = append(projects, proj)
	}
	scope, err := resolveProjectScope(project, projects)
	if err != nil {
		return RoadmapSetResult{Error: err.Error()}, nil
	}
	positions, err := effectivePositions(p.Items)
	if err != nil {
		return RoadmapSetResult{Error: err.Error()}, nil
	}

	// Build payload Items so the projection fold can reconstruct
	// proj_roadmap_view from the event alone (T5-roadmap).
	itemsPayload := make([]events.RoadmapItemPayload, 0, len(p.Items))
	for i, item := range p.Items {
		_, chain := resolved[i][0], resolved[i][1]
		var chainPtr *string
		if chain != "" {
			chainPtr = &chain
		}
		var notePtr *string
		if item.Note != "" {
			n := item.Note
			notePtr = &n
		}
		itemsPayload = append(itemsPayload, events.RoadmapItemPayload{
			Position:  positions[i],
			RefKind:   item.RefKind,
			RefSlug:   item.RefSlug,
			ChainSlug: chainPtr,
			Note:      notePtr,
		})
	}
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		// T5-roadmap: CRUD writes dropped (DELETE + INSERT for the bulk set).
		// The projection fold for RoadmapUpdated.set DELETEs proj_roadmap_view
		// rows for the project and INSERTs the items from the payload.
		itemCount := len(p.Items)
		positionsCopy := append([]int64{}, positions...)
		payload := events.RoadmapUpdatedPayload{
			ActionKind: "set",
			Positions:  positionsCopy,
			ItemCount:  &itemCount,
			Items:      itemsPayload,
		}
		_, emitErr := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewEntityRef("roadmap", "main", scope),
			Payload: payload,
		})
		return emitErr
	})
	if err != nil {
		return RoadmapSetResult{}, err
	}
	return RoadmapSetResult{OK: true, Count: len(p.Items), Project: scope}, nil
}

// RoadmapPreviewRef and RoadmapPreviewMove mirror the Rust structs.
type RoadmapPreviewRef struct {
	RefKind  string `json:"ref_kind"`
	RefSlug  string `json:"ref_slug"`
	Position int64  `json:"position"`
}

type RoadmapPreviewMove struct {
	RefKind string `json:"ref_kind"`
	RefSlug string `json:"ref_slug"`
	Before  int64  `json:"before"`
	After   int64  `json:"after"`
}

type RoadmapPreview struct {
	Removed      []RoadmapPreviewRef  `json:"removed"`
	Added        []RoadmapPreviewRef  `json:"added"`
	Repositioned []RoadmapPreviewMove `json:"repositioned"`
	Unchanged    []RoadmapPreviewRef  `json:"unchanged"`
}

// RoadmapPreviewResult emits either the preview struct or an error envelope.
type RoadmapPreviewResult struct {
	Preview *RoadmapPreview
	Error   string
}

// MarshalJSON unwraps the populated branch.
func (r RoadmapPreviewResult) MarshalJSON() ([]byte, error) {
	if r.Error != "" {
		return json.Marshal(struct {
			Error string `json:"error"`
		}{r.Error})
	}
	return json.Marshal(r.Preview)
}

// HandleRoadmapPreviewSet dry-runs Set against the same validation, no
// DB writes. Returns added / removed / repositioned / unchanged entries.
func HandleRoadmapPreviewSet(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (RoadmapPreviewResult, error) {
	var p roadmapSetParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return RoadmapPreviewResult{Error: err.Error()}, nil
		}
	}
	projects := make([]string, 0, len(p.Items))
	for _, it := range p.Items {
		proj, _, err := resolveRef(ctx, pool, it)
		if err != nil {
			return RoadmapPreviewResult{Error: err.Error()}, nil
		}
		projects = append(projects, proj)
	}
	scope, err := resolveProjectScope(project, projects)
	if err != nil {
		return RoadmapPreviewResult{Error: err.Error()}, nil
	}
	positions, err := effectivePositions(p.Items)
	if err != nil {
		return RoadmapPreviewResult{Error: err.Error()}, nil
	}

	// Snapshot current within scope.
	currentRows, err := pool.DB().QueryContext(ctx,
		`SELECT position, ref_kind, ref_slug FROM proj_roadmap_view WHERE project_id = ? ORDER BY position ASC`, scope)
	if err != nil {
		return RoadmapPreviewResult{}, err
	}
	defer currentRows.Close()
	type key struct{ k, s string }
	currentMap := map[key]int64{}
	type curRow struct {
		pos  int64
		kind string
		slug string
	}
	var cur []curRow
	for currentRows.Next() {
		var r curRow
		if err := currentRows.Scan(&r.pos, &r.kind, &r.slug); err != nil {
			return RoadmapPreviewResult{}, err
		}
		cur = append(cur, r)
		currentMap[key{r.kind, r.slug}] = r.pos
	}

	proposedMap := map[key]int64{}
	for i, it := range p.Items {
		proposedMap[key{it.RefKind, it.RefSlug}] = positions[i]
	}

	preview := &RoadmapPreview{
		Removed:      []RoadmapPreviewRef{},
		Added:        []RoadmapPreviewRef{},
		Repositioned: []RoadmapPreviewMove{},
		Unchanged:    []RoadmapPreviewRef{},
	}
	for _, c := range cur {
		if _, ok := proposedMap[key{c.kind, c.slug}]; !ok {
			preview.Removed = append(preview.Removed, RoadmapPreviewRef{c.kind, c.slug, c.pos})
		}
	}
	for i, it := range p.Items {
		k := key{it.RefKind, it.RefSlug}
		after := positions[i]
		if before, ok := currentMap[k]; !ok {
			preview.Added = append(preview.Added, RoadmapPreviewRef{it.RefKind, it.RefSlug, after})
		} else if before != after {
			preview.Repositioned = append(preview.Repositioned, RoadmapPreviewMove{it.RefKind, it.RefSlug, before, after})
		} else {
			preview.Unchanged = append(preview.Unchanged, RoadmapPreviewRef{it.RefKind, it.RefSlug, after})
		}
	}
	return RoadmapPreviewResult{Preview: preview}, nil
}

// roadmapInsertParams captures a single roadmap insertion at the
// supplied position (project-scoped).
type roadmapInsertParams struct {
	RefKind  string `json:"ref_kind"`
	RefSlug  string `json:"ref_slug"`
	Note     string `json:"note"`
	Position *int64 `json:"position"`
}

// RoadmapInsertResult is the response shape for roadmap_insert.
type RoadmapInsertResult struct {
	OK       bool   `json:"ok,omitempty"`
	ID       int64  `json:"id,omitempty"`
	Position int64  `json:"position,omitempty"`
	RefKind  string `json:"ref_kind,omitempty"`
	RefSlug  string `json:"ref_slug,omitempty"`
	Error    string `json:"error,omitempty"`
}

// HandleRoadmapInsert inserts one entry at the supplied position
// (project-scoped). Shifts existing entries with position >= target
// down by one within the same project.
func HandleRoadmapInsert(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (RoadmapInsertResult, error) {
	var p roadmapInsertParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return RoadmapInsertResult{Error: err.Error()}, nil
		}
	}
	if p.RefKind == "" || p.RefSlug == "" {
		return RoadmapInsertResult{Error: "roadmap_insert requires ref_kind and ref_slug"}, nil
	}
	item := RoadmapSetInput{RefKind: p.RefKind, RefSlug: p.RefSlug, Note: p.Note}
	projID, chainSlug, err := resolveRef(ctx, pool, item)
	if err != nil {
		return RoadmapInsertResult{Error: err.Error()}, nil
	}
	if project != "" && project != projID {
		return RoadmapInsertResult{
			Error: fmt.Sprintf("project hint '%s' doesn't match item's resolved project '%s'", project, projID),
		}, nil
	}

	var rowID, actualPos int64
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		var existing sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM proj_roadmap_view WHERE project_id = ? AND ref_kind = ? AND ref_slug = ?`,
			projID, item.RefKind, item.RefSlug).Scan(&existing); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if existing.Valid {
			return fmt.Errorf("%s '%s' is already on the roadmap; to reposition, call roadmap_set with all current items and the explicit `position` field on the entry to move (positions are per-project and may have gaps; see RoadmapSetInput.Position)", item.RefKind, item.RefSlug)
		}
		var maxPos sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(position) FROM proj_roadmap_view WHERE project_id = ?`, projID).Scan(&maxPos); err != nil {
			return err
		}
		currentMax := maxPos.Int64
		target := currentMax + 1
		if p.Position != nil {
			switch {
			case *p.Position < 1:
				target = 1
			case *p.Position > currentMax:
				target = currentMax + 1
			default:
				target = *p.Position
			}
		}
		// T5-roadmap: position-shift + INSERT roadmap_items drops; fold
		// for RoadmapUpdated.insert handles the position shift on
		// proj_roadmap_view + inserts the new row.
		var chainPtr *string
		if chainSlug != "" {
			chainPtr = &chainSlug
		}
		var notePtr *string
		if item.Note != "" {
			n := item.Note
			notePtr = &n
		}
		actualPos = target
		// rowID is no longer load-bearing post-flip (the CRUD INSERT's
		// RETURNING id is gone). Callers that need a stable identifier
		// reference the projection by (project_id, position) instead.
		rowID = 0
		refKind := item.RefKind
		refSlug := item.RefSlug
		payload := events.RoadmapUpdatedPayload{
			ActionKind: "insert",
			Positions:  []int64{target},
			RefKind:    &refKind,
			RefSlug:    &refSlug,
			Items: []events.RoadmapItemPayload{{
				Position:  target,
				RefKind:   item.RefKind,
				RefSlug:   item.RefSlug,
				ChainSlug: chainPtr,
				Note:      notePtr,
			}},
		}
		_, emitErr := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewEntityRef("roadmap", "main", projID),
			Payload: payload,
		})
		return emitErr
	})
	if err != nil {
		return RoadmapInsertResult{Error: err.Error()}, nil
	}
	return RoadmapInsertResult{
		OK:       true,
		ID:       rowID,
		Position: actualPos,
		RefKind:  item.RefKind,
		RefSlug:  item.RefSlug,
	}, nil
}

// roadmapUpdateParams is the PATCH-shaped update for one roadmap row.
// Identified by `position` (per-project unique). Any field that is
// nil-pointer is LEFT UNCHANGED on the existing row — fixes bug
// `roadmap-set-clobbers-notes-on-partial-item-submission` where
// callers had to repopulate every entry's note to avoid silent
// data loss. Pointers (not bare strings) let the caller distinguish
// "set to empty" (explicit "") from "leave alone" (absent / nil).
type roadmapUpdateParams struct {
	Position int64   `json:"position"`
	Note     *string `json:"note,omitempty"`
	RefKind  *string `json:"ref_kind,omitempty"`
	RefSlug  *string `json:"ref_slug,omitempty"`
}

// RoadmapUpdateResult is the response shape for roadmap_update.
type RoadmapUpdateResult struct {
	OK       bool   `json:"ok,omitempty"`
	Position int64  `json:"position,omitempty"`
	RefKind  string `json:"ref_kind,omitempty"`
	RefSlug  string `json:"ref_slug,omitempty"`
	Project  string `json:"project,omitempty"`
	Error    string `json:"error,omitempty"`
}

// HandleRoadmapUpdate patches a single roadmap entry at the given
// position. Unlike roadmap_set (full-list replacement), this preserves
// every field the caller doesn't explicitly supply. Project scope is
// required (top-level) to disambiguate per-project positions.
func HandleRoadmapUpdate(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (RoadmapUpdateResult, error) {
	var p roadmapUpdateParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return RoadmapUpdateResult{Error: err.Error()}, nil
		}
	}
	if project == "" {
		return RoadmapUpdateResult{Error: "roadmap_update requires top-level `project`"}, nil
	}
	if p.Position <= 0 {
		return RoadmapUpdateResult{Error: "roadmap_update requires `position` (positive integer; identifies the row to update)"}, nil
	}
	if p.Note == nil && p.RefKind == nil && p.RefSlug == nil {
		return RoadmapUpdateResult{Error: "roadmap_update requires at least one of `note`, `ref_kind`, or `ref_slug` to update"}, nil
	}
	// Either both ref_kind+ref_slug change together or neither — the
	// pair points at a chain/task and changing only one is a footgun.
	if (p.RefKind != nil) != (p.RefSlug != nil) {
		return RoadmapUpdateResult{Error: "roadmap_update requires `ref_kind` and `ref_slug` together (or neither); a row's identity is the (kind, slug) pair"}, nil
	}

	// Validate the row exists before any UPDATE.
	var existingRefKind, existingRefSlug string
	if err := pool.DB().QueryRowContext(ctx,
		`SELECT ref_kind, ref_slug FROM proj_roadmap_view WHERE project_id = ? AND position = ?`,
		project, p.Position).Scan(&existingRefKind, &existingRefSlug); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RoadmapUpdateResult{Error: fmt.Sprintf("no roadmap entry at project='%s' position=%d", project, p.Position)}, nil
		}
		return RoadmapUpdateResult{}, err
	}

	// If a ref change was requested, validate the new ref via the
	// shared resolver so we don't write a dangling pointer.
	var newChainSlug string
	if p.RefKind != nil {
		item := RoadmapSetInput{RefKind: *p.RefKind, RefSlug: *p.RefSlug}
		resolvedProj, chain, err := resolveRef(ctx, pool, item)
		if err != nil {
			return RoadmapUpdateResult{Error: err.Error()}, nil
		}
		if resolvedProj != project {
			return RoadmapUpdateResult{
				Error: fmt.Sprintf("new ref %s '%s' resolves to project '%s' but row is in project '%s'", *p.RefKind, *p.RefSlug, resolvedProj, project),
			}, nil
		}
		newChainSlug = chain
	}

	// T5-roadmap: the SQL UPDATE build is dropped; the fold for
	// RoadmapUpdated.update applies field-level patches to
	// proj_roadmap_view from the event payload's Items entry.
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		// T5-roadmap: CRUD UPDATE dropped; fold applies the field
		// changes to proj_roadmap_view from the event payload.
		var refKindOut, refSlugOut string
		if p.RefKind != nil {
			refKindOut = *p.RefKind
			refSlugOut = *p.RefSlug
		} else {
			refKindOut = existingRefKind
			refSlugOut = existingRefSlug
		}
		// Build the Items entry for the final row state. Note/chain_slug
		// only set when the caller supplied them; nil → fold leaves the
		// existing column alone (PATCH semantics).
		updItem := events.RoadmapItemPayload{
			Position: p.Position,
			RefKind:  refKindOut,
			RefSlug:  refSlugOut,
		}
		if p.Note != nil {
			n := *p.Note
			updItem.Note = &n
		}
		if p.RefKind != nil {
			// ref change implies chain_slug recalculation (see newChainSlug above).
			c := newChainSlug
			updItem.ChainSlug = &c
		}
		payload := events.RoadmapUpdatedPayload{
			ActionKind: "update",
			Positions:  []int64{p.Position},
			RefKind:    &refKindOut,
			RefSlug:    &refSlugOut,
			Items:      []events.RoadmapItemPayload{updItem},
		}
		_, emitErr := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewEntityRef("roadmap", "main", project),
			Payload: payload,
		})
		return emitErr
	})
	if err != nil {
		return RoadmapUpdateResult{}, err
	}

	resultKind := existingRefKind
	resultSlug := existingRefSlug
	if p.RefKind != nil {
		resultKind = *p.RefKind
		resultSlug = *p.RefSlug
	}
	return RoadmapUpdateResult{
		OK:       true,
		Position: p.Position,
		RefKind:  resultKind,
		RefSlug:  resultSlug,
		Project:  project,
	}, nil
}

// RoadmapDiff captures pending/active rows that arrived after the last
// reassessed_at and aren't on the roadmap.
type RoadmapDiff struct {
	Chains []UnplacedRef `json:"chains"`
	Tasks  []UnplacedRef `json:"tasks"`
}

type UnplacedRef struct {
	Slug      string `json:"slug"`
	ProjectID string `json:"project_id"`
	CreatedAt string `json:"created_at"`
	ChainSlug string `json:"chain_slug,omitempty"`
}

func HandleRoadmapDiff(ctx context.Context, pool *db.Pool, _ /*project*/ string, _ json.RawMessage) (RoadmapDiff, error) {
	var lastReassessed string
	if err := pool.DB().QueryRowContext(ctx,
		`SELECT value FROM roadmap_meta WHERE key = 'last_reassessed_at'`).Scan(&lastReassessed); err != nil {
		return RoadmapDiff{}, fmt.Errorf("read last_reassessed_at: %w", err)
	}

	chainRows, err := pool.DB().QueryContext(ctx,
		`SELECT slug, project_id, created_at FROM proj_chain_status
		 WHERE status = 'open' AND created_at > ?
		   AND slug NOT IN (SELECT ref_slug FROM proj_roadmap_view WHERE ref_kind = 'chain')
		 ORDER BY created_at ASC`, lastReassessed)
	if err != nil {
		return RoadmapDiff{}, err
	}
	defer chainRows.Close()
	var chains []UnplacedRef
	for chainRows.Next() {
		var u UnplacedRef
		if err := chainRows.Scan(&u.Slug, &u.ProjectID, &u.CreatedAt); err != nil {
			return RoadmapDiff{}, err
		}
		chains = append(chains, u)
	}

	taskRows, err := pool.DB().QueryContext(ctx,
		`SELECT t.slug, c.project_id, t.created_at, c.slug AS chain_slug
		 FROM proj_current_tasks t JOIN proj_chain_status c ON t.chain_id = c.id
		 WHERE t.status IN ('pending', 'active') AND t.created_at > ?
		   AND t.slug NOT IN (SELECT ref_slug FROM proj_roadmap_view WHERE ref_kind = 'task')
		 ORDER BY t.created_at ASC`, lastReassessed)
	if err != nil {
		return RoadmapDiff{}, err
	}
	defer taskRows.Close()
	var tasks []UnplacedRef
	for taskRows.Next() {
		var u UnplacedRef
		if err := taskRows.Scan(&u.Slug, &u.ProjectID, &u.CreatedAt, &u.ChainSlug); err != nil {
			return RoadmapDiff{}, err
		}
		tasks = append(tasks, u)
	}
	return RoadmapDiff{Chains: chains, Tasks: tasks}, nil
}

// RoadmapMarkReassessedResult is the response shape for
// roadmap_mark_reassessed.
type RoadmapMarkReassessedResult struct {
	OK bool `json:"ok"`
}

func HandleRoadmapMarkReassessed(ctx context.Context, pool *db.Pool, _ /*project*/ string, _ json.RawMessage) (RoadmapMarkReassessedResult, error) {
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE roadmap_meta SET value = datetime('now'), updated_at = datetime('now')
			 WHERE key = 'last_reassessed_at'`); err != nil {
			return err
		}
		// chain arc-close-filing-review-substrate-listener-wiring T7:
		// emit RoadmapUpdated. roadmap_mark_reassessed is project-
		// agnostic (per the handler signature dropping project) so the
		// event lands as cross-cutting (no project_id on the envelope).
		// The substrate listener treats cross-cutting events as a no-op
		// for review firing — that's the right behavior: there's no
		// project-scoped session to review.
		payload := events.RoadmapUpdatedPayload{
			ActionKind: "mark_reassessed",
		}
		_, emitErr := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewCrossCuttingEntityRef("roadmap", "main"),
			Payload: payload,
		})
		return emitErr
	})
	if err != nil {
		return RoadmapMarkReassessedResult{}, err
	}
	return RoadmapMarkReassessedResult{OK: true}, nil
}

// ── Action-doc descriptors (parallel-run registry, action_doc.go) ───────

// roadmap_list has no typed param struct (ParamStruct == nil) — its `project`
// param is a top-level envelope key, so its Type is AUTHORED here, not derived.
var roadmapListDoc = ActionDoc{
	Purpose: "List the roadmap (cross-project by default; pass `project` to scope). Returns entries sorted by position, plus their open/closed status from the underlying chain/task.",
	Params: []DocParam{
		{Name: "project", Required: false, Description: "Project id to scope to (top-level param, not in `params`).", Type: "string"},
	},
	Example: `{}`,
	Notes: "Default scope is CROSS-PROJECT. Pass a top-level `project` to scope to one project's rows.\n\n" +
		"Returned `status` reflects the underlying chain/task's status at query time (read via proj_chain_status / proj_current_tasks). In practice every returned row has status='open' because closed chains are pruned from proj_roadmap_view by the ChainClosed projection fold (internal/projections/roadmap.go), and cancelled/completed tasks are pruned by the TaskCancelled / TaskCompleted folds. There is no roadmap-side status filter because there is nothing to filter against — the live roadmap holds only open work.\n\n" +
		"The result is every proj_roadmap_view row for the requested scope; the action does NOT silently truncate. If the caller expects a row and doesn't see it, the row is genuinely absent from proj_roadmap_view (never inserted, or removed when its underlying chain/task closed) — not hidden by a default filter.\n\n" +
		"Empty result marshals as `[]` not `null` (RoadmapListEntries.MarshalJSON).",
}

var roadmapSetDoc = ActionDoc{
	Purpose: "Replace a project's roadmap atomically with the supplied items. Each item carries an optional explicit Position; omitted Position defaults to the 1-based array index. Duplicate positions within one call are rejected.",
	Params: []DocParam{
		{Name: "items", Required: true, Description: "List of {ref_kind: 'chain'|'task', ref_slug: string, note?: string, position?: int64} entries."},
	},
	Example: `{"items":[{"ref_kind":"chain","ref_slug":"agent-first-substrate","position":1,"note":"Phase A"},{"ref_kind":"chain","ref_slug":"query-telemetry-substrate","position":2,"note":"Phase B"}]}`,
	Errors: []ActionError{
		{Condition: "partial item submission", Message: "Item fields omitted in a roadmap_set call are CLEARED (notes set to empty, etc). This is intentional full-replace semantics; if you want partial-update, use roadmap_update."},
	},
	Notes:                "Fixes the silent-clobber footgun documented in bug roadmap-set-clobbers-notes-on-partial-item-submission.",
	EnvelopeRequirements: rationaleEnv(),
}

var roadmapPreviewSetDoc = ActionDoc{
	Purpose: "Dry-run roadmap_set: returns added / removed / repositioned / unchanged without applying. Same input shape as roadmap_set.",
	Params: []DocParam{
		{Name: "items", Required: true, Description: "Same shape as roadmap_set.items."},
	},
	SeeAlso: "roadmap_set",
}

var roadmapInsertDoc = ActionDoc{
	Purpose: "Insert one entry at the supplied position (project-scoped). Shifts existing entries with position >= target down by one within the same project. Returns an error if the ref is already on the roadmap (use roadmap_set with an explicit position to reposition).",
	Params: []DocParam{
		{Name: "ref_kind", Required: true, Description: "chain or task."},
		{Name: "ref_slug", Required: true, Description: "Slug of the chain or task."},
		{Name: "position", Required: false, Description: "Target position (1-indexed). Defaults to append-at-end. Clamps to 1..currentMax+1."},
		{Name: "note", Required: false, Description: "Free-text note attached to the entry."},
	},
	Example:              `{"ref_kind":"chain","ref_slug":"agent-first-substrate","position":1,"note":"foundation"}`,
	EnvelopeRequirements: rationaleEnv(),
}

var roadmapUpdateDoc = ActionDoc{
	Purpose: "PATCH one roadmap entry at the given (project, position). Only fields explicitly supplied are changed; everything else is preserved. Fixes the silent-clobber footgun in roadmap_set where omitted fields got cleared. To change which ref a position points at, supply ref_kind AND ref_slug together; supplying only one returns an error.",
	Params: []DocParam{
		{Name: "position", Required: true, Description: "1-indexed per-project position identifying the row to update."},
		{Name: "note", Required: false, Description: "New note. Empty string clears the existing note. Absent (nil) leaves the note unchanged."},
		{Name: "ref_kind", Required: false, Description: "New ref_kind (chain or task). Must be supplied together with ref_slug."},
		{Name: "ref_slug", Required: false, Description: "New ref_slug. Must be supplied together with ref_kind."},
	},
	Example: `{"position":10,"note":"Updated note text"}`,
	SeeAlso: "roadmap_set",
	Notes:   "Fixes the silent-clobber footgun documented in bug roadmap-set-clobbers-notes-on-partial-item-submission.",
}

var roadmapDiffDoc = ActionDoc{
	Purpose: "Surface pending/active chains and tasks that arrived after the last reassessed_at timestamp and aren't yet on the roadmap.",
	Params:  []DocParam{},
	Example: `{}`,
}

var roadmapMarkReassessedDoc = ActionDoc{
	Purpose:              "Bump the last_reassessed_at marker to now. Use after a roadmap review to acknowledge that diff items have been considered.",
	Params:               []DocParam{},
	Example:              `{}`,
	EnvelopeRequirements: rationaleEnv(),
}
