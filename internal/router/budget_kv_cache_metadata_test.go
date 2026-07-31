package router

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestKVCache_MetadataRoundTrip(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "model.json")
	signature := kvCacheSignature{
		ModelID:     "model",
		ModelPath:   "/models/model.gguf",
		ContextSize: 131072,
		Parallel:    2,
		CacheTypeK:  "q8_0",
		CacheTypeV:  "q8_0",
		KVUnified:   true,
	}
	metadata := newKVCacheMetadata(signature, []kvCacheSlotMetadata{
		{SlotID: 0, Filename: "model-slot-0.bin", Saved: true},
		{SlotID: 1, Filename: "model-slot-1.bin", Saved: false},
	})
	if err := writeMetadataAtomically(path, metadata); err != nil {
		t.Fatalf("writeMetadataAtomically: %v", err)
	}

	got, err := readKVCacheMetadata(path)
	if err != nil {
		t.Fatalf("readKVCacheMetadata: %v", err)
	}
	if got.Version != kvCacheMetadataVersion || got.signature() != signature {
		t.Errorf("metadata=%+v signature=%+v", got, got.signature())
	}
	if len(got.Slots) != 2 || !got.Slots[0].Saved || got.Slots[1].Saved {
		t.Errorf("slots=%+v", got.Slots)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "model.json" {
		t.Errorf("atomic write left temporary files: %v", entries)
	}
}

func TestKVCache_LegacyMetadataSupportsOnlyParallelOne(t *testing.T) {
	for _, parallel := range []int{1, 2} {
		t.Run(fmt.Sprintf("parallel_%d", parallel), func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "model.json")
			data, err := json.Marshal(kvCacheSignature{
				ModelID:     "model",
				ModelPath:   "/models/model.gguf",
				ContextSize: 65536,
				Parallel:    parallel,
				CacheTypeK:  "q8_0",
				CacheTypeV:  "q8_0",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}

			metadata, readErr := readKVCacheMetadata(path)
			if parallel == 2 {
				if readErr == nil {
					t.Fatalf("legacy parallel 2 unexpectedly accepted: %+v", metadata)
				}
				return
			}
			if readErr != nil {
				t.Fatalf("read legacy metadata: %v", readErr)
			}
			if metadata.Version != 1 || len(metadata.Slots) != 1 || metadata.Slots[0].Filename != "model.bin" {
				t.Errorf("legacy metadata=%+v", metadata)
			}
		})
	}
}

func TestKVCache_MetadataRejectsInvalidSlots(t *testing.T) {
	tests := []struct {
		name  string
		slots []kvCacheSlotMetadata
	}{
		{"missing slot", []kvCacheSlotMetadata{{SlotID: 0, Filename: "model-slot-0.bin", Saved: true}}},
		{"duplicate slot", []kvCacheSlotMetadata{{SlotID: 0, Filename: "model-slot-0.bin"}, {SlotID: 0, Filename: "model-slot-0.bin"}}},
		{"unsafe filename", []kvCacheSlotMetadata{{SlotID: 0, Filename: "../model-slot-0.bin"}, {SlotID: 1, Filename: "model-slot-1.bin"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metadata := kvCacheMetadata{Version: 2, ModelID: "model", Parallel: 2, Slots: test.slots}
			if err := metadata.validate(); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
}

func TestKVCache_MetadataRejectsUnsafeModelID(t *testing.T) {
	metadata := kvCacheMetadata{
		Version:  2,
		ModelID:  "../model",
		Parallel: 1,
		Slots: []kvCacheSlotMetadata{
			{SlotID: 0, Filename: "../model-slot-0.bin", Saved: true},
		},
	}
	if err := metadata.validate(); err == nil {
		t.Fatal("unsafe model ID accepted")
	}
}

func TestKVCache_CleanupObsoleteSlotFiles(t *testing.T) {
	directory := t.TempDir()
	files := []string{
		"model-slot-0.bin",
		"model-slot-1.bin",
		"model-slot-2.bin",
		"model-slot-3.bin.tmp",
		"other-slot-9.bin",
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanupObsoleteSlotFiles(directory, "model", 2); err != nil {
		t.Fatalf("cleanupObsoleteSlotFiles: %v", err)
	}
	for _, name := range []string{"model-slot-0.bin", "model-slot-1.bin", "other-slot-9.bin"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Errorf("expected %s to remain: %v", name, err)
		}
	}
	for _, name := range []string{"model-slot-2.bin", "model-slot-3.bin.tmp"} {
		if _, err := os.Stat(filepath.Join(directory, name)); !os.IsNotExist(err) {
			t.Errorf("expected %s removed, got %v", name, err)
		}
	}
}
