package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/artisticbones/hello-agent/config"
)

func loadSystemPrompt(cfg *config.Config) (string, error) {
	if cfg == nil || cfg.Prompt == nil || strings.TrimSpace(cfg.Prompt.SystemPromptFile) == "" {
		return "", nil
	}

	content, err := os.ReadFile(cfg.Prompt.SystemPromptFile)
	if err != nil {
		return "", fmt.Errorf("read system prompt file %q: %w", cfg.Prompt.SystemPromptFile, err)
	}
	prompt := strings.TrimSpace(string(content))
	if prompt == "" {
		return "", fmt.Errorf("system prompt file %q is empty", cfg.Prompt.SystemPromptFile)
	}
	return prompt, nil
}

// saveSystemPrompt persists a new system instruction atomically. The temporary
// file is created beside the target so Rename stays on the same filesystem.
func saveSystemPrompt(path, prompt string) error {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return fmt.Errorf("system prompt cannot be empty")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create system prompt directory: %w", err)
	}
	temporary, err := os.CreateTemp(dir, ".system-prompt-*")
	if err != nil {
		return fmt.Errorf("create temporary system prompt file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)

	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set system prompt permissions: %w", err)
	}
	if _, err := temporary.WriteString(prompt + "\n"); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write system prompt: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync system prompt: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary system prompt file: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("replace system prompt file: %w", err)
	}
	return nil
}
