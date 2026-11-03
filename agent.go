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
	model            ModelClient
	router           ModelRouter
	cfg              *config.Config
	registry         ToolRegistry
	trace            *Trace
	checkpointStore  CheckpointStore
	toolPolicy       ToolPolicy
	idempotency      IdempotencyStore
	contextBuilder   ContextBuilder
	session          Session
	lastRun          *Run
	systemPrompt     string
	promptFile       string
	metrics          *MemoryMetrics
	retriever        Retriever
	retrievalTenant  string
	retrievalVersion string
	retrievalLimit   int

	// One Agent instance represents one conversation. runMu serializes turns
	// so the conversation history cannot be corrupted by concurrent Run calls.
	runMu        sync.Mutex
	promptMu     sync.RWMutex
	conversation []Message
}

// NewAgent 创建一个使用指定 ModelClient 的 Agent Runtime。
func NewAgent(model ModelClient, cfg *config.Config, registry ToolRegistry) (*Agent, error) {
	return newAgent(model, cfg, registry, nil, newDefaultContextBuilder(cfg), &AllowAllToolPolicy{}, NewMemoryIdempotencyStore())
}

// NewAgentWithRouter injects a router while preserving NewAgent's legacy
// ModelClient constructor and its behavior.
func NewAgentWithRouter(router ModelRouter, cfg *config.Config, registry ToolRegistry) (*Agent, error) {
	if router == nil {
		return nil, fmt.Errorf("model router cannot be nil")
	}
	agent, err := newAgent(routerPlaceholder{}, cfg, registry, nil, newDefaultContextBuilder(cfg), &AllowAllToolPolicy{}, NewMemoryIdempotencyStore())
	if err != nil {
		return nil, err
	}
	agent.router = router
	return agent, nil
}

type routerPlaceholder struct{}

func (routerPlaceholder) Complete(context.Context, ModelRequest) (*ModelResponse, error) {
	return nil, fmt.Errorf("router placeholder must not be called")
}

// NewAgentWithContextBuilder injects the context policy used to build each
// model-facing request. NewAgent uses the configured bounded default policy.
func NewAgentWithContextBuilder(model ModelClient, cfg *config.Config, registry ToolRegistry, builder ContextBuilder) (*Agent, error) {
	if builder == nil {
		return nil, fmt.Errorf("context builder cannot be nil")
	}
	return newAgent(model, cfg, registry, nil, builder, &AllowAllToolPolicy{}, NewMemoryIdempotencyStore())
}

// NewAgentWithRetriever enables per-turn retrieval while preserving the
// existing Agent Loop and the legacy constructors.
func NewAgentWithRetriever(model ModelClient, cfg *config.Config, registry ToolRegistry, retriever Retriever, tenant, version string) (*Agent, error) {
	if retriever == nil {
		return nil, fmt.Errorf("retriever cannot be nil")
	}
	tenant = strings.TrimSpace(tenant)
	version = strings.TrimSpace(version)
	if tenant == "" {
		return nil, &AgentError{Kind: InvalidArgument, Message: "retrieval tenant cannot be empty"}
	}
	if version == "" {
		return nil, &AgentError{Kind: InvalidArgument, Message: "retrieval version cannot be empty"}
	}
	agent, err := newAgent(model, cfg, registry, nil, NewContextBuilderWithRetriever(contextBudget(cfg), retriever), &AllowAllToolPolicy{}, NewMemoryIdempotencyStore())
	if err != nil {
		return nil, err
	}
	agent.retriever = retriever
	agent.retrievalTenant = tenant
	agent.retrievalVersion = version
	agent.retrievalLimit = 5
	return agent, nil
}

// NewAgentWithCheckpointStore enables the optional process-recovery path while
// preserving NewAgent's existing API and context budget behavior.
func NewAgentWithCheckpointStore(model ModelClient, cfg *config.Config, registry ToolRegistry, store CheckpointStore) (*Agent, error) {
	if store == nil {
		return nil, fmt.Errorf("checkpoint store cannot be nil")
	}
	return newAgent(model, cfg, registry, store, newDefaultContextBuilder(cfg), &AllowAllToolPolicy{}, NewMemoryIdempotencyStore())
}

// NewAgentWithToolControls injects authorization and idempotency boundaries
// while keeping the existing NewAgent constructor unchanged.
func NewAgentWithToolControls(model ModelClient, cfg *config.Config, registry ToolRegistry, policy ToolPolicy, store IdempotencyStore) (*Agent, error) {
	return newAgent(model, cfg, registry, nil, newDefaultContextBuilder(cfg), policy, store)
}

// NewAgentWithToolPolicy is a convenience constructor for callers that only
// need a custom policy and are happy with the process-local idempotency store.
func NewAgentWithToolPolicy(model ModelClient, cfg *config.Config, registry ToolRegistry, policy ToolPolicy) (*Agent, error) {
	return NewAgentWithToolControls(model, cfg, registry, policy, NewMemoryIdempotencyStore())
}

func newDefaultContextBuilder(cfg *config.Config) ContextBuilder {
	return NewContextBuilder(contextBudget(cfg))
}

func contextBudget(cfg *config.Config) int {
	budget := config.DefaultContextTokenBudget
	if cfg != nil && cfg.ContextTokenBudget > 0 {
		budget = cfg.ContextTokenBudget
	}
	return budget
}

func newAgent(model ModelClient, cfg *config.Config, registry ToolRegistry, store CheckpointStore, builder ContextBuilder, policy ToolPolicy, idempotency IdempotencyStore) (*Agent, error) {
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
	if policy == nil {
		policy = &AllowAllToolPolicy{}
	}
	if idempotency == nil {
		idempotency = NewMemoryIdempotencyStore()
	}
	systemPrompt, err := loadSystemPrompt(cfg)
	if err != nil {
		return nil, err
	}
	return &Agent{
		model:           model,
		cfg:             cfg,
		registry:        registry,
		trace:           NewTrace(),
		metrics:         NewMemoryMetrics(),
		checkpointStore: store,
		toolPolicy:      policy,
		idempotency:     idempotency,
		contextBuilder:  builder,
		session:         NewSession(),
		systemPrompt:    systemPrompt,
		promptFile:      promptFile(cfg),
	}, nil
}

func (a *Agent) Trace() *Trace {
	return a.trace
}

func (a *Agent) Metrics() *MemoryMetrics { return a.metrics }

// Session returns a snapshot of the current conversation identity and history.
func (a *Agent) Session() Session {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return cloneSession(a.session)
}

// LastRun returns the most recently started run, if any.
func (a *Agent) LastRun() (Run, bool) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.lastRun == nil {
		return Run{}, false
	}
	return *a.lastRun, true
}

// GetRun loads a run status from the configured checkpoint store.
func (a *Agent) GetRun(ctx context.Context, runID string) (Run, error) {
	if a.checkpointStore == nil {
		return Run{}, fmt.Errorf("checkpoint store is not configured")
	}
	checkpoint, err := a.checkpointStore.Load(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	return checkpoint.Run, nil
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
	return a.RunWithEvents(ctx, input, "", nil)
}

// RunWithEvents is the service-layer adapter for Agent.Run. An empty runID
// preserves the existing behavior; a supplied ID lets the service publish a
// stable identifier before the asynchronous run starts.
func (a *Agent) RunWithEvents(ctx context.Context, input, runID string, sink func(RunEvent)) (string, error) {
	a.runMu.Lock()
	defer a.runMu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	input = strings.TrimSpace(input)
	if input == "" {
		return "", &AgentError{Kind: InvalidArgument, Message: "user input cannot be empty"}
	}

	run := NewRun(a.session.ID)
	if runID != "" {
		run.ID = runID
	}
	run.Input = input
	a.lastRun = &run
	messages := append(cloneMessages(a.conversation), Message{Content: input, Role: "user"})
	return a.runLoop(ctx, run, messages, sink)
}

// Resume continues a run from its latest checkpoint. A completed run returns
// its stored final answer without calling the model or any Tool again.
func (a *Agent) Resume(ctx context.Context, runID string) (string, error) {
	a.runMu.Lock()
	defer a.runMu.Unlock()

	if a.checkpointStore == nil {
		return "", fmt.Errorf("checkpoint store is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	checkpoint, err := a.checkpointStore.Load(ctx, runID)
	if err != nil {
		return "", err
	}
	a.session = cloneSession(checkpoint.Session)
	a.conversation = cloneMessages(checkpoint.Messages)
	if len(checkpoint.Session.Messages) > 0 {
		a.conversation = cloneMessages(checkpoint.Session.Messages)
	} else {
		a.session.Messages = cloneMessages(checkpoint.Messages)
	}
	if checkpoint.Run.Status == RunCompleted {
		return checkpoint.Result, nil
	}

	run := checkpoint.Run
	a.lastRun = &run
	return a.runLoop(ctx, run, checkpoint.Messages, nil)
}

func (a *Agent) runLoop(ctx context.Context, run Run, messages []Message, sink func(RunEvent)) (answer string, runErr error) {
	runStart := time.Now()
	defer func() { a.metrics.RecordRun(run.SessionID, run.ID, time.Since(runStart), runErr == nil) }()
	conversationMessages := cloneMessages(messages)
	completedTools := make(map[string]ToolResult)
	if a.checkpointStore != nil {
		if checkpoint, err := a.checkpointStore.Load(context.Background(), run.ID); err == nil {
			clonedTools, cloneErr := cloneToolResults(checkpoint.CompletedTools)
			if cloneErr != nil {
				return "", cloneErr
			}
			completedTools = clonedTools
		}
	}
	defer func() {
		a.conversation = cloneMessages(conversationMessages)
		a.session.Messages = sanitizePersistentConversation(a.conversation, a.registry, Identity{SessionID: run.SessionID, RunID: run.ID})
		a.session.UpdatedAt = time.Now().UTC()
		run.UpdatedAt = time.Now().UTC()
		a.lastRun = &run
	}()

	a.trace.SetIdentity(run.SessionID, run.ID)
	a.trace.Add(traceEvent{Event: "agent_start"})
	emitRunEvent(sink, RunEvent{Type: EventRunStarted, RunID: run.ID, SessionID: run.SessionID})
	defer func() {
		event := RunEvent{
			Type:      EventRunCompleted,
			RunID:     run.ID,
			SessionID: run.SessionID,
			Step:      run.Step,
			Status:    run.Status,
			Result:    run.Result,
		}
		if runErr != nil {
			event.Error = runErr.Error()
		}
		emitRunEvent(sink, event)
	}()
	if err := a.persistCheckpoint(&run, conversationMessages, completedTools, "", RunRunning, nil); err != nil {
		return "", err
	}
	businessErrorCalls := make(map[string]int)
	toolRecoveryCount := 0

	for step := 1; step <= a.cfg.MaxSteps; step++ {
		run.Step = step
		if err := a.persistCheckpoint(&run, conversationMessages, completedTools, "", RunRunning, nil); err != nil {
			return "", err
		}
		// RequestTimeout is a per-loop-step budget. The parent context still
		// controls the lifetime of the whole conversation, while each model/tool
		// turn gets a fresh timeout window.
		loopCtx, loopCancel := context.WithTimeout(ctx, a.cfg.RequestTimeout)
		contextStart := time.Now()
		request, contextReport, buildErr := a.contextBuilder.Build(ctx, RuntimeState{
			SystemPrompt:     a.SystemPrompt(),
			Messages:         conversationMessages,
			Tools:            a.registry.Definitions(),
			Retriever:        a.retriever,
			RetrievalTenant:  a.retrievalTenant,
			RetrievalVersion: a.retrievalVersion,
			RetrievalLimit:   a.retrievalLimit,
		})
		a.metrics.RecordContext(run.ID, time.Since(contextStart))
		if buildErr != nil {
			agentErr := classifyRuntimeError(loopCtx, buildErr)
			status := RunFailed
			if ctx.Err() != nil || loopCtx.Err() != nil {
				status = RunInterrupted
			}
			loopCancel()
			_ = a.persistCheckpoint(&run, conversationMessages, completedTools, "", status, agentErr)
			a.trace.Add(traceEvent{Step: step, Event: "agent_stopped", Reason: string(agentErr.Kind), Error: agentErr.Error()})
			return "", agentErr
		}
		a.trace.Add(traceEvent{
			Step:         step,
			Event:        "model_request",
			ContextStats: &contextReport.Stats,
		})
		emitRunEvent(sink, RunEvent{Type: EventModelStarted, RunID: run.ID, SessionID: run.SessionID, Step: step})
		modelStart := time.Now()
		var response *ModelResponse
		var err error
		if a.router != nil {
			response, err = requestCompletionWithRouter(loopCtx, a.router, request)
		} else {
			response, err = requestCompletion(loopCtx, a.model, a.cfg, request, step, a.trace)
		}
		a.metrics.RecordModel(run.ID, a.cfg.Model.Name, time.Since(modelStart), err == nil)
		if err != nil {
			agentErr := classifyRuntimeError(loopCtx, err)
			parentErr := ctx.Err()
			loopErr := loopCtx.Err()
			status := RunFailed
			if parentErr != nil || loopErr != nil {
				status = RunInterrupted
			}
			loopCancel()
			_ = a.persistCheckpoint(&run, conversationMessages, completedTools, "", status, agentErr)
			a.trace.Add(traceEvent{Step: step, Event: "agent_error", Error: agentErr.Error()})
			a.trace.Add(traceEvent{Step: step, Event: "agent_stopped", Reason: string(agentErr.Kind), Error: agentErr.Error()})
			return "", agentErr
		}
		if response == nil {
			agentErr := &AgentError{Kind: ModelError, Message: "model response is nil"}
			loopCancel()
			_ = a.persistCheckpoint(&run, conversationMessages, completedTools, "", RunFailed, agentErr)
			a.trace.Add(traceEvent{Step: step, Event: "agent_stopped", Reason: string(agentErr.Kind), Error: agentErr.Error()})
			return "", agentErr
		}

		a.trace.Add(traceEvent{Step: step, Event: "model_response"})
		if len(response.ToolCalls) == 0 {
			if agentErr := cancelledAgentError(ctx, loopCtx); agentErr != nil {
				loopCancel()
				_ = a.persistCheckpoint(&run, conversationMessages, completedTools, "", RunInterrupted, agentErr)
				a.trace.Add(traceEvent{Step: step, Event: "agent_stopped", Reason: string(agentErr.Kind), Error: agentErr.Error()})
				return "", agentErr
			}
			loopCancel()
			conversationMessages = append(conversationMessages, Message{
				Role:    "assistant",
				Content: response.Content,
			})
			if err := a.persistCheckpoint(&run, conversationMessages, completedTools, response.Content, RunCompleted, nil); err != nil {
				loopCancel()
				return "", err
			}
			a.trace.Add(traceEvent{Step: step, Event: "final_answer", Result: response.Content})
			return response.Content, nil
		}

		// 模型发出的 assistant tool call 必须先进入上下文，后面才能追加对应的 tool result。
		conversationMessages = append(conversationMessages, Message{
			Role:      "assistant",
			Content:   response.Content,
			ToolCalls: response.ToolCalls,
		})
		for _, toolCall := range response.ToolCalls {
			traceSpec := ToolSpec{Name: toolCall.Name}
			if traceTool, ok := a.registry.Find(toolCall.Name); ok {
				traceSpec = toolSpecFor(traceTool)
			}
			a.trace.Add(traceEvent{
				Step:       step,
				Event:      "tool_call",
				Tool:       toolCall.Name,
				Arguments:  summarizeToolArguments(traceSpec, toolCall.Arguments),
				ToolCallID: toolCall.ID,
			})

			start := time.Now()
			emitRunEvent(sink, RunEvent{Type: EventToolStarted, RunID: run.ID, SessionID: run.SessionID, Step: step, Tool: toolCall.Name})
			var result string
			var err error
			var execution toolExecution
			var prepared preparedToolCall
			checkpointReused := false
			if validationErr := validateToolCall(request, toolCall); validationErr != nil {
				err = sanitizeToolError(validationErr, traceSpec, json.RawMessage(toolCall.Arguments))
			} else {
				identity := Identity{
					SessionID: run.SessionID,
					RunID:     run.ID,
				}
				var policyExecution toolExecution
				prepared, policyExecution, err = prepareToolCall(loopCtx, a.registry, a.toolPolicy, identity, toolCall)
				execution = policyExecution
				if err == nil && execution.Status == "" {
					if saved, ok := completedTools[prepared.checkpointKey]; ok {
						result = saved.Content
						execution = toolExecution{Content: result, Status: ToolReused}
						checkpointReused = true
						a.trace.Add(traceEvent{Step: step, Event: "tool_reused", Tool: toolCall.Name, ToolCallID: toolCall.ID, Reason: "checkpoint"})
					} else {
						execution, err = executePreparedToolCall(loopCtx, prepared, a.idempotency)
					}
				}
				result = execution.Content
				if err == nil && execution.Status == ToolReused && !checkpointReused {
					a.trace.Add(traceEvent{Step: step, Event: "tool_reused", Tool: toolCall.Name, ToolCallID: toolCall.ID, Reason: "idempotency"})
				}
				if err == nil && (execution.Status == ToolPendingApproval || execution.Status == ToolDenied) {
					a.trace.Add(traceEvent{Step: step, Event: "tool_policy", Tool: toolCall.Name, ToolCallID: toolCall.ID, Reason: string(execution.Status)})
				}
			}
			if err != nil {
				agentErr := classifyToolError(err)
				safeAgentErr := sanitizeToolError(agentErr, traceSpec, json.RawMessage(toolCall.Arguments))
				safeAgentErrTyped := classifyToolError(safeAgentErr)
				a.trace.AddDuration(traceEvent{Step: step, Event: "tool_error", Tool: toolCall.Name, ToolCallID: toolCall.ID, Error: safeAgentErrTyped.Error()}, start)
				if loopCtx.Err() != nil {
					loopCancel()
					_ = a.persistCheckpoint(&run, conversationMessages, completedTools, "", RunInterrupted, safeAgentErrTyped)
					a.trace.Add(traceEvent{Step: step, Event: "agent_stopped", Tool: toolCall.Name, ToolCallID: toolCall.ID, Reason: string(safeAgentErrTyped.Kind), Error: safeAgentErrTyped.Error()})
					return "", safeAgentErrTyped
				}
				toolRecoveryCount++
				if exceededRecoveryLimit(toolRecoveryCount, a.cfg.Tool.MaxToolRecoveries) {
					stopErr := &AgentError{Kind: BusinessError, Message: fmt.Sprintf("tool recovery limit exceeded: %d", a.cfg.Tool.MaxToolRecoveries), Err: safeAgentErrTyped}
					loopCancel()
					_ = a.persistCheckpoint(&run, conversationMessages, completedTools, "", RunFailed, stopErr)
					a.trace.Add(traceEvent{Step: step, Event: "agent_stopped", Tool: toolCall.Name, ToolCallID: toolCall.ID, Reason: "max_tool_recoveries", Attempt: toolRecoveryCount, Error: stopErr.Error()})
					return "", stopErr
				}
				if shouldStopBusinessRetry(businessErrorCalls, toolCall.Name+"\x00"+toolCall.Arguments, safeAgentErrTyped.Kind) {
					loopCancel()
					_ = a.persistCheckpoint(&run, conversationMessages, completedTools, "", RunFailed, safeAgentErrTyped)
					a.trace.Add(traceEvent{Step: step, Event: "agent_stopped", Tool: toolCall.Name, ToolCallID: toolCall.ID, Reason: "business_error_repeated", Error: safeAgentErrTyped.Error()})
					return "", safeAgentErrTyped
				}
				a.trace.Add(traceEvent{Step: step, Event: "retry", Tool: toolCall.Name, ToolCallID: toolCall.ID, Attempt: toolRecoveryCount, Reason: string(safeAgentErrTyped.Kind)})
				result = "工具执行失败：" + safeAgentErrTyped.Message
			} else if execution.Status == ToolPendingApproval || execution.Status == ToolDenied {
				a.trace.AddDuration(traceEvent{Step: step, Event: "tool_result", Tool: toolCall.Name, ToolCallID: toolCall.ID, Result: sanitizeTraceText(traceSpec, toolCall.Arguments, result), Reason: string(execution.Status)}, start)
			} else {
				a.trace.AddDuration(traceEvent{Step: step, Event: "tool_result", Tool: toolCall.Name, ToolCallID: toolCall.ID, Result: sanitizeTraceText(traceSpec, toolCall.Arguments, result)}, start)
			}
			a.metrics.RecordTool(run.ID, toolCall.Name, toolCall.ID, time.Since(start), err == nil)
			toolEvent := RunEvent{Type: EventToolFinished, RunID: run.ID, SessionID: run.SessionID, Step: step, Tool: toolCall.Name, Result: result}
			if err != nil {
				toolEvent.Error = err.Error()
			}
			emitRunEvent(sink, toolEvent)

			conversationMessages = append(conversationMessages, Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: toolCall.ID,
			})
			if err == nil && (execution.Status == ToolExecuted || execution.Status == ToolReused) {
				completedTools[prepared.checkpointKey] = ToolResult{Content: result}
			}
			if checkpointErr := a.persistCheckpoint(&run, conversationMessages, completedTools, "", RunRunning, nil); checkpointErr != nil {
				loopCancel()
				return "", checkpointErr
			}
			if agentErr := cancelledAgentError(ctx, loopCtx); agentErr != nil {
				loopCancel()
				_ = a.persistCheckpoint(&run, conversationMessages, completedTools, "", RunInterrupted, agentErr)
				a.trace.Add(traceEvent{Step: step, Event: "agent_stopped", Tool: toolCall.Name, Reason: string(agentErr.Kind), Error: agentErr.Error()})
				return "", agentErr
			}
		}
		loopCancel()
	}

	stopErr := &AgentError{Kind: ModelError, Message: fmt.Sprintf("maximum agent steps reached: %d", a.cfg.MaxSteps)}
	_ = a.persistCheckpoint(&run, conversationMessages, completedTools, "", RunFailed, stopErr)
	a.trace.Add(traceEvent{Step: a.cfg.MaxSteps, Event: "agent_stopped", Reason: "max_steps", Error: stopErr.Error()})
	return "", stopErr
}

func emitRunEvent(sink func(RunEvent), event RunEvent) {
	if sink != nil {
		sink(event)
	}
}

func cancelledAgentError(ctx context.Context, loopCtx context.Context) *AgentError {
	if err := ctx.Err(); err != nil {
		return &AgentError{Kind: TimeoutError, Message: "agent context canceled", Err: err}
	}
	if err := loopCtx.Err(); err != nil {
		return &AgentError{Kind: TimeoutError, Message: "agent loop context canceled", Err: err}
	}
	return nil
}

func (a *Agent) persistCheckpoint(run *Run, requestMessages []Message, completedTools map[string]ToolResult, result string, status RunStatus, runErr error) error {
	run.Status = status
	run.Result = result
	run.Error = ""
	if runErr != nil {
		run.Error = runErr.Error()
	}
	run.UpdatedAt = time.Now().UTC()
	conversation := cloneConversationMessages(requestMessages)
	conversation = sanitizePersistentConversation(conversation, a.registry, Identity{SessionID: run.SessionID, RunID: run.ID})
	a.session.Messages = cloneMessages(conversation)
	a.session.UpdatedAt = run.UpdatedAt
	if a.checkpointStore == nil {
		return nil
	}
	session := cloneSession(a.session)
	checkpoint := NewCheckpoint(session, *run, conversation, sanitizePersistentToolResults(completedTools, requestMessages, a.registry, Identity{SessionID: run.SessionID, RunID: run.ID}))
	checkpoint.Result = result
	return a.checkpointStore.Save(context.Background(), checkpoint)
}

func sanitizePersistentConversation(messages []Message, registry ToolRegistry, identity Identity) []Message {
	clone := cloneMessages(messages)
	specs := make(map[string]ToolSpec)
	args := make(map[string]string)
	for i := range clone {
		if clone[i].Role != "assistant" {
			continue
		}
		for j := range clone[i].ToolCalls {
			call := &clone[i].ToolCalls[j]
			tool, ok := registry.Find(call.Name)
			if !ok {
				continue
			}
			spec := toolSpecFor(tool)
			specs[call.ID] = spec
			args[call.ID] = call.Arguments
			call.Arguments = sanitizePersistentToolArguments(spec, call.Arguments)
		}
	}
	for i := range clone {
		if clone[i].Role != "tool" {
			continue
		}
		if spec, ok := specs[clone[i].ToolCallID]; ok {
			clone[i].Content = sanitizePersistentToolText(spec, args[clone[i].ToolCallID], clone[i].Content)
		}
	}
	return clone
}

func sanitizePersistentToolArguments(spec ToolSpec, raw string) string {
	if spec.Sensitive {
		return "[sensitive tool arguments omitted]"
	}
	if len(spec.SensitiveParameters) == 0 {
		return raw
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		return summarizeToolArguments(spec, raw)
	}
	for _, name := range spec.SensitiveParameters {
		if _, ok := object[name]; ok {
			object[name] = "[REDACTED]"
		}
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return summarizeToolArguments(spec, raw)
	}
	return string(encoded)
}

func sanitizePersistentToolText(spec ToolSpec, rawArgs, text string) string {
	if spec.Sensitive {
		return "[sensitive tool output omitted]"
	}
	return sanitizeTraceText(spec, rawArgs, text)
}

func sanitizePersistentToolResults(results map[string]ToolResult, messages []Message, registry ToolRegistry, identity Identity) map[string]ToolResult {
	if len(results) == 0 {
		return nil
	}
	clone, err := cloneToolResults(results)
	if err != nil {
		return results
	}
	for _, message := range messages {
		if message.Role != "assistant" {
			continue
		}
		for _, call := range message.ToolCalls {
			tool, ok := registry.Find(call.Name)
			if !ok {
				continue
			}
			spec := toolSpecFor(tool)
			key := checkpointToolKey(identity, spec, call)
			result, ok := clone[key]
			if !ok {
				continue
			}
			result.Content = sanitizePersistentToolText(spec, call.Arguments, result.Content)
			if spec.Sensitive {
				result.Data = nil
			} else if len(spec.SensitiveParameters) > 0 {
				result.Data = redactToolData(result.Data, spec.SensitiveParameters)
			}
			clone[key] = result
		}
	}
	return clone
}

func redactToolData(data any, names []string) any {
	if data == nil {
		return nil
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return "[REDACTED]"
	}
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return "[REDACTED]"
	}
	redactToolDataValue(value, names)
	return value
}

func redactToolDataValue(value any, names []string) {
	sensitive := make(map[string]bool, len(names))
	for _, name := range names {
		sensitive[name] = true
	}
	var visit func(any)
	visit = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, nested := range typed {
				if sensitive[key] {
					typed[key] = "[REDACTED]"
					continue
				}
				visit(nested)
			}
		case []any:
			for _, nested := range typed {
				visit(nested)
			}
		}
	}
	visit(value)
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
