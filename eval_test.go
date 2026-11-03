package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func intPointer(value int) *int { return &value }

func TestEvalRunnerCoversToolSelectionAndParameterValidation(t *testing.T) {
	runner := NewEvalRunner()
	results := runner.RunAll(context.Background(), []EvalCase{
		{
			Name: "selects calculator", Input: "1 + 2 等于多少",
			ModelResults: []FakeModelResult{
				NewFakeCalculatorToolCallResult("call-1", `{"a":1,"b":2,"operation":"add"}`), NewFakeTextResult("3"),
			}, ExpectedAnswer: "3", ExpectedTool: toolName,
		},
		{
			Name: "rejects mismatched operation", Input: "1 + 2 等于多少",
			ModelResults: []FakeModelResult{
				NewFakeCalculatorToolCallResult("call-1", `{"a":1,"b":2,"operation":"multiply"}`),
				NewFakeTextResult("已修正"),
			}, ExpectedAnswer: "已修正", WantTraceErrorKind: InvalidArgument,
		},
	})
	if len(results) != 2 || !results[0].Passed || !results[1].Passed {
		for _, result := range results {
			t.Logf("result = %+v", result)
		}
		t.Fatal("tool selection/parameter validation eval failed")
	}
}

func TestEvalRunnerCoversCitationAndHallucinationGuardrails(t *testing.T) {
	runner := NewEvalRunner()
	retriever := NewMemoryRetriever([]Document{{
		ID: "refund-doc", TenantID: "tenant-a", Version: "v1",
		Content: "退款政策：30 天内可办理。", Source: Source{ID: "policy-1", URI: "kb://tenant-a/refunds"},
	}})
	results := runner.RunAll(context.Background(), []EvalCase{
		{
			Name: "answer cites retrieved source", Input: "退款政策",
			Retriever: retriever, RetrievalTenant: "tenant-a", RetrievalVersion: "v1",
			ExpectedSources: []string{"policy-1", "kb://tenant-a/refunds"},
			ModelResponder: func(request ModelRequest) (*ModelResponse, error) {
				if !requestContains(request, "policy-1") {
					return nil, fmt.Errorf("source was not retrieved")
				}
				return &ModelResponse{Content: "根据 [policy-1]，30 天内可退款。"}, nil
			},
			ExpectedAnswerContains: []string{"[policy-1]"}, ExpectedRequestContains: []string{"Retrieved context"},
		},
		{
			Name: "does not invent unsupported fact", Input: "资料中没有的城市是什么？",
			ModelResults:            []FakeModelResult{NewFakeTextResult("无法从提供的资料确认。")},
			ExpectedAnswer:          "无法从提供的资料确认。",
			ForbiddenAnswerContains: []string{"北京", "上海"},
		},
	})
	for _, result := range results {
		if !result.Passed {
			t.Fatalf("eval %q failed: %s", result.CaseName, result.Failure)
		}
	}
}

func TestEvalRunnerRejectsCitationWhenSourceIsNotInModelRequest(t *testing.T) {
	result := NewEvalRunner().Run(context.Background(), EvalCase{
		Name: "hard-coded citation is not evidence", Input: "退款政策？",
		ModelResults:           []FakeModelResult{NewFakeTextResult("根据 [policy-1]，30 天内可退款。")},
		ExpectedAnswerContains: []string{"[policy-1]"}, ExpectedSources: []string{"policy-1"},
	})
	if result.Passed || result.ErrorKind != InvalidArgument || !strings.Contains(result.Failure, "Retriever") {
		t.Fatalf("result = %+v, want InvalidArgument retriever failure", result)
	}
}

func TestEvalRunnerRequiresAllCitationEvidenceConfiguration(t *testing.T) {
	base := EvalCase{
		Name: "citation configuration", Input: "退款政策？",
		Retriever:       NewMemoryRetriever([]Document{{Source: Source{ID: "policy-1"}, Content: "退款政策"}}),
		RetrievalTenant: "tenant-a", RetrievalVersion: "v1",
		ExpectedSources:        []string{"policy-1"},
		ModelResults:           []FakeModelResult{NewFakeTextResult("根据 [policy-1]，30 天内可退款。")},
		ExpectedAnswerContains: []string{"[policy-1]"},
	}
	cases := []struct {
		name   string
		mutate func(*EvalCase)
	}{
		{name: "retriever", mutate: func(evalCase *EvalCase) { evalCase.Retriever = nil }},
		{name: "tenant", mutate: func(evalCase *EvalCase) { evalCase.RetrievalTenant = "" }},
		{name: "version", mutate: func(evalCase *EvalCase) { evalCase.RetrievalVersion = "" }},
		{name: "source", mutate: func(evalCase *EvalCase) { evalCase.ExpectedSources = nil }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			evalCase := base
			testCase.mutate(&evalCase)
			result := NewEvalRunner().Run(context.Background(), evalCase)
			if result.Passed || result.ErrorKind != InvalidArgument {
				t.Fatalf("result = %+v, want InvalidArgument failure", result)
			}
		})
	}
}

func TestEvalRunnerEnforcesExecutableBudgets(t *testing.T) {
	cases := []EvalCase{
		{
			Name: "max calls budget", Input: "hello", Tools: []Tool{NewFakeToolExecutor("lookup", ToolResult{Content: "ok"}, nil)},
			ModelResults: []FakeModelResult{{Response: &ModelResponse{ToolCalls: []ToolCall{{ID: "c1", Name: "lookup", Arguments: `{}`}}}}, NewFakeTextResult("ok")},
			Budget:       ModelBudget{MaxCalls: 1}, WantErrorKind: BudgetError,
		},
		{
			Name: "output budget", Input: "hello", ModelResults: []FakeModelResult{NewFakeTextResult("one two three")},
			Budget: ModelBudget{MaxOutputTokens: 1}, WantErrorKind: BudgetError,
		},
		{
			Name: "cost budget", Input: "hello", ModelResults: []FakeModelResult{NewFakeTextResult("ok")},
			Budget: ModelBudget{MaxCost: 0.001}, ModelInputCostPerToken: 0.001, ModelOutputCostPerToken: 0.001,
			WantErrorKind: BudgetError,
		},
	}
	for _, evalCase := range cases {
		result := NewEvalRunner().Run(context.Background(), evalCase)
		if !result.Passed || result.ErrorKind != BudgetError || !result.BudgetExceeded {
			t.Fatalf("budget eval %q = %+v, want clear BudgetError result", evalCase.Name, result)
		}
	}
}

func TestEvalRunnerCoversRecoveryMaxStepsAndSecurityRefusal(t *testing.T) {
	denied := NewFakeToolExecutor("danger", ToolResult{Content: "must not run"}, nil)
	denied.Spec = ToolSpec{Risk: RiskHigh, SideEffect: true}
	runner := NewEvalRunner()
	results := runner.RunAll(context.Background(), []EvalCase{
		{
			Name: "recovers tool error", Input: "try", Tools: []Tool{NewFakeToolExecutor("lookup", ToolResult{}, errors.New("backend unavailable"))},
			ModelResults:   []FakeModelResult{{Response: &ModelResponse{ToolCalls: []ToolCall{{ID: "c1", Name: "lookup", Arguments: `{}`}}}}, NewFakeTextResult("recovered")},
			ExpectedAnswer: "recovered", MaxToolRecoveries: 1,
		},
		{
			Name: "stops at max steps", Input: "loop", MaxSteps: 1,
			ModelResults: []FakeModelResult{{Response: &ModelResponse{ToolCalls: []ToolCall{{ID: "c1", Name: "loop", Arguments: `{}`}}}}, {Response: &ModelResponse{ToolCalls: []ToolCall{{ID: "c2", Name: "loop", Arguments: `{}`}}}}},
			Tools:        []Tool{NewFakeToolExecutor("loop", ToolResult{Content: "again"}, nil)}, WantErrorKind: ModelError, WantReason: "max_steps",
		},
		{
			Name: "refuses dangerous tool", Input: "delete", Tools: []Tool{denied},
			ModelResults:   []FakeModelResult{{Response: &ModelResponse{ToolCalls: []ToolCall{{ID: "c1", Name: "danger", Arguments: `{}`}}}}, NewFakeTextResult("refused")},
			ExpectedAnswer: "refused", WantReason: string(ToolPendingApproval),
			WantRejectedTool: "danger",
		},
	})
	for _, result := range results {
		if !result.Passed {
			t.Fatalf("eval %q failed: %s", result.CaseName, result.Failure)
		}
	}
}

func requestContains(request ModelRequest, fragment string) bool {
	for _, message := range request.Messages {
		if strings.Contains(message.Content, fragment) {
			return true
		}
	}
	return false
}

func TestEvalRunnerHonorsExplicitZeroToolRecoveries(t *testing.T) {
	runner := NewEvalRunner()
	result := runner.Run(context.Background(), EvalCase{
		Name: "zero recoveries stops immediately", Input: "try",
		Tools:                []Tool{NewFakeToolExecutor("lookup", ToolResult{}, errors.New("backend unavailable"))},
		ModelResults:         []FakeModelResult{{Response: &ModelResponse{ToolCalls: []ToolCall{{ID: "c1", Name: "lookup", Arguments: `{}`}}}}},
		MaxToolRecoveries:    0,
		MaxToolRecoveriesSet: true,
		WantErrorKind:        BusinessError,
	})
	if !result.Passed {
		t.Fatalf("zero recovery eval failed: %+v", result)
	}
}

func TestEvalRunnerHonorsExplicitZeroExpectedToolCalls(t *testing.T) {
	runner := NewEvalRunner()
	result := runner.Run(context.Background(), EvalCase{
		Name: "zero expected calls rejects a call", Input: "try",
		Tools: []Tool{NewFakeToolExecutor("lookup", ToolResult{Content: "ok"}, nil)},
		ModelResults: []FakeModelResult{
			{Response: &ModelResponse{ToolCalls: []ToolCall{{ID: "c1", Name: "lookup", Arguments: `{}`}}}},
			NewFakeTextResult("done"),
		},
		ExpectedToolCalls: intPointer(0),
	})
	if result.Passed {
		t.Fatalf("zero expected calls unexpectedly passed: %+v", result)
	}
}
