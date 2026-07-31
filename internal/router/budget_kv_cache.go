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
	"sync"
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

	// Shared by every concurrent victim save so max_parallel_saves caps total
	// cache-file I/O, not just the slots of one model.
	saveSemaphore chan struct{}
}

type slotActionResponse struct {
	IDSlot    *int    `json:"id_slot"`
	Filename  *string `json:"filename"`
	NSaved    *int    `json:"n_saved"`
	NRestored *int    `json:"n_restored"`
}

type slotSaveResult struct {
	metadata kvCacheSlotMetadata
	duration time.Duration
	empty    bool
	err      error
}

func newKVCacheLifecycle(settings config.BudgetKVCacheConfig, models map[string]config.ModelConfig, logger *logmon.Monitor) *kvCacheLifecycle {
	inspected := make(map[string]kvCacheModel, len(models))
	for modelID, model := range models {
		inspected[modelID] = inspectKVCacheModel(modelID, model, settings.Directory)
	}
	saveLimit := settings.MaxParallelSaves
	if saveLimit < 1 {
		saveLimit = 1
	}
	return &kvCacheLifecycle{
		config:        settings,
		models:        inspected,
		logger:        logger,
		client:        &http.Client{},
		saveSemaphore: make(chan struct{}, saveLimit),
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

	metadataPath := filepath.Join(l.config.Directory, modelID+".json")
	if err := os.Remove(metadataPath); err != nil && !os.IsNotExist(err) {
		// Do not replace any slot files while stale metadata could remain and
		// describe a mixture of cache generations.
		l.logger.Warnf("kv-cache: save failed model=%s error=invalidating old metadata: %v", modelID, err)
		return
	}

	started := time.Now()
	l.logger.Infof("kv-cache: saving model=%s slots=%d", modelID, model.signature.Parallel)
	ctx, cancel := context.WithTimeout(parent, l.config.SaveTimeout)
	defer cancel()

	results := l.saveSlots(ctx, modelID, model)
	metadataSlots := make([]kvCacheSlotMetadata, len(results))
	saved, failed, empty := 0, 0, 0
	for slotID, result := range results {
		metadataSlots[slotID] = result.metadata
		finalPath := filepath.Join(l.config.Directory, result.metadata.Filename)
		switch {
		case result.err != nil:
			failed++
			l.logger.Warnf("kv-cache: save failed model=%s slot=%d error=%v", modelID, slotID, result.err)
		case result.metadata.Saved:
			saved++
			if result.empty {
				empty++
				l.logger.Infof("kv-cache: saved model=%s slot=%d file=%s empty=true duration=%s",
					modelID, slotID, finalPath, result.duration.Round(time.Millisecond))
				break
			}
			l.logger.Infof("kv-cache: saved model=%s slot=%d file=%s duration=%s",
				modelID, slotID, finalPath, result.duration.Round(time.Millisecond))
		default:
			empty++
			l.logger.Infof("kv-cache: skipped save model=%s slot=%d reason=empty-slot", modelID, slotID)
		}
	}

	metadata := newKVCacheMetadata(model.signature, metadataSlots)
	if err := writeMetadataAtomically(metadataPath, metadata); err != nil {
		// The previous generation was invalidated before slot publication, so
		// none of the just-published binaries can be restored without this file.
		l.logger.Warnf("kv-cache: save metadata failed model=%s error=%v", modelID, err)
	}
	if err := cleanupObsoleteSlotFiles(l.config.Directory, modelID, model.signature.Parallel); err != nil {
		l.logger.Warnf("kv-cache: stale slot cleanup failed model=%s error=%v", modelID, err)
	}

	l.logger.Infof("kv-cache: save complete model=%s saved=%d failed=%d empty=%d duration=%s",
		modelID, saved, failed, empty, time.Since(started).Round(time.Millisecond))
}

func (l *kvCacheLifecycle) saveSlots(ctx context.Context, modelID string, model kvCacheModel) []slotSaveResult {
	results := make([]slotSaveResult, model.signature.Parallel)

	var wg sync.WaitGroup
	for slotID := range model.signature.Parallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := slotSaveResult{
				metadata: kvCacheSlotMetadata{
					SlotID:   slotID,
					Filename: kvCacheSlotFilename(modelID, slotID),
				},
			}
			select {
			case l.saveSemaphore <- struct{}{}:
				defer func() { <-l.saveSemaphore }()
			case <-ctx.Done():
				result.err = ctx.Err()
				results[slotID] = result
				return
			}
			results[slotID] = l.saveSlot(ctx, model, result.metadata)
		}()
	}
	wg.Wait()
	return results
}

func (l *kvCacheLifecycle) saveSlot(ctx context.Context, model kvCacheModel, metadata kvCacheSlotMetadata) slotSaveResult {
	result := slotSaveResult{metadata: metadata}
	tempFilename := metadata.Filename + ".tmp"
	tempPath := filepath.Join(l.config.Directory, tempFilename)
	finalPath := filepath.Join(l.config.Directory, metadata.Filename)
	_ = os.Remove(tempPath)
	defer os.Remove(tempPath)

	started := time.Now()
	response, err := l.slotAction(ctx, model, metadata.SlotID, "save", tempFilename)
	result.duration = time.Since(started)
	if err != nil {
		result.err = err
		return result
	}
	result.empty = response.NSaved != nil && *response.NSaved == 0

	info, err := os.Lstat(tempPath)
	if err != nil {
		if result.empty && os.IsNotExist(err) {
			// Some llama-server versions acknowledge an empty slot without
			// materializing a file. This is a successful skip, not a model-level
			// save failure.
			return result
		}
		result.err = fmt.Errorf("cache file %s unavailable after save: %w", tempPath, err)
		return result
	}
	if !info.Mode().IsRegular() {
		result.err = fmt.Errorf("cache file %s is not regular", tempPath)
		return result
	}
	if err := replaceFile(tempPath, finalPath); err != nil {
		result.err = fmt.Errorf("publishing cache file: %w", err)
		return result
	}
	result.metadata.Saved = true
	return result
}

func (l *kvCacheLifecycle) AfterModelStart(parent context.Context, modelID string) {
	model, ok := l.usableModel(modelID)
	if !ok {
		return
	}

	metadataPath := filepath.Join(l.config.Directory, modelID+".json")
	metadata, err := readKVCacheMetadata(metadataPath)
	if err != nil {
		if os.IsNotExist(err) {
			l.logger.Debugf("kv-cache: skipped restore model=%s reason=cache-missing", modelID)
		} else {
			l.logger.Warnf("kv-cache: skipped restore model=%s reason=metadata-invalid error=%v", modelID, err)
		}
		return
	}
	if field, cachedValue, currentValue, mismatch := signatureMismatch(metadata.signature(), model.signature); mismatch {
		l.logger.Warnf("kv-cache: skipped restore model=%s reason=config-mismatch field=%s cached=%s current=%s",
			modelID, field, cachedValue, currentValue)
		return
	}

	slots := make([]kvCacheSlotMetadata, 0, len(metadata.Slots))
	for _, slot := range metadata.Slots {
		if slot.Saved {
			slots = append(slots, slot)
		}
	}
	if len(slots) == 0 {
		l.logger.Debugf("kv-cache: skipped restore model=%s reason=no-saved-slots", modelID)
		return
	}

	started := time.Now()
	l.logger.Infof("kv-cache: restoring model=%s slots=%d", modelID, len(slots))
	ctx, cancel := context.WithTimeout(parent, l.config.RestoreTimeout)
	defer cancel()

	restored, failed := 0, 0
	for _, slot := range slots {
		cachePath := filepath.Join(l.config.Directory, slot.Filename)
		if ctx.Err() != nil {
			failed++
			l.logger.Warnf("kv-cache: restore failed model=%s slot=%d error=%v", modelID, slot.SlotID, ctx.Err())
			continue
		}
		if info, statErr := os.Lstat(cachePath); statErr != nil {
			failed++
			l.logger.Warnf("kv-cache: restore failed model=%s slot=%d error=cache file unavailable: %v",
				modelID, slot.SlotID, statErr)
			continue
		} else if !info.Mode().IsRegular() {
			failed++
			l.logger.Warnf("kv-cache: restore failed model=%s slot=%d error=cache file is not regular", modelID, slot.SlotID)
			continue
		}

		slotStarted := time.Now()
		if _, restoreErr := l.slotAction(ctx, model, slot.SlotID, "restore", slot.Filename); restoreErr != nil {
			failed++
			l.logger.Warnf("kv-cache: restore failed model=%s slot=%d error=%v", modelID, slot.SlotID, restoreErr)
			continue
		}
		restored++
		l.logger.Infof("kv-cache: restored model=%s slot=%d file=%s duration=%s",
			modelID, slot.SlotID, cachePath, time.Since(slotStarted).Round(time.Millisecond))
	}

	l.logger.Infof("kv-cache: restore complete model=%s restored=%d failed=%d duration=%s",
		modelID, restored, failed, time.Since(started).Round(time.Millisecond))
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

func (l *kvCacheLifecycle) slotAction(ctx context.Context, model kvCacheModel, slotID int, action, filename string) (slotActionResponse, error) {
	endpoint := slotEndpoint(model.backend, slotID, action)
	body, err := json.Marshal(map[string]string{"filename": filename})
	if err != nil {
		return slotActionResponse{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return slotActionResponse{}, err
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := l.client.Do(request)
	if err != nil {
		return slotActionResponse{}, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxSlotResponseBytes))
	if err != nil {
		return slotActionResponse{}, fmt.Errorf("reading response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return slotActionResponse{}, fmt.Errorf("POST %s returned HTTP %d: %s", endpoint, response.StatusCode, strings.TrimSpace(string(responseBody)))
	}

	var payload slotActionResponse
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return slotActionResponse{}, fmt.Errorf("invalid JSON response: %w", err)
	}
	if payload.IDSlot == nil {
		return slotActionResponse{}, fmt.Errorf("invalid JSON response: missing id_slot")
	}
	if *payload.IDSlot != slotID {
		return slotActionResponse{}, fmt.Errorf("invalid JSON response: unexpected id_slot %d", *payload.IDSlot)
	}
	if payload.Filename == nil {
		return slotActionResponse{}, fmt.Errorf("invalid JSON response: missing filename")
	}
	if *payload.Filename != filename {
		return slotActionResponse{}, fmt.Errorf("invalid JSON response: unexpected filename %q", *payload.Filename)
	}
	return payload, nil
}

func slotEndpoint(backend *url.URL, slotID int, action string) string {
	endpoint := *backend
	endpoint.Path = fmt.Sprintf("/slots/%d", slotID)
	endpoint.RawPath = ""
	endpoint.RawQuery = "action=" + url.QueryEscape(action)
	endpoint.Fragment = ""
	return endpoint.String()
}

func replaceFile(source, destination string) error {
	if runtime.GOOS == "windows" {
		if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Rename(source, destination)
}
