package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/artisticbones/hello-agent/config"
)

// EvalCase describes one deterministic regression scenario. ModelResults and
// Tools use the existing fakes, so evals never call a network service.
type EvalCase struct {
	Name                    string
	Input                   string
	ModelResults            []FakeModelResult
	Tools                   []Tool
	ExpectedAnswer          string
	ExpectedAnswerContains  []string
	ForbiddenAnswerContains []string
	ExpectedTool            string
	WantErrorKind           ErrorKind
	WantTraceErrorKind      ErrorKind
	WantReason              string
	WantToolCalls           int
	ExpectedToolCalls       *int
	MaxSteps                int
	MaxToolRecoveries       int
	MaxToolRecoveriesSet    bool
	Retriever               Retriever
	RetrievalTenant         string
	RetrievalVersion        string
	ExpectedSources         []string
	ExpectedRequestContains []string
	ModelResponder          func(ModelRequest) (*ModelResponse, error)
	Budget                  ModelBudget
	ModelInputCostPerToken  float64
	ModelOutputCostPerToken float64
	WantRejectedTool        string
}

type EvalResult struct {
	CaseName       string
	Passed         bool
	Answer         string
	Failure        string
	ErrorKind      ErrorKind
	ToolCalls      int
	BudgetExceeded bool
	Trace          *Trace
}

type EvalRunner struct{}

func NewEvalRunner() *EvalRunner { return &EvalRunner{} }

func (r *EvalRunner) RunAll(ctx context.Context, cases []EvalCase) []EvalResult {
	results := make([]EvalResult, 0, len(cases))
	for _, evalCase := range cases {
		results = append(results, r.Run(ctx, evalCase))
	}
	return results
}

func (r *EvalRunner) Run(ctx context.Context, evalCase EvalCase) EvalResult {
	result := EvalResult{CaseName: evalCase.Name}
	if strings.TrimSpace(result.CaseName) == "" {
		result.CaseName = "unnamed"
	}
	if err := validateCitationEvalCase(evalCase); err != nil {
		result.ErrorKind = InvalidArgument
		result.Failure = err.Error()
		return result
	}
	model := NewFakeModelClient(evalCase.ModelResults...)
	model.ResponseFunc = evalCase.ModelResponder
	registry := NewToolRegistry()
	tools := evalCase.Tools
	if len(tools) == 0 && evalCase.ExpectedTool == toolName {
		tools = []Tool{calculatorTool()}
	}
	for _, tool := range tools {
		if err := registry.Register(tool); err != nil {
			result.Failure = "register tool: " + err.Error()
			return result
		}
	}
	maxSteps := evalCase.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 5
	}
	recoveries := evalCase.MaxToolRecoveries
	if !evalCase.MaxToolRecoveriesSet && recoveries <= 0 {
		recoveries = 2
	}
	cfg := &config.Config{
		MaxSteps: maxSteps, MaxRetries: 0, RequestTimeout: time.Minute,
		Model: &config.ModelConfig{Name: "eval", BaseURL: "fake://model", InputCostPerToken: evalCase.ModelInputCostPerToken, OutputCostPerToken: evalCase.ModelOutputCostPerToken},
		Tool:  &config.ToolConfig{MaxToolRecoveries: recoveries},
	}
	var agent *Agent
	var err error
	if evalCase.Budget != (ModelBudget{}) {
		router := NewModelRouter(ModelRouterOptions{
			Primary: ModelEndpoint{Name: "eval", Client: model, Capability: ModelCapability{Name: "eval", InputCostPerToken: evalCase.ModelInputCostPerToken, OutputCostPerToken: evalCase.ModelOutputCostPerToken}},
			Budget:  evalCase.Budget,
		})
		agent, err = NewAgentWithRouter(router, cfg, registry)
	} else {
		agent, err = newAgent(model, cfg, registry, nil, NewContextBuilderWithRetriever(contextBudget(cfg), evalCase.Retriever), &AllowAllToolPolicy{}, NewMemoryIdempotencyStore())
	}
	if agent != nil && evalCase.Retriever != nil {
		agent.retriever = evalCase.Retriever
		agent.retrievalTenant = evalCase.RetrievalTenant
		agent.retrievalVersion = evalCase.RetrievalVersion
		agent.retrievalLimit = 5
	}
	if err != nil {
		result.Failure = "create agent: " + err.Error()
		return result
	}
	result.Answer, err = agent.Run(ctx, evalCase.Input)
	result.Trace = agent.Trace()
	result.ToolCalls = countTraceEvents(result.Trace, "tool_call")
	if err != nil {
		result.ErrorKind = errorKindOf(err)
		result.BudgetExceeded = result.ErrorKind == BudgetError
	}
	if evalCase.WantErrorKind != "" {
		if err == nil || result.ErrorKind != evalCase.WantErrorKind {
			result.Failure = fmt.Sprintf("error kind = %q, want %q", result.ErrorKind, evalCase.WantErrorKind)
			return result
		}
	} else if err != nil {
		result.Failure = "unexpected error: " + err.Error()
		return result
	}
	if evalCase.ExpectedAnswer != "" && result.Answer != evalCase.ExpectedAnswer {
		result.Failure = fmt.Sprintf("answer = %q, want %q", result.Answer, evalCase.ExpectedAnswer)
		return result
	}
	for _, fragment := range evalCase.ExpectedAnswerContains {
		if !strings.Contains(result.Answer, fragment) {
			result.Failure = fmt.Sprintf("answer = %q, want it to contain %q", result.Answer, fragment)
			return result
		}
	}
	for _, fragment := range evalCase.ForbiddenAnswerContains {
		if strings.Contains(result.Answer, fragment) {
			result.Failure = fmt.Sprintf("answer = %q, must not contain %q", result.Answer, fragment)
			return result
		}
	}
	if evalCase.ExpectedTool != "" && !traceHasTool(result.Trace, evalCase.ExpectedTool) {
		result.Failure = "expected tool was not selected: " + evalCase.ExpectedTool
		return result
	}
	if evalCase.ExpectedToolCalls != nil && result.ToolCalls != *evalCase.ExpectedToolCalls {
		result.Failure = fmt.Sprintf("tool calls = %d, want %d", result.ToolCalls, *evalCase.ExpectedToolCalls)
		return result
	}
	if evalCase.ExpectedToolCalls == nil && evalCase.WantToolCalls >= 0 && evalCase.WantToolCalls != 0 && result.ToolCalls != evalCase.WantToolCalls {
		result.Failure = fmt.Sprintf("tool calls = %d, want %d", result.ToolCalls, evalCase.WantToolCalls)
		return result
	}
	for _, source := range evalCase.ExpectedSources {
		if !modelRequestsContainRetrievedSource(model.ReceivedRequests, source) {
			result.Failure = "model request did not contain expected retrieved source: " + source
			return result
		}
	}
	for _, fragment := range evalCase.ExpectedRequestContains {
		found := false
		if modelRequestsContain(model.ReceivedRequests, fragment) {
			found = true
		}
		if !found {
			result.Failure = "model request did not contain expected text: " + fragment
			return result
		}
	}
	if evalCase.WantRejectedTool != "" {
		for _, tool := range tools {
			if fake, ok := tool.(*FakeToolExecutor); ok && fake.ToolName == evalCase.WantRejectedTool && fake.CallCount != 0 {
				result.Failure = fmt.Sprintf("rejected tool handler call count = %d, want 0", fake.CallCount)
				return result
			}
		}
		if !traceHasReason(result.Trace, string(ToolPendingApproval)) && !traceHasReason(result.Trace, string(ToolDenied)) {
			result.Failure = "trace did not contain a tool refusal event"
			return result
		}
	}
	if evalCase.WantTraceErrorKind != "" && !traceHasText(result.Trace, "tool_error", string(evalCase.WantTraceErrorKind)) {
		result.Failure = "trace did not contain error kind: " + string(evalCase.WantTraceErrorKind)
		return result
	}
	if evalCase.WantReason != "" && !traceHasReason(result.Trace, evalCase.WantReason) {
		result.Failure = "trace did not contain reason: " + evalCase.WantReason
		return result
	}
	result.Passed = true
	return result
}

func errorKindOf(err error) ErrorKind {
	var agentErr *AgentError
	if errors.As(err, &agentErr) {
		return agentErr.Kind
	}
	return ""
}

func countTraceEvents(trace *Trace, name string) int {
	count := 0
	for _, event := range trace.Events {
		if event.Event == name {
			count++
		}
	}
	return count
}
func traceHasTool(trace *Trace, name string) bool {
	for _, event := range trace.Events {
		if event.Event == "tool_call" && event.Tool == name {
			return true
		}
	}
	return false
}
func traceHasText(trace *Trace, eventName, text string) bool {
	for _, event := range trace.Events {
		if event.Event == eventName && strings.Contains(event.Error, text) {
			return true
		}
	}
	return false
}
func traceHasReason(trace *Trace, reason string) bool {
	for _, event := range trace.Events {
		if event.Reason == reason {
			return true
		}
	}
	return false
}

func requestContainsText(request ModelRequest, fragment string) bool {
	for _, message := range request.Messages {
		if strings.Contains(message.Content, fragment) {
			return true
		}
	}
	return false
}

func modelRequestsContain(requests []ModelRequest, fragment string) bool {
	for _, request := range requests {
		if requestContainsText(request, fragment) {
			return true
		}
	}
	return false
}

func modelRequestsContainRetrievedSource(requests []ModelRequest, fragment string) bool {
	for _, request := range requests {
		for _, message := range request.Messages {
			if message.Role == "system" && strings.Contains(message.Content, "Retrieved context") && strings.Contains(message.Content, fragment) {
				return true
			}
		}
	}
	return false
}

func validateCitationEvalCase(evalCase EvalCase) error {
	if !citationExpected(evalCase) {
		return nil
	}
	if evalCase.Retriever == nil {
		return fmt.Errorf("invalid citation eval case: Retriever is required")
	}
	if strings.TrimSpace(evalCase.RetrievalTenant) == "" {
		return fmt.Errorf("invalid citation eval case: RetrievalTenant is required")
	}
	if strings.TrimSpace(evalCase.RetrievalVersion) == "" {
		return fmt.Errorf("invalid citation eval case: RetrievalVersion is required")
	}
	if len(evalCase.ExpectedSources) == 0 {
		return fmt.Errorf("invalid citation eval case: expected Source/citation identifiers are required")
	}
	for _, source := range evalCase.ExpectedSources {
		if strings.TrimSpace(source) == "" {
			return fmt.Errorf("invalid citation eval case: expected Source/citation identifiers cannot be empty")
		}
	}
	return nil
}

func citationExpected(evalCase EvalCase) bool {
	if len(evalCase.ExpectedSources) > 0 || len(evalCase.ExpectedRequestContains) > 0 {
		return true
	}
	for _, fragment := range evalCase.ExpectedAnswerContains {
		if strings.Contains(fragment, "[") && strings.Contains(fragment, "]") {
			return true
		}
	}
	return false
}
