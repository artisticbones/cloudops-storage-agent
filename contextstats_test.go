package main

import (
	"context"
	"testing"
)

func TestSummarizeContext(t *testing.T) {
	request := ModelRequest{
		Messages: []Message{
			{Role: "user", Content: "查询项目"},
			{Role: "assistant", ToolCalls: []ToolCall{
				{ID: "call_1", Name: "tool_a"},
				{ID: "call_2", Name: "tool_b"},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "结果 1"},
			{Role: "tool", ToolCallID: "call_2", Content: "结果 2"},
		},
		Tools: []ToolDefinition{
			{Name: "tool_a"},
			{Name: "tool_b"},
		},
	}

	got := summarizeContext(request)
	want := ContextStats{
		MessageCount:   4,
		UserMessages:   1,
		AssistantCalls: 1,
		ToolMessages:   2,
		ToolCount:      2,
	}
	if got != want {
		t.Fatalf("summarizeContext() = %+v, want %+v", got, want)
	}
}

func TestAgentTraceRecordsContextStatsPerModelRequest(t *testing.T) {
	client := NewFakeModelClient(
		NewFakeCalculatorToolCallResult("call_1", `{"a":1,"b":2,"operation":"add"}`),
		NewFakeTextResult("结果是 3"),
	)
	agent := newTestAgent(t, client, 3, 0)

	if _, err := agent.Run(context.Background(), "1 + 2 等于多少"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var stats []ContextStats
	for _, event := range agent.Trace().Events {
		if event.Event == "model_request" && event.ContextStats != nil {
			stats = append(stats, *event.ContextStats)
		}
	}
	if len(stats) != 2 {
		t.Fatalf("model request context stats = %d, want 2", len(stats))
	}

	wantFirst := ContextStats{
		MessageCount: 1,
		UserMessages: 1,
		ToolCount:    1,
	}
	if stats[0] != wantFirst {
		t.Fatalf("first context stats = %+v, want %+v", stats[0], wantFirst)
	}

	wantSecond := ContextStats{
		MessageCount:   3,
		UserMessages:   1,
		AssistantCalls: 1,
		ToolMessages:   1,
		ToolCount:      1,
	}
	if stats[1] != wantSecond {
		t.Fatalf("second context stats = %+v, want %+v", stats[1], wantSecond)
	}
}

func TestAgentRuntimeContextStatsAcrossMultipleToolCalls(t *testing.T) {
	client := NewFakeModelClient(
		NewFakeCalculatorToolCallResult("call_1", `{"a":1,"b":2,"operation":"add"}`),
		NewFakeCalculatorToolCallResult("call_2", `{"a":3,"b":4,"operation":"add"}`),
		NewFakeTextResult("两次计算完成"),
	)
	agent := newTestAgent(t, client, 4, 0)

	answer, err := agent.Run(context.Background(), "连续执行两次计算")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if answer != "两次计算完成" {
		t.Fatalf("answer = %q, want %q", answer, "两次计算完成")
	}

	if len(client.ReceivedRequests) != 3 {
		t.Fatalf("received requests = %d, want 3", len(client.ReceivedRequests))
	}

	// 第一次请求只包含本次用户输入和当前 Registry 提供的工具定义。
	first := client.ReceivedRequests[0]
	if len(first.Messages) != 1 || first.Messages[0].Role != "user" {
		t.Fatalf("first request messages = %+v, want one user message", first.Messages)
	}
	if len(first.Tools) != 1 || first.Tools[0].Name != toolName {
		t.Fatalf("first request tools = %+v, want calculator tool", first.Tools)
	}

	// 第二次请求保留原始 user，并追加一条 assistant tool-call 和一条 tool result。
	second := client.ReceivedRequests[1]
	if len(second.Messages) != 3 {
		t.Fatalf("second request message count = %d, want 3", len(second.Messages))
	}
	if second.Messages[0].Role != "user" {
		t.Fatalf("second request first message role = %q, want user", second.Messages[0].Role)
	}
	if second.Messages[1].Role != "assistant" || len(second.Messages[1].ToolCalls) != 1 {
		t.Fatalf("second request assistant message = %+v, want one tool call", second.Messages[1])
	}
	if second.Messages[2].Role != "tool" || second.Messages[2].ToolCallID != "call_1" {
		t.Fatalf("second request tool message = %+v, want call_1 result", second.Messages[2])
	}

	// 第三次请求继续保留前两轮历史，并新增第二轮 assistant/tool 消息。
	third := client.ReceivedRequests[2]
	if len(third.Messages) != 5 {
		t.Fatalf("third request message count = %d, want 5", len(third.Messages))
	}
	if third.Messages[3].Role != "assistant" || len(third.Messages[3].ToolCalls) != 1 {
		t.Fatalf("third request assistant message = %+v, want one tool call", third.Messages[3])
	}
	if third.Messages[4].Role != "tool" || third.Messages[4].ToolCallID != "call_2" {
		t.Fatalf("third request tool message = %+v, want call_2 result", third.Messages[4])
	}

	wantStats := []ContextStats{
		{MessageCount: 1, UserMessages: 1, ToolCount: 1},
		{MessageCount: 3, UserMessages: 1, AssistantCalls: 1, ToolMessages: 1, ToolCount: 1},
		{MessageCount: 5, UserMessages: 1, AssistantCalls: 2, ToolMessages: 2, ToolCount: 1},
	}
	var gotStats []ContextStats
	for _, event := range agent.Trace().Events {
		if event.Event == "model_request" && event.ContextStats != nil {
			gotStats = append(gotStats, *event.ContextStats)
		}
	}
	if len(gotStats) != len(wantStats) {
		t.Fatalf("context stats count = %d, want %d", len(gotStats), len(wantStats))
	}
	for i := range wantStats {
		if gotStats[i] != wantStats[i] {
			t.Fatalf("context stats[%d] = %+v, want %+v", i, gotStats[i], wantStats[i])
		}
	}
}
