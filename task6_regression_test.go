package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/artisticbones/hello-agent/config"
)

func TestClassifyRuntimeErrorPreservesTypedRuntimeErrors(t *testing.T) {
	want := &AgentError{Kind: QuotaError, Message: "quota exhausted"}
	got := classifyRuntimeError(context.Background(), want)
	if got != want || got.Kind != QuotaError {
		t.Fatalf("classifyRuntimeError() = %#v, want original quota error", got)
	}
}

func TestRouterFailureReservationDoesNotBlockFallback(t *testing.T) {
	primary := NewFakeModelClient(NewFakeModelErrorResult(NewModelHTTPError(503, errors.New("down"))))
	fallback := NewFakeModelClient(NewFakeTextResult("fallback"))
	router := NewModelRouter(ModelRouterOptions{
		Primary:  ModelEndpoint{Name: "primary", Client: primary},
		Fallback: ModelEndpoint{Name: "fallback", Client: fallback},
		Budget:   ModelBudget{MaxCalls: 2, MaxInputTokens: 100},
	})
	client, _, err := router.Select(context.Background(), testModelRequest())
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Complete(context.Background(), testModelRequest())
	if err != nil || response.Content != "fallback" {
		t.Fatalf("Complete() response=%+v error=%v", response, err)
	}
	if primary.CallCount != 1 || fallback.CallCount != 1 {
		t.Fatalf("calls primary=%d fallback=%d, want one each", primary.CallCount, fallback.CallCount)
	}
	snapshot := router.(*modelRouter).BudgetSnapshot()
	if snapshot.Calls != 2 || snapshot.InputTokens == 0 {
		t.Fatalf("snapshot = %+v, want both actual calls charged", snapshot)
	}
}

func TestAgentDoesNotRetryRouterCompletionAgain(t *testing.T) {
	primary := NewFakeModelClient(
		NewFakeModelErrorResult(NewModelHTTPError(503, errors.New("down"))),
		NewFakeTextResult("ok"),
	)
	router := NewModelRouter(ModelRouterOptions{
		Primary:    ModelEndpoint{Name: "primary", Client: primary},
		MaxRetries: 1,
	})
	cfg := &config.Config{
		MaxSteps: 1, MaxRetries: 5, RequestTimeout: time.Second,
		Model: &config.ModelConfig{Name: "legacy", BaseURL: "fake://legacy"},
		Tool:  &config.ToolConfig{},
	}
	agent, err := NewAgentWithRouter(router, cfg, NewToolRegistry())
	if err != nil {
		t.Fatal(err)
	}
	answer, err := agent.Run(context.Background(), "hello")
	if err != nil || answer != "ok" {
		t.Fatalf("Run() answer=%q error=%v", answer, err)
	}
	if primary.CallCount != 2 {
		t.Fatalf("primary calls = %d, want router's one retry only", primary.CallCount)
	}
}

func TestAgentCancelsInFlightFakeModelCall(t *testing.T) {
	client := NewFakeModelClient(FakeModelResult{BlockUntilContext: true})
	cfg := &config.Config{
		MaxSteps: 1, MaxRetries: 0, RequestTimeout: 30 * time.Millisecond,
		Model: &config.ModelConfig{Name: "fake", BaseURL: "fake://model"},
		Tool:  &config.ToolConfig{},
	}
	agent, err := NewAgent(client, cfg, NewToolRegistry())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = agent.Run(context.Background(), "hello")
	if err == nil {
		t.Fatal("Run() error = nil, want timeout")
	}
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != TimeoutError {
		t.Fatalf("error = %v, want timeout AgentError", err)
	}
	if !client.ContextCancelled {
		t.Fatal("fake model did not observe in-flight cancellation")
	}
	if time.Since(started) > time.Second {
		t.Fatal("in-flight cancellation took too long")
	}
}

func TestBuildModelRouterUsesConfiguredPrimaryFallbackAndOllama(t *testing.T) {
	cfg := &config.Config{
		Model: &config.ModelConfig{Name: "legacy", BaseURL: "http://legacy/v1", APIKey: "legacy-key"},
		Routing: &config.RoutingConfig{
			Primary:  &config.ModelConfig{Name: "primary", BaseURL: "http://primary/v1", APIKey: "primary-key"},
			Fallback: &config.ModelConfig{Name: "backup", BaseURL: "http://backup/v1", APIKey: "backup-key"},
			Ollama:   &config.ModelConfig{Name: "local", BaseURL: "http://ollama/v1", APIKey: "ollama-key", Local: true},
		},
	}

	router := buildModelRouter(cfg)
	implementation, ok := router.(*modelRouter)
	if !ok {
		t.Fatalf("buildModelRouter() type = %T, want *modelRouter", router)
	}
	if len(implementation.endpoints) != 3 {
		t.Fatalf("configured endpoints = %d, want 3", len(implementation.endpoints))
	}
	wantNames := []string{"primary", "backup", "local"}
	for i, want := range wantNames {
		if got := implementation.endpoints[i].Name; got != want {
			t.Errorf("endpoint[%d] name = %q, want %q", i, got, want)
		}
	}
}

func TestBuildModelRouterPropagatesModelPricing(t *testing.T) {
	cfg := &config.Config{
		Model: &config.ModelConfig{Name: "legacy", BaseURL: "http://legacy/v1"},
		Routing: &config.RoutingConfig{
			Primary: &config.ModelConfig{
				Name: "priced", BaseURL: "http://priced/v1",
				InputCostPerToken:  0.000002,
				OutputCostPerToken: 0.000003,
			},
		},
	}

	router := buildModelRouter(cfg).(*modelRouter)
	capability := router.endpoints[0].Capability
	if capability.InputCostPerToken != 0.000002 || capability.OutputCostPerToken != 0.000003 {
		t.Fatalf("capability pricing = %+v", capability)
	}
}

func TestBuildModelRouterKeepsLegacyPathWhenRoutingIsAbsent(t *testing.T) {
	cfg := &config.Config{
		Model: &config.ModelConfig{Name: "legacy", BaseURL: "http://legacy/v1", APIKey: "legacy-key"},
	}
	if router := buildModelRouter(cfg); router != nil {
		t.Fatalf("buildModelRouter() = %T, want nil without routing", router)
	}
}
