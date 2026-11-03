package main

import (
	"sync"
	"testing"
	"time"

	"github.com/artisticbones/hello-agent/config"
)

func TestMemoryMetricsSnapshotAggregatesCountsAndDurations(t *testing.T) {
	m := NewMemoryMetrics()
	m.RecordRun("session-1", "run-1", 10*time.Millisecond, true)
	m.RecordModel("run-1", "fake", 20*time.Millisecond, false)
	m.RecordTool("run-1", "lookup", "call-1", 30*time.Millisecond, true)
	m.RecordContext("run-1", 2*time.Millisecond)
	m.RecordQueue("run-1", 4*time.Millisecond, false)

	snapshot := m.Snapshot()
	if snapshot.Runs.Count != 1 || snapshot.Runs.Errors != 0 || snapshot.Runs.Successes != 1 {
		t.Fatalf("runs = %+v", snapshot.Runs)
	}
	if snapshot.Models.Count != 1 || snapshot.Models.Errors != 1 {
		t.Fatalf("models = %+v", snapshot.Models)
	}
	if snapshot.Tools.Count != 1 || snapshot.Tools.Errors != 0 || snapshot.Tools.ByName["lookup"].Count != 1 {
		t.Fatalf("tools = %+v", snapshot.Tools)
	}
	if snapshot.Context.Count != 1 || snapshot.Queue.Count != 1 || snapshot.Queue.Errors != 1 {
		t.Fatalf("context/queue = %+v / %+v", snapshot.Context, snapshot.Queue)
	}
	if snapshot.Runs.TotalDuration != 10*time.Millisecond || snapshot.Models.TotalDuration != 20*time.Millisecond {
		t.Fatalf("durations = run %s model %s", snapshot.Runs.TotalDuration, snapshot.Models.TotalDuration)
	}
	if got, ok := snapshot.Get(MetricTools, "lookup"); !ok || got.Count != 1 {
		t.Fatalf("tool query = %+v, %v", got, ok)
	}
}

func TestMemoryMetricsIsSafeForConcurrentWriters(t *testing.T) {
	m := NewMemoryMetrics()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				m.RecordRun("s", "r", time.Nanosecond, false)
				m.RecordTool("r", "tool", "call", time.Nanosecond, false)
			}
		}()
	}
	wg.Wait()
	if got := m.Snapshot().Runs.Count; got != 2000 {
		t.Fatalf("run count = %d, want 2000", got)
	}
}

func TestMemoryMetricsClampsNegativeDurationsToZero(t *testing.T) {
	m := NewMemoryMetrics()
	m.RecordRun("s", "r", -time.Second, true)
	m.RecordModel("r", "model", -time.Second, true)
	m.RecordTool("r", "tool", "call", -time.Second, true)
	m.RecordContext("r", -time.Second)
	m.RecordQueue("r", -time.Second, true)

	snapshot := m.Snapshot()
	stats := []MetricStats{
		snapshot.Runs,
		snapshot.Models.MetricStats,
		snapshot.Models.ByName["model"],
		snapshot.Tools.MetricStats,
		snapshot.Tools.ByName["tool"],
		snapshot.Context,
		snapshot.Queue,
	}
	for i, got := range stats {
		if got.TotalDuration < 0 || got.MinDuration < 0 || got.MaxDuration < 0 {
			t.Fatalf("stats[%d] contains negative duration: %+v", i, got)
		}
	}
}

func TestAgentTraceAndMetricsContainStableIdentitiesWithoutSensitiveArguments(t *testing.T) {
	client := NewFakeModelClient(
		FakeModelResult{Response: &ModelResponse{ToolCalls: []ToolCall{{ID: "call-1", Name: "secret", Arguments: `{"token":"do-not-record"}`}}}},
		NewFakeTextResult("done"),
	)
	tool := NewFakeToolExecutor("secret", ToolResult{Content: "ok"}, nil)
	tool.Spec.Sensitive = true
	registry := NewToolRegistry()
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	cfg := testMetricsConfig()
	agent, err := NewAgent(client, cfg, registry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(nil, "use secret"); err != nil {
		t.Fatal(err)
	}
	trace := agent.Trace()
	if trace.TraceID == "" || trace.SessionID == "" || trace.RunID == "" {
		t.Fatalf("trace identity = %+v", trace)
	}
	for i, event := range trace.Events {
		if event.Sequence != uint64(i+1) || event.TraceID != trace.TraceID || event.SessionID != trace.SessionID || event.RunID != trace.RunID {
			t.Fatalf("event identity[%d] = %+v", i, event)
		}
		if event.Arguments == "do-not-record" || event.Arguments == `{"token":"do-not-record"}` {
			t.Fatalf("sensitive arguments leaked: %q", event.Arguments)
		}
	}
	if got := agent.Metrics().Snapshot(); got.Runs.Count != 1 || got.Models.Count != 2 || got.Tools.Count != 1 || got.Context.Count != 2 {
		t.Fatalf("metrics = %+v", got)
	}
}

func testMetricsConfig() *config.Config {
	return &config.Config{
		MaxSteps: 3, MaxRetries: 0, RequestTimeout: time.Minute,
		Model: &config.ModelConfig{Name: "fake", BaseURL: "fake://model"},
		Tool:  &config.ToolConfig{MaxToolRecoveries: 1},
	}
}
