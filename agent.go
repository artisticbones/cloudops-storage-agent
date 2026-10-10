package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/artisticbones/hello-agent/config"
)

// Agent 封装 Agent Runtime 的运行状态和依赖。
// model 通过构造函数注入，因此 Runtime 不关心底层使用 Ollama、OpenAI 还是 Fake Client。
type Agent struct {
	model        ModelClient
	cfg          *config.Config
	registry     ToolRegistry
	trace        *Trace
	systemPrompt string
	promptFile   string

	// One Agent instance represents one conversation. runMu serializes turns
	// so the conversation history cannot be corrupted by concurrent Run calls.
	runMu        sync.Mutex
	promptMu     sync.RWMutex
	conversation []Message
}

// NewAgent 创建一个使用指定 ModelClient 的 Agent Runtime。
func NewAgent(model ModelClient, cfg *config.Config, registry ToolRegistry) (*Agent, error) {
	if model == nil {
		return nil, fmt.Errorf("model client cannot be nil")
	}
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if registry == nil {
		return nil, fmt.Errorf("tool registry cannot be nil")
	}
	systemPrompt, err := loadSystemPrompt(cfg)
	if err != nil {
		return nil, err
	}
	return &Agent{
		model:        model,
		cfg:          cfg,
		registry:     registry,
		trace:        &Trace{},
		systemPrompt: systemPrompt,
		promptFile:   promptFile(cfg),
	}, nil
}

func (a *Agent) Trace() *Trace {
	return a.trace
}

// SetSystemPrompt persists and immediately activates a new natural-language
// system instruction for this conversation.
func (a *Agent) SetSystemPrompt(prompt string) error {
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	return a.setSystemPromptLocked(prompt)
}

// AddSystemPrompt appends one natural-language rule to the current system
// instruction and persists the combined result.
func (a *Agent) AddSystemPrompt(instruction string) error {
	a.promptMu.Lock()
	defer a.promptMu.Unlock()

	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		return fmt.Errorf("system prompt cannot be empty")
	}
	combined := strings.TrimSpace(a.systemPrompt)
	if combined != "" {
		combined += "\n"
	}
	return a.setSystemPromptLocked(combined + instruction)
}

func (a *Agent) setSystemPromptLocked(prompt string) error {
	prompt = strings.TrimSpace(prompt)

	if a.promptFile == "" {
		return fmt.Errorf("system prompt file is not configured")
	}
	if err := saveSystemPrompt(a.promptFile, prompt); err != nil {
		return err
	}
	a.systemPrompt = prompt
	return nil
}

func (a *Agent) SystemPrompt() string {
	a.promptMu.RLock()
	defer a.promptMu.RUnlock()
	return a.systemPrompt
}

func promptFile(cfg *config.Config) string {
	if cfg == nil || cfg.Prompt == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Prompt.SystemPromptFile)
}

// Run 执行一次完整的 Agent Loop，并返回最终文本回答。
func (a *Agent) Run(ctx context.Context, input string) (string, error) {
	a.runMu.Lock()
	defer a.runMu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	input = strings.TrimSpace(input)
	if input == "" {
		return "", &AgentError{Kind: InvalidArgument, Message: "user input cannot be empty"}
	}

	messages := cloneMessages(a.conversation)
	if a.systemPrompt != "" {
		messages = append([]Message{{Role: "system", Content: a.systemPrompt}}, messages...)
	}
	request := ModelRequest{
		Messages: append(messages, Message{Content: input, Role: "user"}),
		Tools:    a.registry.Definitions(),
	}
	// Persist the messages accumulated during this turn, including tool calls
	// and tool results. The final assistant message is appended below before a
	// successful return.
	defer func() {
		a.conversation = cloneConversationMessages(request.Messages)
	}()
	businessErrorCalls := make(map[string]int)
	toolRecoveryCount := 0
	a.trace.Add(traceEvent{Event: "agent_start"})

	for step := 1; step <= a.cfg.MaxSteps; step++ {
		// A tool may update the persistent system instruction during the
		// previous step. Refresh it before the next model request.
		if len(request.Messages) > 0 && request.Messages[0].Role == "system" {
			request.Messages[0].Content = a.SystemPrompt()
		}
		// RequestTimeout is a per-loop-step budget. The parent context still
		// controls the lifetime of the whole conversation, while each model/tool
		// turn gets a fresh timeout window.
		loopCtx, loopCancel := context.WithTimeout(ctx, a.cfg.RequestTimeout)
		contextStats := summarizeContext(request)
		a.trace.Add(traceEvent{
			Step:         step,
			Event:        "model_request",
			ContextStats: &contextStats,
		})
		response, err := requestCompletion(loopCtx, a.model, a.cfg, request, step, a.trace)
		if err != nil {
			agentErr := classifyRuntimeError(loopCtx, err)
			loopCancel()
			a.trace.Add(traceEvent{Step: step, Event: "agent_error", Error: agentErr.Error()})
			a.trace.Add(traceEvent{Step: step, Event: "agent_stopped", Reason: string(agentErr.Kind), Error: agentErr.Error()})
			return "", agentErr
		}
		if response == nil {
			agentErr := &AgentError{Kind: ModelError, Message: "model response is nil"}
			loopCancel()
			a.trace.Add(traceEvent{Step: step, Event: "agent_stopped", Reason: string(agentErr.Kind), Error: agentErr.Error()})
			return "", agentErr
		}

		a.trace.Add(traceEvent{Step: step, Event: "model_response"})
		if len(response.ToolCalls) == 0 {
			loopCancel()
			request.Messages = append(request.Messages, Message{
				Role:    "assistant",
				Content: response.Content,
			})
			a.trace.Add(traceEvent{Step: step, Event: "final_answer", Result: response.Content})
			return response.Content, nil
		}

		// 模型发出的 assistant tool call 必须先进入上下文，后面才能追加对应的 tool result。
		request.Messages = append(request.Messages, Message{
			Role:      "assistant",
			Content:   response.Content,
			ToolCalls: response.ToolCalls,
		})
		for _, toolCall := range response.ToolCalls {
			a.trace.Add(traceEvent{
				Step:      step,
				Event:     "tool_call",
				Tool:      toolCall.Name,
				Arguments: toolCall.Arguments,
			})

			start := time.Now()
			var result string
			var err error
			if validationErr := validateToolCall(request, toolCall); validationErr != nil {
				err = validationErr
			} else {
				result, err = dispatchTool(loopCtx, a.registry, toolCall.Name, toolCall.Arguments)
			}
			if err != nil {
				agentErr := classifyToolError(err)
				a.trace.AddDuration(traceEvent{Step: step, Event: "tool_error", Tool: toolCall.Name, Error: agentErr.Error()}, start)
				if loopCtx.Err() != nil {
					loopCancel()
					a.trace.Add(traceEvent{Step: step, Event: "agent_stopped", Tool: toolCall.Name, Reason: string(agentErr.Kind), Error: agentErr.Error()})
					return "", agentErr
				}
				toolRecoveryCount++
				if exceededRecoveryLimit(toolRecoveryCount, a.cfg.Tool.MaxToolRecoveries) {
					stopErr := &AgentError{Kind: BusinessError, Message: fmt.Sprintf("tool recovery limit exceeded: %d", a.cfg.Tool.MaxToolRecoveries), Err: err}
					loopCancel()
					a.trace.Add(traceEvent{Step: step, Event: "agent_stopped", Tool: toolCall.Name, Reason: "max_tool_recoveries", Attempt: toolRecoveryCount, Error: stopErr.Error()})
					return "", stopErr
				}
				if shouldStopBusinessRetry(businessErrorCalls, toolCall.Name+"\x00"+toolCall.Arguments, agentErr.Kind) {
					loopCancel()
					a.trace.Add(traceEvent{Step: step, Event: "agent_stopped", Tool: toolCall.Name, Reason: "business_error_repeated", Error: agentErr.Error()})
					return "", agentErr
				}
				a.trace.Add(traceEvent{Step: step, Event: "retry", Tool: toolCall.Name, Attempt: toolRecoveryCount, Reason: string(agentErr.Kind)})
				result = "工具执行失败：" + err.Error()
			} else {
				a.trace.AddDuration(traceEvent{Step: step, Event: "tool_result", Tool: toolCall.Name, Result: result}, start)
			}

			request.Messages = append(request.Messages, Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: toolCall.ID,
			})
		}
		loopCancel()
	}

	stopErr := &AgentError{Kind: ModelError, Message: fmt.Sprintf("maximum agent steps reached: %d", a.cfg.MaxSteps)}
	a.trace.Add(traceEvent{Step: a.cfg.MaxSteps, Event: "agent_stopped", Reason: "max_steps", Error: stopErr.Error()})
	return "", stopErr
}

func cloneMessages(messages []Message) []Message {
	if len(messages) == 0 {
		return nil
	}
	clone := make([]Message, len(messages))
	copy(clone, messages)
	for i := range clone {
		clone[i].ToolCalls = append([]ToolCall(nil), messages[i].ToolCalls...)
	}
	return clone
}

func cloneConversationMessages(messages []Message) []Message {
	clone := cloneMessages(messages)
	conversation := clone[:0]
	for _, message := range clone {
		if message.Role != "system" {
			conversation = append(conversation, message)
		}
	}
	return conversation
}

// validateToolCall checks semantic constraints that JSON Schema cannot express.
// The model may produce syntactically valid JSON while still selecting the
// wrong calculator operation for the user's request.
func validateToolCall(request ModelRequest, call ToolCall) error {
	if call.Name == expressionCalculatorToolName {
		return validateExpressionToolCall(request, call)
	}
	if call.Name != toolName {
		return nil
	}

	var args calculatorArgs
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		// The handler owns malformed JSON validation and will return the detailed
		// argument error to the model.
		return nil
	}

	userInput := latestUserMessage(request.Messages)
	if countExpressionOperators(userInput) > 1 {
		return &AgentError{
			Kind:    InvalidArgument,
			Message: "compound expression must use expression_calculator; do not split it into a single calculator operation",
		}
	}
	expected, ok := expectedCalculatorOperation(userInput)
	if !ok || args.Operation == expected {
		return nil
	}
	return &AgentError{
		Kind:    InvalidArgument,
		Message: fmt.Sprintf("calculator operation %q does not match user request; expected %q", args.Operation, expected),
	}
}

func validateExpressionToolCall(request ModelRequest, call ToolCall) error {
	var args expressionCalculatorArgs
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		return nil
	}
	want := normalizeExpression(latestUserMessage(request.Messages))
	got := normalizeExpression(args.Expression)
	if want == "" || got == want {
		return nil
	}
	return &AgentError{
		Kind:    InvalidArgument,
		Message: fmt.Sprintf("expression must match the user's original expression; expected %q, got %q", want, got),
	}
}

func normalizeExpression(input string) string {
	var builder strings.Builder
	for _, char := range input {
		switch {
		case unicode.IsDigit(char), char == '.', char == '+', char == '-', char == '*', char == '/', char == '(' || char == ')':
			builder.WriteRune(char)
		case char == '×':
			builder.WriteByte('*')
		case char == '÷':
			builder.WriteByte('/')
		case unicode.IsSpace(char):
			// Whitespace is irrelevant to expression identity.
		}
	}
	return builder.String()
}

func countExpressionOperators(input string) int {
	count := 0
	for _, char := range input {
		switch char {
		case '+', '-', '*', '/', '×', '÷':
			count++
		}
	}
	return count
}

func latestUserMessage(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

func expectedCalculatorOperation(input string) (string, bool) {
	input = strings.ToLower(input)
	switch {
	case strings.Contains(input, "+") || strings.Contains(input, "加") || strings.Contains(input, "相加"):
		return "add", true
	case strings.Contains(input, "-") || strings.Contains(input, "减") || strings.Contains(input, "相减"):
		return "subtract", true
	case strings.Contains(input, "*") || strings.Contains(input, "×") || strings.Contains(input, "乘"):
		return "multiply", true
	case strings.Contains(input, "/") || strings.Contains(input, "÷") || strings.Contains(input, "除"):
		return "divide", true
	default:
		return "", false
	}
}
