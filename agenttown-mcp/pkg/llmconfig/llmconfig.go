// Package llmconfig loads the LLM inference backend configuration.
//
// Venus and a self-hosted SGLang service both speak the OpenAI Chat
// Completions protocol, so they share the same client and differ only in
// base_url / model / api_key. A YAML file lets operators switch backends
// (or swap models) without recompiling.
package llmconfig

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the LLM backend connection settings. Both the strategic and
// tactical layers share base_url/api_key/timeout; the model may differ per
// layer (StrategicModel empty → fall back to Model).
//
// The service address is given either as host + port (self-hosted SGLang) or
// as a full base_url (path-bearing services like Venus). base_url wins when
// both are present.
type Config struct {
	Host           string `yaml:"host"`
	Port           int    `yaml:"port"`
	BaseURL        string `yaml:"base_url"` // optional full URL (host:port/path), overrides host+port
	APIKey         string `yaml:"api_key"`
	Model          string `yaml:"model"`
	StrategicModel string `yaml:"strategic_model"`
	TimeoutSec     int    `yaml:"timeout_sec"`
}

// Timeout returns the per-call HTTP timeout, or 0 when unset (the client
// then uses its own default).
func (c *Config) Timeout() time.Duration {
	if c.TimeoutSec <= 0 {
		return 0
	}
	return time.Duration(c.TimeoutSec) * time.Second
}

// ResolvedBaseURL returns the service root URL: base_url when set, otherwise
// "http://host" (or "http://host:port" when Port > 0).
func (c *Config) ResolvedBaseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	if c.Host == "" {
		return ""
	}
	if c.Port > 0 {
		return fmt.Sprintf("http://%s:%d", c.Host, c.Port)
	}
	return "http://" + c.Host
}

// Load reads and parses the YAML config at path. The service address
// (base_url, or host) and model are required; port / api_key /
// strategic_model / timeout_sec are optional.
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
	if cfg.ResolvedBaseURL() == "" {
		return nil, fmt.Errorf("llm config %s: base_url or host is required", path)
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("llm config %s: model is required", path)
	}
	return &cfg, nil
}
