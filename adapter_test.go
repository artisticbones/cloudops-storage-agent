package main

import (
	"encoding/json"
	"testing"

	openai "github.com/openai/openai-go/v3"
)

func TestConvertMessages(t *testing.T) {
	messages, err := convertMessages([]Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "123 + 45"},
		{
			Role:    "assistant",
			Content: "",
			ToolCalls: []ToolCall{{
				ID: "call_1", Name: "calculator", Arguments: `{"a":123,"b":45,"operation":"add"}`,
			}},
		},
		{Role: "tool", ToolCallID: "call_1", Content: "result=168"},
	})
	if err != nil {
		t.Fatalf("convertMessages() error = %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("converted message count = %d, want 4", len(messages))
	}
	if messages[2].OfAssistant == nil || len(messages[2].OfAssistant.ToolCalls) != 1 {
		t.Fatal("assistant tool call was not converted")
	}
	if messages[3].OfTool == nil || messages[3].OfTool.ToolCallID != "call_1" {
		t.Fatal("tool call ID was not preserved")
	}
}

func TestConvertMessagesRejectsInvalidRole(t *testing.T) {
	_, err := convertMessages([]Message{{Role: "invalid", Content: "test"}})
	if err == nil {
		t.Fatal("convertMessages() should reject an unsupported role")
	}
}

func TestConvertTools(t *testing.T) {
	tools := convertTools([]ToolDefinition{{
		Name:        "calculator",
		Description: "calculate numbers",
		Strict:      true,
		Parameters: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"a": map[string]any{"type": "number"}},
			"required":             []string{"a"},
			"additionalProperties": false,
		},
	}})
	if len(tools) != 1 || tools[0].OfFunction == nil {
		t.Fatal("tool was not converted to a function tool")
	}
	if tools[0].OfFunction.Function.Name != "calculator" {
		t.Fatalf("tool name = %q, want calculator", tools[0].OfFunction.Function.Name)
	}
	if !tools[0].OfFunction.Function.Strict.Valid() || !tools[0].OfFunction.Function.Strict.Value {
		t.Fatal("strict option was not preserved")
	}
}

func TestConvertResponse(t *testing.T) {
	// 使用 SDK 的 JSON 反序列化构造真实的 ChatCompletion，确保 union 类型也被覆盖。
	data := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"calculator","arguments":"{\"a\":123}"}}]},"finish_reason":"tool_calls"}]}`)
	var chat openai.ChatCompletion
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat response: %v", err)
	}
	response, err := convertResponse(&chat)
	if err != nil {
		t.Fatalf("convertResponse() error = %v", err)
	}
	if len(response.ToolCalls) != 1 || response.ToolCalls[0].ID != "call_1" {
		t.Fatalf("converted tool calls = %+v", response.ToolCalls)
	}
}
