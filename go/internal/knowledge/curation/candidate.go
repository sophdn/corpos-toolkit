package curation

import "time"

// Candidate is the in-memory shape of a curation_candidates row.
// Mirror of crates/knowledge-shared CandidateRow. The full DB layer
// (List, Read, Add, UpdateScoring, Promote, Reject) lands in T6 —
// this file declares the struct so the SourceMaterialBuilder interface
// and the per-origin builders have a type to operate on.
//
// Fields mirror the schema in crates/shared-db/migrations/022_curation_candidates.sql:
type Candidate struct {
	ID                    int64
	ProjectID             string
	SourceType            string // 'task' | 'vault' | etc.
	SourceRef             string // <project>::<slug> for tasks, path for vault, etc.
	Question              string
	InvokeWhen            string
	Description           string
	Tags                  []string
	QualityScore          *float64
	Origin                string  // 'task_handoff' | 'zero_result_gap' | 'session_mining'
	OriginRef             *string // free-form per-origin reference (task slug, event id, vault path)
	PromotedAutomatically bool
	PromotedAt            *time.Time
	ExpiresAt             *time.Time
	Status                string // 'pending' | 'promoted' | 'rejected' | 'expired'
	CreatedAt             time.Time
}
