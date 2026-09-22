package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// Trace 记录一次 Agent Loop 的完整执行过程。
// 当前阶段使用具体结构体，不引入 Tracer 接口。
type Trace struct {
	Events []traceEvent
}

type traceEvent struct {
	Step       int    `json:"step"`
	Event      string `json:"event"`
	Attempt    int    `json:"attempt,omitempty"`
	Tool       string `json:"tool,omitempty"`
	Arguments  string `json:"arguments,omitempty"`
	Result     string `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
	Reason     string `json:"reason,omitempty"`
	DurationMS int64  `json:"durationMs,omitempty"`
}

func (t *Trace) Add(event traceEvent) {
	if t != nil {
		t.Events = append(t.Events, event)
	}
	data, err := json.Marshal(event)
	if err == nil {
		fmt.Println(string(data))
	}
}

func (t *Trace) AddDuration(event traceEvent, start time.Time) {
	event.DurationMS = time.Since(start).Milliseconds()
	t.Add(event)
}
