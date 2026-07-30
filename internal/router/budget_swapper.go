package router

import (
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// budgetSwapper is owned exclusively by the scheduler event-loop goroutine;
// its activity map and decision cache therefore need no mutex.
type budgetSwapper struct {
	solver *budgetSolver
	logger *logmon.Monitor

	activity   map[string]modelActivity
	generation uint64

	lastTarget     string
	lastRunning    []string
	lastGeneration uint64
	lastResult     budgetSolveResult
	lastValid      bool
}

func newBudgetSwapper(solver *budgetSolver, logger *logmon.Monitor) *budgetSwapper {
	return &budgetSwapper{
		solver:   solver,
		logger:   logger,
		activity: make(map[string]modelActivity),
	}
}

func (s *budgetSwapper) solve(target string, running []string) budgetSolveResult {
	if s.lastValid &&
		s.lastGeneration == s.generation &&
		s.lastTarget == target &&
		slices.Equal(s.lastRunning, running) {
		return s.lastResult
	}
	result := s.solver.Solve(target, running, s.activity)
	s.lastTarget = target
	s.lastRunning = slices.Clone(running)
	s.lastGeneration = s.generation
	s.lastResult = result
	s.lastValid = true
	return result
}

func (s *budgetSwapper) EvictionFor(target string, running []string) []string {
	result := s.solve(target, running)
	if result.Error != nil {
		return nil
	}
	return slices.Clone(result.Evict)
}

// PlanningErrorFor implements scheduler.PlanningErrorSwapper without changing
// the upstream Swapper contract. The scheduler uses it to reject only the
// affected request instead of starting a swap from an invalid decision.
func (s *budgetSwapper) PlanningErrorFor(target string, running []string) error {
	return s.solve(target, running).Error
}

func (s *budgetSwapper) OnSwapStart(target string, running []string) {
	result := s.solve(target, running)
	// A target absent from the running set is a fresh load. Reset any stale
	// activity left behind by an autonomous process TTL stop; exact stop
	// notifications otherwise come from swaps and explicit unloads.
	if !slices.Contains(running, target) {
		s.OnModelStopped(target, time.Time{})
	}
	if result.Error != nil {
		s.logger.Errorf("budget: model=%s decision_error=%v evict=%v", target, result.Error, result.Evict)
		return
	}
	if len(result.Evict) == 0 {
		if len(running) == 0 {
			s.logger.Infof("budget: model=%s starting target=%dMiB budget=%dMiB (no models running)",
				target, result.TargetMiB, s.solver.effectiveMiB)
		} else {
			s.logger.Debugf("budget: model=%s fits used=%dMiB target=%dMiB budget=%dMiB",
				target, result.UsedMiB, result.TargetMiB, s.solver.effectiveMiB)
		}
		return
	}

	s.logger.Infof("budget: model=%s used=%dMiB target=%dMiB budget=%dMiB required_free=%dMiB evict=%v freed=%dMiB busy=%t activity=%s costs=%s policy=lru",
		target,
		result.UsedMiB,
		result.TargetMiB,
		s.solver.effectiveMiB,
		result.RequiredMiB,
		result.Evict,
		result.FreedMiB,
		result.IncludesBusy,
		s.formatActivities(result),
		s.formatCosts(result.Evict),
	)
}

func (s *budgetSwapper) OnModelReady(modelID string, at time.Time) {
	activity, exists := s.activity[modelID]
	if exists && !activity.readyAt.IsZero() {
		return
	}
	activity.readyAt = at
	s.activity[modelID] = activity
	s.generation++
}

func (s *budgetSwapper) OnServeStart(modelID string, at time.Time) {
	activity := s.activity[modelID]
	if activity.readyAt.IsZero() {
		activity.readyAt = at
	}
	activity.active++
	activity.lastServeAt = at
	s.activity[modelID] = activity
	s.generation++
}

func (s *budgetSwapper) OnServeDone(modelID string, at time.Time) {
	activity, exists := s.activity[modelID]
	if !exists || activity.active <= 0 {
		return
	}
	activity.active--
	if activity.active == 0 {
		activity.lastDoneAt = at
	}
	s.activity[modelID] = activity
	s.generation++
}

func (s *budgetSwapper) OnModelStopped(modelID string, _ time.Time) {
	if _, exists := s.activity[modelID]; !exists {
		return
	}
	delete(s.activity, modelID)
	s.generation++
}

func (s *budgetSwapper) formatActivities(result budgetSolveResult) string {
	now := time.Now()
	values := make([]string, 0, len(result.Evict))
	for _, modelID := range result.Evict {
		at := result.Activities[modelID]
		idle := "unknown"
		if !at.IsZero() {
			idle = now.Sub(at).Round(time.Second).String()
		}
		values = append(values, modelID+":"+idle)
	}
	return "[" + strings.Join(values, " ") + "]"
}

func (s *budgetSwapper) formatCosts(modelIDs []string) string {
	sorted := slices.Clone(modelIDs)
	sort.Strings(sorted)
	values := make([]string, 0, len(sorted))
	for _, modelID := range sorted {
		values = append(values, modelID+":"+strconv.Itoa(s.solver.evictCosts[modelID]))
	}
	return "[" + strings.Join(values, " ") + "]"
}
