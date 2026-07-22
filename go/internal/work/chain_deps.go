package work

// chain_deps.go implements the dependency-driven roadmap: chain-level
// dependency edges (chain_dep_add / chain_dep_remove / chain_dep_list)
// and roadmap_plan, which computes the roadmap ORDER from those edges
// instead of a hand-set position.
//
// chain_deps is a direct-write table (see migration 087) — the roadmap_meta
// precedent, not the event-sourced entity tables. Edges are roadmap
// configuration, so add/remove INSERT/DELETE inside their WithWrite tx.
//
// roadmap_plan topologically sorts the open chains over the edge graph:
// each chain is tagged ready (no open prerequisites — actionable now) or
// blocked (waiting on prerequisites), carries its "why" (the prerequisite
// edges + reasons), and a dependency cycle is reported rather than hung.
// Manual roadmap position is demoted to a tiebreak among ready chains.
//
// All chain lookups go through proj_chain_status (never the base `chains`
// table — the retired-CRUD guard forbids it in non-test code).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"toolkit/internal/db"
)

// chainRef is a resolved chain: its numeric id, project, and status.
type chainRef struct {
	id      int64
	project string
	status  string
}

// resolveChain looks up a chain by slug via the projection. Returns a
// not-found error when the slug doesn't match an open-or-closed chain.
func resolveChain(ctx context.Context, pool *db.Pool, slug string) (chainRef, error) {
	var r chainRef
	err := pool.DB().QueryRowContext(ctx,
		`SELECT id, project_id, status FROM proj_chain_status WHERE slug = ?`, slug).
		Scan(&r.id, &r.project, &r.status)
	if errors.Is(err, sql.ErrNoRows) {
		return chainRef{}, fmt.Errorf("chain '%s' not found", slug)
	}
	return r, err
}

// ── chain_dep_add ───────────────────────────────────────────────────────

type chainDepAddParams struct {
	// Dependent depends on (comes after) Prerequisite.
	Dependent    string `json:"dependent_chain"`
	Prerequisite string `json:"prerequisite_chain"`
	// Aliases for ergonomics.
	DependentAlt    string `json:"dependent"`
	PrerequisiteAlt string `json:"prerequisite"`
	DependsOn       string `json:"depends_on"`
	Reason          string `json:"reason"`
}

func (p chainDepAddParams) dependent() string {
	return firstNonEmpty(p.Dependent, p.DependentAlt)
}
func (p chainDepAddParams) prerequisite() string {
	return firstNonEmpty(p.Prerequisite, p.PrerequisiteAlt, p.DependsOn)
}

// ChainDepResult is the success-or-error envelope for chain_dep_add /
// chain_dep_remove.
type ChainDepResult struct {
	OK           bool   `json:"ok,omitempty"`
	Dependent    string `json:"dependent_chain,omitempty"`
	Prerequisite string `json:"prerequisite_chain,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Error        string `json:"error,omitempty"`
}

// HandleChainDepAdd adds a dependency edge (dependent depends on
// prerequisite). Rejects self-edges, cross-project pairs, unknown chains,
// and duplicates.
func HandleChainDepAdd(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (ChainDepResult, error) {
	var p chainDepAddParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return ChainDepResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	dep, prereq := p.dependent(), p.prerequisite()
	if dep == "" || prereq == "" {
		return ChainDepResult{Error: "chain_dep_add requires `dependent_chain` and `prerequisite_chain` (the dependent depends on / comes after the prerequisite)"}, nil
	}
	if dep == prereq {
		return ChainDepResult{Error: fmt.Sprintf("chain '%s' cannot depend on itself", dep)}, nil
	}
	depRef, err := resolveChain(ctx, pool, dep)
	if err != nil {
		return ChainDepResult{Error: err.Error()}, nil
	}
	prereqRef, err := resolveChain(ctx, pool, prereq)
	if err != nil {
		return ChainDepResult{Error: err.Error()}, nil
	}
	if depRef.project != prereqRef.project {
		return ChainDepResult{Error: fmt.Sprintf("cross-project dependency rejected: '%s' is in '%s' but '%s' is in '%s' — dependencies are within one project", dep, depRef.project, prereq, prereqRef.project)}, nil
	}
	if project != "" && project != depRef.project {
		return ChainDepResult{Error: fmt.Sprintf("project hint '%s' doesn't match the chains' project '%s'", project, depRef.project)}, nil
	}

	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		var exists int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM chain_deps WHERE dependent_chain_id = ? AND prerequisite_chain_id = ?`,
			depRef.id, prereqRef.id).Scan(&exists)
		if err == nil {
			return fmt.Errorf("dependency '%s' → '%s' already exists", dep, prereq)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO chain_deps (dependent_chain_id, prerequisite_chain_id, reason)
			 VALUES (?, ?, ?)`, depRef.id, prereqRef.id, p.Reason)
		return err
	})
	if err != nil {
		return ChainDepResult{Error: err.Error()}, nil
	}
	return ChainDepResult{OK: true, Dependent: dep, Prerequisite: prereq, Reason: p.Reason}, nil
}

// ── chain_dep_remove ────────────────────────────────────────────────────

func HandleChainDepRemove(ctx context.Context, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (ChainDepResult, error) {
	var p chainDepAddParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return ChainDepResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	dep, prereq := p.dependent(), p.prerequisite()
	if dep == "" || prereq == "" {
		return ChainDepResult{Error: "chain_dep_remove requires `dependent_chain` and `prerequisite_chain`"}, nil
	}
	depRef, err := resolveChain(ctx, pool, dep)
	if err != nil {
		return ChainDepResult{Error: err.Error()}, nil
	}
	prereqRef, err := resolveChain(ctx, pool, prereq)
	if err != nil {
		return ChainDepResult{Error: err.Error()}, nil
	}
	var removed bool
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM chain_deps WHERE dependent_chain_id = ? AND prerequisite_chain_id = ?`,
			depRef.id, prereqRef.id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		removed = n > 0
		return nil
	})
	if err != nil {
		return ChainDepResult{Error: err.Error()}, nil
	}
	if !removed {
		return ChainDepResult{Error: fmt.Sprintf("no dependency '%s' → '%s' to remove", dep, prereq)}, nil
	}
	return ChainDepResult{OK: true, Dependent: dep, Prerequisite: prereq}, nil
}

// ── chain_dep_list ──────────────────────────────────────────────────────

type chainDepListParams struct {
	Chain     string `json:"chain"`
	ChainSlug string `json:"chain_slug"`
}

// ChainDepEdge is one directed edge in list output.
type ChainDepEdge struct {
	ChainSlug string `json:"chain_slug"`
	Reason    string `json:"reason,omitempty"`
}

// ChainDepListResult reports a chain's prerequisites (what it depends on)
// and dependents (what depends on it).
type ChainDepListResult struct {
	Chain         string         `json:"chain"`
	Prerequisites []ChainDepEdge `json:"prerequisites"`
	Dependents    []ChainDepEdge `json:"dependents"`
	Error         string         `json:"error,omitempty"`
}

func HandleChainDepList(ctx context.Context, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (ChainDepListResult, error) {
	var p chainDepListParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return ChainDepListResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	slug := firstNonEmpty(p.Chain, p.ChainSlug)
	if slug == "" {
		return ChainDepListResult{Error: "chain_dep_list requires `chain` (the chain slug to list edges for)"}, nil
	}
	ref, err := resolveChain(ctx, pool, slug)
	if err != nil {
		return ChainDepListResult{Error: err.Error()}, nil
	}
	result := ChainDepListResult{Chain: slug, Prerequisites: []ChainDepEdge{}, Dependents: []ChainDepEdge{}}

	// Prerequisites: edges where this chain is the dependent.
	preRows, err := pool.DB().QueryContext(ctx,
		`SELECT c.slug, d.reason FROM chain_deps d
		 JOIN proj_chain_status c ON c.id = d.prerequisite_chain_id
		 WHERE d.dependent_chain_id = ? ORDER BY c.slug`, ref.id)
	if err != nil {
		return ChainDepListResult{}, err
	}
	defer preRows.Close()
	for preRows.Next() {
		var e ChainDepEdge
		if err := preRows.Scan(&e.ChainSlug, &e.Reason); err != nil {
			return ChainDepListResult{}, err
		}
		result.Prerequisites = append(result.Prerequisites, e)
	}
	if err := preRows.Err(); err != nil {
		return ChainDepListResult{}, err
	}

	// Dependents: edges where this chain is the prerequisite.
	depRows, err := pool.DB().QueryContext(ctx,
		`SELECT c.slug, d.reason FROM chain_deps d
		 JOIN proj_chain_status c ON c.id = d.dependent_chain_id
		 WHERE d.prerequisite_chain_id = ? ORDER BY c.slug`, ref.id)
	if err != nil {
		return ChainDepListResult{}, err
	}
	defer depRows.Close()
	for depRows.Next() {
		var e ChainDepEdge
		if err := depRows.Scan(&e.ChainSlug, &e.Reason); err != nil {
			return ChainDepListResult{}, err
		}
		result.Dependents = append(result.Dependents, e)
	}
	return result, depRows.Err()
}

// ── roadmap_plan (computed order) ───────────────────────────────────────

type roadmapPlanParams struct{}

// PlanPrereq is one gating prerequisite in a plan entry's "why".
type PlanPrereq struct {
	ChainSlug string `json:"chain_slug"`
	Reason    string `json:"reason,omitempty"`
}

// RoadmapPlanEntry is one chain in the computed order.
type RoadmapPlanEntry struct {
	Position  int          `json:"position"`
	ChainSlug string       `json:"chain_slug"`
	Project   string       `json:"project,omitempty"`
	Status    string       `json:"status"` // "ready" | "blocked"
	DependsOn []PlanPrereq `json:"depends_on,omitempty"`
}

// RoadmapPlanResult is the computed roadmap: open chains in dependency
// topological order. On a dependency cycle, Error + Cycle are set and
// Order holds the successfully-placed prefix.
type RoadmapPlanResult struct {
	Scope string             `json:"scope"`
	Order []RoadmapPlanEntry `json:"order"`
	Cycle []string           `json:"cycle,omitempty"`
	Error string             `json:"error,omitempty"`
}

// planNode is the per-chain working state for the topo sort.
type planNode struct {
	id        int64
	slug      string
	project   string
	position  sql.NullInt64 // manual roadmap position, tiebreak among ready
	createdAt string
}

func HandleRoadmapPlan(ctx context.Context, pool *db.Pool, project string, _ json.RawMessage) (RoadmapPlanResult, error) {
	scope := "cross-project"
	if project != "" {
		scope = project
	}

	// Load open chains in scope, with their manual roadmap position (if any)
	// as the ready-tiebreak.
	nodeSQL := `SELECT c.id, c.slug, c.project_id, r.position, c.created_at
		FROM proj_chain_status c
		LEFT JOIN proj_roadmap_view r ON r.ref_kind = 'chain' AND r.ref_slug = c.slug
		WHERE c.status = 'open'`
	var rows *sql.Rows
	var err error
	if project != "" {
		rows, err = pool.DB().QueryContext(ctx, nodeSQL+` AND c.project_id = ?`, project)
	} else {
		rows, err = pool.DB().QueryContext(ctx, nodeSQL)
	}
	if err != nil {
		return RoadmapPlanResult{}, err
	}
	defer rows.Close()

	nodes := map[int64]*planNode{}
	for rows.Next() {
		var n planNode
		if err := rows.Scan(&n.id, &n.slug, &n.project, &n.position, &n.createdAt); err != nil {
			return RoadmapPlanResult{}, err
		}
		nodes[n.id] = &n
	}
	if err := rows.Err(); err != nil {
		return RoadmapPlanResult{}, err
	}

	// Load edges. Only edges where the prerequisite is itself an open chain
	// in scope gate ordering — a closed prerequisite is already satisfied
	// and is dropped from the graph.
	edgeRows, err := pool.DB().QueryContext(ctx,
		`SELECT dependent_chain_id, prerequisite_chain_id, reason FROM chain_deps`)
	if err != nil {
		return RoadmapPlanResult{}, err
	}
	defer edgeRows.Close()

	type edge struct {
		dep, prereq int64
		reason      string
	}
	adj := map[int64][]int64{}      // prereq -> dependents (open, in scope)
	indeg := map[int64]int{}        // dependent -> count of open prereqs
	prereqsOf := map[int64][]edge{} // dependent -> its gating prereq edges
	for edgeRows.Next() {
		var e edge
		if err := edgeRows.Scan(&e.dep, &e.prereq, &e.reason); err != nil {
			return RoadmapPlanResult{}, err
		}
		// Both endpoints must be open chains in scope to gate.
		if nodes[e.dep] == nil || nodes[e.prereq] == nil {
			continue
		}
		adj[e.prereq] = append(adj[e.prereq], e.dep)
		indeg[e.dep]++
		prereqsOf[e.dep] = append(prereqsOf[e.dep], e)
	}
	if err := edgeRows.Err(); err != nil {
		return RoadmapPlanResult{}, err
	}

	order := kahnSort(nodes, adj, indeg)

	result := RoadmapPlanResult{Scope: scope, Order: []RoadmapPlanEntry{}}
	pos := 1
	for _, id := range order {
		n := nodes[id]
		entry := RoadmapPlanEntry{
			Position:  pos,
			ChainSlug: n.slug,
			Project:   n.project,
			Status:    "ready",
		}
		if edges := prereqsOf[id]; len(edges) > 0 {
			entry.Status = "blocked"
			for _, e := range edges {
				entry.DependsOn = append(entry.DependsOn, PlanPrereq{ChainSlug: nodes[e.prereq].slug, Reason: e.reason})
			}
			sort.Slice(entry.DependsOn, func(i, j int) bool {
				return entry.DependsOn[i].ChainSlug < entry.DependsOn[j].ChainSlug
			})
		}
		result.Order = append(result.Order, entry)
		pos++
	}

	// Any node not placed sits in (or behind) a cycle.
	if len(order) < len(nodes) {
		var cycle []string
		for id, n := range nodes {
			if indeg[id] > 0 {
				cycle = append(cycle, n.slug)
			}
		}
		sort.Strings(cycle)
		result.Cycle = cycle
		result.Error = fmt.Sprintf("dependency cycle detected among %d chain(s): %v — break it with chain_dep_remove", len(cycle), cycle)
	}
	return result, nil
}

// kahnSort returns chain ids in dependency topological order. Among nodes
// that become ready simultaneously, the tiebreak is: manual roadmap
// position (ascending, NULLs last), then created_at, then slug — a
// deterministic, stable order. Nodes left with positive in-degree (a
// cycle) are not returned.
func kahnSort(nodes map[int64]*planNode, adj map[int64][]int64, indeg map[int64]int) []int64 {
	// Local in-degree copy we can decrement.
	deg := make(map[int64]int, len(nodes))
	for id := range nodes {
		deg[id] = indeg[id]
	}
	less := func(a, b int64) bool {
		na, nb := nodes[a], nodes[b]
		// Manual position first (set sorts before unset; lower wins).
		if na.position.Valid != nb.position.Valid {
			return na.position.Valid // a has a position, b doesn't → a first
		}
		if na.position.Valid && nb.position.Valid && na.position.Int64 != nb.position.Int64 {
			return na.position.Int64 < nb.position.Int64
		}
		if na.createdAt != nb.createdAt {
			return na.createdAt < nb.createdAt
		}
		return na.slug < nb.slug
	}

	var ready []int64
	for id := range nodes {
		if deg[id] == 0 {
			ready = append(ready, id)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return less(ready[i], ready[j]) })

	var order []int64
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		var newlyReady []int64
		for _, dep := range adj[id] {
			deg[dep]--
			if deg[dep] == 0 {
				newlyReady = append(newlyReady, dep)
			}
		}
		if len(newlyReady) > 0 {
			ready = append(ready, newlyReady...)
			sort.Slice(ready, func(i, j int) bool { return less(ready[i], ready[j]) })
		}
	}
	return order
}

// ── result marshaling (sections as [] not null) ─────────────────────────

// MarshalJSON keeps empty slices as [] for the list/plan reads.
func (r ChainDepListResult) MarshalJSON() ([]byte, error) {
	type alias ChainDepListResult
	a := alias(r)
	if a.Prerequisites == nil {
		a.Prerequisites = []ChainDepEdge{}
	}
	if a.Dependents == nil {
		a.Dependents = []ChainDepEdge{}
	}
	return json.Marshal(a)
}

// ── Action-doc descriptors ──────────────────────────────────────────────

var chainDepAddDoc = ActionDoc{
	Purpose: "Add a chain-level dependency edge: the dependent chain depends on (must come after) the prerequisite chain. Edges drive roadmap_plan's computed order, so declaring a dependency once is how you shape the roadmap — no manual position-setting. Rejects self-edges, cross-project pairs, unknown chains, and duplicates.",
	Params: []DocParam{
		{Name: "dependent_chain", Required: true, Description: "Slug of the chain that depends on / comes after the prerequisite. Alias: dependent."},
		{Name: "prerequisite_chain", Required: true, Description: "Slug of the chain that must come first. Aliases: prerequisite, depends_on."},
		{Name: "reason", Required: false, Description: "Why the dependency exists — the human-readable half of the 'why this order' answer (e.g. 'needs the typed core before the HTTP layer')."},
	},
	Example:              `{"dependent_chain":"http-api","prerequisite_chain":"typed-core","reason":"needs the domain types"}`,
	EnvelopeRequirements: rationaleEnv(),
}

var chainDepRemoveDoc = ActionDoc{
	Purpose: "Remove a chain-level dependency edge (dependent → prerequisite). Use to break a cycle roadmap_plan reports, or when a prerequisite no longer gates the dependent.",
	Params: []DocParam{
		{Name: "dependent_chain", Required: true, Description: "Dependent chain slug. Alias: dependent."},
		{Name: "prerequisite_chain", Required: true, Description: "Prerequisite chain slug. Aliases: prerequisite, depends_on."},
	},
	Example:              `{"dependent_chain":"http-api","prerequisite_chain":"typed-core"}`,
	EnvelopeRequirements: rationaleEnv(),
}

var chainDepListDoc = ActionDoc{
	Purpose: "List a chain's dependency edges: its prerequisites (what it depends on) and its dependents (what depends on it), each with the edge's reason. Read-only.",
	Params: []DocParam{
		{Name: "chain", Required: true, Description: "Chain slug to list edges for. Alias: chain_slug."},
	},
	Example: `{"chain":"http-api"}`,
}

var roadmapPlanDoc = ActionDoc{
	Purpose: "Computed roadmap — open chains in dependency topological order, derived from chain_deps edges (cross-project by default; pass `project` to scope). Each entry is tagged ready (no open prerequisites — actionable now) or blocked, and carries its depends_on (the gating prerequisite edges + reasons): the 'why this order' the manual roadmap never had. Self-maintaining — closing a prerequisite promotes its dependents to ready on the next call. Read-only.",
	Params:  []DocParam{},
	Example: `{}`,
	Notes: "Order is recomputed on every call from the edge graph, so it never needs manual position-setting. Among chains that are simultaneously ready, the tiebreak is manual roadmap position (roadmap_set/insert) ascending, then created_at, then slug — so a manual position still nudges priority without owning the whole order.\n\n" +
		"A dependency cycle is reported, not hung: the response carries `error` plus a `cycle` list naming the chains in the cycle, and `order` holds the chains that could be placed before the cycle. Break the cycle with chain_dep_remove.\n\n" +
		"Only edges between two open chains gate ordering; a closed prerequisite is treated as satisfied and dropped.",
	SeeAlso: "chain_dep_add, roadmap_list",
}
