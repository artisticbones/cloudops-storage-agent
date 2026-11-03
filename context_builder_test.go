package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/artisticbones/hello-agent/config"
)

func TestContextBuilderKeepsSystemAndRecentMessagesWithinBudget(t *testing.T) {
	builder := NewContextBuilder(24)
	state := RuntimeState{
		SystemPrompt: "always be precise",
		Messages: []Message{
			{Role: "user", Content: "old question that should be removed"},
			{Role: "assistant", Content: "old answer that should be removed"},
			{Role: "user", Content: "recent question"},
			{Role: "assistant", Content: "recent answer"},
			{Role: "user", Content: "current question"},
		},
	}

	request, report, err := builder.Build(context.Background(), state)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if report.AfterTokens > report.TokenBudget {
		t.Fatalf("context tokens = %d, budget = %d", report.AfterTokens, report.TokenBudget)
	}
	if len(request.Messages) == 0 || request.Messages[0].Role != "system" {
		t.Fatalf("first message = %+v, want system message", request.Messages)
	}
	if request.Messages[0].Content != state.SystemPrompt {
		t.Fatalf("system prompt = %q, want %q", request.Messages[0].Content, state.SystemPrompt)
	}
	if request.Messages[len(request.Messages)-1].Role != "user" || request.Messages[len(request.Messages)-1].Content != "current question" {
		t.Fatalf("last message = %+v, want current user message", request.Messages[len(request.Messages)-1])
	}
	for _, message := range request.Messages {
		if strings.Contains(message.Content, "old question") || strings.Contains(message.Content, "old answer") {
			t.Fatalf("old message was retained: %+v", message)
		}
	}
	if report.DroppedMessages == 0 && report.TruncatedMessages == 0 {
		t.Fatal("context report did not record pruning old history")
	}
}

func TestContextBuilderTruncatesToolResultWithoutLeavingOrphanMessages(t *testing.T) {
	builder := NewContextBuilder(30)
	longResult := strings.Repeat("tool-result ", 100)
	state := RuntimeState{
		Messages: []Message{
			{Role: "user", Content: "run the tool"},
			{Role: "assistant", Content: "calling tool", ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup", Arguments: `{"query":"x"}`}}},
			{Role: "tool", ToolCallID: "call-1", Content: longResult},
			{Role: "user", Content: "what did it find?"},
		},
	}

	request, report, err := builder.Build(context.Background(), state)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if report.AfterTokens > report.TokenBudget {
		t.Fatalf("context tokens = %d, budget = %d", report.AfterTokens, report.TokenBudget)
	}
	if report.TruncatedToolResults != 1 {
		t.Fatalf("truncated tool results = %d, want 1", report.TruncatedToolResults)
	}
	if report.Summary == nil || report.Summary.Marker == "" {
		t.Fatalf("summary = %+v, want structured summary placeholder", report.Summary)
	}
	if !containsContextReason(report.Reasons, "tool_result_truncated") || !containsContextReason(report.Reasons, "summary_placeholder") {
		t.Fatalf("report reasons = %v, want tool truncation and summary placeholder", report.Reasons)
	}

	assistantIndex := -1
	toolIndex := -1
	for i, message := range request.Messages {
		if message.Role == "assistant" && len(message.ToolCalls) == 1 && message.ToolCalls[0].ID == "call-1" {
			assistantIndex = i
		}
		if message.Role == "tool" && message.ToolCallID == "call-1" {
			toolIndex = i
			if len(message.Content) >= len(longResult) {
				t.Fatalf("tool result was not truncated: %d bytes", len(message.Content))
			}
		}
		if message.Role == "tool" && message.ToolCallID == "" {
			t.Fatalf("orphan tool result retained: %+v", message)
		}
	}
	if assistantIndex < 0 || toolIndex != assistantIndex+1 {
		t.Fatalf("tool pair indexes = assistant %d, tool %d; messages = %+v", assistantIndex, toolIndex, request.Messages)
	}
}

func TestFitMessageGroupTinyBudgetDoesNotLoop(t *testing.T) {
	done := make(chan bool, 1)
	go func() {
		_, _, ok := fitMessageGroup(messageGroup{messages: []Message{
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup", Arguments: `{"query":"x"}`}}},
			{Role: "tool", ToolCallID: "call-1"},
		}}, 1)
		done <- ok
	}()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("fitMessageGroup() accepted a tool group that cannot fit in one token")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("fitMessageGroup() did not terminate for a tiny budget")
	}
}

func TestAgentSendsContextBuilderSnapshotToFakeModelClient(t *testing.T) {
	client := NewFakeModelClient(NewFakeTextResult("done"))
	cfg := &config.Config{
		MaxSteps:       1,
		MaxRetries:     0,
		RequestTimeout: time.Second,
		Model:          &config.ModelConfig{Name: "fake", BaseURL: "fake://model"},
		Tool:           &config.ToolConfig{MaxToolRecoveries: 0},
	}
	registry := NewToolRegistry()
	agent, err := NewAgentWithContextBuilder(client, cfg, registry, NewContextBuilder(18))
	if err != nil {
		t.Fatalf("NewAgentWithContextBuilder() error = %v", err)
	}
	agent.systemPrompt = "be concise"
	agent.conversation = []Message{
		{Role: "user", Content: "old history " + strings.Repeat("old ", 20)},
		{Role: "assistant", Content: "old answer " + strings.Repeat("answer ", 20)},
	}

	if _, err := agent.Run(context.Background(), "current request"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(client.ReceivedRequests) != 1 {
		t.Fatalf("received requests = %d, want 1", len(client.ReceivedRequests))
	}
	request := client.ReceivedRequests[0]
	if len(request.Messages) < 2 || request.Messages[0].Role != "system" {
		t.Fatalf("request messages = %+v, want system and current user", request.Messages)
	}
	if request.Messages[len(request.Messages)-1].Role != "user" || request.Messages[len(request.Messages)-1].Content != "current request" {
		t.Fatalf("request last message = %+v, want current user", request.Messages[len(request.Messages)-1])
	}
	if request.Messages[0].Content != "be concise" {
		t.Fatalf("request system prompt = %q, want %q", request.Messages[0].Content, "be concise")
	}
	if stats := latestContextStats(agent.Trace()); stats.AfterTokens > stats.TokenBudget {
		t.Fatalf("trace context tokens = %d, budget = %d", stats.AfterTokens, stats.TokenBudget)
	}
	if stats := latestContextStats(agent.Trace()); !strings.Contains(stats.TruncationReasons, "summary_placeholder") {
		t.Fatalf("trace truncation reasons = %q, want summary placeholder reason", stats.TruncationReasons)
	}
}

func TestNewAgentUsesConfiguredDefaultContextBudgetForLongHistory(t *testing.T) {
	client := NewFakeModelClient(NewFakeTextResult("done"))
	cfg := &config.Config{
		MaxSteps:           1,
		MaxRetries:         0,
		RequestTimeout:     time.Second,
		ContextTokenBudget: 24,
		Model:              &config.ModelConfig{Name: "fake", BaseURL: "fake://model"},
		Tool:               &config.ToolConfig{MaxToolRecoveries: 0},
	}
	agent, err := NewAgent(client, cfg, NewToolRegistry())
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}
	agent.systemPrompt = "brief"
	agent.conversation = []Message{{Role: "user", Content: strings.Repeat("old history ", 2000)}}

	if _, err := agent.Run(context.Background(), "current request"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	stats := latestContextStats(agent.Trace())
	if stats.TokenBudget != cfg.ContextTokenBudget {
		t.Fatalf("trace token budget = %d, want %d", stats.TokenBudget, cfg.ContextTokenBudget)
	}
	if stats.AfterTokens > cfg.ContextTokenBudget {
		t.Fatalf("trace context tokens = %d, budget = %d", stats.AfterTokens, cfg.ContextTokenBudget)
	}
}

func latestContextStats(trace *Trace) ContextStats {
	for i := len(trace.Events) - 1; i >= 0; i-- {
		if trace.Events[i].Event == "model_request" && trace.Events[i].ContextStats != nil {
			return *trace.Events[i].ContextStats
		}
	}
	return ContextStats{}
}

func containsContextReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}
