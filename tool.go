package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// Tool 同时描述一个可被模型发现的工具，并提供工具的执行能力。
// Tool 不关心系统中是否存在其他工具。
type Tool interface {
	Definition() ToolDefinition
	Execute(ctx context.Context, args json.RawMessage) (ToolResult, error)
}

// ToolResult 是工具返回给 Agent Runtime 的结构化结果。
// Content 用于兼容当前消息模型；Data 为后续结构化处理保留原始数据。
type ToolResult struct {
	Content string `json:"content"`
	Data    any    `json:"data,omitempty"`
}

type functionTool struct {
	definition ToolDefinition
	handler    func(context.Context, string) (string, error)
}

func (t *functionTool) Definition() ToolDefinition {
	return t.definition
}

func (t *functionTool) Execute(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	content, err := t.handler(ctx, string(args))
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Content: content}, nil
}

// ToolRegistry 管理工具集合，但不暴露内部 map。
type ToolRegistry interface {
	Register(tool Tool) error
	Find(name string) (Tool, bool)
	Definitions() []ToolDefinition
}

type toolRegistry struct {
	tools map[string]Tool
}

func NewToolRegistry() ToolRegistry {
	return &toolRegistry{tools: make(map[string]Tool)}
}

func (r *toolRegistry) Register(tool Tool) error {
	if tool == nil {
		return fmt.Errorf("tool cannot be nil")
	}
	definition := tool.Definition()
	if definition.Name == "" {
		return fmt.Errorf("tool name cannot be empty")
	}
	if _, exists := r.tools[definition.Name]; exists {
		return fmt.Errorf("tool already registered: %s", definition.Name)
	}
	r.tools[definition.Name] = tool
	return nil
}

func (r *toolRegistry) Find(name string) (Tool, bool) {
	tool, ok := r.tools[name]
	return tool, ok
}

func (r *toolRegistry) Definitions() []ToolDefinition {
	definitions := make([]ToolDefinition, 0, len(r.tools))
	for _, tool := range r.tools {
		definitions = append(definitions, tool.Definition())
	}
	return definitions
}
