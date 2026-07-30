package router

import (
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
)

type lifecycleRecordingSwapper struct {
	evict   map[string][]string
	stopped []string
	ready   []string
}

func (s *lifecycleRecordingSwapper) EvictionFor(target string, _ []string) []string {
	return s.evict[target]
}

func (s *lifecycleRecordingSwapper) OnSwapStart(string, []string) {}

func (s *lifecycleRecordingSwapper) OnModelReady(modelID string, _ time.Time) {
	s.ready = append(s.ready, modelID)
}

func (s *lifecycleRecordingSwapper) OnServeStart(string, time.Time) {}

func (s *lifecycleRecordingSwapper) OnServeDone(string, time.Time) {}

func (s *lifecycleRecordingSwapper) OnModelStopped(modelID string, _ time.Time) {
	s.stopped = append(s.stopped, modelID)
}

func TestBudgetLifecycle_TargetStartFailureReportsCompletedVictimStop(t *testing.T) {
	victim := newFakeProcess("victim")
	victim.markReady()
	target := newFakeProcess("target")
	target.setState(process.StateShutdown)
	planner := &lifecycleRecordingSwapper{
		evict: map[string][]string{"target": {"victim"}},
	}
	router := newTestBase(t, map[string]process.Process{
		"target": target,
		"victim": victim,
	}, planner)

	router.ServeHTTP(httptest.NewRecorder(), newRequest("target"))

	if victim.State() != process.StateStopped {
		t.Fatalf("victim state=%s want stopped", victim.State())
	}
	if !slices.Equal(planner.stopped, []string{"victim"}) {
		t.Errorf("stopped hooks=%v want [victim]", planner.stopped)
	}
	if len(planner.ready) != 0 {
		t.Errorf("ready hooks=%v want none for failed target", planner.ready)
	}
}
