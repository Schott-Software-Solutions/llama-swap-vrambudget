package router

import (
	"fmt"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestKVCache_InspectSingleSlotModel(t *testing.T) {
	directory := t.TempDir()
	model := inspectKVCacheModel("gpt-oss-120b", config.ModelConfig{
		Cmd:   fmt.Sprintf(`llama-server --parallel=1 -m /models/gpt.gguf -c 131072 --cache-type-k q8_0 --cache-type-v=q8_0 --slot-save-path %q`, directory),
		Proxy: "http://127.0.0.1:12011",
	}, directory)

	if !model.eligible {
		t.Fatalf("model ineligible: reason=%s", model.skipReason)
	}
	want := kvCacheSignature{
		ModelID:     "gpt-oss-120b",
		ModelPath:   "/models/gpt.gguf",
		ContextSize: 131072,
		Parallel:    1,
		CacheTypeK:  "q8_0",
		CacheTypeV:  "q8_0",
	}
	if model.signature != want {
		t.Errorf("signature=%+v want %+v", model.signature, want)
	}
	if got := slotEndpoint(model.backend, "save"); got != "http://127.0.0.1:12011/slots/0?action=save" {
		t.Errorf("slot endpoint=%q", got)
	}
}

func TestKVCache_ParallelModelsAreIneligible(t *testing.T) {
	for _, parallel := range []int{2, 4} {
		t.Run(fmt.Sprintf("parallel_%d", parallel), func(t *testing.T) {
			directory := t.TempDir()
			model := inspectKVCacheModel("model", config.ModelConfig{
				Cmd:   fmt.Sprintf(`llama-server --parallel %d --slot-save-path %q`, parallel, directory),
				Proxy: "http://127.0.0.1:8080",
			}, directory)
			if model.eligible {
				t.Fatal("multi-slot model unexpectedly eligible")
			}
			if model.skipReason != "parallel-slots" || model.signature.Parallel != parallel {
				t.Errorf("reason=%q parallel=%d", model.skipReason, model.signature.Parallel)
			}
		})
	}
}

func TestKVCache_SignatureMismatchFields(t *testing.T) {
	current := kvCacheSignature{
		ModelID:     "model",
		ModelPath:   "/models/model.gguf",
		ContextSize: 131072,
		Parallel:    1,
		CacheTypeK:  "q8_0",
		CacheTypeV:  "q8_0",
	}
	tests := []struct {
		name   string
		field  string
		mutate func(*kvCacheSignature)
	}{
		{"model id", "model_id", func(s *kvCacheSignature) { s.ModelID = "other" }},
		{"model path", "model_path", func(s *kvCacheSignature) { s.ModelPath = "/models/other.gguf" }},
		{"context size", "context_size", func(s *kvCacheSignature) { s.ContextSize = 65536 }},
		{"parallel", "parallel", func(s *kvCacheSignature) { s.Parallel = 2 }},
		{"cache type k", "cache_type_k", func(s *kvCacheSignature) { s.CacheTypeK = "f16" }},
		{"cache type v", "cache_type_v", func(s *kvCacheSignature) { s.CacheTypeV = "f16" }},
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
