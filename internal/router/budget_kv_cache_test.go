package router

import (
	"context"
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
)

func testKVCacheConfig(directory string) config.BudgetKVCacheConfig {
	return config.BudgetKVCacheConfig{
		Enabled:          true,
		Directory:        directory,
		SaveTimeout:      time.Second,
		RestoreTimeout:   time.Second,
		MaxParallelSaves: 2,
	}
}

func testKVCacheModel(directory, proxy string, parallel int) config.ModelConfig {
	return config.ModelConfig{
		Cmd: fmt.Sprintf(
			"llama-server --model /models/model.gguf --ctx-size 131072 --parallel %d --cache-type-k q8_0 --cache-type-v q8_0 --slot-save-path %q",
			parallel, directory,
		),
		Proxy: proxy,
	}
}

func newTestKVCacheLifecycle(
	t *testing.T,
	directory string,
	proxy string,
	parallel int,
) (*kvCacheLifecycle, *logmon.Monitor) {
	t.Helper()
	logger := logmon.NewWriter(io.Discard)
	lifecycle := newKVCacheLifecycle(
		testKVCacheConfig(directory),
		map[string]config.ModelConfig{"model": testKVCacheModel(directory, proxy, parallel)},
		logger,
	)
	return lifecycle, logger
}

func writeTestKVCache(t *testing.T, lifecycle *kvCacheLifecycle, modelID string, signature kvCacheSignature) {
	t.Helper()
	if err := os.MkdirAll(lifecycle.config.Directory, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	slots := make([]kvCacheSlotMetadata, signature.Parallel)
	for slotID := range signature.Parallel {
		filename := kvCacheSlotFilename(modelID, slotID)
		if err := os.WriteFile(filepath.Join(lifecycle.config.Directory, filename), []byte(fmt.Sprintf("slot-cache-%d", slotID)), 0o600); err != nil {
			t.Fatalf("write cache: %v", err)
		}
		slots[slotID] = kvCacheSlotMetadata{SlotID: slotID, Filename: filename, Saved: true}
	}
	metadata := newKVCacheMetadata(signature, slots)
	if err := writeMetadataAtomically(filepath.Join(lifecycle.config.Directory, modelID+".json"), metadata); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
}

func decodeTestSlotRequest(w http.ResponseWriter, r *http.Request) (int, string, bool) {
	slotID, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/slots/"))
	if err != nil {
		http.Error(w, fmt.Sprintf("slot path %q: %v", r.URL.Path, err), http.StatusBadRequest)
		return 0, "", false
	}
	var body struct {
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("decode slot request: %v", err), http.StatusBadRequest)
		return 0, "", false
	}
	return slotID, body.Filename, true
}

func respondSlotAction(w http.ResponseWriter, slotID int, filename string, fields string) {
	w.Header().Set("Content-Type", "application/json")
	if fields != "" {
		fields = "," + fields
	}
	fmt.Fprintf(w, `{"id_slot":%d,"filename":%q%s}`, slotID, filename, fields)
}

func TestKVCache_SaveAndRestoreAllSlots(t *testing.T) {
	directory := t.TempDir()
	var mu sync.Mutex
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slotID, filename, ok := decodeTestSlotRequest(w, r)
		if !ok {
			return
		}
		action := r.URL.Query().Get("action")
		if action == "save" {
			if err := os.WriteFile(filepath.Join(directory, filename), []byte(fmt.Sprintf("slot-cache-%d", slotID)), 0o600); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		mu.Lock()
		actions = append(actions, fmt.Sprintf("%s:%d:%s", action, slotID, filename))
		mu.Unlock()
		fields := `"n_restored":42`
		if action == "save" {
			fields = `"n_saved":42`
		}
		respondSlotAction(w, slotID, filename, fields)
	}))
	t.Cleanup(server.Close)

	lifecycle, logger := newTestKVCacheLifecycle(t, directory, server.URL, 2)
	for _, stale := range []string{"model-slot-2.bin", "model-slot-3.bin.tmp"} {
		if err := os.WriteFile(filepath.Join(directory, stale), []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lifecycle.BeforeModelStop(t.Context(), "model")

	for slotID := range 2 {
		cachePath := filepath.Join(directory, kvCacheSlotFilename("model", slotID))
		if got, err := os.ReadFile(cachePath); err != nil || string(got) != fmt.Sprintf("slot-cache-%d", slotID) {
			t.Fatalf("slot %d cache=%q err=%v", slotID, got, err)
		}
		if _, err := os.Stat(cachePath + ".tmp"); !os.IsNotExist(err) {
			t.Errorf("slot %d temporary cache remains: %v", slotID, err)
		}
	}
	for _, stale := range []string{"model-slot-2.bin", "model-slot-3.bin.tmp"} {
		if _, err := os.Stat(filepath.Join(directory, stale)); !os.IsNotExist(err) {
			t.Errorf("obsolete slot file %s remains: %v", stale, err)
		}
	}
	metadata, err := readKVCacheMetadata(filepath.Join(directory, "model.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if metadata.Version != 2 || len(metadata.Slots) != 2 || !metadata.Slots[0].Saved || !metadata.Slots[1].Saved {
		t.Errorf("metadata=%+v", metadata)
	}

	lifecycle.AfterModelStart(t.Context(), "model")

	mu.Lock()
	gotActions := append([]string(nil), actions...)
	mu.Unlock()
	if len(gotActions) != 4 {
		t.Fatalf("actions=%v want four save/restore actions", gotActions)
	}
	sort.Strings(gotActions[:2])
	want := []string{
		"save:0:model-slot-0.bin.tmp",
		"save:1:model-slot-1.bin.tmp",
		"restore:0:model-slot-0.bin",
		"restore:1:model-slot-1.bin",
	}
	if fmt.Sprint(gotActions) != fmt.Sprint(want) {
		t.Errorf("actions=%v want %v", gotActions, want)
	}
	history := string(logger.GetHistory())
	if !strings.Contains(history, "save complete model=model saved=2 failed=0") ||
		!strings.Contains(history, "restore complete model=model restored=2 failed=0") {
		t.Errorf("logs missing summaries: %q", history)
	}
}

func TestKVCache_SaveContinuesAfterSlotFailure(t *testing.T) {
	directory := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slotID, filename, ok := decodeTestSlotRequest(w, r)
		if !ok {
			return
		}
		if err := os.WriteFile(filepath.Join(directory, filename), []byte("partial"), 0o600); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if slotID == 1 {
			http.Error(w, "slot failed", http.StatusInternalServerError)
			return
		}
		respondSlotAction(w, slotID, filename, `"n_saved":10`)
	}))
	t.Cleanup(server.Close)

	lifecycle, logger := newTestKVCacheLifecycle(t, directory, server.URL, 3)
	lifecycle.BeforeModelStop(t.Context(), "model")

	metadata, err := readKVCacheMetadata(filepath.Join(directory, "model.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	for slotID, wantSaved := range []bool{true, false, true} {
		if metadata.Slots[slotID].Saved != wantSaved {
			t.Errorf("slot %d saved=%t want %t", slotID, metadata.Slots[slotID].Saved, wantSaved)
		}
		if _, err := os.Stat(filepath.Join(directory, kvCacheSlotFilename("model", slotID)+".tmp")); !os.IsNotExist(err) {
			t.Errorf("slot %d temporary file remains: %v", slotID, err)
		}
	}
	if history := string(logger.GetHistory()); !strings.Contains(history, "save complete model=model saved=2 failed=1") {
		t.Errorf("missing partial-save summary: %q", history)
	}
}

func TestKVCache_EmptySlotWithoutFileIsSkipped(t *testing.T) {
	directory := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slotID, filename, ok := decodeTestSlotRequest(w, r)
		if !ok {
			return
		}
		respondSlotAction(w, slotID, filename, `"n_saved":0`)
	}))
	t.Cleanup(server.Close)

	lifecycle, logger := newTestKVCacheLifecycle(t, directory, server.URL, 1)
	lifecycle.BeforeModelStop(t.Context(), "model")

	metadata, err := readKVCacheMetadata(filepath.Join(directory, "model.json"))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Slots[0].Saved {
		t.Fatal("empty slot without file marked restorable")
	}
	if history := string(logger.GetHistory()); !strings.Contains(history, "reason=empty-slot") || !strings.Contains(history, "failed=0 empty=1") {
		t.Errorf("missing empty-slot logs: %q", history)
	}
}

func TestKVCache_RestoreContinuesAfterSlotFailure(t *testing.T) {
	directory := t.TempDir()
	var mu sync.Mutex
	var restored []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slotID, filename, ok := decodeTestSlotRequest(w, r)
		if !ok {
			return
		}
		mu.Lock()
		restored = append(restored, slotID)
		mu.Unlock()
		if slotID == 1 {
			http.Error(w, "restore failed", http.StatusInternalServerError)
			return
		}
		respondSlotAction(w, slotID, filename, `"n_restored":10`)
	}))
	t.Cleanup(server.Close)

	lifecycle, logger := newTestKVCacheLifecycle(t, directory, server.URL, 3)
	writeTestKVCache(t, lifecycle, "model", lifecycle.models["model"].signature)
	lifecycle.AfterModelStart(t.Context(), "model")

	mu.Lock()
	gotRestored := append([]int(nil), restored...)
	mu.Unlock()
	if fmt.Sprint(gotRestored) != "[0 1 2]" {
		t.Errorf("restored slots=%v want [0 1 2]", gotRestored)
	}
	if history := string(logger.GetHistory()); !strings.Contains(history, "restore complete model=model restored=2 failed=1") {
		t.Errorf("missing partial-restore summary: %q", history)
	}
}

func TestKVCache_RestoreMissingMetadataSkipsHTTP(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	t.Cleanup(server.Close)

	lifecycle, logger := newTestKVCacheLifecycle(t, t.TempDir(), server.URL, 2)
	logger.SetLogLevel(logmon.LevelDebug)
	lifecycle.AfterModelStart(t.Context(), "model")

	if got := calls.Load(); got != 0 {
		t.Errorf("HTTP calls=%d want 0", got)
	}
	if history := string(logger.GetHistory()); !strings.Contains(history, "reason=cache-missing") {
		t.Errorf("missing skip log: %q", history)
	}
}

func TestKVCache_RestoreSignatureMismatchSkipsHTTP(t *testing.T) {
	tests := []struct {
		name   string
		field  string
		mutate func(*kvCacheSignature)
	}{
		{"model path", "model_path", func(s *kvCacheSignature) { s.ModelPath = "/models/old.gguf" }},
		{"context size", "context_size", func(s *kvCacheSignature) { s.ContextSize = 65536 }},
		{"parallel", "parallel", func(s *kvCacheSignature) { s.Parallel = 4 }},
		{"cache type k", "cache_type_k", func(s *kvCacheSignature) { s.CacheTypeK = "f16" }},
		{"cache type v", "cache_type_v", func(s *kvCacheSignature) { s.CacheTypeV = "f16" }},
		{"kv unified", "kv_unified", func(s *kvCacheSignature) { s.KVUnified = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				calls.Add(1)
			}))
			t.Cleanup(server.Close)

			lifecycle, logger := newTestKVCacheLifecycle(t, t.TempDir(), server.URL, 2)
			cached := lifecycle.models["model"].signature
			test.mutate(&cached)
			writeTestKVCache(t, lifecycle, "model", cached)

			lifecycle.AfterModelStart(t.Context(), "model")

			if got := calls.Load(); got != 0 {
				t.Errorf("HTTP calls=%d want 0", got)
			}
			want := "reason=config-mismatch field=" + test.field
			if history := string(logger.GetHistory()); !strings.Contains(history, want) {
				t.Errorf("missing %q in log: %q", want, history)
			}
		})
	}
}

func TestKVCache_RestoreInvalidMetadataSkipsHTTP(t *testing.T) {
	directory := t.TempDir()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	t.Cleanup(server.Close)
	lifecycle, logger := newTestKVCacheLifecycle(t, directory, server.URL, 2)
	if err := os.WriteFile(filepath.Join(directory, "model.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	lifecycle.AfterModelStart(t.Context(), "model")

	if got := calls.Load(); got != 0 {
		t.Errorf("HTTP calls=%d want 0", got)
	}
	if history := string(logger.GetHistory()); !strings.Contains(history, "reason=metadata-invalid") {
		t.Errorf("missing metadata warning: %q", history)
	}
}

func TestKVCache_LegacySingleSlotRestore(t *testing.T) {
	directory := t.TempDir()
	var mu sync.Mutex
	var gotSlot int
	var gotFilename string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slotID, filename, ok := decodeTestSlotRequest(w, r)
		if !ok {
			return
		}
		mu.Lock()
		gotSlot = slotID
		gotFilename = filename
		mu.Unlock()
		respondSlotAction(w, slotID, filename, `"n_restored":10`)
	}))
	t.Cleanup(server.Close)
	lifecycle, _ := newTestKVCacheLifecycle(t, directory, server.URL, 1)
	if err := os.WriteFile(filepath.Join(directory, "model.bin"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := json.Marshal(lifecycle.models["model"].signature)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "model.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	lifecycle.AfterModelStart(t.Context(), "model")

	mu.Lock()
	defer mu.Unlock()
	if gotSlot != 0 || gotFilename != "model.bin" {
		t.Errorf("legacy restore slot=%d filename=%q", gotSlot, gotFilename)
	}
}

func TestKVCache_LegacySingleSlotSkippedForMultiSlotModel(t *testing.T) {
	directory := t.TempDir()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	t.Cleanup(server.Close)
	lifecycle, logger := newTestKVCacheLifecycle(t, directory, server.URL, 2)
	if err := os.WriteFile(filepath.Join(directory, "model.bin"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacySignature := lifecycle.models["model"].signature
	legacySignature.Parallel = 1
	legacy, err := json.Marshal(legacySignature)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "model.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	lifecycle.AfterModelStart(t.Context(), "model")

	if got := calls.Load(); got != 0 {
		t.Errorf("restore HTTP calls=%d want 0", got)
	}
	if history := string(logger.GetHistory()); !strings.Contains(history, "reason=config-mismatch field=parallel cached=1 current=2") {
		t.Errorf("missing legacy parallel mismatch log: %q", history)
	}
}

func TestKVCache_SaveTimeoutBoundsConcurrency(t *testing.T) {
	directory := t.TempDir()
	release := make(chan struct{})
	var started atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		started.Add(1)
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	lifecycle, logger := newTestKVCacheLifecycle(t, directory, server.URL, 4)
	lifecycle.config.SaveTimeout = 25 * time.Millisecond
	lifecycle.config.MaxParallelSaves = 2

	lifecycle.BeforeModelStop(t.Context(), "model")

	if got := started.Load(); got < 1 || got > 2 {
		t.Errorf("started HTTP saves=%d want 1..2", got)
	}
	if history := string(logger.GetHistory()); !strings.Contains(history, "save complete model=model saved=0 failed=4") {
		t.Errorf("missing bounded-timeout summary: %q", history)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("temporary file remains after timeout: %s", entry.Name())
		}
	}
}

func TestKVCache_SaveConcurrencyLimitIsSharedAcrossModels(t *testing.T) {
	directory := t.TempDir()
	release := make(chan struct{})
	started := make(chan struct{}, 6)
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slotID, filename, ok := decodeTestSlotRequest(w, r)
		if !ok {
			return
		}
		started <- struct{}{}
		<-release
		if err := os.WriteFile(filepath.Join(directory, filename), []byte("cache"), 0o600); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		respondSlotAction(w, slotID, filename, `"n_saved":10`)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	lifecycle := newKVCacheLifecycle(
		testKVCacheConfig(directory),
		map[string]config.ModelConfig{
			"a": testKVCacheModel(directory, server.URL, 3),
			"b": testKVCacheModel(directory, server.URL, 3),
		},
		logmon.NewWriter(io.Discard),
	)
	done := make(chan struct{})
	var saves sync.WaitGroup
	for _, modelID := range []string{"a", "b"} {
		saves.Add(1)
		go func() {
			defer saves.Done()
			lifecycle.BeforeModelStop(t.Context(), modelID)
		}()
	}
	go func() {
		saves.Wait()
		close(done)
	}()

	waitSignal(t, started, "first shared save")
	waitSignal(t, started, "second shared save")
	select {
	case <-started:
		t.Fatal("more than max_parallel_saves requests started across models")
	case <-time.After(50 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(release) })
	waitSignal(t, done, "shared saves")
}

func TestKVCache_SlotActionFailures(t *testing.T) {
	tests := []struct {
		name     string
		handler  http.Handler
		wantText string
	}{
		{
			name: "HTTP 500",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "save unavailable", http.StatusInternalServerError)
			}),
			wantText: "HTTP 500",
		},
		{
			name: "invalid JSON",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, "not-json")
			}),
			wantText: "invalid JSON response",
		},
		{
			name: "wrong slot",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"id_slot":0,"filename":"model-slot-1.bin"}`)
			}),
			wantText: "unexpected id_slot",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			server := httptest.NewServer(test.handler)
			t.Cleanup(server.Close)
			lifecycle, _ := newTestKVCacheLifecycle(t, directory, server.URL, 2)

			_, err := lifecycle.slotAction(t.Context(), lifecycle.models["model"], 1, "restore", "model-slot-1.bin")
			if err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("error=%v want substring %q", err, test.wantText)
			}
		})
	}
}

func TestKVCache_SlotActionUnreachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	proxy := server.URL
	server.Close()
	lifecycle, _ := newTestKVCacheLifecycle(t, t.TempDir(), proxy, 2)

	_, err := lifecycle.slotAction(t.Context(), lifecycle.models["model"], 1, "save", "model-slot-1.bin.tmp")
	if err == nil {
		t.Fatal("unreachable backend returned no error")
	}
}

func TestKVCache_SlotActionTimeout(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	lifecycle, _ := newTestKVCacheLifecycle(t, t.TempDir(), server.URL, 2)

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	_, err := lifecycle.slotAction(ctx, lifecycle.models["model"], 1, "save", "model-slot-1.bin.tmp")
	waitSignal(t, started, "slot request")

	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("error=%v want context deadline exceeded", err)
	}
}
