// Package llmconfig loads the per-layer LLM inference backend configuration.
//
// The strategic layer and the tactical/dialogue layers point at separate
// Backends (Venus or a self-hosted OpenAI-compatible service). This lets
// operators, for example, keep the strategic layer on a strong hosted model
// (Venus DeepSeek-v4-pro) while running the higher-frequency tactical and
// dialogue layers on a self-hosted service.
package llmconfig

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Backend is one inference endpoint. The address is given either as
// host + port (self-hosted service) or as a full base_url (path-bearing
// services like Venus); base_url wins when both are present.
type Backend struct {
	Host       string `yaml:"host"`
	Port       int    `yaml:"port"`
	BaseURL    string `yaml:"base_url"`
	APIKey     string `yaml:"api_key"`
	Model      string `yaml:"model"`
	TimeoutSec int    `yaml:"timeout_sec"`
}

// Timeout returns the per-call HTTP timeout, or 0 when unset (the client
// then uses its own default).
func (b *Backend) Timeout() time.Duration {
	if b.TimeoutSec <= 0 {
		return 0
	}
	return time.Duration(b.TimeoutSec) * time.Second
}

// ResolvedBaseURL returns the service root URL: base_url when set, otherwise
// "http://host" (or "http://host:port" when Port > 0).
func (b *Backend) ResolvedBaseURL() string {
	if b.BaseURL != "" {
		return b.BaseURL
	}
	if b.Host == "" {
		return ""
	}
	if b.Port > 0 {
		return fmt.Sprintf("http://%s:%d", b.Host, b.Port)
	}
	return "http://" + b.Host
}

// Config holds the per-layer inference backends. The dialogue layer shares
// the tactical backend's client.
type Config struct {
	Strategic Backend `yaml:"strategic"`
	Tactical  Backend `yaml:"tactical"`
}

// Load reads and parses the YAML config at path. Both backends must specify
// an address (base_url or host) and a model; api_key / timeout_sec are
// optional.
//
// Note: the address is the service root — the client appends
// /v1/chat/completions, so do not include a /v1 suffix.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read llm config %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse llm config %s: %w", path, err)
	}
	if cfg.Strategic.ResolvedBaseURL() == "" {
		return nil, fmt.Errorf("llm config %s: strategic.base_url or strategic.host is required", path)
	}
	if cfg.Strategic.Model == "" {
		return nil, fmt.Errorf("llm config %s: strategic.model is required", path)
	}
	if cfg.Tactical.ResolvedBaseURL() == "" {
		return nil, fmt.Errorf("llm config %s: tactical.base_url or tactical.host is required", path)
	}
	if cfg.Tactical.Model == "" {
		return nil, fmt.Errorf("llm config %s: tactical.model is required", path)
	}
	return &cfg, nil
}
