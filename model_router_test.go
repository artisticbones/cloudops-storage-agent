package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestModelRouterSelectCompletesWithPrimaryModel(t *testing.T) {
	primary := NewFakeModelClient(NewFakeTextResult("primary"))
	router := NewModelRouter(ModelRouterOptions{
		Primary: ModelEndpoint{Name: "primary", Client: primary, Capability: ModelCapability{Name: "primary"}},
	})

	client, capability, err := router.Select(context.Background(), testModelRequest())
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	response, err := client.Complete(context.Background(), testModelRequest())
	if err != nil || response.Content != "primary" {
		t.Fatalf("Complete() response=%+v error=%v", response, err)
	}
	if capability.Name != "primary" || primary.CallCount != 1 {
		t.Fatalf("capability=%+v calls=%d", capability, primary.CallCount)
	}
}

func TestModelRouterContextTimeoutDoesNotFallback(t *testing.T) {
	primary := NewFakeModelClient(NewFakeTextResult("should not run"))
	backup := NewFakeModelClient(NewFakeTextResult("backup"))
	router := NewModelRouter(ModelRouterOptions{
		Primary:  ModelEndpoint{Name: "primary", Client: primary},
		Fallback: ModelEndpoint{Name: "backup", Client: backup},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	_, _, err := router.Select(ctx, testModelRequest())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Select() error = %v, want deadline exceeded", err)
	}
	if primary.CallCount != 0 || backup.CallCount != 0 {
		t.Fatalf("calls primary=%d backup=%d", primary.CallCount, backup.CallCount)
	}
}

func TestModelRouterRateLimitFallsBackToBackup(t *testing.T) {
	primary := NewFakeModelClient(NewFakeModelErrorResult(NewModelHTTPError(429, errors.New("rate limited"))))
	backup := NewFakeModelClient(NewFakeTextResult("backup"))
	router := NewModelRouter(ModelRouterOptions{
		Primary:  ModelEndpoint{Name: "primary", Client: primary},
		Fallback: ModelEndpoint{Name: "backup", Client: backup},
	})

	client, capability, err := router.Select(context.Background(), testModelRequest())
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	response, err := client.Complete(context.Background(), testModelRequest())
	if err != nil || response.Content != "backup" {
		t.Fatalf("Complete() response=%+v error=%v", response, err)
	}
	if capability.Name != "primary" || primary.CallCount != 1 || backup.CallCount != 1 {
		t.Fatalf("capability=%+v calls primary=%d backup=%d", capability, primary.CallCount, backup.CallCount)
	}
}

func TestModelRouterRetriesFiveHundredErrorsWithinBounds(t *testing.T) {
	primary := NewFakeModelClient(
		NewFakeModelErrorResult(NewModelHTTPError(503, errors.New("unavailable"))),
		NewFakeModelErrorResult(NewModelHTTPError(503, errors.New("unavailable"))),
		NewFakeTextResult("recovered"),
	)
	router := NewModelRouter(ModelRouterOptions{
		Primary:      ModelEndpoint{Name: "primary", Client: primary},
		MaxRetries:   2,
		RetryBackoff: time.Millisecond,
		RetryMaxTime: time.Second,
	})
	client, _, err := router.Select(context.Background(), testModelRequest())
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	response, err := client.Complete(context.Background(), testModelRequest())
	if err != nil || response.Content != "recovered" {
		t.Fatalf("Complete() response=%+v error=%v", response, err)
	}
	if primary.CallCount != 3 {
		t.Fatalf("calls = %d, want 3", primary.CallCount)
	}
}

func TestModelRouterInvalidArgumentDoesNotFallback(t *testing.T) {
	primary := NewFakeModelClient(NewFakeModelErrorResult(&AgentError{Kind: InvalidArgument, Message: "bad request"}))
	backup := NewFakeModelClient(NewFakeTextResult("backup"))
	router := NewModelRouter(ModelRouterOptions{
		Primary:  ModelEndpoint{Name: "primary", Client: primary},
		Fallback: ModelEndpoint{Name: "backup", Client: backup},
	})
	client, _, err := router.Select(context.Background(), testModelRequest())
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	_, err = client.Complete(context.Background(), testModelRequest())
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != InvalidArgument {
		t.Fatalf("error = %v, want invalid argument", err)
	}
	if backup.CallCount != 0 {
		t.Fatalf("backup calls = %d, want 0", backup.CallCount)
	}
}

func TestModelRouterQuotaFallsBackToOllamaAndEnforcesBudget(t *testing.T) {
	primary := NewFakeModelClient(NewFakeModelErrorResult(NewQuotaError("primary quota exhausted")))
	ollama := NewFakeModelClient(NewFakeTextResult("local"))
	router := NewModelRouter(ModelRouterOptions{
		Primary: ModelEndpoint{Name: "primary", Client: primary},
		Ollama:  ModelEndpoint{Name: "ollama", Client: ollama, Capability: ModelCapability{Name: "ollama", Local: true}},
		Budget:  ModelBudget{MaxCalls: 2, MaxInputTokens: 100, MaxOutputTokens: 100, MaxCost: 1},
	})
	client, _, err := router.Select(context.Background(), testModelRequest())
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if _, err := client.Complete(context.Background(), testModelRequest()); err != nil {
		t.Fatalf("first Complete() error = %v", err)
	}
	if _, err := client.Complete(context.Background(), testModelRequest()); err == nil {
		t.Fatal("second Complete() error = nil, want budget error")
	}
	if ollama.CallCount != 1 {
		t.Fatalf("ollama calls = %d, want 1", ollama.CallCount)
	}
	implementation, ok := router.(*modelRouter)
	if !ok {
		t.Fatal("NewModelRouter() did not return the built-in router")
	}
	if snapshot := implementation.BudgetSnapshot(); snapshot.Calls != 2 || snapshot.InputTokens == 0 || snapshot.OutputTokens == 0 {
		t.Fatalf("budget snapshot = %+v", snapshot)
	}
}

func TestModelRouterQuotaWithoutFallbackFailsSafely(t *testing.T) {
	primary := NewFakeModelClient(NewFakeModelErrorResult(NewQuotaError("quota exhausted")))
	router := NewModelRouter(ModelRouterOptions{Primary: ModelEndpoint{Name: "primary", Client: primary}})
	client, _, err := router.Select(context.Background(), testModelRequest())
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	_, err = client.Complete(context.Background(), testModelRequest())
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != QuotaError {
		t.Fatalf("error = %v, want quota error", err)
	}
}

func TestModelRouterConcurrentFailuresRespectCallBudget(t *testing.T) {
	const workers = 32
	primary := NewFakeModelClient(NewFakeModelErrorResult(NewModelHTTPError(503, errors.New("down"))))
	router := NewModelRouter(ModelRouterOptions{
		Primary: ModelEndpoint{Name: "primary", Client: primary},
		Budget:  ModelBudget{MaxCalls: 1, MaxInputTokens: 100},
	})

	client, _, err := router.Select(context.Background(), testModelRequest())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = client.Complete(context.Background(), testModelRequest())
		}()
	}
	wg.Wait()

	if primary.CallCount != 1 {
		t.Fatalf("primary calls = %d, want exactly 1", primary.CallCount)
	}
	snapshot := router.(*modelRouter).BudgetSnapshot()
	if snapshot.Calls != primary.CallCount {
		t.Fatalf("budget calls = %d, actual calls = %d", snapshot.Calls, primary.CallCount)
	}
}

func TestModelRouterAuditsSuccessfulCallWhenOutputBudgetExceeded(t *testing.T) {
	primary := &delayedModelClient{delay: 2 * time.Millisecond, response: &ModelResponse{Content: "output exceeds one token"}}
	router := NewModelRouter(ModelRouterOptions{
		Primary: ModelEndpoint{
			Name:       "primary",
			Client:     primary,
			Capability: ModelCapability{Name: "primary", OutputCostPerToken: 0.01},
		},
		Budget: ModelBudget{MaxOutputTokens: 1, MaxCost: 1},
	})

	client, _, err := router.Select(context.Background(), testModelRequest())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Complete(context.Background(), testModelRequest())
	var budgetErr *AgentError
	if !errors.As(err, &budgetErr) || budgetErr.Kind != BudgetError {
		t.Fatalf("Complete() error = %v, want budget error", err)
	}

	snapshot := router.(*modelRouter).BudgetSnapshot()
	if snapshot.Calls != 1 || snapshot.InputTokens == 0 || snapshot.TotalDuration <= 0 {
		t.Fatalf("snapshot = %+v, want audited call/input/duration", snapshot)
	}
}

func TestModelRouterAuditsSuccessfulCallWhenCostBudgetExceeded(t *testing.T) {
	primary := &delayedModelClient{delay: 2 * time.Millisecond, response: &ModelResponse{Content: "costly output"}}
	router := NewModelRouter(ModelRouterOptions{
		Primary: ModelEndpoint{
			Name:       "primary",
			Client:     primary,
			Capability: ModelCapability{Name: "primary", OutputCostPerToken: 1},
		},
		Budget: ModelBudget{MaxCost: 0.5},
	})

	client, _, err := router.Select(context.Background(), testModelRequest())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Complete(context.Background(), testModelRequest())
	var budgetErr *AgentError
	if !errors.As(err, &budgetErr) || budgetErr.Kind != BudgetError {
		t.Fatalf("Complete() error = %v, want budget error", err)
	}

	snapshot := router.(*modelRouter).BudgetSnapshot()
	if snapshot.Calls != 1 || snapshot.InputTokens == 0 || snapshot.TotalDuration <= 0 || snapshot.Cost <= 0 {
		t.Fatalf("snapshot = %+v, want audited call/input/cost/duration", snapshot)
	}
}

func TestModelRouterReservesEntireRemainingDurationForInFlightCall(t *testing.T) {
	primary := &durationBlockingModelClient{started: make(chan struct{})}
	router := NewModelRouter(ModelRouterOptions{
		Primary: ModelEndpoint{Name: "primary", Client: primary},
		Budget:  ModelBudget{MaxDuration: 100 * time.Millisecond},
	})
	client, _, err := router.Select(context.Background(), testModelRequest())
	if err != nil {
		t.Fatal(err)
	}

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := client.Complete(firstCtx, testModelRequest())
		firstDone <- err
	}()
	select {
	case <-primary.started:
	case <-time.After(time.Second):
		t.Fatal("first model call did not start")
	}

	_, err = client.Complete(context.Background(), testModelRequest())
	var budgetErr *AgentError
	if !errors.As(err, &budgetErr) || budgetErr.Kind != BudgetError {
		t.Fatalf("second Complete() error = %v, want duration budget error", err)
	}
	if calls := primary.callCount(); calls != 1 {
		t.Fatalf("provider calls = %d, want 1", calls)
	}

	cancelFirst()
	if err := <-firstDone; err == nil {
		t.Fatal("first Complete() error = nil, want cancellation")
	}
}

func TestWaitRetryRejectsBackoffLongerThanRemainingBudget(t *testing.T) {
	started := time.Unix(100, 0)
	now := func() time.Time { return started.Add(900 * time.Millisecond) }

	err := waitRetry(context.Background(), 200*time.Millisecond, started, time.Second, now)
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != NetworkError || agentErr.Message != "model retry time budget exceeded" {
		t.Fatalf("waitRetry() error = %v, want retry time budget error", err)
	}
}

func testModelRequest() ModelRequest {
	return ModelRequest{Messages: []Message{{Role: "user", Content: "hello"}}}
}

type delayedModelClient struct {
	delay    time.Duration
	response *ModelResponse
}

func (c *delayedModelClient) Complete(context.Context, ModelRequest) (*ModelResponse, error) {
	time.Sleep(c.delay)
	return c.response, nil
}

type durationBlockingModelClient struct {
	started chan struct{}
	mu      sync.Mutex
	calls   int
}

func (c *durationBlockingModelClient) Complete(ctx context.Context, _ ModelRequest) (*ModelResponse, error) {
	c.mu.Lock()
	c.calls++
	if c.calls == 1 {
		close(c.started)
	}
	c.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (c *durationBlockingModelClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}
