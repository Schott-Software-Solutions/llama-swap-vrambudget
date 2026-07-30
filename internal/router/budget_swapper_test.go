package router

import (
	"io"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

func TestBudgetSwapper_SolverFailureIsNonDestructiveAndVisible(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	swapper := newBudgetSwapper(
		newBudgetSolver(100, map[string]int{"known": 40, "target": 50}, nil),
		logger,
	)
	running := []string{"known", "unknown"}

	if evict := swapper.EvictionFor("target", running); len(evict) != 0 {
		t.Fatalf("EvictionFor=%v want no evictions", evict)
	}
	if err := swapper.PlanningErrorFor("target", running); err == nil {
		t.Fatal("PlanningErrorFor=nil want inconsistent-running-set error")
	}

	// Defensive visibility remains in OnSwapStart for callers other than FIFO.
	swapper.OnSwapStart("target", running)
	if history := string(logger.GetHistory()); !strings.Contains(history, "decision_error") {
		t.Errorf("log history=%q want decision_error", history)
	}
}
