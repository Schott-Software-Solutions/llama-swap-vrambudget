package router

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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

func writeSlotSaveResponse(w http.ResponseWriter, slotID int, filename string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id_slot":%d,"filename":%q}`, slotID, filename)
}

func decodeSlotRequest(w http.ResponseWriter, r *http.Request) (int, string, bool) {
	slotID, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/slots/"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return 0, "", false
	}
	var body struct {
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return 0, "", false
	}
	return slotID, body.Filename, true
}

func TestKVCache_BudgetSavesVictimBeforeStop(t *testing.T) {
	directory := t.TempDir()
	var savedSlots atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slotID, filename, ok := decodeSlotRequest(w, r)
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
		savedSlots.Add(1)
		writeSlotSaveResponse(w, slotID, filename)
	}))
	t.Cleanup(server.Close)

	victim := newFakeProcess("victim")
	victim.markReady()
	var stoppedBeforeSave atomic.Bool
	victim.onStop = func(string) {
		if savedSlots.Load() != 2 {
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
		"victim": testKVCacheModel(directory, server.URL, 2),
		"target": testKVCacheModel(directory, server.URL, 2),
	})
	router.lifecycle = lifecycle

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newRequest("target"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := savedSlots.Load(); got != 2 {
		t.Fatalf("saved slots=%d want 2", got)
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
	var restoreStartOnce sync.Once
	var restoredMu sync.Mutex
	var restoredSlots []int
	var target *fakeProcess
	var restoredBeforeReady atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slotID, filename, ok := decodeSlotRequest(w, r)
		if !ok {
			return
		}
		if target.State() != process.StateReady {
			restoredBeforeReady.Store(true)
		}
		restoredMu.Lock()
		restoredSlots = append(restoredSlots, slotID)
		restoredMu.Unlock()
		restoreStartOnce.Do(func() { close(restoreStarted) })
		<-restoreRelease
		writeSlotSaveResponse(w, slotID, filename)
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
		"target": testKVCacheModel(directory, server.URL, 2),
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
	restoredMu.Lock()
	sort.Ints(restoredSlots)
	gotRestored := append([]int(nil), restoredSlots...)
	restoredMu.Unlock()
	if fmt.Sprint(gotRestored) != "[0 1]" {
		t.Errorf("restored slots=%v want [0 1]", gotRestored)
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
	var restoreCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		restoreCalls.Add(1)
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	target := newFakeProcess("target")
	target.autoReady = true
	router := newTestBudget(t, 100, map[string]int{"target": 60}, map[string]process.Process{"target": target})
	lifecycle, logger := testBudgetKVCacheLifecycle(directory, map[string]config.ModelConfig{
		"target": testKVCacheModel(directory, server.URL, 2),
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
	} else if !strings.Contains(history, "restore complete model=target restored=0 failed=2") {
		t.Errorf("missing model-wide timeout summary: %q", history)
	}
	if got := restoreCalls.Load(); got != 1 {
		t.Errorf("restore HTTP calls=%d want 1; later slots should observe the shared timeout", got)
	}
}

func TestKVCache_BudgetDoesNotSaveBusyVictim(t *testing.T) {
	directory := t.TempDir()
	var saveCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slotID, filename, ok := decodeSlotRequest(w, r)
		if !ok {
			return
		}
		saveCalls.Add(1)
		if err := os.WriteFile(filepath.Join(directory, filename), []byte("slot-cache"), 0o600); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeSlotSaveResponse(w, slotID, filename)
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
