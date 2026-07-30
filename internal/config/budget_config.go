package config

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
)

const defaultBudgetMemoryMetadataKey = "projected_total_mib"

// BudgetConfig configures the memory-budget router.
type BudgetConfig struct {
	TotalMiB          int                  `yaml:"total_mib"`
	ReserveMiB        int                  `yaml:"reserve_mib"`
	MemoryMetadataKey string               `yaml:"memory_metadata_key"`
	Eviction          BudgetEvictionConfig `yaml:"eviction"`

	memoryMiB map[string]int
}

// BudgetEvictionConfig configures victim selection for the budget router.
type BudgetEvictionConfig struct {
	Policy     string         `yaml:"policy"`
	EvictCosts map[string]int `yaml:"evict_costs"`
}

// ValidateBudget validates settings and resolves every managed model's static
// memory estimate from metadata.
func ValidateBudget(budget *BudgetConfig, models map[string]ModelConfig) error {
	if budget == nil {
		return fmt.Errorf("budget configuration is required")
	}
	budget.memoryMiB = nil
	if budget.TotalMiB <= 0 {
		return fmt.Errorf("total_mib must be a positive integer, got %d", budget.TotalMiB)
	}
	if budget.ReserveMiB < 0 {
		return fmt.Errorf("reserve_mib must be non-negative, got %d", budget.ReserveMiB)
	}
	if budget.ReserveMiB >= budget.TotalMiB {
		return fmt.Errorf("reserve_mib (%d) must be less than total_mib (%d)", budget.ReserveMiB, budget.TotalMiB)
	}
	if budget.MemoryMetadataKey == "" {
		budget.MemoryMetadataKey = defaultBudgetMemoryMetadataKey
	}
	if budget.Eviction.Policy == "" {
		budget.Eviction.Policy = "lru"
	}
	if budget.Eviction.Policy != "lru" {
		return fmt.Errorf("eviction.policy: unknown policy %q (valid: lru)", budget.Eviction.Policy)
	}

	costModelIDs := make([]string, 0, len(budget.Eviction.EvictCosts))
	for modelID := range budget.Eviction.EvictCosts {
		costModelIDs = append(costModelIDs, modelID)
	}
	sort.Strings(costModelIDs)
	for _, modelID := range costModelIDs {
		cost := budget.Eviction.EvictCosts[modelID]
		if cost < 0 {
			return fmt.Errorf("eviction.evict_costs[%q] must be non-negative, got %d", modelID, cost)
		}
		if _, ok := models[modelID]; !ok {
			return fmt.Errorf("eviction.evict_costs references unknown model %q", modelID)
		}
	}

	effective := budget.TotalMiB - budget.ReserveMiB
	resolved := make(map[string]int, len(models))
	modelIDs := make([]string, 0, len(models))
	for modelID := range models {
		modelIDs = append(modelIDs, modelID)
	}
	sort.Strings(modelIDs)
	for _, modelID := range modelIDs {
		model := models[modelID]
		raw, ok := model.Metadata[budget.MemoryMetadataKey]
		if !ok {
			return fmt.Errorf("model %q metadata.%s is required", modelID, budget.MemoryMetadataKey)
		}
		memoryMiB, ok := positiveInt(raw)
		if !ok {
			return fmt.Errorf("model %q metadata.%s must be a positive integer, got %v", modelID, budget.MemoryMetadataKey, raw)
		}
		if memoryMiB > effective {
			return fmt.Errorf("model %q requires %d MiB, larger than effective budget %d MiB", modelID, memoryMiB, effective)
		}
		resolved[modelID] = memoryMiB
	}
	budget.memoryMiB = resolved
	return nil
}

func positiveInt(value any) (int, bool) {
	var n int64
	switch value := value.(type) {
	case int:
		if value <= 0 {
			return 0, false
		}
		return value, true
	case int8:
		n = int64(value)
	case int16:
		n = int64(value)
	case int32:
		n = int64(value)
	case int64:
		n = value
	case uint:
		if uint64(value) > uint64(math.MaxInt) {
			return 0, false
		}
		return int(value), value > 0
	case uint8:
		return int(value), value > 0
	case uint16:
		return int(value), value > 0
	case uint32:
		if uint64(value) > uint64(math.MaxInt) {
			return 0, false
		}
		return int(value), value > 0
	case uint64:
		if value > uint64(math.MaxInt) {
			return 0, false
		}
		return int(value), value > 0
	case json.Number:
		parsed, err := strconv.ParseInt(string(value), 10, 64)
		if err != nil {
			return 0, false
		}
		n = parsed
	default:
		return 0, false
	}
	if n <= 0 || uint64(n) > uint64(math.MaxInt) {
		return 0, false
	}
	return int(n), true
}

// EffectiveMiB returns the memory available to managed models.
func (b *BudgetConfig) EffectiveMiB() int {
	return b.TotalMiB - b.ReserveMiB
}

// ResolvedMemoryMiB returns a copy of the validated per-model estimates.
func (b *BudgetConfig) ResolvedMemoryMiB() map[string]int {
	result := make(map[string]int, len(b.memoryMiB))
	for modelID, memoryMiB := range b.memoryMiB {
		result[modelID] = memoryMiB
	}
	return result
}

// ResolvedEvictCosts returns configured costs. Unlisted models default to zero.
func (b *BudgetConfig) ResolvedEvictCosts() map[string]int {
	result := make(map[string]int, len(b.Eviction.EvictCosts))
	for modelID, cost := range b.Eviction.EvictCosts {
		result[modelID] = cost
	}
	return result
}
