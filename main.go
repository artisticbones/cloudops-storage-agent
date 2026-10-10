package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/artisticbones/hello-agent/config"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
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

func main() {
	cfg, err := config.LoadConfig()
	if err != nil {
		panic(fmt.Errorf("load config: %w", err))
	}

	// The outer context represents the lifetime of this conversation/process.
	// Per-loop timeouts are applied inside Agent.Run so a multi-step Agent is not
	// forced to share one deadline across all model and tool calls.
	ctx := context.Background()

	if cfg.Diagnostics != nil {
		pprofServer, err := startPprofServer(ctx, cfg.Diagnostics.PprofAddr)
		if err != nil {
			panic(fmt.Errorf("start pprof server: %w", err))
		}
		if pprofServer != nil {
			fmt.Printf("pprof 已启动：http://%s/debug/pprof/\n", pprofServer.Addr())
		}
	}

	client := NewOpenAICompatibleModelClient(newClient(cfg), cfg.Model.Name)
	registry := NewToolRegistry()
	for _, tool := range []Tool{calculatorTool(), expressionCalculatorTool(), getProjectStatusTool()} {
		if err := registry.Register(tool); err != nil {
			panic(fmt.Errorf("register tool: %w", err))
		}
	}

	var agent *Agent
	if err := registry.Register(systemPromptTool(&agent)); err != nil {
		panic(fmt.Errorf("register system prompt tool: %w", err))
	}
	agent, err = NewAgent(client, cfg, registry)
	if err != nil {
		panic(fmt.Errorf("create agent: %w", err))
	}

	// One Agent instance owns one conversation. Keep reading turns until EOF
	// or the user explicitly exits, so history is available to the next Run.
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("请输入问题（输入 exit 退出）：")
	for {
		fmt.Print("> ")
		input, readErr := reader.ReadString('\n')
		if readErr != nil && len(input) == 0 {
			if errors.Is(readErr, io.EOF) {
				break
			}
			fmt.Printf("读取输入失败：%s\n", readErr)
			break
		}
		input = strings.TrimSpace(input)
		if input == "" {
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}
		if strings.EqualFold(input, "exit") || strings.EqualFold(input, "quit") {
			break
		}
		if strings.HasPrefix(input, "/system ") {
			instruction := strings.TrimSpace(strings.TrimPrefix(input, "/system "))
			update := agent.SetSystemPrompt
			if strings.HasPrefix(instruction, "add ") {
				instruction = strings.TrimSpace(strings.TrimPrefix(instruction, "add "))
				update = agent.AddSystemPrompt
			}
			if err := update(instruction); err != nil {
				fmt.Printf("系统指令更新失败：%s\n", err)
			} else {
				fmt.Println("系统指令已更新，下一轮对话立即生效。")
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}
		if input == "/system" {
			fmt.Printf("当前系统指令：\n%s\n", agent.SystemPrompt())
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}

		answer, runErr := agent.Run(ctx, input)
		if runErr != nil {
			fmt.Printf("Agent 已安全停止：%s\n", runErr)
		} else {
			fmt.Println(answer)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
}

func requestCompletion(ctx context.Context, client ModelClient, cfg *config.Config, req ModelRequest, step int, trace *Trace) (*ModelResponse, error) {
	var lastErr error
	for attempt := 1; attempt <= cfg.MaxRetries+1; attempt++ {
		resp, err := client.Complete(ctx, req)
		if err == nil {
			return resp, nil
		}

		lastErr = err
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || attempt > cfg.MaxRetries {
			break
		}
		classified := classifyRuntimeError(ctx, err)
		trace.Add(traceEvent{Step: step, Event: "retry", Attempt: attempt, Reason: string(classified.Kind), Error: classified.Error()})
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
	if errors.Is(err, context.Canceled) {
		return &AgentError{Kind: TimeoutError, Message: "tool execution canceled", Err: err}
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

func calculatorTool() Tool {
	return &functionTool{
		definition: ToolDefinition{
			Name:        toolName,
			Description: "执行用户明确要求的两个数字的单步运算。必须严格按照用户表达式选择 operation：加号或加法使用 add，减号或减法使用 subtract，乘号或乘法使用 multiply，除号或除法使用 divide；不得自行改变用户要求的运算类型。",
			Strict:      true,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"a": map[string]any{"type": "number"},
					"b": map[string]any{"type": "number"},
					"operation": map[string]any{
						"type":        "string",
						"enum":        []string{"add", "subtract", "multiply", "divide"},
						"description": "严格匹配用户表达式：+ 或加法=add；- 或减法=subtract；* 或乘法=multiply；/ 或除法=divide。",
					},
				},
				"required":             []string{"a", "b", "operation"},
				"additionalProperties": false,
			},
		},
		handler: executeCalculator,
	}
}

func getProjectStatusTool() Tool {
	return &functionTool{
		definition: ToolDefinition{
			Name:        "get_project_status",
			Description: "查询、新增、更新或删除项目记录",
			Strict:      true,
			Parameters: map[string]any{
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
		},
		handler: getProjectStatus,
	}
}

func dispatchTool(ctx context.Context, registry ToolRegistry, name, rawArgs string) (string, error) {
	tool, ok := registry.Find(name)
	if !ok {
		return "", &AgentError{Kind: ModelError, Message: "unknown tool: " + name}
	}

	result, err := tool.Execute(ctx, json.RawMessage(rawArgs))
	if err != nil {
		return "", err
	}
	return result.Content, nil
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
