package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

const maxSlotResponseBytes = 1 << 20

type kvCacheLifecycle struct {
	config config.BudgetKVCacheConfig
	models map[string]kvCacheModel
	logger *logmon.Monitor
	client *http.Client
}

func newKVCacheLifecycle(settings config.BudgetKVCacheConfig, models map[string]config.ModelConfig, logger *logmon.Monitor) *kvCacheLifecycle {
	inspected := make(map[string]kvCacheModel, len(models))
	for modelID, model := range models {
		inspected[modelID] = inspectKVCacheModel(modelID, model, settings.Directory)
	}
	return &kvCacheLifecycle{
		config: settings,
		models: inspected,
		logger: logger,
		client: &http.Client{},
	}
}

func (l *kvCacheLifecycle) BeforeModelStop(parent context.Context, modelID string) {
	model, ok := l.usableModel(modelID)
	if !ok {
		return
	}
	if err := os.MkdirAll(l.config.Directory, 0o750); err != nil {
		l.logger.Warnf("kv-cache: save failed model=%s error=%v", modelID, err)
		return
	}

	finalFilename := modelID + ".bin"
	tempFilename := finalFilename + ".tmp"
	tempPath := filepath.Join(l.config.Directory, tempFilename)
	finalPath := filepath.Join(l.config.Directory, finalFilename)
	metadataPath := filepath.Join(l.config.Directory, modelID+".json")
	_ = os.Remove(tempPath)
	defer os.Remove(tempPath)

	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, l.config.SaveTimeout)
	defer cancel()
	if err := l.slotAction(ctx, model, "save", tempFilename); err != nil {
		l.logger.Warnf("kv-cache: save failed model=%s error=%v", modelID, err)
		return
	}
	if info, err := os.Stat(tempPath); err != nil {
		l.logger.Warnf("kv-cache: save failed model=%s error=cache file %s unavailable after save: %v", modelID, tempPath, err)
		return
	} else if !info.Mode().IsRegular() {
		l.logger.Warnf("kv-cache: save failed model=%s error=cache file %s is not regular", modelID, tempPath)
		return
	}
	if err := replaceFile(tempPath, finalPath); err != nil {
		l.logger.Warnf("kv-cache: save failed model=%s error=publishing cache file: %v", modelID, err)
		return
	}
	if err := writeSignatureAtomically(metadataPath, model.signature); err != nil {
		// Without fresh metadata the binary must not be considered restorable.
		_ = os.Remove(metadataPath)
		l.logger.Warnf("kv-cache: save failed model=%s error=writing metadata: %v", modelID, err)
		return
	}

	l.logger.Infof("kv-cache: saved model=%s slot=0 file=%s duration=%s",
		modelID, finalPath, time.Since(started).Round(time.Millisecond))
}

func (l *kvCacheLifecycle) AfterModelStart(parent context.Context, modelID string) {
	model, ok := l.usableModel(modelID)
	if !ok {
		return
	}

	filename := modelID + ".bin"
	cachePath := filepath.Join(l.config.Directory, filename)
	metadataPath := filepath.Join(l.config.Directory, modelID+".json")
	if _, err := os.Stat(cachePath); err != nil {
		if os.IsNotExist(err) {
			l.logger.Debugf("kv-cache: skipped restore model=%s reason=cache-missing", modelID)
		} else {
			l.logger.Warnf("kv-cache: skipped restore model=%s reason=cache-stat error=%v", modelID, err)
		}
		return
	}

	cached, err := readSignature(metadataPath)
	if err != nil {
		l.logger.Warnf("kv-cache: skipped restore model=%s reason=metadata-invalid error=%v", modelID, err)
		return
	}
	if field, cachedValue, currentValue, mismatch := signatureMismatch(cached, model.signature); mismatch {
		l.logger.Warnf("kv-cache: skipped restore model=%s reason=config-mismatch field=%s cached=%s current=%s",
			modelID, field, cachedValue, currentValue)
		return
	}

	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, l.config.RestoreTimeout)
	defer cancel()
	if err := l.slotAction(ctx, model, "restore", filename); err != nil {
		l.logger.Warnf("kv-cache: restore failed model=%s error=%v", modelID, err)
		return
	}
	l.logger.Infof("kv-cache: restored model=%s slot=0 file=%s duration=%s",
		modelID, cachePath, time.Since(started).Round(time.Millisecond))
}

func (l *kvCacheLifecycle) usableModel(modelID string) (kvCacheModel, bool) {
	model, exists := l.models[modelID]
	if !exists {
		l.logger.Warnf("kv-cache: skipping model=%s reason=unknown-model", modelID)
		return kvCacheModel{}, false
	}
	if !model.eligible {
		l.logger.Infof("%s", model.skipLog(modelID))
		return kvCacheModel{}, false
	}
	return model, true
}

func (l *kvCacheLifecycle) slotAction(ctx context.Context, model kvCacheModel, action, filename string) error {
	endpoint := slotEndpoint(model.backend, action)
	body, err := json.Marshal(map[string]string{"filename": filename})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := l.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxSlotResponseBytes))
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("POST %s returned HTTP %d: %s", endpoint, response.StatusCode, strings.TrimSpace(string(responseBody)))
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return fmt.Errorf("invalid JSON response: %w", err)
	}
	var slotID int
	if raw, exists := payload["id_slot"]; !exists {
		return fmt.Errorf("invalid JSON response: missing id_slot")
	} else if err := json.Unmarshal(raw, &slotID); err != nil || slotID != 0 {
		return fmt.Errorf("invalid JSON response: unexpected id_slot")
	}
	var returnedFilename string
	if raw, exists := payload["filename"]; !exists {
		return fmt.Errorf("invalid JSON response: missing filename")
	} else if err := json.Unmarshal(raw, &returnedFilename); err != nil || returnedFilename != filename {
		return fmt.Errorf("invalid JSON response: unexpected filename")
	}
	return nil
}

func slotEndpoint(backend *url.URL, action string) string {
	endpoint := *backend
	endpoint.Path = "/slots/0"
	endpoint.RawPath = ""
	endpoint.RawQuery = "action=" + url.QueryEscape(action)
	endpoint.Fragment = ""
	return endpoint.String()
}

func readSignature(path string) (kvCacheSignature, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return kvCacheSignature{}, err
	}
	var signature kvCacheSignature
	if err := json.Unmarshal(data, &signature); err != nil {
		return kvCacheSignature{}, err
	}
	return signature, nil
}

func writeSignatureAtomically(path string, signature kvCacheSignature) error {
	data, err := json.MarshalIndent(signature, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	directory := filepath.Dir(path)
	temp, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)

	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return replaceFile(tempPath, path)
}

func replaceFile(source, destination string) error {
	if runtime.GOOS == "windows" {
		if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Rename(source, destination)
}
