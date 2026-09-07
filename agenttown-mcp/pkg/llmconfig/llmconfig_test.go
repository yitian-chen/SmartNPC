package llmconfig

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad_HostPort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llm.yaml")
	content := `host: "21.6.67.160"
port: 30000
model: "Qwen2.5-7B-Instruct-GPTQ-Int4"
strategic_model: "Qwen2.5-7B-Instruct-GPTQ-Int4"
timeout_sec: 120
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.ResolvedBaseURL(); got != "http://21.6.67.160:30000" {
		t.Errorf("ResolvedBaseURL = %q, want http://21.6.67.160:30000", got)
	}
	if cfg.Model != "Qwen2.5-7B-Instruct-GPTQ-Int4" {
		t.Errorf("Model = %q", cfg.Model)
	}
	if cfg.Timeout() != 120*time.Second {
		t.Errorf("Timeout = %v, want 120s", cfg.Timeout())
	}
}

func TestLoad_HostNoPort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llm.yaml")
	content := `host: "21.6.67.160"
model: "m"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.ResolvedBaseURL(); got != "http://21.6.67.160" {
		t.Errorf("ResolvedBaseURL = %q, want http://21.6.67.160", got)
	}
}

func TestLoad_BaseURLOverridesHostPort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llm.yaml")
	content := `host: "21.6.67.160"
port: 30000
base_url: "http://v2.open.venus.oa.com/llmproxy"
model: "m"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.ResolvedBaseURL(); got != "http://v2.open.venus.oa.com/llmproxy" {
		t.Errorf("ResolvedBaseURL = %q, want base_url (override host+port)", got)
	}
}

func TestLoad_MissingAddress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llm.yaml")
	if err := os.WriteFile(path, []byte("model: x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for missing base_url/host")
	}
}

func TestLoad_MissingModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llm.yaml")
	if err := os.WriteFile(path, []byte("host: x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for missing model")
	}
}
