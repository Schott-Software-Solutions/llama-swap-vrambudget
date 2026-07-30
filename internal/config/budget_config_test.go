package config

import (
	"strings"
	"testing"
	"time"
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
        kv_cache:
          enabled: true
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
	kvCache := cfg.Routing.Router.Settings.Budget.KVCache
	if !kvCache.Enabled || !kvCache.SingleSlotOnly {
		t.Errorf("KVCache=%+v want enabled single-slot defaults", kvCache)
	}
	if kvCache.Directory != "/tmp/kvcache" {
		t.Errorf("KVCache.Directory=%q want /tmp/kvcache", kvCache.Directory)
	}
	if kvCache.SaveTimeout != 30*time.Second || kvCache.RestoreTimeout != 30*time.Second {
		t.Errorf("KVCache timeouts save=%s restore=%s want 30s", kvCache.SaveTimeout, kvCache.RestoreTimeout)
	}
}

func TestValidateBudget_KVCache(t *testing.T) {
	tests := []struct {
		name    string
		kvCache BudgetKVCacheConfig
		wantErr string
	}{
		{
			name: "programmatic defaults",
			kvCache: BudgetKVCacheConfig{
				Enabled:        true,
				SingleSlotOnly: true,
			},
		},
		{
			name: "negative save timeout",
			kvCache: BudgetKVCacheConfig{
				Enabled:        true,
				SaveTimeout:    -time.Second,
				SingleSlotOnly: true,
			},
			wantErr: "save_timeout",
		},
		{
			name: "negative restore timeout",
			kvCache: BudgetKVCacheConfig{
				Enabled:        true,
				RestoreTimeout: -time.Second,
				SingleSlotOnly: true,
			},
			wantErr: "restore_timeout",
		},
		{
			name:    "multiple slots unsupported",
			kvCache: BudgetKVCacheConfig{Enabled: true},
			wantErr: "single_slot_only",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			budget := &BudgetConfig{
				TotalMiB: 100,
				KVCache:  test.kvCache,
			}
			err := ValidateBudget(budget, budgetModels(map[string]any{"a": 10}))
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateBudget: %v", err)
				}
				if budget.KVCache.Directory != "/tmp/kvcache" ||
					budget.KVCache.SaveTimeout != 30*time.Second ||
					budget.KVCache.RestoreTimeout != 30*time.Second {
					t.Errorf("KVCache defaults=%+v", budget.KVCache)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ValidateBudget error=%v want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestConfig_RoutingBudgetKVCacheCustom(t *testing.T) {
	yaml := `
models:
  model:
    cmd: llama-server --port ${PORT}
    metadata:
      projected_total_mib: 40
routing:
  router:
    use: budget
    settings:
      budget:
        total_mib: 100
        kv_cache:
          enabled: true
          directory: /var/cache/llama-swap/kvcache
          save_timeout: 45s
          restore_timeout: 1m15s
          single_slot_only: true
`
	cfg, err := LoadConfigFromReader(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("LoadConfigFromReader: %v", err)
	}
	kvCache := cfg.Routing.Router.Settings.Budget.KVCache
	if kvCache.Directory != "/var/cache/llama-swap/kvcache" ||
		kvCache.SaveTimeout != 45*time.Second ||
		kvCache.RestoreTimeout != 75*time.Second ||
		!kvCache.Enabled ||
		!kvCache.SingleSlotOnly {
		t.Errorf("KVCache=%+v", kvCache)
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
