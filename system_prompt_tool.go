package main

import (
	"context"
	"encoding/json"
	"fmt"
)

type systemPromptToolArgs struct {
	Instruction string `json:"instruction"`
}

// systemPromptTool allows the model to recognize a user's long-lived
// preference from ordinary natural language. It only appends rules, so the
// model cannot accidentally delete the existing safety instructions.
func systemPromptTool(agent **Agent) Tool {
	return &functionTool{
		definition: ToolDefinition{
			Name:        "update_system_prompt",
			Description: "当用户明确要求以后、今后、始终或默认遵守某条回答/行为规则时，追加并持久化该系统指令。普通问题不要调用。",
			Strict:      true,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"instruction": map[string]any{
						"type":        "string",
						"description": "要长期遵守的自然语言规则，不要加入用户原问题或解释文字",
					},
				},
				"required":             []string{"instruction"},
				"additionalProperties": false,
			},
		},
		handler: func(ctx context.Context, rawArgs string) (string, error) {
			if err := contextErr(ctx); err != nil {
				return "", err
			}
			if agent == nil || *agent == nil {
				return "", fmt.Errorf("agent is not ready")
			}
			var args systemPromptToolArgs
			if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
				return "", &AgentError{Kind: InvalidArgument, Message: "invalid system prompt arguments", Err: err}
			}
			if args.Instruction == "" {
				return "", &AgentError{Kind: InvalidArgument, Message: "system instruction cannot be empty"}
			}
			if err := (*agent).AddSystemPrompt(args.Instruction); err != nil {
				return "", err
			}
			return "system instruction appended and will apply to the next model request", nil
		},
	}
}
