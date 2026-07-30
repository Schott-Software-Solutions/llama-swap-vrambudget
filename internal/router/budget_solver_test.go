package router

import (
	"slices"
	"testing"
	"time"
)

func TestBudgetSolver_EvictionPlans(t *testing.T) {
	base := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		budget    int
		memory    map[string]int
		costs     map[string]int
		running   []string
		target    string
		activity  map[string]modelActivity
		wantEvict []string
		wantBusy  bool
	}{
		{
			name:      "target already running and satisfied",
			budget:    100,
			memory:    map[string]int{"a": 60},
			running:   []string{"a"},
			target:    "a",
			wantEvict: nil,
		},
		{
			name:      "target not running and satisfied",
			budget:    100,
			memory:    map[string]int{"a": 40, "b": 60},
			running:   []string{"a"},
			target:    "b",
			wantEvict: nil,
		},
		{
			name:      "one idle victim sufficient",
			budget:    100,
			memory:    map[string]int{"a": 50, "b": 50, "target": 40},
			running:   []string{"a", "b"},
			target:    "target",
			activity:  activities(base, "a", -2*time.Hour, "b", -time.Hour),
			wantEvict: []string{"a"},
		},
		{
			name:      "multiple idle victims required",
			budget:    100,
			memory:    map[string]int{"a": 30, "b": 30, "target": 80},
			running:   []string{"a", "b"},
			target:    "target",
			activity:  activities(base, "a", -2*time.Hour, "b", -time.Hour),
			wantEvict: []string{"a", "b"},
		},
		{
			name:      "longest idle wins",
			budget:    130,
			memory:    map[string]int{"a": 60, "b": 60, "target": 50},
			running:   []string{"a", "b"},
			target:    "target",
			activity:  activities(base, "a", -2*time.Hour, "b", -time.Hour),
			wantEvict: []string{"a"},
		},
		{
			name:      "one sufficient victim wins over multiple",
			budget:    80,
			memory:    map[string]int{"a": 50, "b": 10, "target": 60},
			running:   []string{"a", "b"},
			target:    "target",
			activity:  activities(base, "a", -2*time.Hour, "b", -10*time.Hour),
			wantEvict: []string{"a"},
		},
		{
			name:      "unnecessarily old extra victim cannot enlarge plan",
			budget:    120,
			memory:    map[string]int{"a": 50, "b": 10, "c": 50, "target": 60},
			running:   []string{"a", "b", "c"},
			target:    "target",
			activity:  activities(base, "a", -2*time.Hour, "b", -10*time.Hour, "c", -time.Hour),
			wantEvict: []string{"a"},
		},
		{
			name:    "idle-only wins over busy",
			budget:  130,
			memory:  map[string]int{"idle": 50, "busy": 80, "target": 40},
			running: []string{"busy", "idle"},
			target:  "target",
			activity: map[string]modelActivity{
				"idle": {readyAt: base.Add(-time.Hour)},
				"busy": {readyAt: base.Add(-2 * time.Hour), active: 1},
			},
			wantEvict: []string{"idle"},
		},
		{
			name:    "busy selected when idle insufficient",
			budget:  110,
			memory:  map[string]int{"idle": 20, "busy": 80, "target": 60},
			running: []string{"busy", "idle"},
			target:  "target",
			activity: map[string]modelActivity{
				"idle": {readyAt: base},
				"busy": {readyAt: base, active: 1},
			},
			wantEvict: []string{"busy"},
			wantBusy:  true,
		},
		{
			name:      "lower cost breaks LRU tie",
			budget:    130,
			memory:    map[string]int{"a": 60, "b": 60, "target": 50},
			costs:     map[string]int{"a": 10, "b": 1},
			running:   []string{"a", "b"},
			target:    "target",
			activity:  activities(base, "a", 0, "b", 0),
			wantEvict: []string{"b"},
		},
		{
			name:      "fewer victims break cost tie",
			budget:    150,
			memory:    map[string]int{"a": 30, "b": 30, "c": 60, "target": 80},
			running:   []string{"a", "b", "c"},
			target:    "target",
			activity:  activities(base, "a", 0, "b", 0, "c", 0),
			wantEvict: []string{"c"},
		},
		{
			name:      "lower excess breaks remaining tie",
			budget:    130,
			memory:    map[string]int{"a": 50, "b": 60, "target": 50},
			running:   []string{"a", "b"},
			target:    "target",
			activity:  activities(base, "a", 0, "b", 0),
			wantEvict: []string{"a"},
		},
		{
			name:      "model id is deterministic final tie-break",
			budget:    130,
			memory:    map[string]int{"a": 60, "b": 60, "target": 50},
			running:   []string{"b", "a"},
			target:    "target",
			activity:  activities(base, "a", 0, "b", 0),
			wantEvict: []string{"a"},
		},
		{
			name:      "target is never evicted",
			budget:    100,
			memory:    map[string]int{"a": 60, "target": 60},
			running:   []string{"target", "a"},
			target:    "target",
			activity:  activities(base, "a", -time.Hour, "target", -2*time.Hour),
			wantEvict: []string{"a"},
		},
		{
			name:      "duplicate running ids counted once",
			budget:    100,
			memory:    map[string]int{"a": 50, "target": 50},
			running:   []string{"a", "a"},
			target:    "target",
			wantEvict: nil,
		},
		{
			name:      "exact budget boundary",
			budget:    100,
			memory:    map[string]int{"a": 40, "target": 60},
			running:   []string{"a"},
			target:    "target",
			wantEvict: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			solver := newBudgetSolver(test.budget, test.memory, test.costs)
			result := solver.Solve(test.target, test.running, test.activity)
			if result.Error != nil {
				t.Fatalf("Solve error: %v", result.Error)
			}
			if !slices.Equal(result.Evict, test.wantEvict) {
				t.Errorf("Evict=%v want %v", result.Evict, test.wantEvict)
			}
			if result.IncludesBusy != test.wantBusy {
				t.Errorf("IncludesBusy=%t want %t", result.IncludesBusy, test.wantBusy)
			}
		})
	}
}

func TestBudgetSolver_UnknownRunningMemoryFailsWithoutEviction(t *testing.T) {
	solver := newBudgetSolver(100, map[string]int{"target": 50, "known": 40}, nil)
	result := solver.Solve("target", []string{"unknown", "known"}, nil)
	if result.Error == nil {
		t.Fatal("Solve error=nil want unknown-memory error")
	}
	if len(result.Evict) != 0 {
		t.Errorf("Evict=%v want no destructive fallback", result.Evict)
	}
}

func TestBudgetSolver_MissingTargetMemoryFailsWithoutEviction(t *testing.T) {
	solver := newBudgetSolver(100, map[string]int{"known": 40}, nil)
	result := solver.Solve("target", []string{"known"}, nil)
	if result.Error == nil {
		t.Fatal("Solve error=nil want missing-target-memory error")
	}
	if len(result.Evict) != 0 {
		t.Errorf("Evict=%v want no destructive fallback", result.Evict)
	}
}

func TestBudgetSolver_GreedyFallbackMinimizesVictims(t *testing.T) {
	memory := map[string]int{"large": 50, "target": 60}
	running := []string{"large"}
	activity := map[string]modelActivity{
		"large": {readyAt: time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)},
	}
	for i := range exhaustiveBudgetCandidateLimit {
		modelID := string(rune('a' + i))
		memory[modelID] = 2
		running = append(running, modelID)
		activity[modelID] = modelActivity{
			readyAt: time.Date(2026, 7, 29, i, 0, 0, 0, time.UTC),
		}
	}

	result := newBudgetSolver(110, memory, nil).Solve("target", running, activity)
	if result.Error != nil {
		t.Fatalf("Solve error: %v", result.Error)
	}
	if !slices.Equal(result.Evict, []string{"large"}) {
		t.Errorf("Evict=%v want [large]", result.Evict)
	}
}

func BenchmarkBudgetSolver_Subsets(b *testing.B) {
	memory := make(map[string]int, 12)
	running := make([]string, 0, 12)
	activity := make(map[string]modelActivity, 12)
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	for i := range 12 {
		id := string(rune('a' + i))
		memory[id] = 10
		running = append(running, id)
		activity[id] = modelActivity{readyAt: now.Add(-time.Duration(i) * time.Minute)}
	}
	memory["target"] = 60
	solver := newBudgetSolver(120, memory, nil)

	b.ResetTimer()
	for range b.N {
		_ = solver.Solve("target", running, activity)
	}
}

func activities(base time.Time, values ...any) map[string]modelActivity {
	result := make(map[string]modelActivity, len(values)/2)
	for i := 0; i < len(values); i += 2 {
		modelID := values[i].(string)
		offset, ok := values[i+1].(time.Duration)
		if !ok {
			offset = time.Duration(values[i+1].(int))
		}
		result[modelID] = modelActivity{readyAt: base.Add(offset)}
	}
	return result
}
