package main

import (
	"context"
	"errors"
	"testing"
)

func TestFakeToolExecutorReturnsPresetResultByToolName(t *testing.T) {
	registry := NewToolRegistry()
	calculator := NewFakeToolExecutor("calculator", ToolResult{Content: "result=3"}, nil)
	search := NewFakeToolExecutor("search", ToolResult{Content: "found"}, nil)
	if err := registry.Register(calculator); err != nil {
		t.Fatalf("register calculator: %v", err)
	}
	if err := registry.Register(search); err != nil {
		t.Fatalf("register search: %v", err)
	}

	result, err := dispatchTool(context.Background(), registry, "calculator", `{"a":1,"b":2}`)
	if err != nil {
		t.Fatalf("calculator dispatch error = %v", err)
	}
	if result != "result=3" {
		t.Fatalf("calculator result = %q, want %q", result, "result=3")
	}
	if calculator.CallCount != 1 || search.CallCount != 0 {
		t.Fatalf("call counts = calculator:%d search:%d, want 1 and 0", calculator.CallCount, search.CallCount)
	}
}

func TestFakeToolExecutorReturnsPresetError(t *testing.T) {
	wantErr := errors.New("fake tool failed")
	tool := NewFakeToolExecutor("search", ToolResult{}, wantErr)
	registry := NewToolRegistry()
	if err := registry.Register(tool); err != nil {
		t.Fatalf("register tool: %v", err)
	}

	_, err := dispatchTool(context.Background(), registry, "search", `{}`)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}

func TestFakeToolExecutorRecordsNameArgumentsAndCalls(t *testing.T) {
	tool := NewFakeToolExecutor("calculator", ToolResult{Content: "ok"}, nil)
	args := `{"a":1,"b":2}`

	if _, err := tool.Execute(context.Background(), []byte(args)); err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if _, err := tool.Execute(context.Background(), []byte(args)); err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}

	if tool.CallCount != 2 {
		t.Fatalf("CallCount = %d, want 2", tool.CallCount)
	}
	if len(tool.Calls) != 2 {
		t.Fatalf("Calls length = %d, want 2", len(tool.Calls))
	}
	if tool.Calls[0].ToolName != "calculator" {
		t.Fatalf("ToolName = %q, want calculator", tool.Calls[0].ToolName)
	}
	if string(tool.Calls[0].Args) != args {
		t.Fatalf("Args = %q, want %q", tool.Calls[0].Args, args)
	}
}

func TestFakeToolExecutorChecksCancelledContext(t *testing.T) {
	tool := NewFakeToolExecutor("calculator", ToolResult{Content: "not returned"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := tool.Execute(ctx, []byte(`{}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if result.Content != "" {
		t.Fatalf("result = %+v, want empty result", result)
	}
	if !tool.ContextClosed {
		t.Fatal("FakeToolExecutor should record cancelled context")
	}
}
