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

func TestAgentTraceRecordsWholeLoop(t *testing.T) {
	client := NewFakeModelClient(
		NewFakeCalculatorToolCallResult("call_1", `{"a":1,"b":2,"operation":"add"}`),
		NewFakeTextResult("结果是 3"),
	)
	agent := newTestAgent(t, client, 3, 0)

	if _, err := agent.Run(context.Background(), "1 + 2 等于多少"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var events []string
	for _, event := range agent.Trace().Events {
		events = append(events, event.Event)
	}
	want := []string{
		"agent_start", "model_request", "model_response", "tool_call", "tool_result",
		"model_request", "model_response", "final_answer",
	}
	if len(events) != len(want) {
		t.Fatalf("trace events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("trace event %d = %q, want %q; all=%v", i, events[i], want[i], events)
		}
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

func TestAgentLoopUsesFreshTimeoutForEachStep(t *testing.T) {
	model := &perLoopDeadlineModel{}
	registry := NewToolRegistry()
	if err := registry.Register(calculatorTool()); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	cfg := &config.Config{
		MaxSteps:       2,
		MaxRetries:     0,
		RequestTimeout: 50 * time.Millisecond,
		Model:          &config.ModelConfig{Name: "fake", BaseURL: "fake://model"},
		Tool:           &config.ToolConfig{MaxToolRecoveries: 1},
	}
	agent, err := NewAgent(model, cfg, registry)
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}

	answer, err := agent.Run(context.Background(), "1 + 2 等于多少")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if answer != "done" {
		t.Fatalf("answer = %q, want %q", answer, "done")
	}
	if len(model.Deadlines) != 2 {
		t.Fatalf("deadline count = %d, want 2", len(model.Deadlines))
	}
	if !model.Deadlines[1].After(model.Deadlines[0]) {
		t.Fatalf("second step did not receive a fresh timeout: first=%s second=%s", model.Deadlines[0], model.Deadlines[1])
	}
}

func TestAgentSupportsContinuousConversation(t *testing.T) {
	client := NewFakeModelClient(
		NewFakeTextResult("第一轮回答"),
		NewFakeTextResult("第二轮回答"),
	)
	agent := newTestAgent(t, client, 3, 0)

	answer, err := agent.Run(context.Background(), "我叫 Crane")
	if err != nil || answer != "第一轮回答" {
		t.Fatalf("first Run() answer=%q, error=%v", answer, err)
	}
	answer, err = agent.Run(context.Background(), "我叫什么？")
	if err != nil || answer != "第二轮回答" {
		t.Fatalf("second Run() answer=%q, error=%v", answer, err)
	}

	if len(client.ReceivedRequests) != 2 {
		t.Fatalf("request count = %d, want 2", len(client.ReceivedRequests))
	}
	messages := client.ReceivedRequests[1].Messages
	if len(messages) != 3 {
		t.Fatalf("second request messages = %d, want 3", len(messages))
	}
	if messages[0].Role != "user" || messages[0].Content != "我叫 Crane" {
		t.Fatalf("first history message = %+v", messages[0])
	}
	if messages[1].Role != "assistant" || messages[1].Content != "第一轮回答" {
		t.Fatalf("assistant history message = %+v", messages[1])
	}
	if messages[2].Role != "user" || messages[2].Content != "我叫什么？" {
		t.Fatalf("second user message = %+v", messages[2])
	}
}

type perLoopDeadlineModel struct {
	Deadlines []time.Time
}

func (m *perLoopDeadlineModel) Complete(ctx context.Context, _ ModelRequest) (*ModelResponse, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("model context has no per-loop deadline")
	}
	m.Deadlines = append(m.Deadlines, deadline)
	if len(m.Deadlines) == 1 {
		return &ModelResponse{ToolCalls: []ToolCall{{
			ID: "call_1", Name: toolName, Arguments: `{"a":1,"b":2,"operation":"add"}`,
		}}}, nil
	}
	return &ModelResponse{Content: "done"}, nil
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
	registry := NewToolRegistry()
	if err := registry.Register(calculatorTool()); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	agent, err := NewAgent(client, cfg, registry)
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}
	return agent
}
