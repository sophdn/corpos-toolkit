package benchmarks

import (
	"strings"
)

// SnapshotTask is one task entry within a SnapshotChain.
type SnapshotTask struct {
	Slug   string // kebab-case
	Status string // pending | active | closed | cancelled
}

// SnapshotChain is one chain entry within a ProjectSnapshot.
type SnapshotChain struct {
	Slug   string         // kebab-case
	Title  string         // one-line description
	Status string         // open | closed
	Tasks  []SnapshotTask // ordered
}

// ProjectSnapshot is a static project-context snapshot used in Layer 4
// benchmark scenarios. Render() produces a compact text block for prompt
// injection.
type ProjectSnapshot struct {
	ID     string // stable identifier
	Chains []SnapshotChain
}

// Render produces the compact text block injected into L4 prompts.
// Behavior mirrors Rust ProjectSnapshot::render — one paragraph per
// chain, indented task list with per-task status, trailing blank line
// after each chain.
func (p ProjectSnapshot) Render() string {
	var b strings.Builder
	b.WriteString("## Project context\n\n")
	for _, chain := range p.Chains {
		b.WriteString("Chain `")
		b.WriteString(chain.Slug)
		b.WriteString("` (")
		b.WriteString(chain.Status)
		b.WriteString(") — ")
		b.WriteString(chain.Title)
		b.WriteString("\n")
		for _, task := range chain.Tasks {
			b.WriteString("  - `")
			b.WriteString(task.Slug)
			b.WriteString("` (")
			b.WriteString(task.Status)
			b.WriteString(")\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ── seed data ────────────────────────────────────────────────────────────────

// bl45Tasks mirrors Rust BL45_TASKS (benchmarks/src/fixtures.rs:67).
var bl45Tasks = []SnapshotTask{
	{Slug: "design-test-data-fixtures", Status: "active"},
	{Slug: "l4-db-schema", Status: "pending"},
	{Slug: "l4-runner", Status: "pending"},
	{Slug: "l4-scenarios", Status: "pending"},
	{Slug: "l5-design-and-runner", Status: "pending"},
}

// bprTasks mirrors Rust BPR_TASKS.
var bprTasks = []SnapshotTask{
	{Slug: "run-id-migration", Status: "pending"},
	{Slug: "timeseries-api-endpoint", Status: "pending"},
	{Slug: "install-recharts", Status: "pending"},
	{Slug: "line-chart-component", Status: "pending"},
	{Slug: "tool-card-graph", Status: "pending"},
}

// ctmsTasks mirrors Rust CTMS_TASKS.
var ctmsTasks = []SnapshotTask{
	{Slug: "ctms-read-tool", Status: "closed"},
	{Slug: "ctms-glob-tool", Status: "closed"},
	{Slug: "ctms-grep-tool", Status: "closed"},
	{Slug: "ctms-write-tool", Status: "closed"},
	{Slug: "ctms-bash-tool", Status: "closed"},
}

// seedChains mirrors Rust SEED_CHAINS.
var seedChains = []SnapshotChain{
	{
		Slug:   "benchmark-l4-l5-extension",
		Title:  "Add L4 (arg accuracy) and L5 (output interpretation) benchmark layers",
		Status: "open",
		Tasks:  bl45Tasks,
	},
	{
		Slug:   "benchmark-page-redesign",
		Title:  "Replace benchmark page with a time-series line graph view",
		Status: "open",
		Tasks:  bprTasks,
	},
	{
		Slug:   "core-tools-mcp-surface",
		Title:  "Surface core file and bash tools on the MCP interface",
		Status: "closed",
		Tasks:  ctmsTasks,
	},
}

// SeedSnapshot is the seed snapshot drawn from real seed-packet chain
// and task slugs. Covers two open chains and one closed chain (15 tasks
// total across 3 chains). Mirrors Rust SEED_SNAPSHOT.
var SeedSnapshot = ProjectSnapshot{
	ID:     "seed-a",
	Chains: seedChains,
}
