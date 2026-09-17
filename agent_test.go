package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/artisticbones/hello-agent/config"
)

func TestAgentLoopReturnsTextAndStops(t *testing.T) {
	client := NewFakeModelClient(NewFakeTextResult("普通文本回答"))
	agent := newTestAgent(t, client, 3, 0)

	answer, err := agent.Run(context.Background(), "你好")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if answer != "普通文本回答" {
		t.Fatalf("answer = %q, want %q", answer, "普通文本回答")
	}
	if client.CallCount != 1 {
		t.Fatalf("model call count = %d, want 1", client.CallCount)
	}
}

func TestAgentLoopExecutesToolAndReturnsResultToModel(t *testing.T) {
	client := NewFakeModelClient(
		NewFakeCalculatorToolCallResult("call_1", `{"a":123,"b":45,"operation":"add"}`),
		NewFakeTextResult("最终结果是 168"),
	)
	agent := newTestAgent(t, client, 3, 0)

	answer, err := agent.Run(context.Background(), "123 + 45 等于多少")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if answer != "最终结果是 168" {
		t.Fatalf("answer = %q, want %q", answer, "最终结果是 168")
	}
	if client.CallCount != 2 {
		t.Fatalf("model call count = %d, want 2", client.CallCount)
	}

	if len(client.ReceivedRequests) != 2 {
		t.Fatalf("recorded request count = %d, want 2", len(client.ReceivedRequests))
	}
	secondMessages := client.ReceivedRequests[1].Messages
	if len(secondMessages) != 3 {
		t.Fatalf("second request message count = %d, want 3", len(secondMessages))
	}
	if secondMessages[1].Role != "assistant" || len(secondMessages[1].ToolCalls) != 1 {
		t.Fatalf("assistant tool-call message = %+v", secondMessages[1])
	}
	if secondMessages[2].Role != "tool" || secondMessages[2].ToolCallID != "call_1" {
		t.Fatalf("tool result message = %+v", secondMessages[2])
	}
	if secondMessages[2].Content != "a=123, b=45, operation=add, result=168" {
		t.Fatalf("tool result = %q", secondMessages[2].Content)
	}
}

func TestAgentLoopRetriesModelErrorWithinLimit(t *testing.T) {
	modelError := errors.New("fake model unavailable")
	client := NewFakeModelClient(
		NewFakeModelErrorResult(modelError),
		NewFakeModelErrorResult(modelError),
		NewFakeTextResult("不会执行到这里"),
	)
	agent := newTestAgent(t, client, 3, 1)

	answer, err := agent.Run(context.Background(), "请回答")
	if answer != "" {
		t.Fatalf("answer = %q, want empty", answer)
	}
	if err == nil {
		t.Fatal("Run() error = nil, want model error")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *AgentError", err)
	}
	if agentErr.Kind != NetworkError {
		t.Fatalf("error kind = %q, want %q", agentErr.Kind, NetworkError)
	}
	if client.CallCount != 2 {
		t.Fatalf("model call count = %d, want 2", client.CallCount)
	}
}

func TestAgentLoopTimeoutSafelyExits(t *testing.T) {
	client := NewFakeModelClient(NewFakeTextResult("不会返回"))
	agent := newTestAgent(t, client, 3, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	answer, err := agent.Run(ctx, "请回答")
	if answer != "" {
		t.Fatalf("answer = %q, want empty", answer)
	}
	if err == nil {
		t.Fatal("Run() error = nil, want timeout error")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *AgentError", err)
	}
	if agentErr.Kind != TimeoutError {
		t.Fatalf("error kind = %q, want %q", agentErr.Kind, TimeoutError)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
	if !client.ContextCancelled {
		t.Fatal("FakeModelClient should observe the cancelled context")
	}
}

func TestAgentLoopStopsAtMaximumSteps(t *testing.T) {
	client := NewFakeModelClient(
		NewFakeCalculatorToolCallResult("call_1", `{"a":1,"b":2,"operation":"add"}`),
		NewFakeCalculatorToolCallResult("call_2", `{"a":2,"b":3,"operation":"add"}`),
		NewFakeTextResult("不会执行到这里"),
	)
	agent := newTestAgent(t, client, 2, 0)

	answer, err := agent.Run(context.Background(), "持续调用工具")
	if answer != "" {
		t.Fatalf("answer = %q, want empty", answer)
	}
	if err == nil {
		t.Fatal("Run() error = nil, want maximum-step error")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *AgentError", err)
	}
	if agentErr.Kind != ModelError {
		t.Fatalf("error kind = %q, want %q", agentErr.Kind, ModelError)
	}
	if client.CallCount != 2 {
		t.Fatalf("model call count = %d, want 2", client.CallCount)
	}
}

func newTestAgent(t *testing.T, client *FakeModelClient, maxSteps, maxRetries int) *Agent {
	t.Helper()
	cfg := &config.Config{
		MaxSteps:       maxSteps,
		MaxRetries:     maxRetries,
		RequestTimeout: time.Minute,
		Model:          &config.ModelConfig{Name: "fake", BaseURL: "fake://model"},
		Tool:           &config.ToolConfig{MaxToolRecoveries: 2},
	}
	agent, err := NewAgent(client, cfg, []ToolDefinition{calculatorTool()})
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}
	return agent
}
