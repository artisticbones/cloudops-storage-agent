package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/artisticbones/hello-agent/config"
)

func TestFakeModelClientReturnsResponsesInOrder(t *testing.T) {
	modelError := errors.New("model unavailable")
	client := NewFakeModelClient(
		NewFakeTextResult("先调用计算器"),
		NewFakeCalculatorToolCallResult("call_1", `{"a":123,"b":45,"operation":"add"}`),
		NewFakeModelErrorResult(modelError),
	)
	request := ModelRequest{
		Messages: []Message{{Role: "user", Content: "123 + 45 等于多少"}},
		Tools:    []ToolDefinition{calculatorTool().Definition()},
	}

	response, err := client.Complete(context.Background(), request)
	if err != nil || response == nil || response.Content != "先调用计算器" {
		t.Fatalf("first response=%+v, error=%v", response, err)
	}

	response, err = client.Complete(context.Background(), request)
	if err != nil || response == nil || len(response.ToolCalls) != 1 {
		t.Fatalf("second response=%+v, error=%v", response, err)
	}
	if response.ToolCalls[0].Name != toolName || response.ToolCalls[0].ID != "call_1" {
		t.Fatalf("tool call=%+v", response.ToolCalls[0])
	}

	response, err = client.Complete(context.Background(), request)
	if response != nil || !errors.Is(err, modelError) {
		t.Fatalf("third response=%+v, error=%v, want model error", response, err)
	}
}

func TestFakeModelClientRecordsRequestsAndCallCount(t *testing.T) {
	client := NewFakeModelClient(NewFakeTextResult("ok"), NewFakeTextResult("done"))
	first := ModelRequest{Messages: []Message{{Role: "user", Content: "first"}}}
	second := ModelRequest{Messages: []Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Name: "calculator", Arguments: `{}`}}},
	}}

	if _, err := client.Complete(context.Background(), first); err != nil {
		t.Fatalf("first Complete() error = %v", err)
	}
	if _, err := client.Complete(context.Background(), second); err != nil {
		t.Fatalf("second Complete() error = %v", err)
	}

	if client.CallCount != 2 {
		t.Fatalf("CallCount = %d, want 2", client.CallCount)
	}
	if len(client.ReceivedRequests) != 2 {
		t.Fatalf("ReceivedRequests length = %d, want 2", len(client.ReceivedRequests))
	}
	if client.ReceivedRequests[0].Messages[0].Content != "first" {
		t.Fatalf("first recorded request = %+v", client.ReceivedRequests[0])
	}
	if len(client.ReceivedRequests[1].Messages[1].ToolCalls) != 1 {
		t.Fatal("tool call was not recorded")
	}
}

func TestFakeModelClientChecksCancelledContext(t *testing.T) {
	client := NewFakeModelClient(NewFakeTextResult("should not be returned"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	response, err := client.Complete(ctx, ModelRequest{})
	if response != nil {
		t.Fatal("cancelled request should not return a response")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if !client.ContextCancelled {
		t.Fatal("FakeModelClient should record cancelled context")
	}
	if client.CallCount != 1 {
		t.Fatalf("CallCount = %d, want 1", client.CallCount)
	}
}

func TestFakeModelClientBlocksUntilContextCancellation(t *testing.T) {
	client := NewFakeModelClient(FakeModelResult{BlockUntilContext: true})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.Complete(ctx, ModelRequest{})
		result <- err
	}()

	select {
	case err := <-result:
		t.Fatalf("Complete returned before cancellation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Complete did not observe context cancellation")
	}
	if !client.ContextCancelled {
		t.Fatal("FakeModelClient should record in-flight cancellation")
	}
}

func TestAgentUsesInjectedFakeModelClient(t *testing.T) {
	client := NewFakeModelClient(
		NewFakeCalculatorToolCallResult("call_1", `{"a":123,"b":45,"operation":"add"}`),
		NewFakeTextResult("计算结果是 168"),
	)
	cfg := &config.Config{
		MaxSteps:       3,
		MaxRetries:     0,
		RequestTimeout: time.Second,
		Model:          &config.ModelConfig{Name: "fake", BaseURL: "fake://model"},
		Tool:           &config.ToolConfig{MaxToolRecoveries: 1},
	}
	registry := NewToolRegistry()
	if err := registry.Register(calculatorTool()); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	agent, err := NewAgent(client, cfg, registry)
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}

	answer, err := agent.Run(context.Background(), "123 + 45 等于多少")
	if err != nil {
		t.Fatalf("Agent.Run() error = %v", err)
	}
	if answer != "计算结果是 168" {
		t.Fatalf("answer = %q, want %q", answer, "计算结果是 168")
	}
	if client.CallCount != 2 {
		t.Fatalf("CallCount = %d, want 2", client.CallCount)
	}
	if len(client.ReceivedRequests) != 2 {
		t.Fatalf("ReceivedRequests length = %d, want 2", len(client.ReceivedRequests))
	}
	secondMessages := client.ReceivedRequests[1].Messages
	if len(secondMessages) != 3 {
		t.Fatalf("second request message count = %d, want 3", len(secondMessages))
	}
	if secondMessages[1].Role != "assistant" || len(secondMessages[1].ToolCalls) != 1 {
		t.Fatalf("assistant tool call message = %+v", secondMessages[1])
	}
	if secondMessages[2].Role != "tool" || secondMessages[2].ToolCallID != "call_1" {
		t.Fatalf("tool result message = %+v", secondMessages[2])
	}
}
