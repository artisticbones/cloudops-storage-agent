package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

const (
	apiKeyEnv = "OPENAI_API_KEY"
	proxyAddr = "http://127.0.0.1:7897"
	maxSteps  = 5
	toolName  = "calculator"
)

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
	"get_project_status": nil,
	"break_down_tasks":   nil,
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	apiKey, ok := os.LookupEnv(apiKeyEnv)
	if !ok || apiKey == "" {
		panic("OPENAI_API_KEY is not set")
	}

	client := newClient(apiKey)
	tools := []responses.ToolUnionParam{calculatorTool()}
	input := responses.ResponseNewParamsInputUnion{
		OfString: openai.String("请计算 123 * 45，并说明计算过程和结果。"),
	}
	responseID := ""

	for step := 1; step <= maxSteps; step++ {
		params := responses.ResponseNewParams{
			Input: input,
			Model: openai.ChatModelGPT5_6Luna,
			Tools: tools,
		}
		if responseID != "" {
			params.PreviousResponseID = openai.String(responseID)
		}

		resp, err := client.Responses.New(ctx, params)
		if err != nil {
			panic(fmt.Errorf("agent loop step %d: %w", step, err))
		}

		responseID = resp.ID
		writeTrace(traceEvent{Step: step, Event: "model_response"})

		toolCall, found := findFunctionCall(resp.Output)
		if !found {
			answer := resp.OutputText()
			fmt.Println(answer)
			writeTrace(traceEvent{Step: step, Event: "final_answer", Result: answer})
			return
		}

		writeTrace(traceEvent{
			Step:      step,
			Event:     "tool_call",
			Tool:      toolCall.Name,
			Arguments: toolCall.Arguments,
		})
		result, err := dispatchTool(toolCall.Name, toolCall.Arguments)
		if err != nil {
			writeTrace(traceEvent{Step: step, Event: "tool_error", Tool: toolCall.Name, Error: err.Error()})
			result = "工具执行失败：" + err.Error()
		} else {
			writeTrace(traceEvent{Step: step, Event: "tool_result", Tool: toolCall.Name, Result: result})
		}

		input = responses.ResponseNewParamsInputUnion{
			OfInputItemList: []responses.ResponseInputItemUnionParam{
				responses.ResponseInputItemParamOfFunctionCallOutput(toolCall.CallID, result),
			},
		}
	}

	panic(fmt.Sprintf("agent stopped: reached max steps (%d)", maxSteps))
}

func newClient(apiKey string) openai.Client {
	proxyURL, err := url.Parse(proxyAddr)
	if err != nil {
		panic(err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxyURL)
	httpClient := &http.Client{Transport: transport, Timeout: 60 * time.Second}
	return openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(httpClient),
	)
}

func calculatorTool() responses.ToolUnionParam {
	return responses.ToolUnionParam{
		OfFunction: &responses.FunctionToolParam{
			Name:        toolName,
			Description: openai.String("执行两个数字的加减乘除运算"),
			Strict:      openai.Bool(true),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"a":         map[string]any{"type": "number"},
					"b":         map[string]any{"type": "number"},
					"operation": map[string]any{"type": "string", "enum": []string{"add", "subtract", "multiply", "divide"}},
				},
				"required":             []string{"a", "b", "operation"},
				"additionalProperties": false,
			},
		},
	}
}

func findFunctionCall(items []responses.ResponseOutputItemUnion) (responses.ResponseFunctionToolCall, bool) {
	for _, item := range items {
		if item.Type == "function_call" {
			return item.AsFunctionCall(), true
		}
	}
	return responses.ResponseFunctionToolCall{}, false
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
