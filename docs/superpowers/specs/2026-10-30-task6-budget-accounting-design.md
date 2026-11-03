# Task6 Budget Accounting Design

## Intent

Close the remaining Task6 specification gaps while preserving the legacy single-model path and Task1-5 behavior.

## Goals

- Carry per-model input/output token prices from `config.ModelConfig` into `ModelCapability`; enforce `MaxCost` using both input and output usage.
- Never synthesize an API key. A provider key is sent only when it is present in configuration or the environment; an unconfigured Ollama endpoint remains keyless.
- Record every real model invocation in the budget snapshot, including successful calls whose output/cost exceeds the configured budget.
- Enforce `MaxDuration` as a strict cumulative reservation: `actual duration + all in-flight duration reservations` must fit before a new call starts. On completion, the reservation is replaced by measured duration.

## Non-goals

- No change to the provider client abstraction or the legacy `NewAgent` single-client constructor.
- No new retry, fallback, or pricing policy beyond the configured values.

## Approach

Extend the existing router reservation ledger. A reservation contains the estimated input tokens/cost and a duration reservation. Before an endpoint call, the router atomically checks committed usage plus all reservations against call, input, cost, and duration limits. Completion atomically removes the reservation and records actual call/input/output/cost/duration; a post-call output/cost violation returns `BudgetError` after recording the call. Failed real attempts also retain call/input/duration audit data. The duration reservation is the remaining budget to the router's fixed budget deadline, so concurrent calls cannot collectively exceed `MaxDuration`.

## Constraints and compatibility

- Keep old `ModelConfig` literals valid by adding optional numeric fields with zero-value pricing.
- Keep default model configuration keyless (`APIKey == ""`) unless `AGENT_API_KEY` is set.
- Preserve endpoint ordering and legacy routing absence behavior.
- Add focused unit tests for pricing propagation, keyless defaults, post-call audit retention, and concurrent duration reservation.
