package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// RuntimeState is the input needed to construct one model request. Messages
// are the durable conversation history and do not need to contain the system
// prompt. Conversation and History are accepted as aliases for callers that
// keep those names in their runtime state.
type RuntimeState struct {
	SystemPrompt       string
	Messages           []Message
	Conversation       []Message
	History            []Message
	Tools              []ToolDefinition
	TokenBudget        int
	Retriever          Retriever
	RetrievalTenant    string
	RetrievalVersion   string
	RetrievalQuery     string
	RetrievalLimit     int
	RetrievedDocuments []Document
}

// ContextReport explains how a durable history became a model request.
type ContextReport struct {
	BeforeMessages       int
	AfterMessages        int
	BeforeTokens         int
	AfterTokens          int
	TokenBudget          int
	DroppedMessages      int
	TruncatedMessages    int
	DroppedToolResults   int
	TruncatedToolResults int
	Reasons              []string
	Summary              *ContextSummary
	Stats                ContextStats
}

// ContextSummary is a structured placeholder for history that was omitted.
// The current Task 2 strategy records the omission for observability; a later
// summarizer can replace this placeholder without changing ContextReport.
type ContextSummary struct {
	Marker          string `json:"marker"`
	Strategy        string `json:"strategy"`
	OmittedMessages int    `json:"omittedMessages"`
	OmittedTokens   int    `json:"omittedTokens"`
}

// ContextBuilder creates a bounded, model-facing request from runtime state.
type ContextBuilder interface {
	Build(context.Context, RuntimeState) (ModelRequest, ContextReport, error)
}

// DefaultContextBuilder uses a small deterministic token approximation. A
// production tokenizer can replace this implementation without changing the
// Agent Loop contract. A zero budget means no context limit.
type DefaultContextBuilder struct {
	tokenBudget int
	retriever   Retriever
}

var _ ContextBuilder = (*DefaultContextBuilder)(nil)

// NewContextBuilder creates a ContextBuilder with tokenBudget as its default
// budget. RuntimeState.TokenBudget, when positive, overrides that default.
func NewContextBuilder(tokenBudget int) *DefaultContextBuilder {
	if tokenBudget < 0 {
		tokenBudget = 0
	}
	return &DefaultContextBuilder{tokenBudget: tokenBudget}
}

// NewContextBuilderWithRetriever adds retrieval to the existing context
// policy without changing the legacy constructor or message history model.
func NewContextBuilderWithRetriever(tokenBudget int, retriever Retriever) *DefaultContextBuilder {
	builder := NewContextBuilder(tokenBudget)
	builder.retriever = retriever
	return builder
}

// Build preserves system instructions and the newest user message, then
// fills remaining budget with the newest complete message units. An assistant
// tool call and all of its results are one unit, so pruning never leaves an
// orphan tool result or a half-finished tool turn.
func (b *DefaultContextBuilder) Build(ctx context.Context, state RuntimeState) (ModelRequest, ContextReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ModelRequest{}, ContextReport{}, err
	}

	messages := runtimeMessages(state)
	documents := append([]Document(nil), state.RetrievedDocuments...)
	retriever := state.Retriever
	if retriever == nil {
		retriever = b.retriever
	}
	if retriever != nil && len(documents) == 0 {
		query := strings.TrimSpace(state.RetrievalQuery)
		if query == "" {
			query = latestUserMessage(messages)
		}
		limit := state.RetrievalLimit
		if limit <= 0 {
			limit = 5
		}
		var err error
		documents, err = retriever.Search(ctx, state.RetrievalTenant, state.RetrievalVersion, query, limit)
		if err != nil {
			return ModelRequest{}, ContextReport{}, err
		}
	}
	if retrievalContext := formatRetrievedDocuments(documents); retrievalContext != "" {
		messages = append([]Message{{Role: "system", Content: retrievalContext}}, messages...)
	}
	if state.SystemPrompt != "" && !hasSystemPrompt(messages, state.SystemPrompt) {
		messages = append([]Message{{Role: "system", Content: state.SystemPrompt}}, messages...)
	}
	tools := cloneToolDefinitions(state.Tools)
	budget := state.TokenBudget
	if budget <= 0 {
		budget = b.tokenBudget
	}

	report := ContextReport{
		BeforeMessages: len(messages),
		BeforeTokens:   estimateMessagesTokens(messages),
		TokenBudget:    budget,
	}
	if budget <= 0 {
		request := ModelRequest{Messages: cloneMessages(messages), Tools: tools}
		report.AfterMessages = len(request.Messages)
		report.AfterTokens = estimateMessagesTokens(request.Messages)
		report.Stats = contextStatsForReport(request, report)
		return request, report, nil
	}

	groups, orphanToolResults := messageGroups(messages)
	report.DroppedToolResults += orphanToolResults
	if orphanToolResults > 0 {
		report.addReason("orphan_tool_result_dropped")
	}

	latestUser := -1
	for i, group := range groups {
		if len(group.messages) == 1 && group.messages[0].Role == "user" {
			latestUser = i
		}
	}

	selected := make(map[int]messageGroup)
	used := 0
	for i, group := range groups {
		if group.required || i == latestUser {
			fitted, changed, ok := fitMessageGroup(group, budget-used)
			if !ok {
				// Keeping the role with an empty content is preferable to
				// silently losing the system instruction or current user turn.
				fitted = minimalMessageGroup(group)
			}
			selected[i] = fitted
			used += estimateMessagesTokens(fitted.messages)
			report.recordChanges(group, fitted, changed)
		}
	}

	for i := len(groups) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return ModelRequest{}, ContextReport{}, err
		}
		if _, ok := selected[i]; ok {
			continue
		}
		fitted, changed, ok := fitMessageGroup(groups[i], budget-used)
		if !ok {
			report.DroppedMessages += len(groups[i].messages)
			report.DroppedToolResults += countToolResults(groups[i].messages)
			report.addReason("message_dropped")
			continue
		}
		selected[i] = fitted
		used += estimateMessagesTokens(fitted.messages)
		report.recordChanges(groups[i], fitted, changed)
	}

	indices := make([]int, 0, len(selected))
	for i := range selected {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	resultMessages := make([]Message, 0)
	for _, i := range indices {
		resultMessages = append(resultMessages, selected[i].messages...)
	}

	request := ModelRequest{Messages: resultMessages, Tools: tools}
	report.AfterMessages = len(request.Messages)
	report.AfterTokens = estimateMessagesTokens(request.Messages)
	if report.AfterTokens > budget {
		return ModelRequest{}, ContextReport{}, fmt.Errorf("context builder exceeded token budget: %d > %d", report.AfterTokens, budget)
	}
	report.finalizeSummary()
	report.Stats = contextStatsForReport(request, report)
	return request, report, nil
}

type messageGroup struct {
	messages []Message
	required bool
}

type groupChanges struct {
	changed bool
}

func runtimeMessages(state RuntimeState) []Message {
	switch {
	case state.Messages != nil:
		return cloneMessages(state.Messages)
	case state.Conversation != nil:
		return cloneMessages(state.Conversation)
	default:
		return cloneMessages(state.History)
	}
}

func hasSystemPrompt(messages []Message, prompt string) bool {
	for _, message := range messages {
		if message.Role == "system" && message.Content == prompt {
			return true
		}
	}
	return false
}

func messageGroups(messages []Message) ([]messageGroup, int) {
	groups := make([]messageGroup, 0, len(messages))
	orphanToolResults := 0
	for i := 0; i < len(messages); i++ {
		message := messages[i]
		if message.Role == "tool" {
			orphanToolResults++
			continue
		}
		if message.Role != "assistant" || len(message.ToolCalls) == 0 {
			groups = append(groups, messageGroup{messages: []Message{message}, required: message.Role == "system"})
			continue
		}

		callIDs := make(map[string]bool, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			callIDs[call.ID] = false
		}
		groupMessages := []Message{message}
		j := i + 1
		for ; j < len(messages) && messages[j].Role == "tool"; j++ {
			if _, ok := callIDs[messages[j].ToolCallID]; !ok {
				continue
			}
			callIDs[messages[j].ToolCallID] = true
			groupMessages = append(groupMessages, messages[j])
		}
		complete := true
		for _, resultSeen := range callIDs {
			if !resultSeen {
				complete = false
				break
			}
		}
		if complete {
			groups = append(groups, messageGroup{messages: groupMessages})
		} else {
			orphanToolResults += countToolResults(groupMessages)
		}
		i = j - 1
	}
	return groups, orphanToolResults
}

func fitMessageGroup(group messageGroup, budget int) (messageGroup, groupChanges, bool) {
	if budget <= 0 {
		return messageGroup{}, groupChanges{}, false
	}
	result := messageGroup{messages: cloneMessages(group.messages), required: group.required}
	if estimateMessagesTokens(result.messages) <= budget {
		return result, groupChanges{}, true
	}

	changed := groupChanges{}
	for estimateMessagesTokens(result.messages) > budget {
		beforeTokens := estimateMessagesTokens(result.messages)
		candidate := -1
		candidateScore := -1
		for i, message := range result.messages {
			if len([]rune(message.Content)) == 0 {
				continue
			}
			score := len([]rune(message.Content))
			if message.Role == "tool" {
				score += 1_000_000
			}
			if score > candidateScore {
				candidate, candidateScore = i, score
			}
		}
		if candidate < 0 || candidateScore == 0 {
			return messageGroup{}, groupChanges{}, false
		}
		message := result.messages[candidate]
		currentTokens := estimateMessageTokens(message)
		targetTokens := currentTokens - (estimateMessagesTokens(result.messages) - budget)
		if targetTokens >= currentTokens {
			targetTokens = currentTokens - 1
		}
		if targetTokens < 1 {
			targetTokens = 1
		}
		trimmed, ok := truncateMessageToTokens(message, targetTokens)
		if !ok || trimmed.Content == message.Content {
			result.messages[candidate].Content = ""
		} else {
			result.messages[candidate] = trimmed
		}
		changed.changed = true
		if estimateMessagesTokens(result.messages) >= beforeTokens {
			return messageGroup{}, groupChanges{}, false
		}
	}
	if estimateMessagesTokens(result.messages) > budget {
		return messageGroup{}, groupChanges{}, false
	}
	return result, changed, true
}

func minimalMessageGroup(group messageGroup) messageGroup {
	result := messageGroup{required: group.required, messages: cloneMessages(group.messages)}
	for i := range result.messages {
		if result.messages[i].Role != "assistant" {
			result.messages[i].Content = ""
		}
	}
	return result
}

func truncateMessageToTokens(message Message, target int) (Message, bool) {
	if estimateMessageTokens(message) <= target {
		return message, true
	}
	runes := []rune(message.Content)
	if len(runes) == 0 {
		return message, false
	}
	marker := " [truncated]"
	low, high := 0, len(runes)
	best := ""
	for low <= high {
		middle := (low + high) / 2
		candidate := string(runes[:middle]) + marker
		copyMessage := message
		copyMessage.Content = candidate
		if estimateMessageTokens(copyMessage) <= target {
			best = candidate
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	if best == "" {
		return Message{}, false
	}
	message.Content = best
	return message, true
}

func estimateTextTokens(text string) int {
	if text == "" {
		return 0
	}
	return (len([]rune(text)) + 3) / 4
}

func estimateMessageTokens(message Message) int {
	tokens := 1 + estimateTextTokens(message.Content)
	if message.ToolCallID != "" {
		tokens++
	}
	for _, call := range message.ToolCalls {
		tokens += 1 + estimateTextTokens(call.Name) + estimateTextTokens(call.Arguments)
	}
	return tokens
}

func estimateMessagesTokens(messages []Message) int {
	tokens := 0
	for _, message := range messages {
		tokens += estimateMessageTokens(message)
	}
	return tokens
}

func countToolResults(messages []Message) int {
	count := 0
	for _, message := range messages {
		if message.Role == "tool" {
			count++
		}
	}
	return count
}

func (r *ContextReport) recordChanges(original, fitted messageGroup, changes groupChanges) {
	if !changes.changed {
		return
	}
	for i := range original.messages {
		if i >= len(fitted.messages) || original.messages[i].Content == fitted.messages[i].Content {
			continue
		}
		r.TruncatedMessages++
		if original.messages[i].Role == "tool" {
			r.TruncatedToolResults++
			r.addReason("tool_result_truncated")
		} else {
			r.addReason("message_truncated")
		}
	}
}

func (r *ContextReport) addReason(reason string) {
	for _, existing := range r.Reasons {
		if existing == reason {
			return
		}
	}
	r.Reasons = append(r.Reasons, reason)
}

func (r *ContextReport) finalizeSummary() {
	if r.DroppedMessages == 0 && r.TruncatedMessages == 0 && r.DroppedToolResults == 0 && r.TruncatedToolResults == 0 {
		return
	}
	r.addReason("summary_placeholder")
	omittedTokens := r.BeforeTokens - r.AfterTokens
	if omittedTokens < 0 {
		omittedTokens = 0
	}
	r.Summary = &ContextSummary{
		Marker:          "[context summary placeholder]",
		Strategy:        "placeholder_only",
		OmittedMessages: r.DroppedMessages,
		OmittedTokens:   omittedTokens,
	}
}

func contextStatsForReport(request ModelRequest, report ContextReport) ContextStats {
	stats := summarizeContext(request)
	if report.TokenBudget <= 0 || report.Summary == nil {
		return stats
	}
	stats.BeforeMessages = report.BeforeMessages
	stats.AfterMessages = report.AfterMessages
	stats.BeforeTokens = report.BeforeTokens
	stats.AfterTokens = report.AfterTokens
	stats.TokenBudget = report.TokenBudget
	stats.DroppedMessages = report.DroppedMessages
	stats.TruncatedMessages = report.TruncatedMessages
	stats.DroppedToolResults = report.DroppedToolResults
	stats.TruncatedToolResults = report.TruncatedToolResults
	stats.TruncationReasons = strings.Join(report.Reasons, ",")
	stats.SummaryMarker = report.Summary.Marker
	stats.SummaryStrategy = report.Summary.Strategy
	return stats
}

func cloneToolDefinitions(tools []ToolDefinition) []ToolDefinition {
	if len(tools) == 0 {
		return nil
	}
	clone := make([]ToolDefinition, len(tools))
	copy(clone, tools)
	for i, tool := range clone {
		if tool.Parameters != nil {
			clone[i].Parameters = make(map[string]any, len(tool.Parameters))
			for key, value := range tool.Parameters {
				clone[i].Parameters[key] = value
			}
		}
	}
	return clone
}

func (r ContextReport) String() string {
	return strings.Join(r.Reasons, ",")
}
