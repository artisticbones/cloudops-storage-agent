package main

import (
	"sync"
	"time"
)

// MetricStats is a process-local aggregate. Durations never contain request
// data; labels are limited to stable model/tool names.
type MetricStats struct {
	Count         uint64        `json:"count"`
	Successes     uint64        `json:"successes"`
	Errors        uint64        `json:"errors"`
	TotalDuration time.Duration `json:"totalDuration"`
	MinDuration   time.Duration `json:"minDuration,omitempty"`
	MaxDuration   time.Duration `json:"maxDuration,omitempty"`
}

type NamedMetricStats struct {
	MetricStats
	ByName map[string]MetricStats `json:"byName,omitempty"`
}

type MetricsSnapshot struct {
	Runs    MetricStats      `json:"runs"`
	Models  NamedMetricStats `json:"models"`
	Tools   NamedMetricStats `json:"tools"`
	Context MetricStats      `json:"context"`
	Queue   MetricStats      `json:"queue"`
}

type MetricKind string

const (
	MetricRuns    MetricKind = "run"
	MetricModels  MetricKind = "model"
	MetricTools   MetricKind = "tool"
	MetricContext MetricKind = "context"
	MetricQueue   MetricKind = "queue"
)

// Get returns an aggregate when name is empty, or a named model/tool series.
func (s MetricsSnapshot) Get(kind MetricKind, name string) (MetricStats, bool) {
	switch kind {
	case MetricRuns:
		return s.Runs, true
	case MetricModels:
		if name == "" {
			return s.Models.MetricStats, true
		}
		stats, ok := s.Models.ByName[name]
		return stats, ok
	case MetricTools:
		if name == "" {
			return s.Tools.MetricStats, true
		}
		stats, ok := s.Tools.ByName[name]
		return stats, ok
	case MetricContext:
		return s.Context, true
	case MetricQueue:
		return s.Queue, true
	default:
		return MetricStats{}, false
	}
}

// Metrics is the small instrumentation boundary used by Agent. Implementors
// must not retain input, arguments, results, or other sensitive payloads.
type Metrics interface {
	RecordRun(string, string, time.Duration, bool)
	RecordModel(string, string, time.Duration, bool)
	RecordTool(string, string, string, time.Duration, bool)
	RecordContext(string, time.Duration)
	RecordQueue(string, time.Duration, bool)
	Snapshot() MetricsSnapshot
}

// MemoryMetrics is a thread-safe in-memory metrics implementation suitable for
// one process. It is intentionally dependency-free and can later be adapted
// to Prometheus or OpenTelemetry without changing Agent.
type MemoryMetrics struct {
	mu     sync.RWMutex
	runs   MetricStats
	models NamedMetricStats
	tools  NamedMetricStats
	ctx    MetricStats
	queue  MetricStats
}

var _ Metrics = (*MemoryMetrics)(nil)

func NewMemoryMetrics() *MemoryMetrics {
	return &MemoryMetrics{
		models: NamedMetricStats{ByName: make(map[string]MetricStats)},
		tools:  NamedMetricStats{ByName: make(map[string]MetricStats)},
	}
}

func (m *MemoryMetrics) RecordRun(_ string, _ string, duration time.Duration, success bool) {
	m.record(&m.runs, duration, success)
}

func (m *MemoryMetrics) RecordModel(_ string, name string, duration time.Duration, success bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordLocked(&m.models.MetricStats, duration, success)
	stats := m.models.ByName[name]
	m.recordLocked(&stats, duration, success)
	m.models.ByName[name] = stats
}

func (m *MemoryMetrics) RecordTool(_ string, name, _ string, duration time.Duration, success bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordLocked(&m.tools.MetricStats, duration, success)
	stats := m.tools.ByName[name]
	m.recordLocked(&stats, duration, success)
	m.tools.ByName[name] = stats
}

func (m *MemoryMetrics) RecordContext(_ string, duration time.Duration) {
	m.record(&m.ctx, duration, true)
}

func (m *MemoryMetrics) RecordQueue(_ string, duration time.Duration, success bool) {
	m.record(&m.queue, duration, success)
}

func (m *MemoryMetrics) record(stats *MetricStats, duration time.Duration, success bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordLocked(stats, duration, success)
}

func (m *MemoryMetrics) recordLocked(stats *MetricStats, duration time.Duration, success bool) {
	if duration < 0 {
		duration = 0
	}
	stats.Count++
	if success {
		stats.Successes++
	} else {
		stats.Errors++
	}
	stats.TotalDuration += duration
	if stats.Count == 1 || duration < stats.MinDuration {
		stats.MinDuration = duration
	}
	if duration > stats.MaxDuration {
		stats.MaxDuration = duration
	}
}

func (m *MemoryMetrics) Snapshot() MetricsSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	snapshot := MetricsSnapshot{Runs: m.runs, Models: m.models, Tools: m.tools, Context: m.ctx, Queue: m.queue}
	snapshot.Models.ByName = cloneMetricMap(m.models.ByName)
	snapshot.Tools.ByName = cloneMetricMap(m.tools.ByName)
	return snapshot
}

func cloneMetricMap(input map[string]MetricStats) map[string]MetricStats {
	output := make(map[string]MetricStats, len(input))
	for name, stats := range input {
		output[name] = stats
	}
	return output
}
