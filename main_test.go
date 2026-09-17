package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/artisticbones/hello-agent/config"
	openai "github.com/openai/openai-go/v3"
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
			got, err := executeCalculator(context.Background(), tt.rawArgs)

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
			toolName:  "break_down_tasks",
			rawArgs:   `{}`,
			wantError: "tool is not implemented",
		},
		{
			name:     "dispatch project status",
			toolName: "get_project_status",
			rawArgs:  `{"title":"test","operation":"query"}`,
			want:     `"status":"processing"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dispatchTool(context.Background(), tt.toolName, tt.rawArgs)

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
	if tool.Name != toolName {
		t.Fatalf("tool name=%q, want %q", tool.Name, toolName)
	}
	if !tool.Strict {
		t.Fatal("calculator tool should use strict arguments")
	}

	properties, ok := tool.Parameters["properties"].(map[string]any)
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
	tests := []struct {
		name       string
		wantExists bool
		wantNil    bool
	}{
		{name: "calculator", wantExists: true},
		{name: "get_project_status", wantExists: true},
		{name: "break_down_tasks", wantExists: true, wantNil: true},
		{name: "unknown", wantExists: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, ok := toolRegistry[tt.name]
			if ok != tt.wantExists {
				t.Fatalf("exists=%v, want %v", ok, tt.wantExists)
			}
			if ok && (handler == nil) != tt.wantNil {
				t.Fatalf("handler nil=%v, want %v", handler == nil, tt.wantNil)
			}
		})
	}
}

func TestGetProjectStatus(t *testing.T) {
	tests := []struct {
		name      string
		rawArgs   string
		want      string
		wantError string
	}{
		{
			name:    "query existing project",
			rawArgs: `{"title":"test","operation":"query"}`,
			want:    `"status":"processing"`,
		},
		{
			name:      "invalid json",
			rawArgs:   `{"title":}`,
			wantError: "invalid character",
		},
		{
			name:      "missing project identifier",
			rawArgs:   `{"operation":"query"}`,
			wantError: "projectId and title cannot both be empty",
		},
		{
			name:      "project not found",
			rawArgs:   `{"title":"missing","operation":"query"}`,
			wantError: "no record for missing",
		},
		{
			name:      "unknown operation",
			rawArgs:   `{"title":"test","operation":"archive"}`,
			wantError: "unknown operation: archive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getProjectStatus(context.Background(), tt.rawArgs)
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

func TestGetProjectStatusCRUD(t *testing.T) {
	const title = "crud-project"
	delete(projectDataBase, title)
	t.Cleanup(func() { delete(projectDataBase, title) })

	ctx := context.Background()
	addResult, err := getProjectStatus(ctx, `{"title":"crud-project","operation":"add","data":{"status":"pending","assignee":"D"}}`)
	if err != nil {
		t.Fatalf("add returned error: %v", err)
	}
	if !strings.Contains(addResult, `"status":"pending"`) {
		t.Fatalf("add result=%q, want pending status", addResult)
	}

	updateResult, err := getProjectStatus(ctx, `{"title":"crud-project","operation":"update","data":{"status":"processing","description":"updated"}}`)
	if err != nil {
		t.Fatalf("update returned error: %v", err)
	}
	if !strings.Contains(updateResult, `"status":"processing"`) || !strings.Contains(updateResult, `"description":"updated"`) {
		t.Fatalf("update result=%q, want updated fields", updateResult)
	}

	queryResult, err := getProjectStatus(ctx, `{"projectId":3,"operation":"query","data":{}}`)
	if err != nil {
		t.Fatalf("query by project ID returned error: %v", err)
	}
	if !strings.Contains(queryResult, `"title":"crud-project"`) {
		t.Fatalf("query result=%q, want crud-project", queryResult)
	}

	deleteResult, err := getProjectStatus(ctx, `{"title":"crud-project","operation":"delete","data":{}}`)
	if err != nil {
		t.Fatalf("delete returned error: %v", err)
	}
	if !strings.Contains(deleteResult, "deleted") {
		t.Fatalf("delete result=%q, want deleted", deleteResult)
	}
	if _, ok := projectDataBase[title]; ok {
		t.Fatal("project should be deleted")
	}
}

func TestGetProjectStatusCRUDErrors(t *testing.T) {
	tests := []struct {
		name      string
		rawArgs   string
		wantError string
	}{
		{
			name:      "duplicate add",
			rawArgs:   `{"title":"test","operation":"add","data":{}}`,
			wantError: "project already exists",
		},
		{
			name:      "empty update",
			rawArgs:   `{"title":"test","operation":"update","data":{}}`,
			wantError: "data cannot be empty",
		},
		{
			name:      "unsupported update field",
			rawArgs:   `{"title":"test","operation":"update","data":{"priority":"high"}}`,
			wantError: "unsupported project field",
		},
		{
			name:      "missing add title",
			rawArgs:   `{"operation":"add","data":{}}`,
			wantError: "title cannot be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := getProjectStatus(context.Background(), tt.rawArgs)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("error=%v, want it to contain %q", err, tt.wantError)
			}
		})
	}
}

func TestGetProjectStatusToolSchema(t *testing.T) {
	tool := getProjectStatusTool()
	if tool.Name != "get_project_status" {
		t.Fatalf("tool name=%q, want %q", tool.Name, "get_project_status")
	}
	if !tool.Strict {
		t.Fatal("get project status tool should use strict arguments")
	}

	properties, ok := tool.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("tool schema should contain properties")
	}
	for _, name := range []string{"projectId", "title", "operation", "data"} {
		if _, ok := properties[name]; !ok {
			t.Fatalf("tool schema should contain parameter %q", name)
		}
	}

	required, ok := tool.Parameters["required"].([]string)
	if !ok {
		t.Fatal("tool schema should contain required fields")
	}
	for _, name := range []string{"projectId", "title", "operation", "data"} {
		if !containsString(required, name) {
			t.Fatalf("tool schema required should contain %q", name)
		}
	}

	additionalProperties, ok := tool.Parameters["additionalProperties"].(bool)
	if !ok || additionalProperties {
		t.Fatal("tool schema should reject unknown top-level parameters")
	}

	operation, ok := properties["operation"].(map[string]any)
	if !ok {
		t.Fatal("operation schema should be an object")
	}
	if _, ok := operation["enum"].([]string); !ok {
		t.Fatal("operation schema should contain enum")
	}

	data, ok := properties["data"].(map[string]any)
	if !ok || data["type"] != "object" {
		t.Fatal("data schema should be an object")
	}
	if _, ok := data["additionalProperties"].(map[string]any); !ok {
		t.Fatal("data schema should describe map value types")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestHandleToolError(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "invalid argument", err: &AgentError{Kind: InvalidArgument, Message: "bad args"}, want: "allow it to fix arguments"},
		{name: "business error", err: &AgentError{Kind: BusinessError, Message: "not found"}, want: "do not retry"},
		{name: "network error", err: &AgentError{Kind: NetworkError, Message: "connection refused"}, want: "limited retry"},
		{name: "timeout error", err: &AgentError{Kind: TimeoutError, Message: "deadline exceeded"}, want: "stop: timeout"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := handleToolError(ctx, tt.err); !strings.Contains(got, tt.want) {
				t.Fatalf("action=%q, want it to contain %q", got, tt.want)
			}
		})
	}

	t.Run("context timeout takes priority", func(t *testing.T) {
		deadline, cancel := context.WithTimeout(context.Background(), 0)
		defer cancel()
		if got := handleToolError(deadline, &AgentError{Kind: NetworkError, Message: "late"}); got != "stop: timeout" {
			t.Fatalf("action=%q, want stop: timeout", got)
		}
	})
}

func TestClassifyToolError(t *testing.T) {
	want := &AgentError{Kind: InvalidArgument, Message: "bad args"}
	if got := classifyToolError(want); got != want {
		t.Fatal("classifyToolError should preserve AgentError")
	}

	got := classifyToolError(errors.New("plain failure"))
	if got.Kind != BusinessError {
		t.Fatalf("kind=%q, want %q", got.Kind, BusinessError)
	}
}

func TestRequestCompletionTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	client := NewOpenAICompatibleModelClient(newClient(cfg), cfg.Model.Name)
	resp, err := requestCompletion(ctx, client, cfg, ModelRequest{}, 1)
	if resp != nil {
		t.Fatal("timed out request should not return a response")
	}
	if err == nil {
		t.Fatal("timed out request should return an error")
	}

	var agentErr *AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error=%T, want *AgentError", err)
	}
	if agentErr.Kind != TimeoutError {
		t.Fatalf("error kind=%q, want %q", agentErr.Kind, TimeoutError)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want context deadline exceeded to be preserved", err)
	}
}

func TestGetFirstChoice(t *testing.T) {
	wantChoice := openai.ChatCompletionChoice{Index: 0}
	tests := []struct {
		name    string
		resp    *openai.ChatCompletion
		wantErr string
	}{
		{name: "nil response", resp: nil, wantErr: "model response is nil"},
		{name: "nil choices", resp: &openai.ChatCompletion{Choices: nil}, wantErr: "choices in model response is nil"},
		{name: "empty choices", resp: &openai.ChatCompletion{Choices: []openai.ChatCompletionChoice{}}, wantErr: "model returned no choices"},
		{name: "normal response", resp: &openai.ChatCompletion{Choices: []openai.ChatCompletionChoice{wantChoice}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getFirstChoice(tt.resp)
			if tt.wantErr != "" {
				if err == nil || err.Error() != "model_error: "+tt.wantErr {
					t.Fatalf("error=%v, want model_error: %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Index != wantChoice.Index {
				t.Fatalf("choice index=%d, want %d", got.Index, wantChoice.Index)
			}
		})
	}
}

func TestShouldStopBusinessRetry(t *testing.T) {
	failures := make(map[string]int)
	key := "get_project_status\x00{\"title\":\"missing\"}"

	if shouldStopBusinessRetry(failures, key, BusinessError) {
		t.Fatal("first business error should be allowed to return to the LLM")
	}
	if !shouldStopBusinessRetry(failures, key, BusinessError) {
		t.Fatal("repeated business error should stop retrying")
	}
	if shouldStopBusinessRetry(failures, key, InvalidArgument) {
		t.Fatal("invalid argument should not use business retry protection")
	}
}

func TestExceededRecoveryLimit(t *testing.T) {
	if exceededRecoveryLimit(2, 2) {
		t.Fatal("recovery count at the limit should still be allowed")
	}
	if !exceededRecoveryLimit(3, 2) {
		t.Fatal("recovery count over the limit should stop the agent")
	}
}

func TestToolHandlerContextTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	_, err := dispatchTool(ctx, "calculator", `{"a":1,"b":2,"operation":"add"}`)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want context deadline exceeded", err)
	}

	agentErr := classifyToolError(err)
	if agentErr.Kind != TimeoutError {
		t.Fatalf("error kind=%q, want %q", agentErr.Kind, TimeoutError)
	}
}
