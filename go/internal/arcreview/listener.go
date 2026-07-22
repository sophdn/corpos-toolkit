package arcreview

import (
	"context"
	"database/sql"

	"toolkit/internal/events"
	"toolkit/internal/obs"
)

// SubstrateTriggerEventTypes is the closed set of write-side event
// types whose emission counts as an arc-close trigger from the
// substrate side. All five of the design's trigger events are wired
// after chain arc-close-filing-review-substrate-listener-wiring T7.
//
// Per docs/ARC_CLOSE_FILING_REVIEW.md §Trigger-model B.
var SubstrateTriggerEventTypes = map[string]string{
	"BugResolved":    "event_bug_resolved",
	"TaskCompleted":  "event_task_completed",
	"ChainClosed":    "event_chain_closed",
	"CommitLanded":   "event_commit_landed",
	"RoadmapUpdated": "event_roadmap_updated",
}

// TriggerObserver is the harness-side seam for substrate trigger
// detection. When a trigger event lands the goroutine pre-INSERT
// fold notices it (via the chained FoldHook), calls Observe with
// the trigger payload, and lets the observer decide what to do.
//
// Stage 3 deliberately ships a no-op observer (LogOnlyTriggerObserver)
// because firing a real review from the substrate side requires a
// session_id + transcript_path the listener does not yet have access
// to (see docs/ARC_CLOSE_FILING_REVIEW.md §Architecture). The chain's
// follow-up task will design the substrate-side snapshot path —
// likely via a session_registry table written by the harness path
// that the listener queries to find the most-recently-active
// session for a project.
type TriggerObserver interface {
	Observe(ctx context.Context, evt SubstrateTriggerEvent)
}

// SubstrateTriggerEvent is one detected trigger. The fields cover what
// the listener can derive without firing an actual review.
type SubstrateTriggerEvent struct {
	EventID     string
	EventType   string
	TriggerSlug string // "event_bug_resolved" / "event_task_completed" / "event_chain_closed"
	EntityKind  string
	EntitySlug  string
	ProjectID   *string
}

// LogOnlyTriggerObserver is the v1 observer: it logs every detected
// trigger via obs.Logger and returns. No review fires, no events
// emit. Useful as the production default until substrate-side firing
// is designed.
type LogOnlyTriggerObserver struct{}

// Observe writes an info-level log line for each detected trigger.
// The log line is shaped so a future grep over the daemon log can
// surface "substrate would have fired N reviews this week" without
// needing a structured signal.
func (LogOnlyTriggerObserver) Observe(ctx context.Context, evt SubstrateTriggerEvent) {
	projectID := ""
	if evt.ProjectID != nil {
		projectID = *evt.ProjectID
	}
	obs.Logger(ctx).Info("arcreview substrate trigger detected (firing deferred to follow-up chain)",
		"event_id", evt.EventID,
		"event_type", evt.EventType,
		"trigger_slug", evt.TriggerSlug,
		"entity_kind", evt.EntityKind,
		"entity_slug", evt.EntitySlug,
		"project_id", projectID,
	)
}

// InstallListenerFoldHook chains a trigger-detection hook in front of
// the current FoldHook (typically projections.NewSubstrateFoldHook).
// Call once at toolkit-server startup AFTER projections has installed
// its hook.
//
// The chained hook runs synchronously inside the same tx as the event
// INSERT; observer.Observe is invoked from there. A blocking observer
// would extend the write-transaction lifetime — LogOnlyTriggerObserver
// is non-blocking (just a log line). Future observers that emit
// events or fire reviews should do so asynchronously (kick a
// goroutine) to keep the tx tight.
func InstallListenerFoldHook(observer TriggerObserver) {
	prev := events.CurrentFoldHook()
	events.SetFoldHook(func(ctx context.Context, tx *sql.Tx, evt events.RawEvent) error {
		triggerSlug, isTrigger := SubstrateTriggerEventTypes[evt.Type]
		if isTrigger && observer != nil {
			observer.Observe(ctx, SubstrateTriggerEvent{
				EventID:     evt.EventID,
				EventType:   evt.Type,
				TriggerSlug: triggerSlug,
				EntityKind:  evt.EntityKind,
				EntitySlug:  evt.EntitySlug,
				ProjectID:   evt.EntityProjectID,
			})
		}
		// Delegate to the previously-installed hook (projections
		// refresh) so the chain stays intact.
		if prev != nil {
			return prev(ctx, tx, evt)
		}
		return nil
	})
}
