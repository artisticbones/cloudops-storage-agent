package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type ModelConfig struct {
	APIKey  string `json:"apiKey"`
	Name    string `json:"name"`
	BaseURL string `json:"baseUrl"`
}

type Config struct {
	MaxSteps       int           `json:"maxSteps"`
	MaxRetries     int           `json:"maxRetry"`
	RequestTimeout time.Duration `json:"requestTimeout"`
	Model          *ModelConfig  `json:"model"`
	Tool           *ToolConfig   `json:"tool"`
}

type ToolConfig struct {
	MaxToolRecoveries int `json:"maxToolRecoveries"`
}

var GlobalConfig *Config

const (
	defaultAPIKey            = "ollama"
	defaultModel             = "qwen3:1.7b"
	defaultBaseURL           = "http://localhost:11434/v1/"
	defaultMaxSteps          = 5
	defaultMaxRetries        = 2
	defaultRequestTimeout    = 3 * time.Minute
	defaultMaxToolRecoveries = 2
)

// LoadConfig loads configuration from environment variables and applies
// development-friendly defaults when an environment variable is absent.
//
// Supported variables:
//   - AGENT_API_KEY
//   - AGENT_MODEL
//   - AGENT_BASE_URL
//   - AGENT_MAX_STEPS
//   - AGENT_MAX_RETRIES
//   - AGENT_REQUEST_TIMEOUT (for example: 3m, 30s)
//   - AGENT_MAX_TOOL_RECOVERIES
func LoadConfig() (*Config, error) {
	cfg := &Config{
		MaxSteps:       defaultMaxSteps,
		MaxRetries:     defaultMaxRetries,
		RequestTimeout: defaultRequestTimeout,
		Model: &ModelConfig{
			APIKey:  getEnv("AGENT_API_KEY", defaultAPIKey),
			Name:    getEnv("AGENT_MODEL", defaultModel),
			BaseURL: getEnv("AGENT_BASE_URL", defaultBaseURL),
		},
		Tool: &ToolConfig{
			MaxToolRecoveries: defaultMaxToolRecoveries,
		},
	}

	var err error
	if cfg.MaxSteps, err = getIntEnv("AGENT_MAX_STEPS", defaultMaxSteps); err != nil {
		return nil, err
	}
	if cfg.MaxRetries, err = getIntEnv("AGENT_MAX_RETRIES", defaultMaxRetries); err != nil {
		return nil, err
	}
	if cfg.RequestTimeout, err = getDurationEnv("AGENT_REQUEST_TIMEOUT", defaultRequestTimeout); err != nil {
		return nil, err
	}
	if cfg.Tool.MaxToolRecoveries, err = getIntEnv("AGENT_MAX_TOOL_RECOVERIES", defaultMaxToolRecoveries); err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("config cannot be nil")
	}
	if c.Model == nil {
		return fmt.Errorf("model config cannot be nil")
	}
	if c.Tool == nil {
		return fmt.Errorf("tool config cannot be nil")
	}
	if strings.TrimSpace(c.Model.Name) == "" {
		return fmt.Errorf("model name cannot be empty")
	}
	if strings.TrimSpace(c.Model.BaseURL) == "" {
		return fmt.Errorf("model base URL cannot be empty")
	}
	if c.MaxSteps <= 0 {
		return fmt.Errorf("max steps must be positive")
	}
	if c.MaxRetries < 0 {
		return fmt.Errorf("max retries cannot be negative")
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("request timeout must be positive")
	}
	if c.Tool.MaxToolRecoveries < 0 {
		return fmt.Errorf("max tool recoveries cannot be negative")
	}
	return nil
}

func getEnv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func getIntEnv(key string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}

	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	return parsed, nil
}

func getDurationEnv(key string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}

	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	return parsed, nil
}
