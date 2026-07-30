package router

import (
	"fmt"
	"slices"
	"sort"
	"time"
)

const exhaustiveBudgetCandidateLimit = 20

type budgetSolver struct {
	effectiveMiB int
	memoryMiB    map[string]int
	evictCosts   map[string]int
}

func newBudgetSolver(effectiveMiB int, memoryMiB, evictCosts map[string]int) *budgetSolver {
	return &budgetSolver{
		effectiveMiB: effectiveMiB,
		memoryMiB:    memoryMiB,
		evictCosts:   evictCosts,
	}
}

type budgetSolveResult struct {
	Evict        []string
	UsedMiB      int
	TargetMiB    int
	RequiredMiB  int
	FreedMiB     int
	IncludesBusy bool
	Activities   map[string]time.Time
	Error        error
}

type budgetCandidate struct {
	id        string
	memoryMiB int
	evictCost int
	busy      bool
	at        time.Time
}

type evictionPlan struct {
	victims        []string
	includesBusy   bool
	idleOrder      []time.Time
	totalEvictCost int
	freedMiB       int
	excessMiB      int
}

func (s *budgetSolver) Solve(target string, running []string, activity map[string]modelActivity) budgetSolveResult {
	targetMiB, ok := s.memoryMiB[target]
	if !ok {
		return s.conservativeFailure(target, running, 0, fmt.Errorf("no memory estimate for target %q", target))
	}

	seen := make(map[string]struct{}, len(running))
	unique := make([]string, 0, len(running))
	usedMiB := 0
	targetRunning := false
	for _, modelID := range running {
		if _, duplicate := seen[modelID]; duplicate {
			continue
		}
		seen[modelID] = struct{}{}
		memoryMiB, exists := s.memoryMiB[modelID]
		if !exists {
			return s.conservativeFailure(target, running, usedMiB, fmt.Errorf("no memory estimate for running model %q", modelID))
		}
		unique = append(unique, modelID)
		usedMiB += memoryMiB
		if modelID == target {
			targetRunning = true
		}
	}

	additionalMiB := targetMiB
	if targetRunning {
		additionalMiB = 0
	}
	requiredMiB := usedMiB + additionalMiB - s.effectiveMiB
	result := budgetSolveResult{
		UsedMiB:     usedMiB,
		TargetMiB:   targetMiB,
		RequiredMiB: requiredMiB,
	}
	if requiredMiB <= 0 {
		return result
	}

	candidates := make([]budgetCandidate, 0, len(unique))
	for _, modelID := range unique {
		if modelID == target {
			continue
		}
		modelActivity := activity[modelID]
		at := modelActivity.idleSince()
		if modelActivity.active > 0 {
			at = modelActivity.activityAt()
		}
		candidates = append(candidates, budgetCandidate{
			id:        modelID,
			memoryMiB: s.memoryMiB[modelID],
			evictCost: s.evictCosts[modelID],
			busy:      modelActivity.active > 0,
			at:        at,
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].id < candidates[j].id
	})

	var plan *evictionPlan
	if len(candidates) <= exhaustiveBudgetCandidateLimit {
		plan = solveBudgetExhaustive(candidates, requiredMiB)
	} else {
		// Above 20 candidates, avoid exponential work. The deterministic
		// fallback retains the two primary rules (idle-only when possible and
		// minimum victim count) by taking the largest candidates first. LRU,
		// cost, and model ID break equal-size ties.
		plan = solveBudgetGreedy(candidates, requiredMiB)
	}
	if plan == nil {
		result.Error = fmt.Errorf("running models can free only %d MiB, need %d MiB for target %q", candidateMemory(candidates), requiredMiB, target)
		result.Evict = candidateIDs(candidates)
		result.FreedMiB = candidateMemory(candidates)
		result.IncludesBusy = hasBusy(candidates)
		return result
	}

	result.Evict = plan.victims
	result.FreedMiB = plan.freedMiB
	result.IncludesBusy = plan.includesBusy
	result.Activities = make(map[string]time.Time, len(plan.victims))
	for _, candidate := range candidates {
		if slices.Contains(plan.victims, candidate.id) {
			result.Activities[candidate.id] = candidate.at
		}
	}
	return result
}

func (s *budgetSolver) conservativeFailure(target string, running []string, usedMiB int, err error) budgetSolveResult {
	seen := make(map[string]struct{}, len(running))
	evict := make([]string, 0, len(running))
	for _, modelID := range running {
		if modelID == target {
			continue
		}
		if _, duplicate := seen[modelID]; duplicate {
			continue
		}
		seen[modelID] = struct{}{}
		evict = append(evict, modelID)
	}
	sort.Strings(evict)
	return budgetSolveResult{Evict: evict, UsedMiB: usedMiB, Error: err}
}

func solveBudgetExhaustive(candidates []budgetCandidate, requiredMiB int) *evictionPlan {
	var best *evictionPlan
	subsetCount := 1 << len(candidates)
	for mask := 1; mask < subsetCount; mask++ {
		plan := evictionPlan{}
		for i, candidate := range candidates {
			if mask&(1<<i) == 0 {
				continue
			}
			plan.victims = append(plan.victims, candidate.id)
			plan.freedMiB += candidate.memoryMiB
			plan.totalEvictCost += candidate.evictCost
			plan.includesBusy = plan.includesBusy || candidate.busy
			plan.idleOrder = append(plan.idleOrder, candidate.at)
		}
		if plan.freedMiB < requiredMiB {
			continue
		}
		plan.excessMiB = plan.freedMiB - requiredMiB
		sort.Slice(plan.idleOrder, func(i, j int) bool {
			return plan.idleOrder[i].Before(plan.idleOrder[j])
		})
		if best == nil || betterBudgetPlan(&plan, best) {
			copy := plan
			best = &copy
		}
	}
	return best
}

func solveBudgetGreedy(candidates []budgetCandidate, requiredMiB int) *evictionPlan {
	idleMemoryMiB := 0
	for _, candidate := range candidates {
		if !candidate.busy {
			idleMemoryMiB += candidate.memoryMiB
		}
	}

	allowBusy := idleMemoryMiB < requiredMiB
	ordered := make([]budgetCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if !candidate.busy || allowBusy {
			ordered = append(ordered, candidate)
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].memoryMiB != ordered[j].memoryMiB {
			return ordered[i].memoryMiB > ordered[j].memoryMiB
		}
		if !ordered[i].at.Equal(ordered[j].at) {
			return ordered[i].at.Before(ordered[j].at)
		}
		if ordered[i].evictCost != ordered[j].evictCost {
			return ordered[i].evictCost < ordered[j].evictCost
		}
		return ordered[i].id < ordered[j].id
	})

	plan := &evictionPlan{}
	for _, candidate := range ordered {
		plan.victims = append(plan.victims, candidate.id)
		plan.freedMiB += candidate.memoryMiB
		plan.totalEvictCost += candidate.evictCost
		plan.includesBusy = plan.includesBusy || candidate.busy
		plan.idleOrder = append(plan.idleOrder, candidate.at)
		if plan.freedMiB >= requiredMiB {
			break
		}
	}
	if plan.freedMiB < requiredMiB {
		return nil
	}
	sort.Strings(plan.victims)
	sort.Slice(plan.idleOrder, func(i, j int) bool {
		return plan.idleOrder[i].Before(plan.idleOrder[j])
	})
	plan.excessMiB = plan.freedMiB - requiredMiB
	return plan
}

func betterBudgetPlan(candidate, current *evictionPlan) bool {
	// Plans are ranked lexicographically:
	//   1. Never choose a busy victim when an idle-only plan can make progress.
	//   2. Evict the fewest models, preventing an old but unnecessary model
	//      from making a larger plan win.
	//   3. Compare the sorted activity times oldest-first. With equal victim
	//      counts this preserves LRU as the dominant choice among minimal plans.
	//   4. Prefer lower configured eviction cost.
	//   5. Prefer less excess memory freed.
	//   6. Compare sorted model IDs for a deterministic final tie-break.
	if candidate.includesBusy != current.includesBusy {
		return !candidate.includesBusy
	}
	if len(candidate.victims) != len(current.victims) {
		return len(candidate.victims) < len(current.victims)
	}
	for i := 0; i < min(len(candidate.idleOrder), len(current.idleOrder)); i++ {
		if candidate.idleOrder[i].Equal(current.idleOrder[i]) {
			continue
		}
		return candidate.idleOrder[i].Before(current.idleOrder[i])
	}
	if candidate.totalEvictCost != current.totalEvictCost {
		return candidate.totalEvictCost < current.totalEvictCost
	}
	if candidate.excessMiB != current.excessMiB {
		return candidate.excessMiB < current.excessMiB
	}
	return slices.Compare(candidate.victims, current.victims) < 0
}

func candidateMemory(candidates []budgetCandidate) int {
	total := 0
	for _, candidate := range candidates {
		total += candidate.memoryMiB
	}
	return total
}

func candidateIDs(candidates []budgetCandidate) []string {
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		result = append(result, candidate.id)
	}
	return result
}

func hasBusy(candidates []budgetCandidate) bool {
	for _, candidate := range candidates {
		if candidate.busy {
			return true
		}
	}
	return false
}
