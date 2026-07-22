package router

import (
	"context"

	"toolkit/internal/qwenctx"
)

// stampTaskID reads the task_id stamp the caller placed on ctx via
// qwenctx.WithTaskID. Unstamped callers map to qwenctx.Unattributed so
// the row still lands in inference_invocations.
//
// Lives in its own file so the router package's import of qwenctx is
// visible at the package boundary without polluting router.go's
// interface surface.
func stampTaskID(ctx context.Context) string { return qwenctx.TaskID(ctx) }
