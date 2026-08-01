package router

import (
	"context"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
)

type blockingModelLifecycle struct {
	beforeCalls   atomic.Int32
	beforeStarted chan struct{}
	release       chan struct{}
	completed     atomic.Bool
	startOnce     sync.Once
}

func newBlockingModelLifecycle() *blockingModelLifecycle {
	return &blockingModelLifecycle{
		beforeStarted: make(chan struct{}),
		release:       make(chan struct{}),
	}
}

func (l *blockingModelLifecycle) BeforeModelStop(ctx context.Context, _ string) {
	l.beforeCalls.Add(1)
	l.startOnce.Do(func() { close(l.beforeStarted) })
	select {
	case <-l.release:
	case <-ctx.Done():
	}
	l.completed.Store(true)
}

func (*blockingModelLifecycle) AfterModelStart(context.Context, string) {}

func waitForPlannedStop(t *testing.T, b *baseRouter, modelID string) {
	t.Helper()
	state := b.modelStopState(modelID)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state.mu.Lock()
		stopping := state.stop != nil
		state.mu.Unlock()
		if stopping {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("planned stop for %s did not begin", modelID)
}

func TestModelStop_ManualUnloadDrainsBeforeLifecycleAndStop(t *testing.T) {
	p := newFakeProcess("model")
	p.markReady()
	p.serveBlock = make(chan struct{})
	lifecycle := newBlockingModelLifecycle()
	p.onStop = func(string) {
		if !lifecycle.completed.Load() {
			t.Error("Process.Stop ran before the lifecycle hook completed")
		}
	}

	b := newTestBase(t, map[string]process.Process{"model": p}, &stubPlanner{})
	b.lifecycle = lifecycle

	requestDone := make(chan struct{})
	go func() {
		b.ServeHTTP(httptest.NewRecorder(), newRequest("model"))
		close(requestDone)
	}()
	waitSignal(t, p.serveStarted, "first request")

	unloadDone := make(chan struct{})
	go func() {
		b.Unload(time.Second, "model")
		close(unloadDone)
	}()
	waitForPlannedStop(t, b, "model")

	select {
	case <-lifecycle.beforeStarted:
		t.Fatal("lifecycle started before the active request drained")
	default:
	}
	if got := p.stopCalls.Load(); got != 0 {
		t.Fatalf("Stop calls=%d before request drain", got)
	}

	// The run loop is occupied by the unload and the stop fence is closed.
	// A new request must be cancellable without ever reaching the process.
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	secondDone := make(chan struct{})
	go func() {
		b.ServeHTTP(httptest.NewRecorder(), newRequestCtx(requestCtx, "model"))
		close(secondDone)
	}()
	cancelRequest()
	waitSignal(t, secondDone, "request arriving during stop")
	if got := p.serveCalls.Load(); got != 1 {
		t.Fatalf("serve calls=%d want 1; new request reached stopping model", got)
	}

	close(p.serveBlock)
	waitSignal(t, lifecycle.beforeStarted, "lifecycle after drain")
	if got := p.inFlightServe.Load(); got != 0 {
		t.Fatalf("in-flight process requests=%d when lifecycle started", got)
	}
	if got := p.stopCalls.Load(); got != 0 {
		t.Fatalf("Stop calls=%d while lifecycle save is blocked", got)
	}

	close(lifecycle.release)
	waitSignal(t, unloadDone, "manual unload")
	waitSignal(t, requestDone, "drained request")
	if got := lifecycle.beforeCalls.Load(); got != 1 {
		t.Errorf("lifecycle calls=%d want 1", got)
	}
	if got := p.stopCalls.Load(); got != 1 {
		t.Errorf("Stop calls=%d want 1", got)
	}
	if p.stoppedWhileServing.Load() {
		t.Error("process stopped while a request was still serving")
	}
}

func TestModelStop_ConcurrentStopsShareOneLifecycleSave(t *testing.T) {
	p := newFakeProcess("model")
	p.markReady()
	lifecycle := newBlockingModelLifecycle()
	b := newTestBase(t, map[string]process.Process{"model": p}, &stubPlanner{})
	b.lifecycle = lifecycle

	var stops sync.WaitGroup
	for range 2 {
		stops.Add(1)
		go func() {
			defer stops.Done()
			b.stopProcesses(context.Background(), time.Second, []string{"model"})
		}()
	}
	waitSignal(t, lifecycle.beforeStarted, "shared lifecycle")
	close(lifecycle.release)
	stops.Wait()

	if got := lifecycle.beforeCalls.Load(); got != 1 {
		t.Errorf("lifecycle calls=%d want 1", got)
	}
	if got := p.stopCalls.Load(); got != 1 {
		t.Errorf("Stop calls=%d want 1", got)
	}
}

func TestModelStop_RejectsNewGrantDuringLifecycleSave(t *testing.T) {
	p := newFakeProcess("model")
	p.markReady()
	lifecycle := newBlockingModelLifecycle()
	b := newTestBase(t, map[string]process.Process{"model": p}, &stubPlanner{})
	b.lifecycle = lifecycle

	stopDone := make(chan struct{})
	go func() {
		b.stopProcesses(context.Background(), time.Second, []string{"model"})
		close(stopDone)
	}()
	waitSignal(t, lifecycle.beforeStarted, "lifecycle save")

	req := scheduler.HandlerReq{
		Ctx:     context.Background(),
		Respond: make(chan scheduler.HandlerResp, 1),
	}
	if granted := b.GrantServe(req, "model"); granted {
		t.Fatal("request was granted while lifecycle save was running")
	}
	if response := <-req.Respond; response.Err == nil {
		t.Fatal("request rejected during lifecycle save without an error")
	}
	if got := p.serveCalls.Load(); got != 0 {
		t.Errorf("serve calls=%d want 0", got)
	}

	close(lifecycle.release)
	waitSignal(t, stopDone, "planned stop")
}

func TestModelStop_TTLAndGlobalTTLUsePlannedLifecycle(t *testing.T) {
	p := newFakeProcess("model")
	p.markReady()
	lifecycle := newBlockingModelLifecycle()
	close(lifecycle.release)
	b := newTestBase(t, map[string]process.Process{"model": p}, &stubPlanner{})
	b.lifecycle = lifecycle

	b.idleStopHandler("model")()

	if got := lifecycle.beforeCalls.Load(); got != 1 {
		t.Errorf("lifecycle calls=%d want 1", got)
	}
	if got := p.stopCalls.Load(); got != 1 {
		t.Errorf("Stop calls=%d want 1", got)
	}
}

func TestModelStop_ShutdownTimeoutBoundsLifecycleSave(t *testing.T) {
	p := newFakeProcess("model")
	p.markReady()
	lifecycle := newBlockingModelLifecycle()
	b := newTestBase(t, map[string]process.Process{"model": p}, &stubPlanner{})
	b.lifecycle = lifecycle

	started := time.Now()
	if err := b.Shutdown(30 * time.Millisecond); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded shutdown took %v", elapsed)
	}
	if got := lifecycle.beforeCalls.Load(); got != 1 {
		t.Errorf("lifecycle calls=%d want 1", got)
	}
	if got := p.stopCalls.Load(); got != 1 {
		t.Errorf("Stop calls=%d want 1", got)
	}
	if got := p.lastStopTimeout(); got > 30*time.Millisecond {
		t.Errorf("process stop timeout=%v exceeds shutdown budget", got)
	}
}
