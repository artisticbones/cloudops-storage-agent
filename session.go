package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"
)

// Session is the durable conversation identity and its ordered history.
// Trace data deliberately does not live here; checkpoints are the recovery
// representation, while Trace remains diagnostic output.
type Session struct {
	ID        string    `json:"id"`
	Messages  []Message `json:"messages"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// RunStatus describes the durable state of one Agent loop execution.
type RunStatus string

const (
	RunPending     RunStatus = "pending"
	RunRunning     RunStatus = "running"
	RunInterrupted RunStatus = "interrupted"
	RunCompleted   RunStatus = "completed"
	RunFailed      RunStatus = "failed"
)

// Run identifies one execution within a Session.
type Run struct {
	ID        string    `json:"id"`
	SessionID string    `json:"sessionId"`
	Input     string    `json:"input"`
	Step      int       `json:"step"`
	Status    RunStatus `json:"status"`
	Error     string    `json:"error,omitempty"`
	Result    string    `json:"result,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// NewSession creates a new conversation with a stable identity.
func NewSession() Session {
	now := time.Now().UTC()
	return Session{
		ID:        newRuntimeID("session"),
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// NewRun creates a pending run belonging to sessionID.
func NewRun(sessionID string) Run {
	now := time.Now().UTC()
	return Run{
		ID:        newRuntimeID("run"),
		SessionID: sessionID,
		Status:    RunPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

var runtimeIDFallback uint64

func newRuntimeID(prefix string) string {
	var random [12]byte
	if _, err := rand.Read(random[:]); err == nil {
		return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(random[:]))
	}
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano(), atomic.AddUint64(&runtimeIDFallback, 1))
}
