package router

import (
	"fmt"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// Budget routes models according to a static memory budget and LRU eviction.
type Budget struct {
	*baseRouter
}

func NewBudget(conf config.Config, proxylog, upstreamlog *logmon.Monitor) (*Budget, error) {
	settings := conf.Routing.Router.Settings.Budget
	if settings == nil {
		return nil, fmt.Errorf("budget router requires a budget configuration")
	}
	if err := config.ValidateBudget(settings, conf.Models); err != nil {
		return nil, fmt.Errorf("validating budget configuration: %w", err)
	}

	solver := newBudgetSolver(settings.EffectiveMiB(), settings.ResolvedMemoryMiB(), settings.ResolvedEvictCosts())
	swapper := newBudgetSwapper(solver, proxylog)

	processes := make(map[string]process.Process, len(conf.Models))
	base, err := newBaseRouter("budget", conf, processes, proxylog, swapper)
	if err != nil {
		return nil, fmt.Errorf("creating base router: %w", err)
	}
	if settings.KVCache.Enabled {
		base.lifecycle = newKVCacheLifecycle(settings.KVCache, conf.Models, proxylog)
	}

	for modelID, modelCfg := range conf.Models {
		procLog := logmon.NewWriter(upstreamlog)
		p, err := process.New(base.procCtx, modelID, modelCfg, procLog, proxylog,
			process.WithIdleStopHandler(base.idleStopHandler(modelID)))
		if err != nil {
			base.shutdownFn()
			base.procCancel()
			return nil, fmt.Errorf("creating process for %q: %w", modelID, err)
		}
		processes[modelID] = p
	}

	router := &Budget{baseRouter: base}
	go base.run()
	return router, nil
}
