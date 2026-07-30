package scheduler

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
)

type failingPlanner struct {
	stubPlanner
	err error
}

func (p *failingPlanner) PlanningErrorFor(string, []string) error {
	return p.err
}

func TestFIFO_PlanningErrorRejectsRequestWithoutSwap(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	planner := &failingPlanner{
		stubPlanner: stubPlanner{evict: map[string][]string{"target": {"victim"}}},
		err:         errors.New("inconsistent running set"),
	}
	effects := newFakeEffects()
	effects.states["target"] = process.StateStopped
	effects.states["victim"] = process.StateReady
	scheduler := NewFIFO("test", logger, planner, config.FifoConfig{}, nil, effects)

	request := req("target")
	scheduler.OnRequest(request)

	if len(effects.starts) != 0 {
		t.Fatalf("StartSwap calls=%v want none", effects.starts)
	}
	if got := effects.errored("target"); got != 1 {
		t.Fatalf("errored(target)=%d want 1", got)
	}
	if history := string(logger.GetHistory()); !strings.Contains(history, "inconsistent running set") {
		t.Errorf("log history=%q want planning error", history)
	}
}
