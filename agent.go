package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/artisticbones/hello-agent/config"
)

// Agent 封装 Agent Runtime 的运行状态和依赖。
// model 通过构造函数注入，因此 Runtime 不关心底层使用 Ollama、OpenAI 还是 Fake Client。
type Agent struct {
	model    ModelClient
	cfg      *config.Config
	registry ToolRegistry
	trace    *Trace
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
	return &Agent{
		model:    model,
		cfg:      cfg,
		registry: registry,
		trace:    &Trace{},
	}, nil
}

func (a *Agent) Trace() *Trace {
	return a.trace
}

// Run 执行一次完整的 Agent Loop，并返回最终文本回答。
func (a *Agent) Run(ctx context.Context, input string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	input = strings.TrimSpace(input)
	if input == "" {
		return "", &AgentError{Kind: InvalidArgument, Message: "user input cannot be empty"}
	}

	request := ModelRequest{
		Messages: []Message{{Content: input, Role: "user"}},
		Tools:    a.registry.Definitions(),
	}
	businessErrorCalls := make(map[string]int)
	toolRecoveryCount := 0
	a.trace.Add(traceEvent{Event: "agent_start"})

	for step := 1; step <= a.cfg.MaxSteps; step++ {
		a.trace.Add(traceEvent{Step: step, Event: "model_request"})
		response, err := requestCompletion(ctx, a.model, a.cfg, request, step, a.trace)
		if err != nil {
			agentErr := classifyRuntimeError(ctx, err)
			a.trace.Add(traceEvent{Step: step, Event: "agent_error", Error: agentErr.Error()})
			a.trace.Add(traceEvent{Step: step, Event: "agent_stopped", Reason: string(agentErr.Kind), Error: agentErr.Error()})
			return "", agentErr
		}
		if response == nil {
			agentErr := &AgentError{Kind: ModelError, Message: "model response is nil"}
			a.trace.Add(traceEvent{Step: step, Event: "agent_stopped", Reason: string(agentErr.Kind), Error: agentErr.Error()})
			return "", agentErr
		}

		a.trace.Add(traceEvent{Step: step, Event: "model_response"})
		if len(response.ToolCalls) == 0 {
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
			result, err := dispatchTool(ctx, a.registry, toolCall.Name, toolCall.Arguments)
			if err != nil {
				agentErr := classifyToolError(err)
				a.trace.AddDuration(traceEvent{Step: step, Event: "tool_error", Tool: toolCall.Name, Error: agentErr.Error()}, start)
				if ctx.Err() != nil {
					a.trace.Add(traceEvent{Step: step, Event: "agent_stopped", Tool: toolCall.Name, Reason: string(agentErr.Kind), Error: agentErr.Error()})
					return "", agentErr
				}
				toolRecoveryCount++
				if exceededRecoveryLimit(toolRecoveryCount, a.cfg.Tool.MaxToolRecoveries) {
					stopErr := &AgentError{Kind: BusinessError, Message: fmt.Sprintf("tool recovery limit exceeded: %d", a.cfg.Tool.MaxToolRecoveries), Err: err}
					a.trace.Add(traceEvent{Step: step, Event: "agent_stopped", Tool: toolCall.Name, Reason: "max_tool_recoveries", Attempt: toolRecoveryCount, Error: stopErr.Error()})
					return "", stopErr
				}
				if shouldStopBusinessRetry(businessErrorCalls, toolCall.Name+"\x00"+toolCall.Arguments, agentErr.Kind) {
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
	}

	stopErr := &AgentError{Kind: ModelError, Message: fmt.Sprintf("maximum agent steps reached: %d", a.cfg.MaxSteps)}
	a.trace.Add(traceEvent{Step: a.cfg.MaxSteps, Event: "agent_stopped", Reason: "max_steps", Error: stopErr.Error()})
	return "", stopErr
}
