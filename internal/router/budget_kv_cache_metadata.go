package router

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const kvCacheMetadataVersion = 2

type kvCacheSlotMetadata struct {
	SlotID   int    `json:"slot_id"`
	Filename string `json:"filename"`
	Saved    bool   `json:"saved"`
}

type kvCacheMetadata struct {
	Version     int                   `json:"version"`
	ModelID     string                `json:"model_id"`
	ModelPath   string                `json:"model_path"`
	ContextSize int                   `json:"context_size"`
	Parallel    int                   `json:"parallel"`
	CacheTypeK  string                `json:"cache_type_k"`
	CacheTypeV  string                `json:"cache_type_v"`
	KVUnified   bool                  `json:"kv_unified"`
	Slots       []kvCacheSlotMetadata `json:"slots"`
}

func newKVCacheMetadata(signature kvCacheSignature, slots []kvCacheSlotMetadata) kvCacheMetadata {
	return kvCacheMetadata{
		Version:     kvCacheMetadataVersion,
		ModelID:     signature.ModelID,
		ModelPath:   signature.ModelPath,
		ContextSize: signature.ContextSize,
		Parallel:    signature.Parallel,
		CacheTypeK:  signature.CacheTypeK,
		CacheTypeV:  signature.CacheTypeV,
		KVUnified:   signature.KVUnified,
		Slots:       slots,
	}
}

func (m kvCacheMetadata) signature() kvCacheSignature {
	return kvCacheSignature{
		ModelID:     m.ModelID,
		ModelPath:   m.ModelPath,
		ContextSize: m.ContextSize,
		Parallel:    m.Parallel,
		CacheTypeK:  m.CacheTypeK,
		CacheTypeV:  m.CacheTypeV,
		KVUnified:   m.KVUnified,
	}
}

func readKVCacheMetadata(path string) (kvCacheMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return kvCacheMetadata{}, err
	}

	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return kvCacheMetadata{}, err
	}
	if header.Version == 0 {
		return readLegacyKVCacheMetadata(data)
	}
	if header.Version != kvCacheMetadataVersion {
		return kvCacheMetadata{}, fmt.Errorf("unsupported metadata version %d", header.Version)
	}

	var metadata kvCacheMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return kvCacheMetadata{}, err
	}
	if err := metadata.validate(); err != nil {
		return kvCacheMetadata{}, err
	}
	return metadata, nil
}

func readLegacyKVCacheMetadata(data []byte) (kvCacheMetadata, error) {
	var signature kvCacheSignature
	if err := json.Unmarshal(data, &signature); err != nil {
		return kvCacheMetadata{}, err
	}
	if signature.Parallel != 1 {
		return kvCacheMetadata{}, fmt.Errorf("legacy metadata requires parallel 1, got %d", signature.Parallel)
	}
	if !safeCacheModelID(signature.ModelID) {
		return kvCacheMetadata{}, fmt.Errorf("legacy metadata contains unsafe model ID")
	}
	return kvCacheMetadata{
		Version:     1,
		ModelID:     signature.ModelID,
		ModelPath:   signature.ModelPath,
		ContextSize: signature.ContextSize,
		Parallel:    signature.Parallel,
		CacheTypeK:  signature.CacheTypeK,
		CacheTypeV:  signature.CacheTypeV,
		KVUnified:   false,
		Slots: []kvCacheSlotMetadata{
			{SlotID: 0, Filename: signature.ModelID + ".bin", Saved: true},
		},
	}, nil
}

func (m kvCacheMetadata) validate() error {
	if !safeCacheModelID(m.ModelID) {
		return fmt.Errorf("metadata contains unsafe model ID")
	}
	if m.Parallel < 1 {
		return fmt.Errorf("parallel must be positive, got %d", m.Parallel)
	}
	if len(m.Slots) != m.Parallel {
		return fmt.Errorf("slot metadata count %d does not match parallel %d", len(m.Slots), m.Parallel)
	}
	seen := make(map[int]struct{}, len(m.Slots))
	for _, slot := range m.Slots {
		if slot.SlotID < 0 || slot.SlotID >= m.Parallel {
			return fmt.Errorf("slot ID %d outside parallel range", slot.SlotID)
		}
		if _, exists := seen[slot.SlotID]; exists {
			return fmt.Errorf("duplicate slot ID %d", slot.SlotID)
		}
		seen[slot.SlotID] = struct{}{}
		if filepath.Base(slot.Filename) != slot.Filename || strings.ContainsAny(slot.Filename, `/\\`) {
			return fmt.Errorf("unsafe filename for slot %d", slot.SlotID)
		}
		expected := kvCacheSlotFilename(m.ModelID, slot.SlotID)
		if slot.Filename != expected {
			return fmt.Errorf("unexpected filename for slot %d: %q", slot.SlotID, slot.Filename)
		}
	}
	sort.Slice(m.Slots, func(i, j int) bool { return m.Slots[i].SlotID < m.Slots[j].SlotID })
	return nil
}

func kvCacheSlotFilename(modelID string, slotID int) string {
	return modelID + "-slot-" + strconv.Itoa(slotID) + ".bin"
}

func writeMetadataAtomically(path string, metadata kvCacheMetadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
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

func cleanupObsoleteSlotFiles(directory, modelID string, parallel int) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	prefix := modelID + "-slot-"
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(name, prefix)
		suffix = strings.TrimSuffix(suffix, ".tmp")
		if !strings.HasSuffix(suffix, ".bin") {
			continue
		}
		slotID, parseErr := strconv.Atoi(strings.TrimSuffix(suffix, ".bin"))
		if parseErr != nil || slotID < parallel {
			continue
		}
		if removeErr := os.Remove(filepath.Join(directory, name)); removeErr != nil && !os.IsNotExist(removeErr) {
			return removeErr
		}
	}
	return nil
}
