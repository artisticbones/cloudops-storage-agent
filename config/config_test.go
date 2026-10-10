package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadConfigUsesDefaults(t *testing.T) {
	clearConfigEnv(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if cfg.MaxSteps != defaultMaxSteps {
		t.Fatalf("MaxSteps = %d, want %d", cfg.MaxSteps, defaultMaxSteps)
	}
	if cfg.MaxRetries != defaultMaxRetries {
		t.Fatalf("MaxRetries = %d, want %d", cfg.MaxRetries, defaultMaxRetries)
	}
	if cfg.RequestTimeout != defaultRequestTimeout {
		t.Fatalf("RequestTimeout = %s, want %s", cfg.RequestTimeout, defaultRequestTimeout)
	}
	if cfg.Model.Name != defaultModel || cfg.Model.BaseURL != defaultBaseURL {
		t.Fatalf("unexpected model defaults: %+v", cfg.Model)
	}
	if cfg.Tool.MaxToolRecoveries != defaultMaxToolRecoveries {
		t.Fatalf("MaxToolRecoveries = %d, want %d", cfg.Tool.MaxToolRecoveries, defaultMaxToolRecoveries)
	}
	if cfg.Diagnostics == nil || cfg.Diagnostics.PprofAddr != defaultPprofAddr {
		t.Fatalf("unexpected diagnostics defaults: %+v", cfg.Diagnostics)
	}
	if cfg.Prompt == nil || cfg.Prompt.SystemPromptFile != defaultSystemPromptFile {
		t.Fatalf("unexpected prompt defaults: %+v", cfg.Prompt)
	}
}

func TestLoadConfigReadsEnvironment(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("AGENT_API_KEY", "test-key")
	t.Setenv("AGENT_MODEL", "test-model")
	t.Setenv("AGENT_BASE_URL", "http://example.com/v1/")
	t.Setenv("AGENT_MAX_STEPS", "8")
	t.Setenv("AGENT_MAX_RETRIES", "3")
	t.Setenv("AGENT_REQUEST_TIMEOUT", "45s")
	t.Setenv("AGENT_MAX_TOOL_RECOVERIES", "4")
	t.Setenv("AGENT_PPROF_ADDR", "127.0.0.1:6060")
	t.Setenv("AGENT_SYSTEM_PROMPT_FILE", "test-system.md")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if cfg.Model.APIKey != "test-key" || cfg.Model.Name != "test-model" || cfg.Model.BaseURL != "http://example.com/v1/" {
		t.Fatalf("unexpected model config: %+v", cfg.Model)
	}
	if cfg.MaxSteps != 8 || cfg.MaxRetries != 3 || cfg.RequestTimeout != 45*time.Second || cfg.Tool.MaxToolRecoveries != 4 {
		t.Fatalf("unexpected runtime config: %+v", cfg)
	}
	if cfg.Diagnostics.PprofAddr != "127.0.0.1:6060" {
		t.Fatalf("PprofAddr = %q, want %q", cfg.Diagnostics.PprofAddr, "127.0.0.1:6060")
	}
	if cfg.Prompt.SystemPromptFile != "test-system.md" {
		t.Fatalf("SystemPromptFile = %q, want %q", cfg.Prompt.SystemPromptFile, "test-system.md")
	}
}

func TestLoadConfigRejectsInvalidEnvironment(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("AGENT_MAX_STEPS", "not-a-number")

	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "parse AGENT_MAX_STEPS") {
		t.Fatalf("error = %v, want invalid max steps error", err)
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
	}{
		{name: "nil config", cfg: nil},
		{name: "nil model", cfg: &Config{Tool: &ToolConfig{}}},
		{name: "nil tool", cfg: &Config{Model: &ModelConfig{}}},
		{name: "empty model name", cfg: validConfig(func(c *Config) { c.Model.Name = " " })},
		{name: "empty base url", cfg: validConfig(func(c *Config) { c.Model.BaseURL = "" })},
		{name: "zero max steps", cfg: validConfig(func(c *Config) { c.MaxSteps = 0 })},
		{name: "negative retries", cfg: validConfig(func(c *Config) { c.MaxRetries = -1 })},
		{name: "zero timeout", cfg: validConfig(func(c *Config) { c.RequestTimeout = 0 })},
		{name: "negative recoveries", cfg: validConfig(func(c *Config) { c.Tool.MaxToolRecoveries = -1 })},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
		})
	}
}

func validConfig(mutators ...func(*Config)) *Config {
	cfg := &Config{
		MaxSteps:       5,
		MaxRetries:     2,
		RequestTimeout: time.Minute,
		Model:          &ModelConfig{Name: "model", BaseURL: "http://localhost"},
		Tool:           &ToolConfig{MaxToolRecoveries: 2},
		Diagnostics:    &DiagnosticsConfig{},
	}
	for _, mutate := range mutators {
		mutate(cfg)
	}
	return cfg
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"AGENT_API_KEY",
		"AGENT_MODEL",
		"AGENT_BASE_URL",
		"AGENT_MAX_STEPS",
		"AGENT_MAX_RETRIES",
		"AGENT_REQUEST_TIMEOUT",
		"AGENT_MAX_TOOL_RECOVERIES",
		"AGENT_PPROF_ADDR",
		"AGENT_SYSTEM_PROMPT_FILE",
	} {
		t.Setenv(key, "")
	}
}
