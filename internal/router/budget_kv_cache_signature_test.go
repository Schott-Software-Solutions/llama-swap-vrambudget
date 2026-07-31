package router

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestKVCache_InspectMultiSlotModel(t *testing.T) {
	directory := t.TempDir()
	model := inspectKVCacheModel("qwen3.6-35b-a3b", config.ModelConfig{
		Cmd:   fmt.Sprintf(`llama-server -np=4 -m /models/qwen.gguf -c 512000 -ctk q8_0 -ctv=q8_0 --kv-unified --slot-save-path %q`, directory),
		Proxy: "http://127.0.0.1:12011",
	}, directory)

	if !model.eligible {
		t.Fatalf("model ineligible: reason=%s", model.skipReason)
	}
	want := kvCacheSignature{
		ModelID:     "qwen3.6-35b-a3b",
		ModelPath:   "/models/qwen.gguf",
		ContextSize: 512000,
		Parallel:    4,
		CacheTypeK:  "q8_0",
		CacheTypeV:  "q8_0",
		KVUnified:   true,
	}
	if model.signature != want {
		t.Errorf("signature=%+v want %+v", model.signature, want)
	}
	if got := slotEndpoint(model.backend, 3, "save"); got != "http://127.0.0.1:12011/slots/3?action=save" {
		t.Errorf("slot endpoint=%q", got)
	}
}

func TestKVCache_ParallelModelsAreEligible(t *testing.T) {
	for _, parallel := range []int{1, 2, 4} {
		t.Run(fmt.Sprintf("parallel_%d", parallel), func(t *testing.T) {
			directory := t.TempDir()
			model := inspectKVCacheModel("model", config.ModelConfig{
				Cmd:   fmt.Sprintf(`llama-server --parallel %d --slot-save-path %q`, parallel, directory),
				Proxy: "http://127.0.0.1:8080",
			}, directory)
			if !model.eligible {
				t.Fatalf("model ineligible: reason=%s", model.skipReason)
			}
			if model.signature.Parallel != parallel {
				t.Errorf("parallel=%d want %d", model.signature.Parallel, parallel)
			}
			if strings.Contains(model.skipLog("model"), "parallel-slots") {
				t.Errorf("legacy parallel skip remains: %q", model.skipLog("model"))
			}
		})
	}
}

func TestKVCache_InvalidParallelIsIneligible(t *testing.T) {
	directory := t.TempDir()
	model := inspectKVCacheModel("model", config.ModelConfig{
		Cmd:   fmt.Sprintf(`llama-server --parallel 0 --slot-save-path %q`, directory),
		Proxy: "http://127.0.0.1:8080",
	}, directory)
	if model.eligible || model.skipReason != "parallel-count" {
		t.Errorf("eligible=%t reason=%q", model.eligible, model.skipReason)
	}
}

func TestKVCache_SignatureMismatchFields(t *testing.T) {
	current := kvCacheSignature{
		ModelID:     "model",
		ModelPath:   "/models/model.gguf",
		ContextSize: 131072,
		Parallel:    2,
		CacheTypeK:  "q8_0",
		CacheTypeV:  "q8_0",
		KVUnified:   true,
	}
	tests := []struct {
		name   string
		field  string
		mutate func(*kvCacheSignature)
	}{
		{"model id", "model_id", func(s *kvCacheSignature) { s.ModelID = "other" }},
		{"model path", "model_path", func(s *kvCacheSignature) { s.ModelPath = "/models/other.gguf" }},
		{"context size", "context_size", func(s *kvCacheSignature) { s.ContextSize = 65536 }},
		{"parallel", "parallel", func(s *kvCacheSignature) { s.Parallel = 4 }},
		{"cache type k", "cache_type_k", func(s *kvCacheSignature) { s.CacheTypeK = "f16" }},
		{"cache type v", "cache_type_v", func(s *kvCacheSignature) { s.CacheTypeV = "f16" }},
		{"kv unified", "kv_unified", func(s *kvCacheSignature) { s.KVUnified = false }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cached := current
			test.mutate(&cached)
			field, _, _, mismatch := signatureMismatch(cached, current)
			if !mismatch || field != test.field {
				t.Errorf("mismatch=%t field=%q want %q", mismatch, field, test.field)
			}
		})
	}
	if _, _, _, mismatch := signatureMismatch(current, current); mismatch {
		t.Fatal("matching signatures reported as mismatched")
	}
}

func TestKVCache_UnsafeModelIDIsIneligible(t *testing.T) {
	model := inspectKVCacheModel("../model", config.ModelConfig{}, t.TempDir())
	if model.eligible || model.skipReason != "unsafe-model-id" {
		t.Errorf("eligible=%t reason=%q", model.eligible, model.skipReason)
	}
}
