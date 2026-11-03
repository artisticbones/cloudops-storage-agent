package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeToolPolicy struct {
	err   error
	calls []fakePolicyCall
}

type fakePolicyCall struct {
	identity Identity
	spec     ToolSpec
	args     string
}

func (p *fakeToolPolicy) Authorize(_ context.Context, identity Identity, spec ToolSpec, args json.RawMessage) error {
	p.calls = append(p.calls, fakePolicyCall{
		identity: identity,
		spec:     spec,
		args:     string(args),
	})
	return p.err
}

type fakeIdempotencyStore struct {
	values map[string]string
	gets   []string
	puts   []string
}

func (s *fakeIdempotencyStore) Get(_ context.Context, key string) (string, bool, error) {
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.gets = append(s.gets, key)
	value, ok := s.values[key]
	return value, ok, nil
}

func (s *fakeIdempotencyStore) Put(_ context.Context, key, value string) error {
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.puts = append(s.puts, key)
	s.values[key] = value
	return nil
}

func TestAgentRejectsUnauthorizedToolBeforeHandler(t *testing.T) {
	model := NewFakeModelClient(
		FakeModelResult{Response: &ModelResponse{ToolCalls: []ToolCall{{
			ID: "call-unauthorized", Name: "write_file", Arguments: `{"path":"/tmp/out","content":"ok"}`,
		}}}},
		NewFakeTextResult("已拒绝"),
	)
	tool := NewFakeToolExecutor("write_file", ToolResult{Content: "must not execute"}, nil)
	tool.Spec = ToolSpec{
		Risk:       RiskLow,
		SideEffect: true,
	}
	policy := &fakeToolPolicy{err: ErrToolUnauthorized}
	store := &fakeIdempotencyStore{}
	agent := newPolicyTestAgent(t, model, tool, policy, store)

	answer, err := agent.Run(context.Background(), "写文件")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if answer != "已拒绝" {
		t.Fatalf("answer = %q, want model recovery answer", answer)
	}
	if tool.CallCount != 0 {
		t.Fatalf("handler call count = %d, want 0", tool.CallCount)
	}
	if len(policy.calls) != 1 || policy.calls[0].identity.SessionID == "" || policy.calls[0].identity.RunID == "" {
		t.Fatalf("policy calls = %+v, want one call with runtime identity", policy.calls)
	}
	if !strings.Contains(model.ReceivedRequests[1].Messages[2].Content, "拒绝") {
		t.Fatalf("tool policy result = %q, want denial status", model.ReceivedRequests[1].Messages[2].Content)
	}
	if !traceHasReason(agent.Trace(), string(ToolDenied)) && !traceHasReason(agent.Trace(), string(ToolPendingApproval)) {
		t.Fatalf("trace = %+v, want explicit refusal event", agent.Trace().Events)
	}
}

func TestAgentPersistenceRedactsSensitiveToolDataButPreservesSafeData(t *testing.T) {
	secret := "super-secret-token"
	model := NewFakeModelClient(
		FakeModelResult{Response: &ModelResponse{ToolCalls: []ToolCall{{ID: "sensitive-call", Name: "sensitive", Arguments: `{"token":"super-secret-token","note":"keep"}`}}}},
		NewFakeTextResult("done"),
	)
	sensitive := NewFakeToolExecutor("sensitive", ToolResult{Content: "result contains super-secret-token"}, nil)
	sensitive.Spec = ToolSpec{SensitiveParameters: []string{"token"}}
	safe := NewFakeToolExecutor("safe", ToolResult{Content: "safe-result"}, nil)
	registry := NewToolRegistry()
	if err := registry.Register(sensitive); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(safe); err != nil {
		t.Fatal(err)
	}
	store := NewMemoryCheckpointStore()
	agent := newCheckpointTestAgent(t, model, registry, store)
	if _, err := agent.Run(context.Background(), "use token"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := agent.Session(); strings.Contains(sessionText(got), secret) {
		t.Fatalf("session leaked secret: %+v", got)
	}
	run, ok := agent.LastRun()
	if !ok {
		t.Fatal("LastRun() missing")
	}
	checkpoint, err := store.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(checkpointText(checkpoint), secret) {
		t.Fatalf("checkpoint leaked secret: %+v", checkpoint)
	}
	for _, event := range agent.Trace().Events {
		if strings.Contains(event.Arguments+event.Result+event.Error, secret) {
			t.Fatalf("trace leaked secret: %+v", event)
		}
	}
	if !strings.Contains(sessionText(agent.Session()), "[REDACTED]") {
		t.Fatalf("session = %+v, want safe placeholder", agent.Session())
	}
	checkpoint.Run.Status = RunInterrupted
	if err := store.Save(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	resumedModel := NewFakeModelClient(NewFakeTextResult("resumed"))
	resumedAgent := newCheckpointTestAgent(t, resumedModel, registry, store)
	if _, err := resumedAgent.Resume(context.Background(), run.ID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if len(resumedModel.ReceivedRequests) != 1 || !requestContainsText(resumedModel.ReceivedRequests[0], "[REDACTED]") {
		t.Fatalf("resumed request = %+v, want safe placeholder", resumedModel.ReceivedRequests)
	}
}

func sessionText(session Session) string {
	var builder strings.Builder
	for _, message := range session.Messages {
		builder.WriteString(message.Content)
		for _, call := range message.ToolCalls {
			builder.WriteString(call.Arguments)
		}
	}
	return builder.String()
}

func checkpointText(checkpoint Checkpoint) string {
	return sessionText(checkpoint.Session) + sessionText(Session{Messages: checkpoint.Messages}) + fmt.Sprint(checkpoint.CompletedTools)
}

func TestAgentRejectsMaliciousArgumentsBeforeHandler(t *testing.T) {
	model := NewFakeModelClient(
		FakeModelResult{Response: &ModelResponse{ToolCalls: []ToolCall{{
			ID: "call-malicious", Name: "read_file", Arguments: `{"path":"../../etc/passwd"}`,
		}}}},
		NewFakeTextResult("参数不安全"),
	)
	tool := NewFakeToolExecutor("read_file", ToolResult{Content: "must not execute"}, nil)
	tool.Spec = ToolSpec{
		Validate: func(args json.RawMessage) error {
			if strings.Contains(string(args), "..") {
				return &AgentError{Kind: InvalidArgument, Message: "path traversal is not allowed"}
			}
			return nil
		},
	}
	policy := &fakeToolPolicy{}
	agent := newPolicyTestAgent(t, model, tool, policy, &fakeIdempotencyStore{})

	answer, err := agent.Run(context.Background(), "读取文件")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if answer != "参数不安全" {
		t.Fatalf("answer = %q, want model recovery answer", answer)
	}
	if tool.CallCount != 0 {
		t.Fatalf("handler call count = %d, want 0", tool.CallCount)
	}
	if len(policy.calls) != 0 {
		t.Fatalf("policy calls = %d, want 0 when secondary validation rejects", len(policy.calls))
	}
}

func TestAgentRejectsTrailingJSONBeforeHandler(t *testing.T) {
	model := NewFakeModelClient(
		FakeModelResult{Response: &ModelResponse{ToolCalls: []ToolCall{{
			ID: "call-trailing-json", Name: "safe_tool", Arguments: `{} {}`,
		}}}},
		NewFakeTextResult("参数格式错误"),
	)
	tool := NewFakeToolExecutor("safe_tool", ToolResult{Content: "must not execute"}, nil)
	agent := newPolicyTestAgent(t, model, tool, &fakeToolPolicy{}, &fakeIdempotencyStore{})

	answer, err := agent.Run(context.Background(), "调用工具")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if answer != "参数格式错误" {
		t.Fatalf("answer = %q, want final answer", answer)
	}
	if tool.CallCount != 0 {
		t.Fatalf("handler call count = %d, want 0", tool.CallCount)
	}
}

func TestValidateToolSpecArgumentsRejectsMultipleJSONValues(t *testing.T) {
	err := validateToolSpecArguments(ToolSpec{Definition: ToolDefinition{Parameters: map[string]any{"type": "object"}}}, json.RawMessage(`{} {}`))
	if err == nil {
		t.Fatal("validateToolSpecArguments() accepted multiple JSON values")
	}
}

func TestValidateJSONSchemaRejectsValueOutsideEnum(t *testing.T) {
	err := validateJSONSchema(map[string]any{
		"type": "string",
		"enum": []any{"add", "subtract"},
	}, "multiply")
	if err == nil {
		t.Fatal("validateJSONSchema() accepted value outside enum")
	}
}

func TestAgentRejectsMissingRequiredParameterBeforeHandler(t *testing.T) {
	model := NewFakeModelClient(
		FakeModelResult{Response: &ModelResponse{ToolCalls: []ToolCall{{
			ID: "call-required", Name: "required_tool", Arguments: `{}`,
		}}}},
		NewFakeTextResult("缺少参数"),
	)
	tool := NewFakeToolExecutor("required_tool", ToolResult{Content: "must not execute"}, nil)
	tool.Spec = ToolSpec{
		Definition: ToolDefinition{
			Name: "required_tool",
			Parameters: map[string]any{
				"type":     "object",
				"required": []string{"path"},
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
			},
		},
	}
	agent := newPolicyTestAgent(t, model, tool, &fakeToolPolicy{}, &fakeIdempotencyStore{})

	answer, err := agent.Run(context.Background(), "调用工具")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if answer != "缺少参数" {
		t.Fatalf("answer = %q, want final answer", answer)
	}
	if tool.CallCount != 0 {
		t.Fatalf("handler call count = %d, want 0", tool.CallCount)
	}
}

func TestAgentReusesSuccessfulSideEffectByStableIdempotencyKey(t *testing.T) {
	model := NewFakeModelClient(
		FakeModelResult{Response: &ModelResponse{ToolCalls: []ToolCall{{
			Name: "send_email", Arguments: `{"to":"user@example.com","body":"hello"}`,
		}}}},
		FakeModelResult{Response: &ModelResponse{ToolCalls: []ToolCall{{
			Name: "send_email", Arguments: `{"body":"hello","to":"user@example.com"}`,
		}}}},
		NewFakeTextResult("发送完成"),
	)
	tool := NewFakeToolExecutor("send_email", ToolResult{Content: "sent"}, nil)
	tool.Spec = ToolSpec{SideEffect: true}
	store := &fakeIdempotencyStore{}
	agent := newPolicyTestAgent(t, model, tool, &fakeToolPolicy{}, store)

	answer, err := agent.Run(context.Background(), "发送邮件")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if answer != "发送完成" {
		t.Fatalf("answer = %q, want final answer", answer)
	}
	if tool.CallCount != 1 {
		t.Fatalf("handler call count = %d, want 1", tool.CallCount)
	}
	if len(store.puts) != 1 {
		t.Fatalf("idempotency puts = %d, want 1", len(store.puts))
	}
}

func TestDispatchReusesSuccessfulSideEffectByIdempotencyStore(t *testing.T) {
	tool := NewFakeToolExecutor("send_email", ToolResult{Content: "sent"}, nil)
	tool.Spec = ToolSpec{SideEffect: true}
	registry := singleToolRegistry(tool)
	store := &fakeIdempotencyStore{}
	identity := Identity{SessionID: "session-store", RunID: "run-store"}
	callA := ToolCall{ID: "model-call-a", Name: "send_email", Arguments: `{"to":"user@example.com"}`}
	callB := ToolCall{ID: "model-call-b", Name: "send_email", Arguments: `{"to":"user@example.com"}`}

	first, err := dispatchToolWithPolicy(context.Background(), registry, &AllowAllToolPolicy{}, store, identity, callA)
	if err != nil || first.Content != "sent" {
		t.Fatalf("first dispatch = %+v, error = %v", first, err)
	}
	second, err := dispatchToolWithPolicy(context.Background(), registry, &AllowAllToolPolicy{}, store, identity, callB)
	if err != nil || second.Content != "sent" {
		t.Fatalf("second dispatch = %+v, error = %v", second, err)
	}
	if tool.CallCount != 1 {
		t.Fatalf("handler call count = %d, want 1", tool.CallCount)
	}
	if len(store.gets) != 2 || len(store.puts) != 1 {
		t.Fatalf("store operations = gets:%d puts:%d, want gets:2 puts:1", len(store.gets), len(store.puts))
	}
}

func TestAgentReturnsPendingApprovalForHighRiskTool(t *testing.T) {
	model := NewFakeModelClient(
		FakeModelResult{Response: &ModelResponse{ToolCalls: []ToolCall{{
			ID: "call-delete", Name: "delete_account", Arguments: `{"account":"user-1"}`,
		}}}},
		NewFakeTextResult("等待审批"),
	)
	tool := NewFakeToolExecutor("delete_account", ToolResult{Content: "must not execute"}, nil)
	tool.Spec = ToolSpec{Risk: RiskHigh, SideEffect: true}
	agent := newPolicyTestAgent(t, model, tool, &AllowAllToolPolicy{}, &fakeIdempotencyStore{})

	answer, err := agent.Run(context.Background(), "删除账号")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if answer != "等待审批" {
		t.Fatalf("answer = %q, want model recovery answer", answer)
	}
	if tool.CallCount != 0 {
		t.Fatalf("handler call count = %d, want 0", tool.CallCount)
	}
	if !strings.Contains(model.ReceivedRequests[1].Messages[2].Content, "待审批") {
		t.Fatalf("tool status = %q, want pending approval", model.ReceivedRequests[1].Messages[2].Content)
	}
}

func TestAgentNeverExecutesHighRiskToolWhenPolicyAllows(t *testing.T) {
	model := NewFakeModelClient(
		FakeModelResult{Response: &ModelResponse{ToolCalls: []ToolCall{{
			ID: "call-high-risk", Name: "drop_database", Arguments: `{"name":"demo"}`,
		}}}},
		NewFakeTextResult("需要审批"),
	)
	tool := NewFakeToolExecutor("drop_database", ToolResult{Content: "must not execute"}, nil)
	tool.Spec = ToolSpec{Risk: RiskHigh, SideEffect: true}
	policy := &fakeToolPolicy{}
	agent := newPolicyTestAgent(t, model, tool, policy, &fakeIdempotencyStore{})

	answer, err := agent.Run(context.Background(), "删除数据库")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if answer != "需要审批" {
		t.Fatalf("answer = %q, want final answer", answer)
	}
	if tool.CallCount != 0 {
		t.Fatalf("handler call count = %d, want 0 for high-risk tool", tool.CallCount)
	}
}

func TestAgentTraceSummarizesSensitiveArguments(t *testing.T) {
	secret := "super-secret-token"
	model := NewFakeModelClient(
		FakeModelResult{Response: &ModelResponse{ToolCalls: []ToolCall{{
			ID: "call-secret", Name: "use_token", Arguments: `{"token":"` + secret + `"}`,
		}}}},
		NewFakeTextResult("完成"),
	)
	tool := NewFakeToolExecutor("use_token", ToolResult{Content: "ok"}, nil)
	tool.Spec = ToolSpec{Sensitive: true}
	agent := newPolicyTestAgent(t, model, tool, &fakeToolPolicy{}, &fakeIdempotencyStore{})

	if _, err := agent.Run(context.Background(), "使用令牌"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, event := range agent.Trace().Events {
		if event.Event == "tool_call" {
			if strings.Contains(event.Arguments, secret) {
				t.Fatalf("trace leaked sensitive argument: %q", event.Arguments)
			}
			if !strings.HasPrefix(event.Arguments, "sha256:") {
				t.Fatalf("trace arguments = %q, want sha256 summary", event.Arguments)
			}
			return
		}
	}
	t.Fatal("trace did not contain tool_call event")
}

func TestCheckpointResultCannotBypassToolPolicy(t *testing.T) {
	model := NewFakeModelClient(
		FakeModelResult{Response: &ModelResponse{ToolCalls: []ToolCall{{
			ID: "call-reuse", Name: "write_file", Arguments: `{"path":"/tmp/out","content":"new"}`,
		}}}},
		NewFakeTextResult("拒绝完成"),
	)
	tool := NewFakeToolExecutor("write_file", ToolResult{Content: "must not execute"}, nil)
	tool.Spec = ToolSpec{SideEffect: true}
	policy := &fakeToolPolicy{err: ErrToolUnauthorized}
	checkpointStore := NewMemoryCheckpointStore()
	agent := newPolicyTestAgentWithCheckpoint(t, model, tool, policy, &fakeIdempotencyStore{}, checkpointStore)
	run := NewRun(agent.session.ID)
	run.Input = "写文件"
	if err := checkpointStore.Save(context.Background(), NewCheckpoint(
		agent.session,
		run,
		[]Message{{Role: "user", Content: run.Input}},
		map[string]ToolResult{"call-reuse": {Content: "stale checkpoint result"}},
	)); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	answer, err := agent.Resume(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if answer != "拒绝完成" {
		t.Fatalf("answer = %q, want model recovery answer", answer)
	}
	if tool.CallCount != 0 {
		t.Fatalf("handler call count = %d, want 0", tool.CallCount)
	}
	if !strings.Contains(model.ReceivedRequests[1].Messages[2].Content, "拒绝") {
		t.Fatalf("checkpoint bypassed policy: %q", model.ReceivedRequests[1].Messages[2].Content)
	}
}

func TestStableIdempotencyKeyScopesSessionAndRunWithoutCallID(t *testing.T) {
	spec := ToolSpec{Name: "send_email", SideEffect: true}
	callA := ToolCall{ID: "model-call-a", Name: "send_email", Arguments: `{"to":"user@example.com"}`}
	callB := ToolCall{ID: "model-call-b", Name: "send_email", Arguments: `{"to":"user@example.com"}`}
	identity := Identity{SessionID: "session-1", RunID: "run-1"}

	if got, want := stableIdempotencyKey(identity, spec, callA), stableIdempotencyKey(identity, spec, callB); got != want {
		t.Fatalf("keys differ only because call IDs differ: %q != %q", got, want)
	}
	if got, want := stableIdempotencyKey(identity, spec, callA), stableIdempotencyKey(Identity{SessionID: "session-2", RunID: "run-1"}, spec, callA); got == want {
		t.Fatal("keys must differ across sessions")
	}
	if got, want := stableIdempotencyKey(identity, spec, callA), stableIdempotencyKey(Identity{SessionID: "session-1", RunID: "run-2"}, spec, callA); got == want {
		t.Fatal("keys must differ across runs")
	}
}

func TestMemoryIdempotencyStoreExecutesConcurrentSideEffectOnce(t *testing.T) {
	store := NewMemoryIdempotencyStore()
	tool := NewFakeToolExecutor("send_email", ToolResult{Content: "sent"}, nil)
	var arrived atomic.Int32
	allArrived := make(chan struct{})
	call := ToolCall{ID: "different-model-id", Name: "send_email", Arguments: `{"to":"user@example.com"}`}
	identity := Identity{SessionID: "session-concurrent", RunID: "run-concurrent"}

	const callers = 16
	tool.Spec = ToolSpec{
		SideEffect: true,
		Validate: func(json.RawMessage) error {
			if arrived.Add(1) == callers {
				close(allArrived)
			}
			<-allArrived
			return nil
		},
	}
	var wait sync.WaitGroup
	errorsCh := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := dispatchToolWithPolicy(context.Background(), singleToolRegistry(tool), &AllowAllToolPolicy{}, store, identity, call)
			errorsCh <- err
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent dispatch error = %v", err)
		}
	}
	if tool.CallCount != 1 {
		t.Fatalf("handler call count = %d, want 1", tool.CallCount)
	}
}

func TestSensitiveToolErrorsDoNotLeakArguments(t *testing.T) {
	secret := "super-secret-token"
	rawArgs := `{"token":"` + secret + `"}`
	tool := NewFakeToolExecutor("use_token", ToolResult{}, fmt.Errorf("backend rejected %s", rawArgs))
	tool.Spec = ToolSpec{Sensitive: true}

	execution, err := dispatchToolWithPolicy(context.Background(), singleToolRegistry(tool), &AllowAllToolPolicy{}, NewMemoryIdempotencyStore(), Identity{SessionID: "s", RunID: "r"}, ToolCall{
		Name:      "use_token",
		Arguments: rawArgs,
	})
	if err == nil {
		t.Fatalf("dispatch error = nil, execution = %+v", execution)
	}
	if strings.Contains(err.Error(), rawArgs) || strings.Contains(err.Error(), secret) {
		t.Fatalf("sensitive tool error leaked arguments: %q", err.Error())
	}
	_, directErr := dispatchTool(context.Background(), singleToolRegistry(tool), "use_token", rawArgs)
	if directErr == nil {
		t.Fatal("direct dispatch error = nil")
	}
	if strings.Contains(directErr.Error(), rawArgs) || strings.Contains(directErr.Error(), secret) {
		t.Fatalf("direct dispatch leaked sensitive arguments: %q", directErr.Error())
	}
}

func TestSensitiveValidationErrorsDoNotLeakArguments(t *testing.T) {
	secret := "super-secret-token"
	rawArgs := `{"token":"` + secret + `"}`
	tool := NewFakeToolExecutor("validate_token", ToolResult{Content: "must not execute"}, nil)
	tool.Spec = ToolSpec{
		Sensitive: true,
		Validate: func(args json.RawMessage) error {
			return fmt.Errorf("invalid token arguments: %s", string(args))
		},
	}

	_, err := dispatchToolWithPolicy(context.Background(), singleToolRegistry(tool), &AllowAllToolPolicy{}, NewMemoryIdempotencyStore(), Identity{SessionID: "s", RunID: "r"}, ToolCall{
		Name:      "validate_token",
		Arguments: rawArgs,
	})
	if err == nil {
		t.Fatal("validation error = nil")
	}
	if strings.Contains(err.Error(), rawArgs) || strings.Contains(err.Error(), secret) {
		t.Fatalf("sensitive validation error leaked arguments: %q", err.Error())
	}
}

func newPolicyTestAgent(t *testing.T, model *FakeModelClient, tool *FakeToolExecutor, policy ToolPolicy, store IdempotencyStore) *Agent {
	t.Helper()
	return newPolicyTestAgentWithCheckpoint(t, model, tool, policy, store, nil)
}

func newPolicyTestAgentWithCheckpoint(t *testing.T, model *FakeModelClient, tool *FakeToolExecutor, policy ToolPolicy, store IdempotencyStore, checkpointStore CheckpointStore) *Agent {
	t.Helper()
	registry := NewToolRegistry()
	if err := registry.Register(tool); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	cfg := testConfig()
	agent, err := newAgent(model, cfg, registry, checkpointStore, newDefaultContextBuilder(cfg), policy, store)
	if err != nil {
		t.Fatalf("newAgent() error = %v", err)
	}
	return agent
}

func singleToolRegistry(tool Tool) ToolRegistry {
	registry := NewToolRegistry()
	if err := registry.Register(tool); err != nil {
		panic(err)
	}
	return registry
}

func TestToolPolicyErrorKindsAreDistinguishable(t *testing.T) {
	if !errors.Is((&ToolPolicyError{Decision: ToolPolicyDenied, Err: ErrToolUnauthorized}), ErrToolUnauthorized) {
		t.Fatal("unauthorized policy error should unwrap to ErrToolUnauthorized")
	}
	if !errors.Is((&ToolPolicyError{Decision: ToolPolicyPendingApproval, Err: ErrToolApprovalRequired}), ErrToolApprovalRequired) {
		t.Fatal("approval policy error should unwrap to ErrToolApprovalRequired")
	}
}
