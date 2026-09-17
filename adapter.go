package main

import (
	"context"
	"fmt"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"
)

type ModelClient interface {
	Complete(ctx context.Context, request ModelRequest) (*ModelResponse, error)
}

type OpenAICompatibleModelClient struct {
	client openai.Client
	model  string
}

func NewOpenAICompatibleModelClient(client openai.Client, model string) *OpenAICompatibleModelClient {
	return &OpenAICompatibleModelClient{
		client: client,
		model:  model,
	}
}

func (c *OpenAICompatibleModelClient) Complete(ctx context.Context, req ModelRequest) (*ModelResponse, error) {
	sdkMessage, err := convertMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	sdkTools := convertTools(req.Tools)

	chat, err := c.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Messages: sdkMessage,
		Model:    openai.ChatModel(c.model),
		Tools:    sdkTools,
	})
	if err != nil {
		return nil, err
	}

	return convertResponse(chat)
}

func convertMessages(messages []Message) ([]openai.ChatCompletionMessageParamUnion, error) {
	result := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case "system":
			result = append(result, openai.SystemMessage(message.Content))
		case "developer":
			result = append(result, openai.DeveloperMessage(message.Content))
		case "user":
			result = append(result, openai.UserMessage(message.Content))
		case "assistant":
			assistant := openai.ChatCompletionAssistantMessageParam{}
			if message.Content != "" {
				assistant.Content.OfString = param.NewOpt(message.Content)
			}
			assistant.ToolCalls = make([]openai.ChatCompletionMessageToolCallUnionParam, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: call.ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      call.Name,
							Arguments: call.Arguments,
						},
					},
				})
			}
			result = append(result, openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant})
		case "tool":
			if message.ToolCallID == "" {
				return nil, fmt.Errorf("tool message requires tool call id")
			}
			result = append(result, openai.ToolMessage(message.Content, message.ToolCallID))
		default:
			return nil, fmt.Errorf("unsupported message role: %s", message.Role)
		}
	}
	return result, nil
}

func convertTools(tools []ToolDefinition) []openai.ChatCompletionToolUnionParam {
	result := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		definition := shared.FunctionDefinitionParam{
			Name:        tool.Name,
			Description: param.NewOpt(tool.Description),
			Strict:      param.NewOpt(tool.Strict),
			Parameters:  shared.FunctionParameters(tool.Parameters),
		}
		result = append(result, openai.ChatCompletionFunctionTool(definition))
	}
	return result
}

func convertResponse(chat *openai.ChatCompletion) (*ModelResponse, error) {
	choice, err := getFirstChoice(chat)
	if err != nil {
		return nil, err
	}

	response := &ModelResponse{Content: choice.Message.Content}
	for _, toolCall := range choice.Message.ToolCalls {
		functionCall := toolCall.AsFunction()
		if toolCall.Type != "function" {
			return nil, &AgentError{Kind: ModelError, Message: "model returned unsupported tool call"}
		}
		response.ToolCalls = append(response.ToolCalls, ToolCall{
			ID:        functionCall.ID,
			Name:      functionCall.Function.Name,
			Arguments: functionCall.Function.Arguments,
		})
	}
	return response, nil
}

type Message struct {
	ToolCallID string     `json:"toolCallId,omitempty"`
	Content    string     `json:"content"`
	Role       string     `json:"role"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
}

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict"`
}

type ModelRequest struct {
	Messages []Message        `json:"messages"`
	Tools    []ToolDefinition `json:"tools"`
}

type ModelResponse struct {
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"toolCalls"`
}
