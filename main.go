package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

const (
	ollamaAPIKey  = "ollama"
	ollamaBaseURL = "http://localhost:11434/v1/"
	ollamaModel   = "qwen3:1.7b"
	maxSteps      = 5
	toolName      = "calculator"
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
	Tool      string `json:"tool,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Result    string `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
}

type ToolHandler func(rawArgs string) (string, error)

var toolRegistry = map[string]ToolHandler{
	"calculator":         executeCalculator,
	"get_project_status": getProjectStatus,
	"break_down_tasks":   nil,
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := newClient()
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

	for step := 1; step <= maxSteps; step++ {
		resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Messages: messages,
			Model:    openai.ChatModel(ollamaModel),
			Tools:    tools,
		})
		if err != nil {
			panic(fmt.Errorf("agent loop step %d: %w", step, err))
		}

		writeTrace(traceEvent{Step: step, Event: "model_response"})
		if len(resp.Choices) == 0 {
			panic(fmt.Errorf("agent loop step %d: model returned no choices", step))
		}

		message := resp.Choices[0].Message
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

			result, err := dispatchTool(functionCall.Function.Name, functionCall.Function.Arguments)
			if err != nil {
				writeTrace(traceEvent{Step: step, Event: "tool_error", Tool: functionCall.Function.Name, Error: err.Error()})
				result = "工具执行失败：" + err.Error()
			} else {
				writeTrace(traceEvent{Step: step, Event: "tool_result", Tool: functionCall.Function.Name, Result: result})
			}

			// 工具结果必须使用同一个 call ID，模型才能把结果对应回这次调用。
			messages = append(messages, openai.ToolMessage(result, functionCall.ID))
		}
	}

	panic(fmt.Sprintf("agent stopped: reached max steps (%d)", maxSteps))
}

func newClient() openai.Client {
	return openai.NewClient(
		option.WithAPIKey(ollamaAPIKey),
		option.WithBaseURL(ollamaBaseURL),
	)
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

func dispatchTool(name, rawArgs string) (string, error) {
	handler, ok := toolRegistry[name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	if handler == nil {
		return "", fmt.Errorf("tool is not implemented: %s", name)
	}

	return handler(rawArgs)
}

func getProjectStatus(rawArgs string) (string, error) {
	var args getProjectStatusArgs

	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", err
	}

	switch args.Operation {
	case "query":
		project, _, err := findProject(args.ProjectId, args.Title)
		if err != nil {
			return "", err
		}
		return formatProjectResult(args, project)
	case "update":
		project, _, err := findProject(args.ProjectId, args.Title)
		if err != nil {
			return "", err
		}
		if len(args.Data) == 0 {
			return "", fmt.Errorf("data cannot be empty for update")
		}
		if err := updateProject(project, args.Data); err != nil {
			return "", err
		}
		return formatProjectResult(args, project)
	case "add":
		if args.Title == "" {
			return "", fmt.Errorf("title cannot be empty for add")
		}
		if _, ok := projectDataBase[args.Title]; ok {
			return "", fmt.Errorf("project already exists: %s", args.Title)
		}
		if args.ProjectId == 0 {
			args.ProjectId = nextProjectID()
		}
		project := map[string]any{
			"projectId": args.ProjectId,
			"title":     args.Title,
		}
		if err := updateProject(project, args.Data); err != nil {
			return "", err
		}
		projectDataBase[args.Title] = project
		return formatProjectResult(args, project)
	case "delete":
		_, key, err := findProject(args.ProjectId, args.Title)
		if err != nil {
			return "", err
		}
		delete(projectDataBase, key)
		return fmt.Sprintf("title=%s, projectId=%d, operation=delete, result=deleted", args.Title, args.ProjectId), nil
	default:
		return "", fmt.Errorf("unknown operation: %s", args.Operation)
	}

}

func findProject(projectID int, title string) (map[string]any, string, error) {
	if projectID == 0 && title == "" {
		return nil, "", fmt.Errorf("projectId and title cannot both be empty")
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
			if project["projectId"] == projectID {
				return project, key, nil
			}
		}
	}

	if title != "" {
		return nil, "", fmt.Errorf("no record for %s", title)
	}
	return nil, "", fmt.Errorf("no record for projectId %d", projectID)
}

func updateProject(project map[string]any, data map[string]string) error {
	for field, value := range data {
		switch field {
		case "status", "assignor", "assignee", "description":
			project[field] = value
		default:
			return fmt.Errorf("unsupported project field: %s", field)
		}
	}
	return nil
}

func nextProjectID() int {
	nextID := 1
	for _, project := range projectDataBase {
		if projectID, ok := project["projectId"].(int); ok && projectID >= nextID {
			nextID = projectID + 1
		}
	}
	return nextID
}

func formatProjectResult(args getProjectStatusArgs, project map[string]any) (string, error) {
	body, err := json.Marshal(project)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("title=%v, projectId=%v, operation=%s, result=%s", project["title"], project["projectId"], args.Operation, string(body)), nil
}

func executeCalculator(rawArgs string) (string, error) {
	var args calculatorArgs

	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", err
	}

	var result float64

	switch args.Operation {
	case "add":
		result = args.A + args.B
	case "subtract":
		result = args.A - args.B
	case "multiply":
		result = args.A * args.B
	case "divide":
		if args.B == 0 {
			return "", fmt.Errorf("division by zero")
		}
		result = args.A / args.B
	default:
		return "", fmt.Errorf("unsupported operation: %s", args.Operation)

	}

	return fmt.Sprintf("a=%g, b=%g, operation=%s, result=%g", args.A, args.B, args.Operation, result), nil
}

func writeTrace(event traceEvent) {
	data, err := json.Marshal(event)
	if err == nil {
		fmt.Println(string(data))
	}
}
