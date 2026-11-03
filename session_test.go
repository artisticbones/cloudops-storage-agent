package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/artisticbones/hello-agent/config"
)

func TestSessionRunCheckpointMemoryStoreRoundTrip(t *testing.T) {
	session := NewSession()
	if session.ID == "" {
		t.Fatal("NewSession() returned an empty ID")
	}
	run := NewRun(session.ID)
	if run.ID == "" || run.SessionID != session.ID {
		t.Fatalf("NewRun() = %+v, want a session-linked run with an ID", run)
	}

	want := Checkpoint{
		ID:      run.ID,
		Session: session,
		Run:     run,
		Messages: []Message{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
		},
		CompletedTools: map[string]ToolResult{
			"call-1": {Content: "tool result"},
		},
	}
	store := NewMemoryCheckpointStore()
	if err := store.Save(context.Background(), want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %+v, want %+v", got, want)
	}

	got.Messages[0].Content = "mutated"
	got.CompletedTools["call-1"] = ToolResult{Content: "mutated"}
	again, err := store.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Load() after mutation error = %v", err)
	}
	if again.Messages[0].Content != "hello" || again.CompletedTools["call-1"].Content != "tool result" {
		t.Fatalf("store returned aliased checkpoint data: %+v", again)
	}
}

func TestMemoryCheckpointStoreUsesRunIDAndDeepCopiesToolData(t *testing.T) {
	store := NewMemoryCheckpointStore()
	run := NewRun("session-1")
	nested := map[string]any{
		"nested": map[string]any{
			"items": []any{map[string]any{"value": "original"}},
		},
	}
	checkpoint := Checkpoint{
		ID:  "wrong-checkpoint-key",
		Run: run,
		CompletedTools: map[string]ToolResult{
			"call-1": {Content: "ok", Data: nested},
		},
	}
	if err := store.Save(context.Background(), checkpoint); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	nested["nested"].(map[string]any)["items"].([]any)[0].(map[string]any)["value"] = "mutated-source"

	got, err := store.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Load() by Run.ID error = %v", err)
	}
	if got.ID != run.ID {
		t.Fatalf("checkpoint ID = %q, want Run.ID %q", got.ID, run.ID)
	}
	gotValue := got.CompletedTools["call-1"].Data.(map[string]any)["nested"].(map[string]any)["items"].([]any)[0].(map[string]any)["value"]
	if gotValue != "original" {
		t.Fatalf("stored nested ToolResult.Data = %v, want original", gotValue)
	}

	got.CompletedTools["call-1"].Data.(map[string]any)["nested"].(map[string]any)["items"].([]any)[0].(map[string]any)["value"] = "mutated-load"
	again, err := store.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("second Load() error = %v", err)
	}
	againValue := again.CompletedTools["call-1"].Data.(map[string]any)["nested"].(map[string]any)["items"].([]any)[0].(map[string]any)["value"]
	if againValue != "original" {
		t.Fatalf("store returned aliased nested ToolResult.Data = %v, want original", againValue)
	}
}

func TestMemoryCheckpointStoreDeepCopiesStructToolData(t *testing.T) {
	type nestedPointer struct {
		Name string
	}
	type nestedStruct struct {
		Attributes map[string]string
		Items      []string
		Pointer    *nestedPointer
	}

	store := NewMemoryCheckpointStore()
	run := NewRun("session-struct")
	want := nestedStruct{
		Attributes: map[string]string{"key": "original"},
		Items:      []string{"one"},
		Pointer:    &nestedPointer{Name: "pointer"},
	}
	if err := store.Save(context.Background(), Checkpoint{
		Run: run,
		CompletedTools: map[string]ToolResult{
			"call-struct": {Data: want},
		},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	want.Attributes["key"] = "source-mutated"
	want.Items[0] = "source-mutated"
	want.Pointer.Name = "source-mutated"
	got, err := store.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	data, ok := got.CompletedTools["call-struct"].Data.(nestedStruct)
	if !ok {
		t.Fatalf("loaded ToolResult.Data type = %T, want nestedStruct", got.CompletedTools["call-struct"].Data)
	}
	if data.Attributes["key"] != "original" || data.Items[0] != "one" || data.Pointer.Name != "pointer" {
		t.Fatalf("loaded struct data = %+v, source mutation leaked", data)
	}

	data.Attributes["key"] = "load-mutated"
	data.Items[0] = "load-mutated"
	data.Pointer.Name = "load-mutated"
	again, err := store.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("second Load() error = %v", err)
	}
	againData := again.CompletedTools["call-struct"].Data.(nestedStruct)
	if againData.Attributes["key"] != "original" || againData.Items[0] != "one" || againData.Pointer.Name != "pointer" {
		t.Fatalf("loaded struct data = %+v, load mutation leaked", againData)
	}
}

func TestMemoryCheckpointStoreRejectsUnsafeToolData(t *testing.T) {
	store := NewMemoryCheckpointStore()
	run := NewRun("session-unsafe")
	err := store.Save(context.Background(), Checkpoint{
		Run: run,
		CompletedTools: map[string]ToolResult{
			"call-unsafe": {Data: func() {}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "tool result data cannot be safely cloned") {
		t.Fatalf("Save() error = %v, want explicit unsafe ToolResult.Data error", err)
	}
}

func TestAgentPersistsFailedCheckpointForOrdinaryModelError(t *testing.T) {
	store := NewMemoryCheckpointStore()
	registry := NewToolRegistry()
	agent := newCheckpointTestAgent(t, NewFakeModelClient(NewFakeModelErrorResult(errors.New("model unavailable"))), registry, store)

	_, err := agent.Run(context.Background(), "ordinary model error")
	if err == nil {
		t.Fatal("Run() error = nil, want model error")
	}
	run, ok := agent.LastRun()
	if !ok {
		t.Fatal("LastRun() returned no run")
	}
	checkpoint, loadErr := store.Load(context.Background(), run.ID)
	if loadErr != nil {
		t.Fatalf("Load() error = %v", loadErr)
	}
	if checkpoint.Run.Status != RunFailed {
		t.Fatalf("checkpoint status = %q, want %q", checkpoint.Run.Status, RunFailed)
	}
	if strings.Contains(checkpoint.Run.Error, "context") {
		t.Fatalf("ordinary model error was classified as context error: %q", checkpoint.Run.Error)
	}
}

func TestAgentPersistsInterruptedCheckpointForParentContext(t *testing.T) {
	store := NewMemoryCheckpointStore()
	registry := NewToolRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := NewFakeModelClient(NewFakeTextResult("must not return"))
	agent := newCheckpointTestAgent(t, client, registry, store)

	_, err := agent.Run(ctx, "cancelled model request")
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	run, ok := agent.LastRun()
	if !ok {
		t.Fatal("LastRun() returned no run")
	}
	checkpoint, loadErr := store.Load(context.Background(), run.ID)
	if loadErr != nil {
		t.Fatalf("Load() error = %v", loadErr)
	}
	if checkpoint.Run.Status != RunInterrupted {
		t.Fatalf("checkpoint status = %q, want %q", checkpoint.Run.Status, RunInterrupted)
	}
	if client.CallCount != 0 {
		t.Fatalf("model call count = %d, want no model call after parent cancellation", client.CallCount)
	}
}

func TestMemoryCheckpointStoreRechecksCancelledContextAfterWaitingForLock(t *testing.T) {
	store := NewMemoryCheckpointStore()
	run := NewRun("session-1")
	checkpoint := Checkpoint{Run: run}
	store.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- store.Save(ctx, checkpoint) }()
	cancel()
	store.mu.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Save() error after lock wait = %v, want context.Canceled", err)
	}
}

func TestAgentPersistsFailedCheckpointForRecoveryTermination(t *testing.T) {
	tests := []struct {
		name              string
		maxRecoveries     int
		wantErrorContains string
	}{
		{name: "recovery limit", maxRecoveries: 0, wantErrorContains: "tool recovery limit exceeded"},
		{name: "repeated business error", maxRecoveries: 3, wantErrorContains: "business_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := NewToolRegistry()
			if err := registry.Register(NewFakeToolExecutor("failing_tool", ToolResult{}, errors.New("backend failed"))); err != nil {
				t.Fatalf("Register() error = %v", err)
			}
			store := NewMemoryCheckpointStore()
			agent := newCheckpointTestAgent(t, &repeatedToolModel{}, registry, store)
			agent.cfg.Tool.MaxToolRecoveries = tt.maxRecoveries

			_, err := agent.Run(context.Background(), "run failing tool")
			if err == nil {
				t.Fatal("Run() error = nil, want recovery termination")
			}
			run, ok := agent.LastRun()
			if !ok {
				t.Fatal("LastRun() returned no run")
			}
			checkpoint, loadErr := store.Load(context.Background(), run.ID)
			if loadErr != nil {
				t.Fatalf("Load() error = %v", loadErr)
			}
			if checkpoint.Run.Status != RunFailed {
				t.Fatalf("checkpoint status = %q, want %q", checkpoint.Run.Status, RunFailed)
			}
			if !strings.Contains(checkpoint.Run.Error, tt.wantErrorContains) {
				t.Fatalf("checkpoint error = %q, want substring %q", checkpoint.Run.Error, tt.wantErrorContains)
			}
		})
	}
}

func TestResumeCompletedRunRestoresSessionBeforeReturning(t *testing.T) {
	store := NewMemoryCheckpointStore()
	registry := NewToolRegistry()
	firstAgent := newCheckpointTestAgent(t, NewFakeModelClient(NewFakeTextResult("first")), registry, store)
	if _, err := firstAgent.Run(context.Background(), "first question"); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	firstRun, ok := firstAgent.LastRun()
	if !ok {
		t.Fatal("first LastRun() returned no run")
	}
	wantSession := firstAgent.Session()

	secondModel := NewFakeModelClient(NewFakeTextResult("second"))
	secondAgent := newCheckpointTestAgent(t, secondModel, registry, store)
	answer, err := secondAgent.Resume(context.Background(), firstRun.ID)
	if err != nil || answer != "first" {
		t.Fatalf("completed Resume() answer=%q, error=%v", answer, err)
	}
	if got := secondAgent.Session(); got.ID != wantSession.ID || !reflect.DeepEqual(got.Messages, wantSession.Messages) {
		t.Fatalf("restored session = %+v, want %+v", got, wantSession)
	}

	if _, err := secondAgent.Run(context.Background(), "second question"); err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	run, ok := secondAgent.LastRun()
	if !ok || run.SessionID != wantSession.ID {
		t.Fatalf("new Run() = %+v, want session ID %q", run, wantSession.ID)
	}
	request := secondModel.ReceivedRequests[0]
	wantMessages := []Message{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first"},
		{Role: "user", Content: "second question"},
	}
	if !reflect.DeepEqual(request.Messages, wantMessages) {
		t.Fatalf("new Run() messages = %+v, want %+v", request.Messages, wantMessages)
	}
}

func TestAgentResumeDoesNotRepeatSuccessfulToolAndPreservesMessageOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	model := &checkpointModel{}
	tool := &cancelAfterSuccessfulTool{cancel: cancel}
	registry := NewToolRegistry()
	if err := registry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	store := NewMemoryCheckpointStore()
	agent := newCheckpointTestAgent(t, model, registry, store)

	answer, err := agent.Run(ctx, "查询 demo")
	if answer != "" {
		t.Fatalf("interrupted answer = %q, want empty", answer)
	}
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted Run() error = %v, want context.Canceled", err)
	}
	if tool.calls != 1 {
		t.Fatalf("tool calls after interruption = %d, want 1", tool.calls)
	}

	run, ok := agent.LastRun()
	if !ok || run.ID == "" {
		t.Fatalf("LastRun() = %+v, %v; want an interrupted run", run, ok)
	}
	if run.Status != RunInterrupted {
		t.Fatalf("interrupted run status = %q, want %q", run.Status, RunInterrupted)
	}

	answer, err = agent.Resume(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if answer != "恢复完成" {
		t.Fatalf("resumed answer = %q, want %q", answer, "恢复完成")
	}
	if tool.calls != 1 {
		t.Fatalf("tool calls after resume = %d, want 1", tool.calls)
	}

	if len(model.requests) != 2 {
		t.Fatalf("model request count = %d, want 2", len(model.requests))
	}
	resumedMessages := model.requests[1].Messages
	wantMessages := []Message{
		{Role: "user", Content: "查询 demo"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call-1", Name: "checkpoint_tool", Arguments: `{}`}}},
		{Role: "tool", ToolCallID: "call-1", Content: "成功结果"},
	}
	if !reflect.DeepEqual(resumedMessages, wantMessages) {
		t.Fatalf("resumed message order = %+v, want %+v", resumedMessages, wantMessages)
	}

	loaded, err := store.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Load() after resume error = %v", err)
	}
	if loaded.Run.Status != RunCompleted || loaded.Result != "恢复完成" {
		t.Fatalf("completed checkpoint = %+v, want completed result", loaded)
	}
}

type checkpointModel struct {
	mu       sync.Mutex
	calls    int
	requests []ModelRequest
}

type repeatedToolModel struct{}

func (*repeatedToolModel) Complete(context.Context, ModelRequest) (*ModelResponse, error) {
	return &ModelResponse{ToolCalls: []ToolCall{{ID: "failure-call", Name: "failing_tool", Arguments: `{}`}}}, nil
}

func (m *checkpointModel) Complete(ctx context.Context, request ModelRequest) (*ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, cloneModelRequest(request))
	m.calls++
	if m.calls == 1 {
		return &ModelResponse{ToolCalls: []ToolCall{{
			ID: "call-1", Name: "checkpoint_tool", Arguments: `{}`,
		}}}, nil
	}
	return &ModelResponse{Content: "恢复完成"}, nil
}

type cancelAfterSuccessfulTool struct {
	cancel context.CancelFunc
	calls  int
}

func (t *cancelAfterSuccessfulTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "checkpoint_tool", Description: "test tool", Parameters: map[string]any{"type": "object"}}
}

func (t *cancelAfterSuccessfulTool) Execute(context.Context, json.RawMessage) (ToolResult, error) {
	t.calls++
	t.cancel()
	return ToolResult{Content: "成功结果"}, nil
}

func newCheckpointTestAgent(t *testing.T, model ModelClient, registry ToolRegistry, store CheckpointStore) *Agent {
	t.Helper()
	cfg := &config.Config{
		MaxSteps:       3,
		MaxRetries:     0,
		RequestTimeout: time.Minute,
		Model:          &config.ModelConfig{Name: "fake", BaseURL: "fake://model"},
		Tool:           &config.ToolConfig{MaxToolRecoveries: 1},
	}
	agent, err := NewAgentWithCheckpointStore(model, cfg, registry, store)
	if err != nil {
		t.Fatalf("NewAgentWithCheckpointStore() error = %v", err)
	}
	return agent
}
