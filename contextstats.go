package main

// ContextStats 描述一次 ModelRequest 中的上下文规模。
//
// 它属于 Agent Runtime 的观测数据，不会被追加到 ModelRequest，
// 也不会发送给模型。Runtime 可以用它记录上下文增长趋势，
// 为后续的裁剪、摘要或 Tool Schema 选择提供依据。
type ContextStats struct {
	MessageCount         int    `json:"messageCount"`
	UserMessages         int    `json:"userMessages"`
	AssistantCalls       int    `json:"assistantCalls"`
	ToolMessages         int    `json:"toolMessages"`
	ToolCount            int    `json:"toolCount"`
	BeforeMessages       int    `json:"beforeMessages,omitempty"`
	AfterMessages        int    `json:"afterMessages,omitempty"`
	BeforeTokens         int    `json:"beforeTokens,omitempty"`
	AfterTokens          int    `json:"afterTokens,omitempty"`
	TokenBudget          int    `json:"tokenBudget,omitempty"`
	DroppedMessages      int    `json:"droppedMessages,omitempty"`
	TruncatedMessages    int    `json:"truncatedMessages,omitempty"`
	DroppedToolResults   int    `json:"droppedToolResults,omitempty"`
	TruncatedToolResults int    `json:"truncatedToolResults,omitempty"`
	TruncationReasons    string `json:"truncationReasons,omitempty"`
	SummaryMarker        string `json:"summaryMarker,omitempty"`
	SummaryStrategy      string `json:"summaryStrategy,omitempty"`
}

// summarizeContext 统计一次 ModelRequest 的上下文组成。
// AssistantCalls 表示包含 Tool Call 的 assistant message 数量，
// 而不是单个 Tool Call 的数量；一个 assistant message 可以包含多个 Tool Call。
func summarizeContext(request ModelRequest) ContextStats {
	stats := ContextStats{
		MessageCount: len(request.Messages),
		ToolCount:    len(request.Tools),
	}

	for _, message := range request.Messages {
		switch message.Role {
		case "user":
			stats.UserMessages++
		case "assistant":
			if len(message.ToolCalls) > 0 {
				stats.AssistantCalls++
			}
		case "tool":
			stats.ToolMessages++
		}
	}

	return stats
}
