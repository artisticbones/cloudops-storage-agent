package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ModelRouter selects an injected model implementation for one model request.
// The returned client may perform bounded retries and fallback when Complete
// is called; Agent remains independent of any provider SDK.
type ModelRouter interface {
	Select(context.Context, ModelRequest) (ModelClient, ModelCapability, error)
}

type ModelCapability struct {
	Name               string
	Provider           string
	Local              bool
	InputCostPerToken  float64
	OutputCostPerToken float64
}

type ModelEndpoint struct {
	Name       string
	Client     ModelClient
	Capability ModelCapability
}

type ModelBudget struct {
	MaxInputTokens  int
	MaxOutputTokens int
	MaxCalls        int
	MaxCost         float64
	MaxDuration     time.Duration
}

type ModelRouterOptions struct {
	Primary      ModelEndpoint
	Fallback     ModelEndpoint
	Ollama       ModelEndpoint
	MaxRetries   int
	RetryBackoff time.Duration
	RetryMaxTime time.Duration
	Budget       ModelBudget
	Now          func() time.Time
}

type ModelBudgetSnapshot struct {
	InputTokens   int
	OutputTokens  int
	Calls         int
	Cost          float64
	TotalDuration time.Duration
}

type modelRouter struct {
	endpoints    []ModelEndpoint
	maxRetries   int
	retryBackoff time.Duration
	retryMaxTime time.Duration
	budget       ModelBudget
	now          func() time.Time

	mu            sync.Mutex
	snapshot      ModelBudgetSnapshot
	reservedCalls int
	reservedInput int
	reservedCost  float64
	reservedTime  time.Duration
	budgetStart   time.Time
}

var _ ModelRouter = (*modelRouter)(nil)

func NewModelRouter(options ModelRouterOptions) ModelRouter {
	endpoints := make([]ModelEndpoint, 0, 3)
	for _, endpoint := range []ModelEndpoint{options.Primary, options.Fallback, options.Ollama} {
		if endpoint.Client != nil {
			if endpoint.Capability.Name == "" {
				endpoint.Capability.Name = endpoint.Name
			}
			if endpoint.Name == "" {
				endpoint.Name = endpoint.Capability.Name
			}
			endpoints = append(endpoints, endpoint)
		}
	}
	if options.MaxRetries < 0 {
		options.MaxRetries = 0
	}
	if options.RetryBackoff < 0 {
		options.RetryBackoff = 0
	}
	return &modelRouter{
		endpoints:    endpoints,
		maxRetries:   options.MaxRetries,
		retryBackoff: options.RetryBackoff,
		retryMaxTime: options.RetryMaxTime,
		budget:       options.Budget,
		now:          chooseClock(options.Now),
	}
}

func chooseClock(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}
	return now
}

func (r *modelRouter) Select(ctx context.Context, request ModelRequest) (ModelClient, ModelCapability, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, ModelCapability{}, &AgentError{Kind: TimeoutError, Message: "model selection context canceled", Err: err}
	}
	if len(r.endpoints) == 0 {
		return nil, ModelCapability{}, &AgentError{Kind: ModelError, Message: "no model endpoint configured"}
	}
	return &routedModelClient{router: r, request: request, start: 0}, r.endpoints[0].Capability, nil
}

func (r *modelRouter) BudgetSnapshot() ModelBudgetSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshot
}

type routedModelClient struct {
	router  *modelRouter
	request ModelRequest
	start   int
}

func (c *routedModelClient) Complete(ctx context.Context, request ModelRequest) (*ModelResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return c.router.complete(ctx, request, c.start)
}

func (r *modelRouter) complete(ctx context.Context, request ModelRequest, start int) (*ModelResponse, error) {
	inputTokens := estimateMessagesTokens(request.Messages)
	if start < 0 || start >= len(r.endpoints) {
		return nil, &AgentError{Kind: ModelError, Message: "invalid model route"}
	}
	callCtx, cancel := r.budgetContext(ctx)
	defer cancel()
	var lastErr *AgentError
	for index := start; index < len(r.endpoints); index++ {
		endpoint := r.endpoints[index]
		attemptStart := r.now()
		for attempt := 0; ; attempt++ {
			if err := callCtx.Err(); err != nil {
				return nil, &AgentError{Kind: TimeoutError, Message: "model context canceled", Err: err}
			}
			reservation, err := r.reserve(inputTokens, endpoint.Capability)
			if err != nil {
				return nil, err
			}
			callStart := r.now()
			response, err := endpoint.Client.Complete(callCtx, request)
			duration := r.now().Sub(callStart)
			if duration < 0 {
				duration = 0
			}
			if err == nil {
				if response == nil {
					r.finalize(reservation, nil, endpoint.Capability, duration)
					return nil, &AgentError{Kind: ModelError, Message: "model response is nil"}
				}
				if budgetErr := r.finalize(reservation, response, endpoint.Capability, duration); budgetErr != nil {
					return nil, budgetErr
				}
				return response, nil
			}
			r.finalize(reservation, nil, endpoint.Capability, duration)
			classified := classifyModelError(callCtx, err)
			lastErr = classified
			if !shouldRetryModel(callCtx, classified, attempt, r.maxRetries, attemptStart, r.retryMaxTime, r.now) {
				if shouldFallbackModel(classified) && index+1 < len(r.endpoints) {
					break
				}
				return nil, classified
			}
			if err := waitRetry(callCtx, r.retryBackoff, attemptStart, r.retryMaxTime, r.now); err != nil {
				return nil, err
			}
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, &AgentError{Kind: ModelError, Message: "all model fallbacks failed"}
}

type budgetReservation struct {
	inputTokens int
	inputCost   float64
	duration    time.Duration
}

// reserve atomically reserves all known pre-call budget dimensions. Duration
// uses the remaining cumulative budget, which is conservative but guarantees
// that concurrent calls cannot collectively exceed MaxDuration.
func (r *modelRouter) reserve(inputTokens int, capability ModelCapability) (budgetReservation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inputCost := float64(inputTokens) * capability.InputCostPerToken
	if r.budget.MaxCalls > 0 && r.snapshot.Calls+r.reservedCalls >= r.budget.MaxCalls {
		return budgetReservation{}, &AgentError{Kind: BudgetError, Message: "model call budget exceeded"}
	}
	if r.budget.MaxInputTokens > 0 && r.snapshot.InputTokens+r.reservedInput+inputTokens > r.budget.MaxInputTokens {
		return budgetReservation{}, &AgentError{Kind: BudgetError, Message: "model input token budget exceeded"}
	}
	if r.budget.MaxCost > 0 && r.snapshot.Cost+r.reservedCost+inputCost > r.budget.MaxCost {
		return budgetReservation{}, &AgentError{Kind: BudgetError, Message: "model cost budget exceeded"}
	}
	var duration time.Duration
	if r.budget.MaxDuration > 0 {
		if r.budgetStart.IsZero() {
			r.budgetStart = r.now()
		}
		remaining := r.budget.MaxDuration - r.snapshot.TotalDuration - r.reservedTime
		if remaining <= 0 {
			return budgetReservation{}, &AgentError{Kind: BudgetError, Message: "model duration budget exceeded"}
		}
		duration = remaining
	}
	r.reservedCalls++
	r.reservedInput += inputTokens
	r.reservedCost += inputCost
	r.reservedTime += duration
	return budgetReservation{inputTokens: inputTokens, inputCost: inputCost, duration: duration}, nil
}

// finalize converts one in-flight reservation into an actual audit record.
// Budget violations are checked only after recording the real call, so a
// provider invocation is never erased from the ledger.
func (r *modelRouter) finalize(reservation budgetReservation, response *ModelResponse, capability ModelCapability, duration time.Duration) error {
	outputTokens := 0
	if response != nil {
		outputTokens = estimateTextTokens(response.Content)
		for _, call := range response.ToolCalls {
			outputTokens += estimateTextTokens(call.Name) + estimateTextTokens(call.Arguments)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cost := float64(outputTokens) * capability.OutputCostPerToken
	r.reservedCalls--
	r.reservedInput -= reservation.inputTokens
	r.reservedCost -= reservation.inputCost
	r.reservedTime -= reservation.duration
	r.snapshot.Calls++
	r.snapshot.InputTokens += reservation.inputTokens
	r.snapshot.OutputTokens += outputTokens
	r.snapshot.Cost += reservation.inputCost + cost
	r.snapshot.TotalDuration += duration
	if response == nil {
		return nil
	}
	if r.budget.MaxOutputTokens > 0 && r.snapshot.OutputTokens > r.budget.MaxOutputTokens {
		return &AgentError{Kind: BudgetError, Message: "model output token budget exceeded"}
	}
	if r.budget.MaxCost > 0 && r.snapshot.Cost > r.budget.MaxCost {
		return &AgentError{Kind: BudgetError, Message: "model cost budget exceeded"}
	}
	return nil
}

func (r *modelRouter) budgetContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if r.budget.MaxDuration <= 0 {
		return ctx, func() {}
	}
	r.mu.Lock()
	if r.budgetStart.IsZero() {
		r.budgetStart = r.now()
	}
	deadline := r.budgetStart.Add(r.budget.MaxDuration)
	r.mu.Unlock()
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, deadline)
}

func classifyModelError(ctx context.Context, err error) *AgentError {
	if ctx != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return &AgentError{Kind: TimeoutError, Message: "model context canceled", Err: ctxErr}
		}
	}
	var agentErr *AgentError
	if errors.As(err, &agentErr) {
		return agentErr
	}
	var httpErr *ModelHTTPError
	if errors.As(err, &httpErr) {
		switch {
		case httpErr.StatusCode == 429:
			return &AgentError{Kind: RateLimitError, Message: "model rate limited", Err: err}
		case httpErr.StatusCode >= 500:
			return &AgentError{Kind: NetworkError, Message: "model server error", Err: err}
		case httpErr.StatusCode >= 400:
			return &AgentError{Kind: InvalidArgument, Message: "model rejected request", Err: err}
		}
	}
	var quotaErr *modelQuotaError
	if errors.As(err, &quotaErr) {
		return &AgentError{Kind: QuotaError, Message: quotaErr.Error(), Err: err}
	}
	return &AgentError{Kind: NetworkError, Message: err.Error(), Err: err}
}

func shouldFallbackModel(err *AgentError) bool {
	return err.Kind == RateLimitError || err.Kind == QuotaError || err.Kind == NetworkError
}

func shouldRetryModel(ctx context.Context, err *AgentError, attempt, maxRetries int, started time.Time, maxTime time.Duration, now func() time.Time) bool {
	if err.Kind != NetworkError || attempt >= maxRetries {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	return maxTime <= 0 || now().Sub(started) < maxTime
}

func waitRetry(ctx context.Context, backoff time.Duration, started time.Time, maxTime time.Duration, now func() time.Time) error {
	if backoff <= 0 {
		return nil
	}
	if maxTime > 0 {
		remaining := maxTime - now().Sub(started)
		if remaining <= 0 {
			return &AgentError{Kind: NetworkError, Message: "model retry time budget exceeded"}
		}
		if backoff > remaining {
			return &AgentError{Kind: NetworkError, Message: "model retry time budget exceeded"}
		}
	}
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return &AgentError{Kind: TimeoutError, Message: "model retry canceled", Err: ctx.Err()}
	}
}
