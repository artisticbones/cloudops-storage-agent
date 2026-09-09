package main

import (
	"strings"
	"testing"
)

func TestExecuteCalculator(t *testing.T) {
	tests := []struct {
		name      string
		rawArgs   string
		want      string
		wantError string
	}{
		{
			name:    "addition",
			rawArgs: `{"a":123,"b":45,"operation":"add"}`,
			want:    "result=168",
		},
		{
			name:    "subtraction",
			rawArgs: `{"a":123,"b":45,"operation":"subtract"}`,
			want:    "result=78",
		},
		{
			name:    "multiplication",
			rawArgs: `{"a":123,"b":45,"operation":"multiply"}`,
			want:    "result=5535",
		},
		{
			name:    "division",
			rawArgs: `{"a":100,"b":4,"operation":"divide"}`,
			want:    "result=25",
		},
		{
			name:      "division by zero",
			rawArgs:   `{"a":100,"b":0,"operation":"divide"}`,
			wantError: "division by zero",
		},
		{
			name:      "unsupported operation",
			rawArgs:   `{"a":100,"b":4,"operation":"power"}`,
			wantError: "unsupported operation",
		},
		{
			name:      "invalid json",
			rawArgs:   `{"a":100,"b":}`,
			wantError: "invalid character",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := executeCalculator(tt.rawArgs)

			if tt.wantError != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil; result=%q", tt.wantError, got)
				}
				if !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("error=%q, want it to contain %q", err.Error(), tt.wantError)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(got, tt.want) {
				t.Fatalf("result=%q, want it to contain %q", got, tt.want)
			}
		})
	}
}

func TestDispatchTool(t *testing.T) {
	tests := []struct {
		name      string
		toolName  string
		rawArgs   string
		want      string
		wantError string
	}{
		{
			name:     "dispatch calculator",
			toolName: "calculator",
			rawArgs:  `{"a":123,"b":45,"operation":"multiply"}`,
			want:     "result=5535",
		},
		{
			name:      "unknown tool",
			toolName:  "unknown_tool",
			rawArgs:   `{}`,
			wantError: "unknown tool",
		},
		{
			name:      "registered but not implemented",
			toolName:  "get_project_status",
			rawArgs:   `{}`,
			wantError: "tool is not implemented",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dispatchTool(tt.toolName, tt.rawArgs)

			if tt.wantError != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil; result=%q", tt.wantError, got)
				}
				if !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("error=%q, want it to contain %q", err.Error(), tt.wantError)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(got, tt.want) {
				t.Fatalf("result=%q, want it to contain %q", got, tt.want)
			}
		})
	}
}

func TestCalculatorToolSchema(t *testing.T) {
	tool := calculatorTool()
	if tool.OfFunction == nil {
		t.Fatal("calculator tool should contain a function definition")
	}

	fn := tool.OfFunction
	if fn.Name != toolName {
		t.Fatalf("tool name=%q, want %q", fn.Name, toolName)
	}
	if !fn.Strict.Valid() || !fn.Strict.Value {
		t.Fatal("calculator tool should use strict arguments")
	}

	properties, ok := fn.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("tool schema should contain properties")
	}
	if _, ok := properties["a"]; !ok {
		t.Fatal("tool schema should contain parameter a")
	}
	if _, ok := properties["b"]; !ok {
		t.Fatal("tool schema should contain parameter b")
	}
	if _, ok := properties["operation"]; !ok {
		t.Fatal("tool schema should contain parameter operation")
	}
}

func TestToolRegistry(t *testing.T) {
	handler, ok := toolRegistry["calculator"]
	if !ok {
		t.Fatal("calculator should be registered")
	}
	if handler == nil {
		t.Fatal("calculator handler should not be nil")
	}
}
