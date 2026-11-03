package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Trace 记录一次 Agent Loop 的完整执行过程。
// 当前阶段使用具体结构体，不引入 Tracer 接口。
type Trace struct {
	TraceID   string
	SessionID string
	RunID     string
	Events    []traceEvent
	mu        sync.Mutex
}

type traceEvent struct {
	TraceID      string        `json:"traceId,omitempty"`
	SessionID    string        `json:"sessionId,omitempty"`
	RunID        string        `json:"runId,omitempty"`
	Sequence     uint64        `json:"sequence,omitempty"`
	Step         int           `json:"step"`
	Event        string        `json:"event"`
	Attempt      int           `json:"attempt,omitempty"`
	Tool         string        `json:"tool,omitempty"`
	Arguments    string        `json:"arguments,omitempty"`
	Result       string        `json:"result,omitempty"`
	Error        string        `json:"error,omitempty"`
	Reason       string        `json:"reason,omitempty"`
	ToolCallID   string        `json:"toolCallId,omitempty"`
	DurationMS   int64         `json:"durationMs,omitempty"`
	ContextStats *ContextStats `json:"contextStats,omitempty"`
}

func (t *Trace) Add(event traceEvent) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.TraceID == "" {
		t.TraceID = newRuntimeID("trace")
	}
	event.TraceID = t.TraceID
	event.SessionID = t.SessionID
	event.RunID = t.RunID
	event.Sequence = uint64(len(t.Events) + 1)
	t.Events = append(t.Events, event)
	t.mu.Unlock()
	data, err := json.Marshal(event)
	if err == nil {
		fmt.Println(string(data))
	}
}

func NewTrace() *Trace { return &Trace{TraceID: newRuntimeID("trace")} }

func (t *Trace) SetIdentity(sessionID, runID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.SessionID = sessionID
	t.RunID = runID
	t.mu.Unlock()
}

func (t *Trace) Snapshot() []traceEvent {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]traceEvent(nil), t.Events...)
}

func (t *Trace) AddDuration(event traceEvent, start time.Time) {
	event.DurationMS = time.Since(start).Milliseconds()
	t.Add(event)
}
