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
	content := `base_url: "http://21.6.67.160:30000"
api_key: ""
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
	if cfg.BaseURL != "http://21.6.67.160:30000" {
		t.Errorf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.Model != "Qwen2.5-7B-Instruct-GPTQ-Int4" {
		t.Errorf("Model = %q", cfg.Model)
	}
	if cfg.StrategicModel != "Qwen2.5-7B-Instruct-GPTQ-Int4" {
		t.Errorf("StrategicModel = %q", cfg.StrategicModel)
	}
	if cfg.Timeout() != 120*time.Second {
		t.Errorf("Timeout = %v, want 120s", cfg.Timeout())
	}
}

func TestLoad_StrategicModelEmptyFallsBackToModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llm.yaml")
	content := `base_url: "http://21.6.67.160:30000"
model: "Qwen2.5-7B-Instruct-GPTQ-Int4"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// strategic_model 留空 → 上层应回退到 model；此处只验证解析不报错。
	if cfg.StrategicModel != "" {
		t.Errorf("StrategicModel = %q, want empty (fallback handled by caller)", cfg.StrategicModel)
	}
}

func TestLoad_MissingBaseURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llm.yaml")
	if err := os.WriteFile(path, []byte("model: x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for missing base_url")
	}
}

func TestLoad_MissingModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llm.yaml")
	if err := os.WriteFile(path, []byte("base_url: http://x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for missing model")
	}
}
