package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// FakeModelResult 表示 FakeModelClient 在一次调用中预设的结果。
// Response 和 Err 至少应该设置一个；两者同时设置时优先返回 Err。
type FakeModelResult struct {
	Response *ModelResponse
	Err      error
}

// FakeModelClient 是 ModelClient 的内存实现，用于测试 Agent Runtime。
// 它不会访问真实模型，只按 Responses 的顺序返回结果。
type FakeModelClient struct {
	Responses []FakeModelResult

	// ReceivedRequests 保存每次收到的请求，顺序与 Complete 调用顺序一致。
	ReceivedRequests []ModelRequest
	// CallCount 是 Complete 被调用的次数。
	CallCount int
	// ContextCancelled 表示至少有一次调用发现 context 已经取消。
	ContextCancelled bool

	mu sync.Mutex
}

var _ ModelClient = (*FakeModelClient)(nil)

// NewFakeModelClient 创建一个按顺序返回预设结果的 Fake Client。
func NewFakeModelClient(results ...FakeModelResult) *FakeModelClient {
	return &FakeModelClient{Responses: results}
}

// NewFakeTextResult 创建普通文本响应。
func NewFakeTextResult(content string) FakeModelResult {
	return FakeModelResult{Response: &ModelResponse{Content: content}}
}

// NewFakeCalculatorToolCallResult 创建 calculator 工具调用响应。
func NewFakeCalculatorToolCallResult(callID, arguments string) FakeModelResult {
	return FakeModelResult{Response: &ModelResponse{
		ToolCalls: []ToolCall{{
			ID:        callID,
			Name:      toolName,
			Arguments: arguments,
		}},
	}}
}

// NewFakeModelErrorResult 创建模型错误响应。
func NewFakeModelErrorResult(err error) FakeModelResult {
	if err == nil {
		err = errors.New("fake model error")
	}
	return FakeModelResult{Err: err}
}

func (f *FakeModelClient) Complete(ctx context.Context, request ModelRequest) (*ModelResponse, error) {
	f.mu.Lock()
	f.CallCount++
	f.ReceivedRequests = append(f.ReceivedRequests, cloneModelRequest(request))
	callIndex := f.CallCount - 1
	f.mu.Unlock()

	if ctx != nil {
		select {
		case <-ctx.Done():
			f.mu.Lock()
			f.ContextCancelled = true
			f.mu.Unlock()
			return nil, ctx.Err()
		default:
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if callIndex >= len(f.Responses) {
		return nil, fmt.Errorf("fake model has no response for call %d", f.CallCount)
	}
	result := f.Responses[callIndex]
	if result.Err != nil {
		return nil, result.Err
	}
	return cloneModelResponse(result.Response), nil
}

func cloneModelRequest(request ModelRequest) ModelRequest {
	clone := ModelRequest{
		Messages: make([]Message, len(request.Messages)),
		Tools:    make([]ToolDefinition, len(request.Tools)),
	}
	copy(clone.Tools, request.Tools)
	for i, message := range request.Messages {
		clone.Messages[i] = message
		clone.Messages[i].ToolCalls = append([]ToolCall(nil), message.ToolCalls...)
	}
	return clone
}

func cloneModelResponse(response *ModelResponse) *ModelResponse {
	if response == nil {
		return nil
	}
	clone := *response
	clone.ToolCalls = append([]ToolCall(nil), response.ToolCalls...)
	return &clone
}
