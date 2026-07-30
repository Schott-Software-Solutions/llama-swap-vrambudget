package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
)

func newTestBudget(t *testing.T, budgetMiB int, memoryMiB map[string]int, processes map[string]process.Process) *Budget {
	t.Helper()
	models := make(map[string]config.ModelConfig, len(memoryMiB))
	for modelID, sizeMiB := range memoryMiB {
		models[modelID] = config.ModelConfig{
			Metadata: map[string]any{"projected_total_mib": sizeMiB},
		}
	}
	settings := &config.BudgetConfig{TotalMiB: budgetMiB}
	if err := config.ValidateBudget(settings, models); err != nil {
		t.Fatalf("ValidateBudget: %v", err)
	}
	conf := config.Config{
		HealthCheckTimeout: 5,
		Models:             models,
		Routing: config.RoutingConfig{
			Router: config.RouterConfig{
				Use:      "budget",
				Settings: config.RouterSettings{Budget: settings},
			},
		},
	}
	logger := logmon.NewWriter(io.Discard)
	swapper := newBudgetSwapper(
		newBudgetSolver(settings.EffectiveMiB(), settings.ResolvedMemoryMiB(), settings.ResolvedEvictCosts()),
		logger,
	)
	base, err := newBaseRouter("budget", conf, processes, logger, swapper)
	if err != nil {
		t.Fatalf("newBaseRouter: %v", err)
	}
	base.testProcessed = make(chan struct{}, 64)
	router := &Budget{baseRouter: base}
	go base.run()
	t.Cleanup(func() {
		if !router.shuttingDown.Load() {
			_ = router.Shutdown(time.Second)
		}
	})
	return router
}

func TestBudget_TargetFitsWithoutEviction(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	go a.Run(0)
	b := newFakeProcess("b")
	b.autoReady = true
	router := newTestBudget(t, 100, map[string]int{"a": 40, "b": 60}, map[string]process.Process{"a": a, "b": b})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newRequest("b"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := a.stopCalls.Load(); got != 0 {
		t.Errorf("a.stopCalls=%d want 0", got)
	}
	if got := b.runCalls.Load(); got != 1 {
		t.Errorf("b.runCalls=%d want 1", got)
	}
}

func TestBudget_IdleVictimStoppedBeforeTargetStarts(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	go a.Run(0)
	b := newFakeProcess("b")
	b.autoReady = true
	router := newTestBudget(t, 100, map[string]int{"a": 60, "b": 60}, map[string]process.Process{"a": a, "b": b})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newRequest("b"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := a.stopCalls.Load(); got != 1 {
		t.Errorf("a.stopCalls=%d want 1", got)
	}
	if got := b.runCalls.Load(); got != 1 {
		t.Errorf("b.runCalls=%d want 1", got)
	}
}

func TestBudget_BusyVictimQueuesUntilServeDone(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	a.serveBlock = make(chan struct{})
	go a.Run(0)
	b := newFakeProcess("b")
	b.autoReady = true
	router := newTestBudget(t, 100, map[string]int{"a": 60, "b": 60}, map[string]process.Process{"a": a, "b": b})

	aDone := make(chan struct{})
	go func() {
		router.ServeHTTP(httptest.NewRecorder(), newRequest("a"))
		close(aDone)
	}()
	<-a.serveStarted

	bRecorder := httptest.NewRecorder()
	bDone := make(chan struct{})
	go func() {
		router.ServeHTTP(bRecorder, newRequest("b"))
		close(bDone)
	}()
	waitProcessed(t, router.testProcessed, 1)

	if got := b.runCalls.Load(); got != 0 {
		t.Fatalf("b.runCalls=%d want 0 while a serves", got)
	}
	if a.stoppedWhileServing.Load() {
		t.Fatal("a was stopped while serving")
	}

	close(a.serveBlock)
	select {
	case <-aDone:
	case <-time.After(time.Second):
		t.Fatal("a request did not finish")
	}
	select {
	case <-bDone:
	case <-time.After(time.Second):
		t.Fatal("b request did not resume after a finished")
	}

	if got := a.stopCalls.Load(); got != 1 {
		t.Errorf("a.stopCalls=%d want 1 after it became idle", got)
	}
	if a.stoppedWhileServing.Load() {
		t.Fatal("a was stopped while serving")
	}
	if got := b.runCalls.Load(); got != 1 {
		t.Errorf("b.runCalls=%d want 1", got)
	}
}

func TestBudget_ConcurrentRequestsShareSingleSwap(t *testing.T) {
	target := newFakeProcess("target")
	router := newTestBudget(t, 100, map[string]int{"target": 60}, map[string]process.Process{"target": target})

	const requestCount = 3
	done := make([]chan struct{}, requestCount)
	recorders := make([]*httptest.ResponseRecorder, requestCount)
	for i := range requestCount {
		done[i] = make(chan struct{})
		recorders[i] = httptest.NewRecorder()
		go func(i int) {
			router.ServeHTTP(recorders[i], newRequest("target"))
			close(done[i])
		}(i)
		waitProcessed(t, router.testProcessed, 1)
		if i == 0 {
			waitSignal(t, target.runStarted, "target start")
		}
	}

	if got := target.runCalls.Load(); got != 1 {
		t.Fatalf("target.runCalls=%d want one shared swap", got)
	}
	target.markReady()
	for i := range requestCount {
		waitSignal(t, done[i], "shared target request")
		if recorders[i].Code != http.StatusOK {
			t.Errorf("request %d status=%d body=%q", i, recorders[i].Code, recorders[i].Body.String())
		}
	}
	if got := target.serveCalls.Load(); got != requestCount {
		t.Errorf("target.serveCalls=%d want %d", got, requestCount)
	}
}

func TestBudget_CoexistingSwapTargetsStartInParallel(t *testing.T) {
	a := newFakeProcess("a")
	b := newFakeProcess("b")
	router := newTestBudget(t, 100, map[string]int{"a": 40, "b": 60}, map[string]process.Process{"a": a, "b": b})

	aDone := make(chan struct{})
	bDone := make(chan struct{})
	go func() {
		router.ServeHTTP(httptest.NewRecorder(), newRequest("a"))
		close(aDone)
	}()
	waitProcessed(t, router.testProcessed, 1)
	waitSignal(t, a.runStarted, "a start")

	go func() {
		router.ServeHTTP(httptest.NewRecorder(), newRequest("b"))
		close(bDone)
	}()
	waitProcessed(t, router.testProcessed, 1)
	waitSignal(t, b.runStarted, "b parallel start")

	a.markReady()
	b.markReady()
	waitSignal(t, aDone, "a request")
	waitSignal(t, bDone, "b request")

	if got := a.runCalls.Load(); got != 1 {
		t.Errorf("a.runCalls=%d want 1", got)
	}
	if got := b.runCalls.Load(); got != 1 {
		t.Errorf("b.runCalls=%d want 1", got)
	}
}

func TestBudget_ParallelSwapTargetsCannotOvercommit(t *testing.T) {
	a := newFakeProcess("a")
	b := newFakeProcess("b")
	router := newTestBudget(t, 100, map[string]int{"a": 60, "b": 60}, map[string]process.Process{"a": a, "b": b})

	aDone := make(chan struct{})
	bDone := make(chan struct{})
	go func() {
		router.ServeHTTP(httptest.NewRecorder(), newRequest("a"))
		close(aDone)
	}()
	waitProcessed(t, router.testProcessed, 1)
	waitSignal(t, a.runStarted, "a start")

	go func() {
		router.ServeHTTP(httptest.NewRecorder(), newRequest("b"))
		close(bDone)
	}()
	waitProcessed(t, router.testProcessed, 1)

	select {
	case <-b.runStarted:
		t.Fatal("b started in parallel and overcommitted the budget")
	default:
	}

	a.markReady()
	waitSignal(t, aDone, "a request")
	waitSignal(t, b.runStarted, "b deferred start")
	b.markReady()
	waitSignal(t, bDone, "b request")

	if got := a.stopCalls.Load(); got != 1 {
		t.Errorf("a.stopCalls=%d want 1 before b starts", got)
	}
}
