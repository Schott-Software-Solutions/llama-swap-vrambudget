package scheduler

import (
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
)

type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	return c.now
}

type activityEvent struct {
	kind    string
	modelID string
	at      time.Time
}

type activityPlanner struct {
	stubPlanner
	events []activityEvent
}

func (p *activityPlanner) OnModelReady(modelID string, at time.Time) {
	p.events = append(p.events, activityEvent{"ready", modelID, at})
}

func (p *activityPlanner) OnServeStart(modelID string, at time.Time) {
	p.events = append(p.events, activityEvent{"start", modelID, at})
}

func (p *activityPlanner) OnServeDone(modelID string, at time.Time) {
	p.events = append(p.events, activityEvent{"done", modelID, at})
}

func (p *activityPlanner) OnModelStopped(modelID string, at time.Time) {
	p.events = append(p.events, activityEvent{"stopped", modelID, at})
}

func TestFIFO_ActivityLifecycle(t *testing.T) {
	planner := &activityPlanner{
		stubPlanner: stubPlanner{evict: map[string][]string{"target": {"victim"}}},
	}
	effects := newFakeEffects()
	effects.states["target"] = process.StateStopped
	effects.states["victim"] = process.StateReady
	scheduler := newFIFO(planner, effects)
	clock := &fakeClock{now: time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)}
	scheduler.clock = clock

	scheduler.OnRequest(req("target"))
	clock.now = clock.now.Add(time.Minute)
	effects.states["victim"] = process.StateStopped
	effects.states["target"] = process.StateReady
	scheduler.OnSwapDone(SwapDone{ModelID: "target"})
	clock.now = clock.now.Add(time.Minute)
	scheduler.OnServeDone(ServeDoneEvent{ModelID: "target"})

	want := []activityEvent{
		{"stopped", "victim", time.Date(2026, 7, 30, 10, 1, 0, 0, time.UTC)},
		{"ready", "target", time.Date(2026, 7, 30, 10, 1, 0, 0, time.UTC)},
		{"start", "target", time.Date(2026, 7, 30, 10, 1, 0, 0, time.UTC)},
		{"done", "target", time.Date(2026, 7, 30, 10, 2, 0, 0, time.UTC)},
	}
	if len(planner.events) != len(want) {
		t.Fatalf("events=%v want %v", planner.events, want)
	}
	for i := range want {
		if planner.events[i] != want[i] {
			t.Errorf("event[%d]=%v want %v", i, planner.events[i], want[i])
		}
	}
}

func TestFIFO_NonActivitySwapperUnchanged(t *testing.T) {
	effects := newFakeEffects()
	effects.states["a"] = process.StateReady
	scheduler := newFIFO(&stubPlanner{}, effects)

	scheduler.OnRequest(req("a"))
	scheduler.OnServeDone(ServeDoneEvent{ModelID: "a"})

	if got := effects.served("a"); got != 1 {
		t.Errorf("served(a)=%d want 1", got)
	}
}
