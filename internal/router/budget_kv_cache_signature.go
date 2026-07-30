package router

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
)

type kvCacheSignature struct {
	ModelID     string `json:"model_id"`
	ModelPath   string `json:"model_path"`
	ContextSize int    `json:"context_size"`
	Parallel    int    `json:"parallel"`
	CacheTypeK  string `json:"cache_type_k"`
	CacheTypeV  string `json:"cache_type_v"`
}

type kvCacheModel struct {
	backend    *url.URL
	signature  kvCacheSignature
	eligible   bool
	skipReason string
}

func inspectKVCacheModel(modelID string, model config.ModelConfig, directory string) kvCacheModel {
	result := kvCacheModel{
		signature: kvCacheSignature{ModelID: modelID},
	}
	if !safeCacheModelID(modelID) {
		result.skipReason = "unsafe-model-id"
		return result
	}

	args, err := config.SanitizeCommand(model.Cmd)
	if err != nil {
		result.skipReason = "invalid-command"
		return result
	}

	parallelValue, ok := commandFlag(args, "--parallel", "-np")
	if !ok {
		result.skipReason = "parallel-slots"
		return result
	}
	parallel, err := strconv.Atoi(parallelValue)
	if err != nil || parallel < 1 {
		result.skipReason = "parallel-slots"
		return result
	}
	result.signature.Parallel = parallel
	if parallel != 1 {
		result.skipReason = "parallel-slots"
		return result
	}

	slotSavePath, ok := commandFlag(args, "--slot-save-path")
	if !ok || filepath.Clean(slotSavePath) != filepath.Clean(directory) {
		result.skipReason = "slot-save-path"
		return result
	}

	result.signature.ModelPath, _ = commandFlag(args, "--model", "-m")
	if contextValue, exists := commandFlag(args, "--ctx-size", "-c"); exists {
		contextSize, parseErr := strconv.Atoi(contextValue)
		if parseErr != nil || contextSize < 0 {
			result.skipReason = "context-size"
			return result
		}
		result.signature.ContextSize = contextSize
	}
	result.signature.CacheTypeK, _ = commandFlag(args, "--cache-type-k")
	result.signature.CacheTypeV, _ = commandFlag(args, "--cache-type-v")

	backend, err := url.Parse(model.Proxy)
	if err != nil || (backend.Scheme != "http" && backend.Scheme != "https") || backend.Host == "" {
		result.skipReason = "backend-url"
		return result
	}
	result.backend = backend
	result.eligible = true
	return result
}

func commandFlag(args []string, names ...string) (string, bool) {
	for i := len(args) - 1; i >= 0; i-- {
		for _, name := range names {
			if args[i] == name {
				if i+1 < len(args) {
					return args[i+1], true
				}
				return "", false
			}
			if value, found := strings.CutPrefix(args[i], name+"="); found {
				return value, true
			}
		}
	}
	return "", false
}

func safeCacheModelID(modelID string) bool {
	if modelID == "" || modelID == "." || modelID == ".." || filepath.Base(modelID) != modelID {
		return false
	}
	return !strings.ContainsAny(modelID, `/\`)
}

func signatureMismatch(cached, current kvCacheSignature) (field, cachedValue, currentValue string, mismatch bool) {
	fields := []struct {
		name    string
		cached  string
		current string
	}{
		{"model_id", cached.ModelID, current.ModelID},
		{"model_path", cached.ModelPath, current.ModelPath},
		{"context_size", strconv.Itoa(cached.ContextSize), strconv.Itoa(current.ContextSize)},
		{"parallel", strconv.Itoa(cached.Parallel), strconv.Itoa(current.Parallel)},
		{"cache_type_k", cached.CacheTypeK, current.CacheTypeK},
		{"cache_type_v", cached.CacheTypeV, current.CacheTypeV},
	}
	for _, candidate := range fields {
		if candidate.cached != candidate.current {
			return candidate.name, candidate.cached, candidate.current, true
		}
	}
	return "", "", "", false
}

func (m kvCacheModel) skipLog(modelID string) string {
	if m.skipReason == "parallel-slots" {
		return fmt.Sprintf("kv-cache: skipping model=%s reason=parallel-slots count=%d", modelID, m.signature.Parallel)
	}
	return fmt.Sprintf("kv-cache: skipping model=%s reason=%s", modelID, m.skipReason)
}
