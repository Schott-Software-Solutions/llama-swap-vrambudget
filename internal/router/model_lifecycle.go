package router

import "context"

// modelLifecycle is an optional, router-specific extension around process
// transitions. Implementations must be best effort: hook failures may be
// logged, but must never prevent a stop or a successful model grant.
type modelLifecycle interface {
	BeforeModelStop(ctx context.Context, modelID string)
	AfterModelStart(ctx context.Context, modelID string)
}
