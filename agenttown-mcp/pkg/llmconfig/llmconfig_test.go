package llmconfig

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llm.yaml")
	content := `strategic:
  base_url: "http://v2.open.venus.oa.com/llmproxy"
  model: "deepseek-v4-pro"
tactical:
  host: "21.6.67.160"
  port: 8000
  model: "Qwen2.5-7B-Instruct-GPTQ-Int4"
  timeout_sec: 120
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Strategic.ResolvedBaseURL(); got != "http://v2.open.venus.oa.com/llmproxy" {
		t.Errorf("strategic base URL = %q", got)
	}
	if cfg.Strategic.Model != "deepseek-v4-pro" {
		t.Errorf("strategic model = %q", cfg.Strategic.Model)
	}
	if got := cfg.Tactical.ResolvedBaseURL(); got != "http://21.6.67.160:8000" {
		t.Errorf("tactical base URL = %q", got)
	}
	if cfg.Tactical.Model != "Qwen2.5-7B-Instruct-GPTQ-Int4" {
		t.Errorf("tactical model = %q", cfg.Tactical.Model)
	}
	if cfg.Tactical.Timeout() != 120*time.Second {
		t.Errorf("tactical timeout = %v, want 120s", cfg.Tactical.Timeout())
	}
}

func TestResolvedBaseURL(t *testing.T) {
	b := Backend{Host: "h", Port: 8000}
	if got := b.ResolvedBaseURL(); got != "http://h:8000" {
		t.Errorf("host+port = %q", got)
	}
	b = Backend{Host: "h"}
	if got := b.ResolvedBaseURL(); got != "http://h" {
		t.Errorf("host only = %q", got)
	}
	b = Backend{Host: "h", Port: 8000, BaseURL: "http://x/y"}
	if got := b.ResolvedBaseURL(); got != "http://x/y" {
		t.Errorf("base_url override = %q", got)
	}
}

func TestLoad_MissingStrategic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llm.yaml")
	content := `tactical:
  host: "h"
  model: "m"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for missing strategic backend")
	}
}

func TestLoad_MissingTactical(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llm.yaml")
	content := `strategic:
  host: "h"
  model: "m"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for missing tactical backend")
	}
}

func TestLoad_MissingModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llm.yaml")
	content := `strategic:
  host: "h"
  model: "m"
tactical:
  host: "h"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for missing tactical model")
	}
}
