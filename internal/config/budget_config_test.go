package config

import (
	"strings"
	"testing"
)

func TestValidateBudget_Valid(t *testing.T) {
	budget := &BudgetConfig{
		TotalMiB:   100,
		ReserveMiB: 10,
		Eviction: BudgetEvictionConfig{
			EvictCosts: map[string]int{"a": 0, "b": 5},
		},
	}
	models := budgetModels(map[string]any{"a": 40, "b": int64(50)})

	if err := ValidateBudget(budget, models); err != nil {
		t.Fatalf("ValidateBudget: %v", err)
	}
	if budget.MemoryMetadataKey != "projected_total_mib" {
		t.Errorf("MemoryMetadataKey=%q want projected_total_mib", budget.MemoryMetadataKey)
	}
	if budget.Eviction.Policy != "lru" {
		t.Errorf("Policy=%q want lru", budget.Eviction.Policy)
	}
	if got := budget.ResolvedMemoryMiB(); got["a"] != 40 || got["b"] != 50 {
		t.Errorf("ResolvedMemoryMiB=%v want a=40 b=50", got)
	}
}

func TestValidateBudget_Invalid(t *testing.T) {
	tests := []struct {
		name    string
		budget  *BudgetConfig
		models  map[string]ModelConfig
		wantErr string
	}{
		{
			name:    "total non-positive",
			budget:  &BudgetConfig{},
			models:  budgetModels(map[string]any{"a": 1}),
			wantErr: "total_mib",
		},
		{
			name:    "negative reserve",
			budget:  &BudgetConfig{TotalMiB: 100, ReserveMiB: -1},
			models:  budgetModels(map[string]any{"a": 1}),
			wantErr: "reserve_mib",
		},
		{
			name:    "reserve consumes total",
			budget:  &BudgetConfig{TotalMiB: 100, ReserveMiB: 100},
			models:  budgetModels(map[string]any{"a": 1}),
			wantErr: "less than total_mib",
		},
		{
			name:    "missing metadata",
			budget:  &BudgetConfig{TotalMiB: 100},
			models:  map[string]ModelConfig{"a": {}},
			wantErr: "metadata.projected_total_mib is required",
		},
		{
			name:    "non-integer metadata",
			budget:  &BudgetConfig{TotalMiB: 100},
			models:  budgetModels(map[string]any{"a": 1.5}),
			wantErr: "positive integer",
		},
		{
			name:    "zero metadata",
			budget:  &BudgetConfig{TotalMiB: 100},
			models:  budgetModels(map[string]any{"a": 0}),
			wantErr: "positive integer",
		},
		{
			name:    "model exceeds effective budget",
			budget:  &BudgetConfig{TotalMiB: 100, ReserveMiB: 10},
			models:  budgetModels(map[string]any{"a": 91}),
			wantErr: "larger than effective budget",
		},
		{
			name: "negative eviction cost",
			budget: &BudgetConfig{
				TotalMiB: 100,
				Eviction: BudgetEvictionConfig{EvictCosts: map[string]int{"a": -1}},
			},
			models:  budgetModels(map[string]any{"a": 10}),
			wantErr: "must be non-negative",
		},
		{
			name: "unknown policy",
			budget: &BudgetConfig{
				TotalMiB: 100,
				Eviction: BudgetEvictionConfig{Policy: "fifo"},
			},
			models:  budgetModels(map[string]any{"a": 10}),
			wantErr: "unknown policy",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateBudget(test.budget, test.models)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ValidateBudget error=%v want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestConfig_RoutingBudget(t *testing.T) {
	yaml := `
models:
  a:
    cmd: run-a --port ${PORT}
    metadata:
      memory: 40
  b:
    cmd: run-b --port ${PORT}
    metadata:
      memory: 50
routing:
  router:
    use: budget
    settings:
      budget:
        total_mib: 100
        reserve_mib: 10
        memory_metadata_key: memory
        eviction:
          policy: lru
          evict_costs:
            b: 3
`
	cfg, err := LoadConfigFromReader(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("LoadConfigFromReader: %v", err)
	}
	if cfg.Routing.Router.Use != "budget" {
		t.Errorf("router use=%q want budget", cfg.Routing.Router.Use)
	}
	if cfg.Routing.Router.Settings.Budget == nil {
		t.Fatal("budget settings are nil")
	}
	if got := cfg.Routing.Router.Settings.Budget.ResolvedMemoryMiB(); got["a"] != 40 || got["b"] != 50 {
		t.Errorf("resolved memory=%v want a=40 b=50", got)
	}
	if len(cfg.Groups) != 0 || cfg.Matrix != nil {
		t.Errorf("budget config unexpectedly normalized groups=%v matrix=%v", cfg.Groups, cfg.Matrix)
	}
}

func budgetModels(values map[string]any) map[string]ModelConfig {
	models := make(map[string]ModelConfig, len(values))
	for modelID, value := range values {
		models[modelID] = ModelConfig{
			Metadata: map[string]any{"projected_total_mib": value},
		}
	}
	return models
}
