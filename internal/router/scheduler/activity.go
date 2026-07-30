package scheduler

import "time"

// ActivityAwareSwapper is an optional extension implemented by eviction
// policies that need request lifecycle information. All callbacks run on the
// scheduler's single event-loop goroutine.
type ActivityAwareSwapper interface {
	OnModelReady(modelID string, at time.Time)
	OnServeStart(modelID string, at time.Time)
	OnServeDone(modelID string, at time.Time)
	OnModelStopped(modelID string, at time.Time)
}

// Clock makes scheduler lifecycle timestamps deterministic in tests.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time {
	return time.Now()
}

func notifyModelReady(planner Swapper, modelID string, at time.Time) {
	if tracker, ok := planner.(ActivityAwareSwapper); ok {
		tracker.OnModelReady(modelID, at)
	}
}

func notifyServeStart(planner Swapper, modelID string, at time.Time) {
	if tracker, ok := planner.(ActivityAwareSwapper); ok {
		tracker.OnServeStart(modelID, at)
	}
}

func notifyServeDone(planner Swapper, modelID string, at time.Time) {
	if tracker, ok := planner.(ActivityAwareSwapper); ok {
		tracker.OnServeDone(modelID, at)
	}
}

func notifyModelStopped(planner Swapper, modelID string, at time.Time) {
	if tracker, ok := planner.(ActivityAwareSwapper); ok {
		tracker.OnModelStopped(modelID, at)
	}
}
