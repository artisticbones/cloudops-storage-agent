package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// FakeToolCall 保存 FakeToolExecutor 最近收到的一次调用。
type FakeToolCall struct {
	ToolName string
	Args     json.RawMessage
}

// FakeToolExecutor 是 Tool 的测试实现。
// 一个实例代表一个工具；多个实例通过 ToolRegistry 按工具名路由。
type FakeToolExecutor struct {
	ToolName   string
	ToolSchema ToolDefinition
	Spec       ToolSpec
	Result     ToolResult
	Err        error

	Calls         []FakeToolCall
	CallCount     int
	ContextClosed bool

	mu sync.Mutex
}

var _ Tool = (*FakeToolExecutor)(nil)

func NewFakeToolExecutor(name string, result ToolResult, err error) *FakeToolExecutor {
	return &FakeToolExecutor{
		ToolName: name,
		ToolSchema: ToolDefinition{
			Name:        name,
			Description: "fake tool for tests",
			Parameters:  map[string]any{"type": "object"},
		},
		Result: result,
		Err:    err,
	}
}

func (f *FakeToolExecutor) Definition() ToolDefinition {
	if f.ToolSchema.Name == "" {
		f.ToolSchema.Name = f.ToolName
	}
	return f.ToolSchema
}

func (f *FakeToolExecutor) ToolSpec() ToolSpec {
	spec := f.Spec
	if spec.Name == "" {
		spec.Name = f.ToolName
	}
	if spec.Definition.Name == "" {
		spec.Definition = f.Definition()
	}
	return spec
}

func (f *FakeToolExecutor) Execute(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	f.mu.Lock()
	f.CallCount++
	f.Calls = append(f.Calls, FakeToolCall{
		ToolName: f.ToolName,
		Args:     append(json.RawMessage(nil), args...),
	})
	f.mu.Unlock()

	if ctx != nil {
		select {
		case <-ctx.Done():
			f.mu.Lock()
			f.ContextClosed = true
			f.mu.Unlock()
			return ToolResult{}, ctx.Err()
		default:
		}
	}

	if f.ToolName == "" {
		return ToolResult{}, fmt.Errorf("fake tool name cannot be empty")
	}
	if f.Err != nil {
		return ToolResult{}, f.Err
	}
	return f.Result, nil
}
