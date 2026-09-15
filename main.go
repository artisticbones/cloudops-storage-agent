package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/artisticbones/hello-agent/config"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

const (
	toolName = "calculator"
)

var projectDataBase = map[string]map[string]any{
	"test": {
		"projectId":   1,
		"title":       "test",
		"status":      "processing",
		"assignor":    "A",
		"assignee":    "B",
		"description": "for test",
	},
	"bones": {
		"projectId":   2,
		"title":       "bones",
		"status":      "processing",
		"assignor":    "A",
		"assignee":    "C",
		"description": "for test bones",
	},
}

type getProjectStatusArgs struct {
	ProjectId int    `json:"projectId"`
	Title     string `json:"title"`
	Operation string `json:"operation"`
	// status, assignor, assignee, description
	Data map[string]string `json:"data"`
}

type calculatorArgs struct {
	A         float64 `json:"a"`
	B         float64 `json:"b"`
	Operation string  `json:"operation"`
}

type traceEvent struct {
	Step      int    `json:"step"`
	Event     string `json:"event"`
	Attempt   int    `json:"attempt,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Result    string `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type ToolHandler func(ctx context.Context, rawArgs string) (string, error)

var toolRegistry = map[string]ToolHandler{
	"calculator":         executeCalculator,
	"get_project_status": getProjectStatus,
	"break_down_tasks":   nil,
}

func main() {
	cfg, err := config.LoadConfig()
	if err != nil {
		panic(fmt.Errorf("load config: %w", err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
	defer cancel()

	client := newClient(cfg)
	tools := []openai.ChatCompletionToolUnionParam{calculatorTool(), getProjectStatusTool()}

	fmt.Print("请输入问题：")
	input, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && len(input) == 0 {
		panic(fmt.Errorf("read user input: %w", err))
	}
	input = strings.TrimSpace(input)
	if input == "" {
		panic("user input cannot be empty")
	}

	messages := []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage(input),
	}
	businessErrorCalls := make(map[string]int)
	toolRecoveryCount := 0

	for step := 1; step <= cfg.MaxSteps; step++ {
		resp, err := requestCompletion(ctx, client, cfg, messages, tools, step)
		if err != nil {
			agentErr := classifyRuntimeError(ctx, err)
			writeTrace(traceEvent{Step: step, Event: "agent_error", Error: agentErr.Error()})
			writeTrace(traceEvent{Step: step, Event: "agent_stopped", Reason: string(agentErr.Kind), Error: agentErr.Error()})
			fmt.Printf("Agent 已安全停止：%s\n", agentErr)
			return
		}

		writeTrace(traceEvent{Step: step, Event: "model_response"})
		choice, err := getFirstChoice(resp)
		if err != nil {
			var agentErr *AgentError
			if !errors.As(err, &agentErr) {
				agentErr = classifyRuntimeError(ctx, err)
			}
			writeTrace(traceEvent{Step: step, Event: "agent_stopped", Reason: string(agentErr.Kind), Error: agentErr.Error()})
			fmt.Printf("Agent 已安全停止：%s\n", agentErr)
			return
		}

		message := choice.Message
		if len(message.ToolCalls) == 0 {
			answer := message.Content
			fmt.Println(answer)
			writeTrace(traceEvent{Step: step, Event: "final_answer", Result: answer})
			return
		}

		// 下一轮请求必须带上模型刚刚发出的 assistant tool call。
		messages = append(messages, message.ToParam())
		for _, toolCall := range message.ToolCalls {
			functionCall := toolCall.AsFunction()
			writeTrace(traceEvent{
				Step:      step,
				Event:     "tool_call",
				Tool:      functionCall.Function.Name,
				Arguments: functionCall.Function.Arguments,
			})

			result, err := dispatchTool(ctx, functionCall.Function.Name, functionCall.Function.Arguments)
			if err != nil {
				agentErr := classifyToolError(err)
				writeTrace(traceEvent{Step: step, Event: "tool_error", Tool: functionCall.Function.Name, Error: agentErr.Error()})
				toolRecoveryCount++
				if exceededRecoveryLimit(toolRecoveryCount, cfg.Tool.MaxToolRecoveries) {
					writeTrace(traceEvent{Step: step, Event: "agent_stopped", Tool: functionCall.Function.Name, Reason: "max_tool_recoveries", Attempt: toolRecoveryCount, Error: fmt.Sprintf("limit:%d", cfg.Tool.MaxToolRecoveries)})
					fmt.Printf("Agent 已安全停止：工具恢复次数达到上限 %d\n", cfg.Tool.MaxToolRecoveries)
					return
				}
				if shouldStopBusinessRetry(businessErrorCalls, functionCall.Function.Name+"\x00"+functionCall.Function.Arguments, agentErr.Kind) {
					writeTrace(traceEvent{Step: step, Event: "agent_stopped", Tool: functionCall.Function.Name, Reason: "business_error_repeated", Error: agentErr.Error()})
					fmt.Println("工具执行失败：相同业务错误已重复，停止重试")
					return
				}
				writeTrace(traceEvent{Step: step, Event: "retry", Tool: functionCall.Function.Name, Attempt: toolRecoveryCount, Reason: string(agentErr.Kind)})
				result = "工具执行失败：" + err.Error()
			} else {
				writeTrace(traceEvent{Step: step, Event: "tool_result", Tool: functionCall.Function.Name, Result: result})
			}

			// 工具结果必须使用同一个 call ID，模型才能把结果对应回这次调用。
			messages = append(messages, openai.ToolMessage(result, functionCall.ID))
		}
	}

	writeTrace(traceEvent{Step: cfg.MaxSteps, Event: "agent_stopped", Reason: "max_steps", Error: fmt.Sprintf("limit:%d", cfg.MaxSteps)})
	fmt.Printf("Agent 已安全停止：达到最大步数 %d\n", cfg.MaxSteps)
}

func requestCompletion(ctx context.Context, client openai.Client, cfg *config.Config, messages []openai.ChatCompletionMessageParamUnion, tools []openai.ChatCompletionToolUnionParam, step int) (*openai.ChatCompletion, error) {
	var lastErr error
	for attempt := 1; attempt <= cfg.MaxRetries+1; attempt++ {
		resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Messages: messages,
			Model:    openai.ChatModel(cfg.Model.Name),
			Tools:    tools,
		})
		if err == nil {
			return resp, nil
		}

		lastErr = err
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || attempt > cfg.MaxRetries {
			break
		}
		classified := classifyRuntimeError(ctx, err)
		writeTrace(traceEvent{Step: step, Event: "retry", Attempt: attempt, Reason: string(classified.Kind), Error: classified.Error()})
	}
	return nil, classifyRuntimeError(ctx, lastErr)
}

func classifyRuntimeError(ctx context.Context, err error) *AgentError {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &AgentError{Kind: TimeoutError, Message: "agent context deadline exceeded", Err: ctx.Err()}
	}
	return &AgentError{Kind: NetworkError, Message: err.Error(), Err: err}
}

func classifyToolError(err error) *AgentError {
	var agentErr *AgentError
	if errors.As(err, &agentErr) {
		return agentErr
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &AgentError{Kind: TimeoutError, Message: "tool execution timed out", Err: err}
	}
	return &AgentError{Kind: BusinessError, Message: err.Error(), Err: err}
}

func getFirstChoice(resp *openai.ChatCompletion) (openai.ChatCompletionChoice, error) {
	if resp == nil {
		return openai.ChatCompletionChoice{}, &AgentError{
			Kind:    ModelError,
			Message: "model response is nil",
		}
	}
	if resp.Choices == nil {
		return openai.ChatCompletionChoice{}, &AgentError{
			Kind:    ModelError,
			Message: "choices in model response is nil",
		}
	}
	if len(resp.Choices) == 0 {
		return openai.ChatCompletionChoice{}, &AgentError{
			Kind:    ModelError,
			Message: "model returned no choices",
		}
	}
	return resp.Choices[0], nil
}

func shouldStopBusinessRetry(failures map[string]int, key string, kind ErrorKind) bool {
	if kind != BusinessError {
		return false
	}
	failures[key]++
	return failures[key] > 1
}

func exceededRecoveryLimit(recoveryCount, maxRecoveries int) bool {
	return recoveryCount > maxRecoveries
}

func newClient(cfg *config.Config) openai.Client {
	return openai.NewClient(
		option.WithAPIKey(cfg.Model.APIKey),
		option.WithBaseURL(cfg.Model.BaseURL),
	)
}

func handleToolError(ctx context.Context, err error) (action string) {
	var agentErr *AgentError

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "stop: timeout"
	case errors.As(err, &agentErr):
		switch agentErr.Kind {
		case InvalidArgument:
			return "return error to LLM and allow it to fix arguments"
		case BusinessError:
			return "return business error to LLM, do not retry"
		case ModelError, NetworkError:
			return "retry with a limited retry count"
		case TimeoutError:
			return "stop: timeout"
		}
	default:
		return "stop: unknown error"
	}

	return "stop: unhandled error"
}

func calculatorTool() openai.ChatCompletionToolUnionParam {
	return openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
		Name:        toolName,
		Description: openai.String("执行两个数字的加减乘除运算"),
		Strict:      openai.Bool(true),
		Parameters: shared.FunctionParameters{
			"type": "object",
			"properties": map[string]any{
				"a":         map[string]any{"type": "number"},
				"b":         map[string]any{"type": "number"},
				"operation": map[string]any{"type": "string", "enum": []string{"add", "subtract", "multiply", "divide"}},
			},
			"required":             []string{"a", "b", "operation"},
			"additionalProperties": false,
		},
	})
}

func getProjectStatusTool() openai.ChatCompletionToolUnionParam {
	return openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
		Name:        "get_project_status",
		Description: openai.String("查询、新增、更新或删除项目记录"),
		Strict:      openai.Bool(true),
		Parameters: shared.FunctionParameters{
			"type": "object",
			"properties": map[string]any{
				"projectId": map[string]any{"type": "integer", "description": "项目 ID；不使用 ID 时传 0"},
				"title":     map[string]any{"type": "string", "description": "项目标题；不使用标题时传空字符串"},
				"operation": map[string]any{"type": "string", "enum": []string{"add", "update", "delete", "query"}, "description": "要执行的项目操作"},
				"data": map[string]any{
					"type":                 "object",
					"description":          "新增或更新的项目字段；query 和 delete 时传空对象",
					"additionalProperties": map[string]any{"type": "string"},
				},
			},
			"required":             []string{"projectId", "title", "operation", "data"},
			"additionalProperties": false,
		},
	})
}

func dispatchTool(ctx context.Context, name, rawArgs string) (string, error) {
	handler, ok := toolRegistry[name]
	if !ok {
		return "", &AgentError{Kind: ModelError, Message: "unknown tool: " + name}
	}
	if handler == nil {
		return "", &AgentError{Kind: ModelError, Message: "tool is not implemented: " + name}
	}

	return handler(ctx, rawArgs)
}

func getProjectStatus(ctx context.Context, rawArgs string) (string, error) {
	if err := contextErr(ctx); err != nil {
		return "", err
	}
	var args getProjectStatusArgs

	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", &AgentError{Kind: InvalidArgument, Message: "invalid project arguments", Err: err}
	}

	switch args.Operation {
	case "query":
		project, _, err := findProject(ctx, args.ProjectId, args.Title)
		if err != nil {
			return "", err
		}
		return formatProjectResult(args, project)
	case "update":
		project, _, err := findProject(ctx, args.ProjectId, args.Title)
		if err != nil {
			return "", err
		}
		if len(args.Data) == 0 {
			return "", &AgentError{Kind: InvalidArgument, Message: "data cannot be empty for update"}
		}
		if err := updateProject(ctx, project, args.Data); err != nil {
			return "", err
		}
		return formatProjectResult(args, project)
	case "add":
		if args.Title == "" {
			return "", &AgentError{Kind: InvalidArgument, Message: "title cannot be empty for add"}
		}
		if _, ok := projectDataBase[args.Title]; ok {
			return "", &AgentError{Kind: BusinessError, Message: "project already exists: " + args.Title}
		}
		if args.ProjectId == 0 {
			projectID, err := nextProjectID(ctx)
			if err != nil {
				return "", err
			}
			args.ProjectId = projectID
		}
		project := map[string]any{
			"projectId": args.ProjectId,
			"title":     args.Title,
		}
		if err := updateProject(ctx, project, args.Data); err != nil {
			return "", err
		}
		projectDataBase[args.Title] = project
		return formatProjectResult(args, project)
	case "delete":
		_, key, err := findProject(ctx, args.ProjectId, args.Title)
		if err != nil {
			return "", err
		}
		delete(projectDataBase, key)
		return fmt.Sprintf("title=%s, projectId=%d, operation=delete, result=deleted", args.Title, args.ProjectId), nil
	default:
		return "", &AgentError{Kind: InvalidArgument, Message: "unknown operation: " + args.Operation}
	}

}

func findProject(ctx context.Context, projectID int, title string) (map[string]any, string, error) {
	if projectID == 0 && title == "" {
		return nil, "", &AgentError{Kind: InvalidArgument, Message: "projectId and title cannot both be empty"}
	}

	if title != "" {
		if project, ok := projectDataBase[title]; ok {
			if projectID == 0 || project["projectId"] == projectID {
				return project, title, nil
			}
		}
	}

	if projectID != 0 {
		for key, project := range projectDataBase {
			if err := contextErr(ctx); err != nil {
				return nil, "", err
			}
			if project["projectId"] == projectID {
				return project, key, nil
			}
		}
	}

	if title != "" {
		return nil, "", &AgentError{Kind: BusinessError, Message: "no record for " + title}
	}
	return nil, "", &AgentError{Kind: BusinessError, Message: fmt.Sprintf("no record for projectId %d", projectID)}
}

func updateProject(ctx context.Context, project map[string]any, data map[string]string) error {
	for field, value := range data {
		if err := contextErr(ctx); err != nil {
			return err
		}
		switch field {
		case "status", "assignor", "assignee", "description":
			project[field] = value
		default:
			return &AgentError{Kind: InvalidArgument, Message: "unsupported project field: " + field}
		}
	}
	return nil
}

func nextProjectID(ctx context.Context) (int, error) {
	nextID := 1
	for _, project := range projectDataBase {
		if err := contextErr(ctx); err != nil {
			return 0, err
		}
		if projectID, ok := project["projectId"].(int); ok && projectID >= nextID {
			nextID = projectID + 1
		}
	}
	return nextID, nil
}

func formatProjectResult(args getProjectStatusArgs, project map[string]any) (string, error) {
	body, err := json.Marshal(project)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("title=%v, projectId=%v, operation=%s, result=%s", project["title"], project["projectId"], args.Operation, string(body)), nil
}

func executeCalculator(ctx context.Context, rawArgs string) (string, error) {
	if err := contextErr(ctx); err != nil {
		return "", err
	}
	var args calculatorArgs

	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", &AgentError{Kind: InvalidArgument, Message: "invalid calculator arguments", Err: err}
	}

	var result float64
	if err := contextErr(ctx); err != nil {
		return "", err
	}

	switch args.Operation {
	case "add":
		result = args.A + args.B
	case "subtract":
		result = args.A - args.B
	case "multiply":
		result = args.A * args.B
	case "divide":
		if args.B == 0 {
			return "", &AgentError{Kind: BusinessError, Message: "division by zero"}
		}
		result = args.A / args.B
	default:
		return "", &AgentError{Kind: InvalidArgument, Message: "unsupported operation: " + args.Operation}

	}

	return fmt.Sprintf("a=%g, b=%g, operation=%s, result=%g", args.A, args.B, args.Operation, result), nil
}

func writeTrace(event traceEvent) {
	data, err := json.Marshal(event)
	if err == nil {
		fmt.Println(string(data))
	}
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
