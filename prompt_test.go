package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/artisticbones/hello-agent/config"
)

func TestAgentInjectsPersistentSystemPrompt(t *testing.T) {
	promptPath := filepath.Join(t.TempDir(), "system.md")
	if err := os.WriteFile(promptPath, []byte("只使用中文回答"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	client := NewFakeModelClient(NewFakeTextResult("第一轮"), NewFakeTextResult("第二轮"))
	registry := NewToolRegistry()
	cfg := &config.Config{
		MaxSteps:       2,
		MaxRetries:     0,
		RequestTimeout: time.Second,
		Model:          &config.ModelConfig{Name: "fake", BaseURL: "fake://model"},
		Tool:           &config.ToolConfig{MaxToolRecoveries: 1},
		Prompt:         &config.PromptConfig{SystemPromptFile: promptPath},
	}
	agent, err := NewAgent(client, cfg, registry)
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}

	if _, err := agent.Run(context.Background(), "第一问"); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	if _, err := agent.Run(context.Background(), "第二问"); err != nil {
		t.Fatalf("second Run() error = %v", err)
	}

	second := client.ReceivedRequests[1].Messages
	if len(second) != 4 {
		t.Fatalf("second request messages = %d, want 4", len(second))
	}
	if second[0].Role != "system" || second[0].Content != "只使用中文回答" {
		t.Fatalf("system message = %+v", second[0])
	}
	for i, message := range second {
		if i > 0 && message.Role == "system" {
			t.Fatalf("system prompt should appear once, messages=%+v", second)
		}
	}
}

func TestSetSystemPromptPersistsAndTakesEffectImmediately(t *testing.T) {
	promptPath := filepath.Join(t.TempDir(), "system.md")
	if err := os.WriteFile(promptPath, []byte("旧指令\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	client := NewFakeModelClient(NewFakeTextResult("ok"))
	cfg := &config.Config{
		MaxSteps:       1,
		MaxRetries:     0,
		RequestTimeout: time.Second,
		Model:          &config.ModelConfig{Name: "fake", BaseURL: "fake://model"},
		Tool:           &config.ToolConfig{MaxToolRecoveries: 1},
		Prompt:         &config.PromptConfig{SystemPromptFile: promptPath},
	}
	agent, err := NewAgent(client, cfg, NewToolRegistry())
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}

	want := "最新系统指令：使用中文回答，不输出隐藏思考过程"
	if err := agent.SetSystemPrompt(want); err != nil {
		t.Fatalf("SetSystemPrompt() error = %v", err)
	}
	if agent.SystemPrompt() != want {
		t.Fatalf("SystemPrompt() = %q, want %q", agent.SystemPrompt(), want)
	}
	content, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != want+"\n" {
		t.Fatalf("persisted prompt = %q, want %q", string(content), want+"\n")
	}

	if _, err := agent.Run(context.Background(), "回答问题"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	message := client.ReceivedRequests[0].Messages[0]
	if message.Role != "system" || message.Content != want {
		t.Fatalf("system message = %+v", message)
	}
}

func TestAddSystemPromptAppendsAndTakesEffectImmediately(t *testing.T) {
	promptPath := filepath.Join(t.TempDir(), "system.md")
	if err := os.WriteFile(promptPath, []byte("基础指令\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	client := NewFakeModelClient(NewFakeTextResult("ok"))
	cfg := &config.Config{
		MaxSteps:       1,
		MaxRetries:     0,
		RequestTimeout: time.Second,
		Model:          &config.ModelConfig{Name: "fake", BaseURL: "fake://model"},
		Tool:           &config.ToolConfig{MaxToolRecoveries: 1},
		Prompt:         &config.PromptConfig{SystemPromptFile: promptPath},
	}
	agent, err := NewAgent(client, cfg, NewToolRegistry())
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}

	if err := agent.AddSystemPrompt("所有最终回答使用中文"); err != nil {
		t.Fatalf("AddSystemPrompt() error = %v", err)
	}
	want := "基础指令\n所有最终回答使用中文"
	if agent.SystemPrompt() != want {
		t.Fatalf("SystemPrompt() = %q, want %q", agent.SystemPrompt(), want)
	}
	content, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != want+"\n" {
		t.Fatalf("persisted prompt = %q, want %q", string(content), want+"\n")
	}

	if _, err := agent.Run(context.Background(), "回答问题"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	message := client.ReceivedRequests[0].Messages[0]
	if message.Role != "system" || message.Content != want {
		t.Fatalf("system message = %+v", message)
	}
}

func TestModelCanAppendSystemPromptThroughToolCall(t *testing.T) {
	promptPath := filepath.Join(t.TempDir(), "system.md")
	if err := os.WriteFile(promptPath, []byte("基础指令\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	client := NewFakeModelClient(
		FakeModelResult{Response: &ModelResponse{ToolCalls: []ToolCall{{
			ID:        "call_system_1",
			Name:      "update_system_prompt",
			Arguments: `{"instruction":"最终回答使用中文"}`,
		}}}},
		NewFakeTextResult("已记住"),
	)
	var agent *Agent
	registry := NewToolRegistry()
	if err := registry.Register(systemPromptTool(&agent)); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	cfg := &config.Config{
		MaxSteps:       2,
		MaxRetries:     0,
		RequestTimeout: time.Second,
		Model:          &config.ModelConfig{Name: "fake", BaseURL: "fake://model"},
		Tool:           &config.ToolConfig{MaxToolRecoveries: 1},
		Prompt:         &config.PromptConfig{SystemPromptFile: promptPath},
	}
	var err error
	agent, err = NewAgent(client, cfg, registry)
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}

	if _, err := agent.Run(context.Background(), "以后请使用中文回答"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if agent.SystemPrompt() != "基础指令\n最终回答使用中文" {
		t.Fatalf("SystemPrompt() = %q", agent.SystemPrompt())
	}
	if len(client.ReceivedRequests) != 2 {
		t.Fatalf("model calls = %d, want 2", len(client.ReceivedRequests))
	}
	secondSystem := client.ReceivedRequests[1].Messages[0]
	if secondSystem.Role != "system" || secondSystem.Content != agent.SystemPrompt() {
		t.Fatalf("second system message = %+v", secondSystem)
	}
}
