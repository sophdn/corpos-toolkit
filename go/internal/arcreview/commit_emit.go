package arcreview

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"toolkit/internal/arcreview/arcparams"
	"toolkit/internal/events"
)

// EmitCommitLandedResult mirrors what the action returns: the new event_id
// when emit succeeded, plus a status discriminator for the advisor's
// success/no-op log line.
type EmitCommitLandedResult struct {
	Status  string `json:"status"`
	EventID string `json:"event_id,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// HandleEmitCommitLanded accepts a commit-details payload, calls
// events.Emit with entity_kind="commit" + entity_slug=commit_sha, and
// returns the new event_id. The emit lands inside the daemon's process
// so the chained fold hook (SubstrateReviewObserver from
// InstallListenerFoldHook) fires on the same tx, kicks the review
// goroutine, and queues decisions for the next Stop hook to drain.
//
// Fail-open per advisor discipline: validation failures and schema-
// validator failures return status="skipped" with a reason; the caller
// (the advisor) treats both as no-op and never blocks the post-commit
// hook.
func HandleEmitCommitLanded(ctx context.Context, deps Deps, project string, params json.RawMessage) (EmitCommitLandedResult, error) {
	var p arcparams.EmitCommitLandedParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return EmitCommitLandedResult{Status: "skipped", Reason: "parse params: " + err.Error()}, nil
		}
	}
	if p.CommitSHA == "" {
		return EmitCommitLandedResult{Status: "skipped", Reason: "commit_sha is required"}, nil
	}
	if deps.Pool == nil {
		return EmitCommitLandedResult{Status: "skipped", Reason: "pool not configured"}, nil
	}

	payload := events.CommitLandedPayload{
		CommitSHA:         p.CommitSHA,
		Branch:            p.Branch,
		FilesChangedCount: p.FilesChangedCount,
		Author:            p.Author,
		Subject:           p.Subject,
	}
	var entity events.EntityRef
	if project == "" {
		entity = events.NewCrossCuttingEntityRef("commit", p.CommitSHA)
	} else {
		entity = events.NewEntityRef("commit", p.CommitSHA, project)
	}
	rationale := "post-commit-restart-advisor: every commit emits a CommitLanded event so the arcreview substrate listener can fire a session-scoped review"
	var eventID string
	err := deps.Pool.WithWrite(ctx, func(tx *sql.Tx) error {
		id, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:    entity,
			Payload:   payload,
			Rationale: &rationale,
		})
		if err != nil {
			return err
		}
		eventID = id
		return nil
	})
	if err != nil {
		return EmitCommitLandedResult{Status: "skipped", Reason: fmt.Sprintf("emit: %s", err.Error())}, nil
	}
	return EmitCommitLandedResult{Status: "ok", EventID: eventID}, nil
}
