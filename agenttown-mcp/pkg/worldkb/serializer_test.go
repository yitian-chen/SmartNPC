package worldkb

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteYAML_RoundTrip(t *testing.T) {
	kb, _, err := Merge(minimalGenerated(), minimalAuthored())
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	dir := t.TempDir()
	outPath := filepath.Join(dir, "world_kb.yaml")
	data, err := WriteYAML(kb, outPath)
	if err != nil {
		t.Fatalf("WriteYAML: %v", err)
	}

	// File should exist and match returned bytes.
	onDisk, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(data, onDisk) {
		t.Errorf("on-disk content != returned bytes: %d vs %d bytes", len(onDisk), len(data))
	}

	// Should contain expected keys.
	s := string(data)
	for _, want := range []string{
		"version:", "narrative:", "zones:", "objects:", "agents:",
		"display_name:", "bounds:", "extent:", "entry_point:",
		"interaction_point:", "zone_id:", "initial_zone:",
	} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("YAML missing key %q\n%s", want, s)
		}
	}
}

func TestWriteYAML_Deterministic(t *testing.T) {
	// Two merges of identical input must produce byte-identical output.
	kb1, _, err := Merge(minimalGenerated(), minimalAuthored())
	if err != nil {
		t.Fatalf("merge1: %v", err)
	}
	kb2, _, err := Merge(minimalGenerated(), minimalAuthored())
	if err != nil {
		t.Fatalf("merge2: %v", err)
	}

	dir := t.TempDir()
	data1, err := WriteYAML(kb1, filepath.Join(dir, "a.yaml"))
	if err != nil {
		t.Fatalf("write1: %v", err)
	}
	data2, err := WriteYAML(kb2, filepath.Join(dir, "b.yaml"))
	if err != nil {
		t.Fatalf("write2: %v", err)
	}
	if !bytes.Equal(data1, data2) {
		t.Errorf("YAML output not deterministic:\n--- v1 ---\n%s\n--- v2 ---\n%s", data1, data2)
	}
}

func TestWriteYAML_NilKB(t *testing.T) {
	_, err := WriteYAML(nil, filepath.Join(t.TempDir(), "x.yaml"))
	if err == nil {
		t.Errorf("expected error for nil kb")
	}
}

func TestWriteYAML_AtomicReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "world_kb.yaml")

	// Pre-write a stub to ensure it gets overwritten.
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatalf("prefill: %v", err)
	}

	kb, _, err := Merge(minimalGenerated(), minimalAuthored())
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if _, err := WriteYAML(kb, path); err != nil {
		t.Fatalf("WriteYAML: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if bytes.HasPrefix(got, []byte("stale")) {
		t.Errorf("file was not atomically replaced")
	}
}

func TestWriteManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	gen := []byte(`{"a":1}`)
	auth := []byte(`{"b":2}`)
	merged := []byte(`version: "1.0"`)

	if err := WriteManifest(gen, auth, merged, path, "/Game/Map"); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Manifest should be valid JSON with expected fields.
	s := string(data)
	for _, want := range []string{
		"schema_version", "generated_sha256", "authored_sha256",
		"merged_sha256", "source_map", "merged_at",
	} {
		if !contains(data, want) {
			t.Errorf("manifest missing %q: %s", want, s)
		}
	}
}

func contains(haystack []byte, needle string) bool {
	return bytes.Contains(haystack, []byte(needle))
}
