package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAgentServiceHTTPCreatesSessionRunsQueriesAndStreamsEvents(t *testing.T) {
	agent := newTestAgent(t, NewFakeModelClient(NewFakeTextResult("hello")), 2, 0)
	service := NewAgentService(agent)
	defer service.Close()
	server := httptest.NewServer(service)
	defer server.Close()

	var session Session
	postJSON(t, server.URL+"/sessions", "", &session)
	if session.ID == "" {
		t.Fatal("created session has no ID")
	}

	var run Run
	postJSON(t, server.URL+"/sessions/"+session.ID+"/runs", `{"input":"hi"}`, &run)
	if run.ID == "" || run.SessionID != session.ID {
		t.Fatalf("created run = %+v", run)
	}

	deadline := time.Now().Add(time.Second)
	for {
		var got Run
		getJSON(t, server.URL+"/runs/"+run.ID, &got)
		if got.Status == RunCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not complete: %+v", got)
		}
		time.Sleep(time.Millisecond)
	}

	response, err := http.Get(server.URL + "/runs/" + run.ID + "/events?after=0")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d", response.StatusCode)
	}
	var events []RunEvent
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			var event RunEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatal(err)
			}
			events = append(events, event)
			if event.Type == EventRunCompleted {
				break
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) < 3 || events[0].Type != EventRunStarted || events[len(events)-1].Type != EventRunCompleted {
		t.Fatalf("events = %+v", events)
	}
	for i, event := range events {
		if event.Sequence != uint64(i+1) {
			t.Fatalf("event %d sequence = %d", i, event.Sequence)
		}
	}

	read, err := service.ReadEvents(context.Background(), run.ID, events[0].Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if len(read) != len(events)-1 || read[0].Sequence != events[1].Sequence {
		t.Fatalf("ReadEvents(after first) = %+v", read)
	}
}

func TestAgentServiceCancelPropagatesToModelAndTool(t *testing.T) {
	model := &blockingModelClient{started: make(chan struct{})}
	tool := &blockingTool{started: make(chan struct{}), cancelled: make(chan struct{})}
	registry := NewToolRegistry()
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	agent := newCheckpointTestAgent(t, model, registry, NewMemoryCheckpointStore())
	service := NewAgentService(agent)
	defer service.Close()

	session, err := service.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.StartRun(context.Background(), session.ID, "cancel me")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("model did not start")
	}
	if err := service.CancelRun(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.cancelled:
	case <-time.After(time.Second):
		t.Fatal("model did not observe cancellation")
	}
	deadline := time.Now().Add(time.Second)
	for {
		got, err := service.GetRun(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == RunInterrupted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run after cancellation = %+v", got)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAgentServiceDirectRunUsesLimiterAndReleasesAfterCompletion(t *testing.T) {
	limiter := &countingLimiter{}
	agent := newTestAgent(t, NewFakeModelClient(NewFakeTextResult("hello")), 2, 0)
	service := NewAgentServiceWithOptions(agent, AgentServiceOptions{Limiter: limiter})
	defer service.Close()

	session, err := service.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.StartRun(context.Background(), session.ID, "limited")
	if err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, service, run.ID, RunCompleted)
	if got := limiter.allowCount(); got != 1 {
		t.Fatalf("limiter Allow count = %d, want 1", got)
	}
	if got := limiter.releaseCount(); got != 1 {
		t.Fatalf("limiter Release count = %d, want 1", got)
	}
}

func TestAgentServiceDirectRunReleasesLimiterAfterCancellation(t *testing.T) {
	limiter := &countingLimiter{}
	model := &blockingModelClient{started: make(chan struct{})}
	agent := newCheckpointTestAgent(t, model, NewToolRegistry(), NewMemoryCheckpointStore())
	service := NewAgentServiceWithOptions(agent, AgentServiceOptions{Limiter: limiter})
	defer service.Close()

	session, err := service.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.StartRun(context.Background(), session.ID, "cancel limited")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("model did not start")
	}
	if err := service.CancelRun(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, service, run.ID, RunInterrupted)
	if got := limiter.allowCount(); got != 1 {
		t.Fatalf("limiter Allow count = %d, want 1", got)
	}
	if got := limiter.releaseCount(); got != 1 {
		t.Fatalf("limiter Release count = %d, want 1", got)
	}
}

type countingLimiter struct {
	mu       sync.Mutex
	allows   int
	releases int
}

func (l *countingLimiter) Allow(context.Context, string) error {
	l.mu.Lock()
	l.allows++
	l.mu.Unlock()
	return nil
}

func (l *countingLimiter) Release(string) {
	l.mu.Lock()
	l.releases++
	l.mu.Unlock()
}

func (l *countingLimiter) allowCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.allows
}

func (l *countingLimiter) releaseCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.releases
}

func TestAgentServiceCloseDoesNotDeadlockWhenRunPublishesAfterCancellation(t *testing.T) {
	model := &blockingModelClient{started: make(chan struct{})}
	agent := newCheckpointTestAgent(t, model, NewToolRegistry(), NewMemoryCheckpointStore())
	service := NewAgentService(agent)

	session, err := service.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.StartRun(context.Background(), session.ID, "close me")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("model did not start")
	}
	service.mu.Lock()
	record := service.runs[run.ID]
	originalCancel := record.cancel
	record.cancel = func() {
		service.mu.RLock()
		service.mu.RUnlock()
		originalCancel()
	}
	service.mu.Unlock()

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- service.Close()
	}()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("service.Close deadlocked while the run published its completion event")
	}

	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentServiceCancelPropagatesToTool(t *testing.T) {
	model := NewFakeModelClient(FakeModelResult{Response: &ModelResponse{ToolCalls: []ToolCall{{ID: "call", Name: "blocking", Arguments: `{}`}}}})
	tool := &blockingTool{started: make(chan struct{}), cancelled: make(chan struct{})}
	registry := NewToolRegistry()
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	agent := newCheckpointTestAgent(t, model, registry, NewMemoryCheckpointStore())
	service := NewAgentService(agent)
	defer service.Close()

	session, err := service.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.StartRun(context.Background(), session.ID, "cancel tool")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-tool.started:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}
	if err := service.CancelRun(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tool.cancelled:
	case <-time.After(time.Second):
		t.Fatal("tool did not observe cancellation")
	}
}

func TestAgentServiceEventsResumeFromLastEventIDWithoutDuplicates(t *testing.T) {
	agent := newTestAgent(t, NewFakeModelClient(NewFakeTextResult("hello")), 2, 0)
	service := NewAgentService(agent)
	defer service.Close()

	session, err := service.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.StartRun(context.Background(), session.ID, "resume me")
	if err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, service, run.ID, RunCompleted)

	all := readEventRequest(t, service, "/runs/"+run.ID+"/events?after=0", "")
	if len(all) < 2 {
		t.Fatalf("events = %+v", all)
	}
	resumeAfter := all[0].Sequence
	resumed := readEventRequest(t, service, "/runs/"+run.ID+"/events", strconv.FormatUint(resumeAfter, 10))
	if len(resumed) != len(all)-1 {
		t.Fatalf("resumed events = %+v, all events = %+v", resumed, all)
	}
	for i, event := range resumed {
		wantSequence := all[i+1].Sequence
		if event.Sequence != wantSequence {
			t.Fatalf("resumed event %d sequence = %d, want %d", i, event.Sequence, wantSequence)
		}
	}
}

func TestAgentServiceEventsQueryAfterTakesPriorityOverLastEventID(t *testing.T) {
	agent := newTestAgent(t, NewFakeModelClient(NewFakeTextResult("hello")), 2, 0)
	service := NewAgentService(agent)
	defer service.Close()

	session, err := service.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.StartRun(context.Background(), session.ID, "query wins")
	if err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, service, run.ID, RunCompleted)

	events := readEventRequest(t, service, "/runs/"+run.ID+"/events?after=1", "0")
	if len(events) == 0 || events[0].Sequence <= 1 {
		t.Fatalf("events = %+v", events)
	}
}

func TestAgentServiceHTTPDeleteAndEventErrors(t *testing.T) {
	agent := newTestAgent(t, NewFakeModelClient(NewFakeTextResult("hello")), 2, 0)
	service := NewAgentService(agent)
	defer service.Close()
	server := httptest.NewServer(service)
	defer server.Close()

	response, err := http.Get(server.URL + "/runs/missing/events?after=not-a-number")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid after status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}

	request, err := http.NewRequest(http.MethodGet, server.URL+"/runs/missing/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Last-Event-ID", "not-a-number")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid Last-Event-ID status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}

	request, err = http.NewRequest(http.MethodDelete, server.URL+"/runs/missing", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing DELETE status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}

	session, err := service.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.StartRun(context.Background(), session.ID, "delete me")
	if err != nil {
		t.Fatal(err)
	}
	request, err = http.NewRequest(http.MethodDelete, server.URL+"/runs/"+run.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("existing DELETE status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
}

func TestAgentServiceCancelAndPublishProtectRunStatus(t *testing.T) {
	service := NewAgentService(nil)
	run := NewRun("session")
	_, cancel := context.WithCancel(context.Background())
	service.runs[run.ID] = &serviceRun{run: run, cancel: cancel}
	service.events[run.ID] = newEventBroker()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = service.CancelRun(context.Background(), run.ID)
		}()
		go func() {
			defer wg.Done()
			service.publish(run.ID, RunEvent{Type: EventRunCompleted, Status: RunCompleted})
		}()
	}
	wg.Wait()
	got, err := service.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != RunCompleted {
		t.Fatalf("run status = %q, want %q", got.Status, RunCompleted)
	}
}

func TestEventBrokerLargeReplayDoesNotBlockPublish(t *testing.T) {
	broker := newEventBroker()
	for i := 0; i < 256; i++ {
		broker.publish(RunEvent{Type: EventModelStarted})
	}
	broker.terminal = false

	done := make(chan struct{})
	go func() {
		_, _, _ = broker.subscribe(context.Background(), 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("large event replay blocked on an unconsumed subscriber")
	}
}

func TestEventBrokerBackgroundSubscriptionClosesWhenServiceCloses(t *testing.T) {
	service := NewAgentService(nil)
	run := NewRun("session")
	_, cancel := context.WithCancel(context.Background())
	service.runs[run.ID] = &serviceRun{run: run, cancel: cancel}
	broker := newEventBroker()
	service.events[run.ID] = broker

	stream, unsubscribe, err := broker.subscribe(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-stream:
		if ok {
			t.Fatal("subscriber channel remained open after service close")
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber channel was not closed by service close")
	}

	unsubscribe()
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEventBrokerUnsubscribeIsIdempotentAndContextCancellationCloses(t *testing.T) {
	broker := newEventBroker()
	ctx, cancel := context.WithCancel(context.Background())
	stream, unsubscribe, err := broker.subscribe(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	unsubscribe()
	unsubscribe()
	cancel()
	select {
	case _, ok := <-stream:
		if ok {
			t.Fatal("unsubscribed channel remained open")
		}
	case <-time.After(time.Second):
		t.Fatal("unsubscribed channel was not closed")
	}

	broker.publish(RunEvent{Type: EventModelStarted})
	broker.close()
	broker.close()
}

func TestAgentServiceCloseStopsRunningSSE(t *testing.T) {
	service := NewAgentService(nil)
	run := NewRun("session")
	_, cancel := context.WithCancel(context.Background())
	service.runs[run.ID] = &serviceRun{run: run, cancel: cancel}
	service.events[run.ID] = newEventBroker()

	request := httptest.NewRequest(http.MethodGet, "/runs/"+run.ID+"/events", nil)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		service.ServeHTTP(recorder, request)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("SSE handler returned before service close")
	case <-time.After(20 * time.Millisecond):
	}

	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not exit after service close")
	}
}

func TestEventBrokerBackgroundSubscriptionsDoNotCreatePersistentGoroutines(t *testing.T) {
	broker := newEventBroker()
	before := runtime.NumGoroutine()
	for i := 0; i < 100; i++ {
		_, unsubscribe, err := broker.subscribe(context.Background(), 0)
		if err != nil {
			t.Fatal(err)
		}
		unsubscribe()
	}
	runtime.GC()
	after := runtime.NumGoroutine()
	if after-before > 2 {
		t.Fatalf("background subscriptions increased goroutines: before=%d after=%d", before, after)
	}
}

func waitForRunStatus(t *testing.T, service *agentService, runID string, want RunStatus) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		run, err := service.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run status = %q, want %q", run.Status, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func readEventRequest(t *testing.T, service *agentService, target, lastEventID string) []RunEvent {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	recorder := httptest.NewRecorder()
	service.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("events status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var events []RunEvent
	scanner := bufio.NewScanner(recorder.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			var event RunEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatal(err)
			}
			events = append(events, event)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func postJSON(t *testing.T, url, body string, target any) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	response, err := http.Post(url, "application/json", reader)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("POST %s status = %d", url, response.StatusCode)
	}
	if target != nil && json.NewDecoder(response.Body).Decode(target) != nil {
		t.Fatal("invalid JSON response")
	}
}

func getJSON(t *testing.T, url string, target any) {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d", url, response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}

type blockingModelClient struct {
	started   chan struct{}
	cancelled chan struct{}
	once      sync.Once
}

func (m *blockingModelClient) Complete(ctx context.Context, _ ModelRequest) (*ModelResponse, error) {
	m.once.Do(func() {
		close(m.started)
		m.cancelled = make(chan struct{})
	})
	<-ctx.Done()
	close(m.cancelled)
	return nil, ctx.Err()
}

type blockingTool struct {
	started   chan struct{}
	cancelled chan struct{}
}

func (t *blockingTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "blocking", Parameters: map[string]any{"type": "object"}}
}

func (t *blockingTool) Execute(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
	close(t.started)
	<-ctx.Done()
	close(t.cancelled)
	return ToolResult{}, ctx.Err()
}
