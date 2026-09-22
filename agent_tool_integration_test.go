package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/artisticbones/hello-agent/config"
)

func TestAgentUsesInjectedToolExecutorForCompleteCallChain(t *testing.T) {
	model := NewFakeModelClient(
		FakeModelResult{Response: &ModelResponse{ToolCalls: []ToolCall{{
			ID: "call_fake", Name: "fake_lookup", Arguments: `{"project":"demo"}`,
		}}}},
		NewFakeTextResult("工具结果已处理"),
	)
	fakeTool := NewFakeToolExecutor("fake_lookup", ToolResult{Content: "project is healthy"}, nil)
	registry := NewToolRegistry()
	if err := registry.Register(fakeTool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	agent := newAgentWithRegistry(t, model, registry)

	answer, err := agent.Run(context.Background(), "查询 demo")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if answer != "工具结果已处理" {
		t.Fatalf("answer = %q, want %q", answer, "工具结果已处理")
	}
	if fakeTool.CallCount != 1 {
		t.Fatalf("fake tool call count = %d, want 1", fakeTool.CallCount)
	}
	if string(fakeTool.Calls[0].Args) != `{"project":"demo"}` {
		t.Fatalf("fake tool args = %s", fakeTool.Calls[0].Args)
	}

	requests := model.ReceivedRequests
	if len(requests) != 2 {
		t.Fatalf("received requests = %d, want 2", len(requests))
	}
	last := requests[1].Messages
	if len(last) != 3 || last[2].Role != "tool" || last[2].ToolCallID != "call_fake" {
		t.Fatalf("second request messages = %+v", last)
	}
	if last[2].Content != "project is healthy" {
		t.Fatalf("tool message = %q, want %q", last[2].Content, "project is healthy")
	}
}

func TestAgentUnknownToolReturnsErrorToModelSafely(t *testing.T) {
	model := NewFakeModelClient(
		FakeModelResult{Response: &ModelResponse{ToolCalls: []ToolCall{{
			ID: "call_unknown", Name: "missing_tool", Arguments: `{}`,
		}}}},
		NewFakeTextResult("无法调用该工具"),
	)
	registry := NewToolRegistry()
	agent := newAgentWithRegistry(t, model, registry)

	answer, err := agent.Run(context.Background(), "调用不存在的工具")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if answer != "无法调用该工具" {
		t.Fatalf("answer = %q, want model recovery answer", answer)
	}
	if len(model.ReceivedRequests[1].Messages) != 3 {
		t.Fatalf("second request messages = %+v", model.ReceivedRequests[1].Messages)
	}
	if !containsText(model.ReceivedRequests[1].Messages[2].Content, "unknown tool: missing_tool") {
		t.Fatalf("unknown tool error was not returned to model: %q", model.ReceivedRequests[1].Messages[2].Content)
	}
}

func TestAgentToolErrorFollowsRecoveryPolicy(t *testing.T) {
	wantErr := errors.New("fake backend unavailable")
	model := NewFakeModelClient(
		FakeModelResult{Response: &ModelResponse{ToolCalls: []ToolCall{{
			ID: "call_error", Name: "unstable_tool", Arguments: `{}`,
		}}}},
		NewFakeTextResult("模型已理解工具失败"),
	)
	fakeTool := NewFakeToolExecutor("unstable_tool", ToolResult{}, wantErr)
	registry := NewToolRegistry()
	if err := registry.Register(fakeTool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	agent := newAgentWithRegistry(t, model, registry)

	answer, err := agent.Run(context.Background(), "调用不稳定工具")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if answer != "模型已理解工具失败" {
		t.Fatalf("answer = %q, want recovery answer", answer)
	}
	if model.ReceivedRequests[1].Messages[2].Content != "工具执行失败：fake backend unavailable" {
		t.Fatalf("tool error message = %q", model.ReceivedRequests[1].Messages[2].Content)
	}
}

func TestAgentCancelledContextStopsInjectedTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	model := &cancelAfterToolCallModel{cancel: cancel}
	fakeTool := NewFakeToolExecutor("cancelled_tool", ToolResult{Content: "must not run"}, nil)
	registry := NewToolRegistry()
	if err := registry.Register(fakeTool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	agent := newAgentWithRegistry(t, model, registry)

	_, err := agent.Run(ctx, "执行工具")
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if !fakeTool.ContextClosed {
		t.Fatal("injected tool should observe cancelled context")
	}
	if fakeTool.CallCount != 1 {
		t.Fatalf("fake tool call count = %d, want 1", fakeTool.CallCount)
	}
}

type cancelAfterToolCallModel struct {
	cancel context.CancelFunc
}

func (m *cancelAfterToolCallModel) Complete(_ context.Context, _ ModelRequest) (*ModelResponse, error) {
	m.cancel()
	return &ModelResponse{ToolCalls: []ToolCall{{
		ID: "call_cancel", Name: "cancelled_tool", Arguments: `{}`,
	}}}, nil
}

func newAgentWithRegistry(t *testing.T, model ModelClient, registry ToolRegistry) *Agent {
	t.Helper()
	agent, err := NewAgent(model, testConfig(), registry)
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}
	return agent
}

func containsText(content, want string) bool {
	return strings.Contains(content, want)
}

func testConfig() *config.Config {
	return &config.Config{
		MaxSteps:       3,
		MaxRetries:     0,
		RequestTimeout: time.Minute,
		Model:          &config.ModelConfig{Name: "fake", BaseURL: "fake://model"},
		Tool:           &config.ToolConfig{MaxToolRecoveries: 2},
	}
}
