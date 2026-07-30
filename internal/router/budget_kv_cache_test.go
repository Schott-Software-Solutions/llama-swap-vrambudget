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
		Enabled:        true,
		Directory:      directory,
		SaveTimeout:    time.Second,
		RestoreTimeout: time.Second,
		SingleSlotOnly: true,
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
	if err := os.WriteFile(filepath.Join(lifecycle.config.Directory, modelID+".bin"), []byte("slot-cache"), 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	if err := writeSignatureAtomically(filepath.Join(lifecycle.config.Directory, modelID+".json"), signature); err != nil {
		t.Fatalf("write signature: %v", err)
	}
}

func TestKVCache_SaveAndRestore(t *testing.T) {
	directory := t.TempDir()
	var mu sync.Mutex
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Filename string `json:"filename"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		action := r.URL.Query().Get("action")
		if action == "save" {
			if err := os.WriteFile(filepath.Join(directory, body.Filename), []byte("slot-cache"), 0o600); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		mu.Lock()
		actions = append(actions, action)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id_slot":0,"filename":%q}`, body.Filename)
	}))
	t.Cleanup(server.Close)

	lifecycle, logger := newTestKVCacheLifecycle(t, directory, server.URL, 1)
	lifecycle.BeforeModelStop(t.Context(), "model")

	cachePath := filepath.Join(directory, "model.bin")
	if got, err := os.ReadFile(cachePath); err != nil || string(got) != "slot-cache" {
		t.Fatalf("cache file=%q err=%v", got, err)
	}
	if _, err := os.Stat(cachePath + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary cache remains after publish: %v", err)
	}
	cachedSignature, err := readSignature(filepath.Join(directory, "model.json"))
	if err != nil {
		t.Fatalf("read signature: %v", err)
	}
	if want := lifecycle.models["model"].signature; cachedSignature != want {
		t.Errorf("signature=%+v want %+v", cachedSignature, want)
	}

	lifecycle.AfterModelStart(t.Context(), "model")

	mu.Lock()
	gotActions := append([]string(nil), actions...)
	mu.Unlock()
	if fmt.Sprint(gotActions) != "[save restore]" {
		t.Errorf("actions=%v want [save restore]", gotActions)
	}
	history := string(logger.GetHistory())
	if !strings.Contains(history, "kv-cache: saved model=model") ||
		!strings.Contains(history, "kv-cache: restored model=model") {
		t.Errorf("logs missing save/restore success: %q", history)
	}
}

func TestKVCache_RestoreMissingCacheSkipsHTTP(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	t.Cleanup(server.Close)

	lifecycle, logger := newTestKVCacheLifecycle(t, t.TempDir(), server.URL, 1)
	logger.SetLogLevel(logmon.LevelDebug)
	lifecycle.AfterModelStart(t.Context(), "model")

	if got := calls.Load(); got != 0 {
		t.Errorf("HTTP calls=%d want 0", got)
	}
	if history := string(logger.GetHistory()); !strings.Contains(history, "reason=cache-missing") {
		t.Errorf("missing skip log: %q", history)
	}
}

func TestKVCache_MultiSlotModelsSkipSaveAndRestore(t *testing.T) {
	for _, parallel := range []int{2, 4} {
		t.Run(fmt.Sprintf("parallel_%d", parallel), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				calls.Add(1)
			}))
			t.Cleanup(server.Close)

			lifecycle, logger := newTestKVCacheLifecycle(t, t.TempDir(), server.URL, parallel)
			lifecycle.BeforeModelStop(t.Context(), "model")
			lifecycle.AfterModelStart(t.Context(), "model")

			if got := calls.Load(); got != 0 {
				t.Errorf("HTTP calls=%d want 0", got)
			}
			want := fmt.Sprintf("reason=parallel-slots count=%d", parallel)
			if history := string(logger.GetHistory()); !strings.Contains(history, want) {
				t.Errorf("missing %q in log: %q", want, history)
			}
		})
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
		{"parallel", "parallel", func(s *kvCacheSignature) { s.Parallel = 2 }},
		{"cache type k", "cache_type_k", func(s *kvCacheSignature) { s.CacheTypeK = "f16" }},
		{"cache type v", "cache_type_v", func(s *kvCacheSignature) { s.CacheTypeV = "f16" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				calls.Add(1)
			}))
			t.Cleanup(server.Close)

			lifecycle, logger := newTestKVCacheLifecycle(t, t.TempDir(), server.URL, 1)
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
	lifecycle, logger := newTestKVCacheLifecycle(t, directory, server.URL, 1)
	if err := os.WriteFile(filepath.Join(directory, "model.bin"), []byte("slot-cache"), 0o600); err != nil {
		t.Fatal(err)
	}
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
			name: "missing slot",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"filename":"model.bin"}`)
			}),
			wantText: "missing id_slot",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			server := httptest.NewServer(test.handler)
			t.Cleanup(server.Close)
			lifecycle, _ := newTestKVCacheLifecycle(t, directory, server.URL, 1)

			err := lifecycle.slotAction(t.Context(), lifecycle.models["model"], "restore", "model.bin")
			if err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("error=%v want substring %q", err, test.wantText)
			}
		})
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
	lifecycle, _ := newTestKVCacheLifecycle(t, t.TempDir(), server.URL, 1)

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	err := lifecycle.slotAction(ctx, lifecycle.models["model"], "save", "model.bin.tmp")
	waitSignal(t, started, "slot request")

	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("error=%v want context deadline exceeded", err)
	}
}

func TestKVCache_SlotActionUnreachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	proxy := server.URL
	server.Close()
	lifecycle, _ := newTestKVCacheLifecycle(t, t.TempDir(), proxy, 1)

	err := lifecycle.slotAction(t.Context(), lifecycle.models["model"], "save", "model.bin.tmp")
	if err == nil {
		t.Fatal("unreachable backend returned no error")
	}
}
