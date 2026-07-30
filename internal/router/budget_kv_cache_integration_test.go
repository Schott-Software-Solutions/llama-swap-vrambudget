package router

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
)

func testBudgetKVCacheLifecycle(
	directory string,
	models map[string]config.ModelConfig,
) (*kvCacheLifecycle, *logmon.Monitor) {
	logger := logmon.NewWriter(io.Discard)
	return newKVCacheLifecycle(testKVCacheConfig(directory), models, logger), logger
}

func writeSlotSaveResponse(w http.ResponseWriter, filename string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id_slot":0,"filename":%q}`, filename)
}

func decodeSlotFilename(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct {
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return "", false
	}
	return body.Filename, true
}

func TestKVCache_BudgetSavesVictimBeforeStop(t *testing.T) {
	directory := t.TempDir()
	var saveCompleted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filename, ok := decodeSlotFilename(w, r)
		if !ok {
			return
		}
		if got := r.URL.Query().Get("action"); got != "save" {
			http.Error(w, "unexpected action "+got, http.StatusBadRequest)
			return
		}
		if err := os.WriteFile(filepath.Join(directory, filename), []byte("slot-cache"), 0o600); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		saveCompleted.Store(true)
		writeSlotSaveResponse(w, filename)
	}))
	t.Cleanup(server.Close)

	victim := newFakeProcess("victim")
	victim.markReady()
	var stoppedBeforeSave atomic.Bool
	victim.onStop = func(string) {
		if !saveCompleted.Load() {
			stoppedBeforeSave.Store(true)
		}
	}
	target := newFakeProcess("target")
	target.autoReady = true
	router := newTestBudget(t, 100, map[string]int{"victim": 60, "target": 60}, map[string]process.Process{
		"victim": victim,
		"target": target,
	})
	lifecycle, _ := testBudgetKVCacheLifecycle(directory, map[string]config.ModelConfig{
		"victim": testKVCacheModel(directory, server.URL, 1),
		"target": testKVCacheModel(directory, server.URL, 2),
	})
	router.lifecycle = lifecycle

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newRequest("target"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if !saveCompleted.Load() {
		t.Fatal("victim cache was not saved")
	}
	if stoppedBeforeSave.Load() {
		t.Fatal("victim Stop ran before its cache save completed")
	}
	if got := victim.stopCalls.Load(); got != 1 {
		t.Errorf("victim.stopCalls=%d want 1", got)
	}
}

func TestKVCache_BudgetSaveFailureStillEvicts(t *testing.T) {
	directory := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "save failed", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	victim := newFakeProcess("victim")
	victim.markReady()
	target := newFakeProcess("target")
	target.autoReady = true
	router := newTestBudget(t, 100, map[string]int{"victim": 60, "target": 60}, map[string]process.Process{
		"victim": victim,
		"target": target,
	})
	lifecycle, logger := testBudgetKVCacheLifecycle(directory, map[string]config.ModelConfig{
		"victim": testKVCacheModel(directory, server.URL, 1),
		"target": testKVCacheModel(directory, server.URL, 1),
	})
	router.lifecycle = lifecycle

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newRequest("target"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := victim.stopCalls.Load(); got != 1 {
		t.Errorf("victim.stopCalls=%d want 1", got)
	}
	if history := string(logger.GetHistory()); !strings.Contains(history, "kv-cache: save failed model=victim") {
		t.Errorf("missing save failure log: %q", history)
	}
}

func TestKVCache_BudgetSaveTimeoutStillEvicts(t *testing.T) {
	directory := t.TempDir()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	victim := newFakeProcess("victim")
	victim.markReady()
	target := newFakeProcess("target")
	target.autoReady = true
	router := newTestBudget(t, 100, map[string]int{"victim": 60, "target": 60}, map[string]process.Process{
		"victim": victim,
		"target": target,
	})
	lifecycle, logger := testBudgetKVCacheLifecycle(directory, map[string]config.ModelConfig{
		"victim": testKVCacheModel(directory, server.URL, 1),
		"target": testKVCacheModel(directory, server.URL, 1),
	})
	lifecycle.config.SaveTimeout = 25 * time.Millisecond
	router.lifecycle = lifecycle

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newRequest("target"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := victim.stopCalls.Load(); got != 1 {
		t.Errorf("victim.stopCalls=%d want 1", got)
	}
	if history := string(logger.GetHistory()); !strings.Contains(history, "context deadline exceeded") {
		t.Errorf("missing timeout log: %q", history)
	}
}

func TestKVCache_BudgetRestoreGatesFirstRequest(t *testing.T) {
	directory := t.TempDir()
	restoreStarted := make(chan struct{})
	restoreRelease := make(chan struct{})
	var target *fakeProcess
	var restoredBeforeReady atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filename, ok := decodeSlotFilename(w, r)
		if !ok {
			return
		}
		if target.State() != process.StateReady {
			restoredBeforeReady.Store(true)
		}
		close(restoreStarted)
		<-restoreRelease
		writeSlotSaveResponse(w, filename)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		select {
		case <-restoreRelease:
		default:
			close(restoreRelease)
		}
	})

	target = newFakeProcess("target")
	target.autoReady = true
	router := newTestBudget(t, 100, map[string]int{"target": 60}, map[string]process.Process{"target": target})
	lifecycle, _ := testBudgetKVCacheLifecycle(directory, map[string]config.ModelConfig{
		"target": testKVCacheModel(directory, server.URL, 1),
	})
	writeTestKVCache(t, lifecycle, "target", lifecycle.models["target"].signature)
	router.lifecycle = lifecycle

	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(recorder, newRequest("target"))
		close(done)
	}()
	waitSignal(t, restoreStarted, "restore start")

	if restoredBeforeReady.Load() {
		t.Fatal("restore started before the target passed its readiness check")
	}
	if got := target.serveCalls.Load(); got != 0 {
		t.Fatalf("target.serveCalls=%d while restore is blocked", got)
	}
	select {
	case <-done:
		t.Fatal("request completed before restore was released")
	default:
	}

	close(restoreRelease)
	waitSignal(t, done, "request after restore")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := target.serveCalls.Load(); got != 1 {
		t.Errorf("target.serveCalls=%d want 1", got)
	}
}

func TestKVCache_BudgetRestoreFailureStillServes(t *testing.T) {
	directory := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "restore failed", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	target := newFakeProcess("target")
	target.autoReady = true
	router := newTestBudget(t, 100, map[string]int{"target": 60}, map[string]process.Process{"target": target})
	lifecycle, logger := testBudgetKVCacheLifecycle(directory, map[string]config.ModelConfig{
		"target": testKVCacheModel(directory, server.URL, 1),
	})
	writeTestKVCache(t, lifecycle, "target", lifecycle.models["target"].signature)
	router.lifecycle = lifecycle

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newRequest("target"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if history := string(logger.GetHistory()); !strings.Contains(history, "kv-cache: restore failed model=target") {
		t.Errorf("missing restore failure log: %q", history)
	}
}

func TestKVCache_BudgetRestoreTimeoutStillServes(t *testing.T) {
	directory := t.TempDir()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	target := newFakeProcess("target")
	target.autoReady = true
	router := newTestBudget(t, 100, map[string]int{"target": 60}, map[string]process.Process{"target": target})
	lifecycle, logger := testBudgetKVCacheLifecycle(directory, map[string]config.ModelConfig{
		"target": testKVCacheModel(directory, server.URL, 1),
	})
	lifecycle.config.RestoreTimeout = 25 * time.Millisecond
	writeTestKVCache(t, lifecycle, "target", lifecycle.models["target"].signature)
	router.lifecycle = lifecycle

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newRequest("target"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if history := string(logger.GetHistory()); !strings.Contains(history, "context deadline exceeded") {
		t.Errorf("missing timeout log: %q", history)
	}
}

func TestKVCache_BudgetDoesNotSaveBusyVictim(t *testing.T) {
	directory := t.TempDir()
	var saveCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filename, ok := decodeSlotFilename(w, r)
		if !ok {
			return
		}
		saveCalls.Add(1)
		if err := os.WriteFile(filepath.Join(directory, filename), []byte("slot-cache"), 0o600); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeSlotSaveResponse(w, filename)
	}))
	t.Cleanup(server.Close)

	victim := newFakeProcess("victim")
	victim.markReady()
	victim.serveBlock = make(chan struct{})
	target := newFakeProcess("target")
	target.autoReady = true
	router := newTestBudget(t, 100, map[string]int{"victim": 60, "target": 60}, map[string]process.Process{
		"victim": victim,
		"target": target,
	})
	lifecycle, _ := testBudgetKVCacheLifecycle(directory, map[string]config.ModelConfig{
		"victim": testKVCacheModel(directory, server.URL, 1),
		"target": testKVCacheModel(directory, server.URL, 2),
	})
	router.lifecycle = lifecycle

	victimDone := make(chan struct{})
	go func() {
		router.ServeHTTP(httptest.NewRecorder(), newRequest("victim"))
		close(victimDone)
	}()
	waitSignal(t, victim.serveStarted, "victim serve")

	targetRecorder := httptest.NewRecorder()
	targetDone := make(chan struct{})
	go func() {
		router.ServeHTTP(targetRecorder, newRequest("target"))
		close(targetDone)
	}()
	waitProcessed(t, router.testProcessed, 1)

	if got := saveCalls.Load(); got != 0 {
		t.Fatalf("save calls=%d while victim is busy", got)
	}
	if got := victim.stopCalls.Load(); got != 0 {
		t.Fatalf("victim.stopCalls=%d while victim is busy", got)
	}

	close(victim.serveBlock)
	waitSignal(t, victimDone, "victim request")
	waitSignal(t, targetDone, "target request")
	if got := saveCalls.Load(); got != 1 {
		t.Errorf("save calls=%d after victim drained, want 1", got)
	}
	if targetRecorder.Code != http.StatusOK {
		t.Errorf("target status=%d body=%q", targetRecorder.Code, targetRecorder.Body.String())
	}
}
