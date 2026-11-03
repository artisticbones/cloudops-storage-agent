# Task6 Budget Accounting Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Close Task6's pricing, API-key, audit-accounting, and strict concurrent duration-budget gaps without breaking the legacy single-model path.

**Architecture:** Extend the existing `modelRouter` reservation ledger. Model configuration carries prices into endpoint capabilities; each in-flight call reserves call/input/cost/duration capacity atomically, and completion converts that reservation into actual audit usage before returning any post-call budget error. The legacy path remains unchanged when routing is absent.

**Tech Stack:** Go, standard `context`, `sync`, `time`, existing OpenAI-compatible client and test fakes.

**Spec:** `docs/superpowers/specs/2026-10-30-task6-budget-accounting-design.md`

## Global Constraints

- `MaxDuration = 已累计实际时长 + 所有 in-flight 调用预留时长`; insufficient reservation rejects a new call.
- A successful real call that exceeds output/cost budget must still record real `Calls`, `InputTokens`, and call duration.
- Only configured/environment-provided API keys may be passed to model clients; no default `ollama` key.
- Preserve old single-model compatibility and Task1-5 behavior.
- Finish with `gofmt`, `GOCACHE=/tmp/hello-agent-gocache go vet ./...`, `go test ./... -count=1`, `go test -race ./... -count=1`, and `git diff --check`.

## Review Focus

- Zero-price legacy endpoint: existing model literals remain valid and cost stays zero.
- Keyless Ollama/default config: no implicit API key is generated or passed.
- Successful over-output/cost call: budget error is returned only after real calls/tokens/duration are audited.
- Concurrent duration boundary: a second call is rejected while the first call's reserved duration consumes the remaining budget.
- Failed/retried/fallback calls: real attempts remain counted and duration is not lost.

### Task 1: Configuration and endpoint capability propagation

**Files:**
- Modify: `config/config.go`
- Modify: `config/config_test.go`
- Modify: `main.go`
- Modify: `task6_regression_test.go`

**Interfaces:**
- `config.ModelConfig` produces `InputCostPerToken` and `OutputCostPerToken`.
- `modelEndpoint` copies those values into `ModelCapability`.

- [ ] Write failing tests for configured price propagation and default keyless configuration.
- [ ] Run the focused tests and confirm they fail because prices/default key behavior is missing.
- [ ] Add optional price fields and remove the hard-coded API-key fallback while retaining environment precedence.
- [ ] Copy model prices into `ModelCapability` and ensure `newClientForModel` receives exactly the configured key, including empty.
- [ ] Run the focused tests and confirm they pass.

### Task 2: Real-call audit accounting and cost enforcement

**Files:**
- Modify: `model_router.go`
- Modify: `model_router_test.go`
- Modify: `task6_regression_test.go`

**Interfaces:**
- `budgetReservation` tracks all in-flight reservations, including duration.
- Completion records actual call/input/output/cost/duration before returning a post-call budget error.

- [ ] Write failing tests for successful calls exceeding output/cost budgets and assert `Calls`, `InputTokens`, and duration remain recorded.
- [ ] Run the focused tests and confirm they fail because `commit` currently drops the audit record.
- [ ] Refactor reserve/complete bookkeeping so every actual endpoint invocation is finalized exactly once and output/cost checks happen after audit recording.
- [ ] Keep failed attempts and fallback attempts counted as actual calls, preserving existing retry/fallback expectations.
- [ ] Run router and regression tests and confirm they pass.

### Task 3: Strict concurrent duration reservation

**Files:**
- Modify: `model_router.go`
- Modify: `model_router_test.go`

**Interfaces:**
- Duration reservation is checked atomically with other budget reservations.
- On completion, reserved duration is replaced by measured duration and the cumulative snapshot is updated.

- [ ] Write a deterministic concurrent boundary test using an injected clock and blocking fake client; assert the second call is rejected before provider invocation.
- [ ] Run the test and confirm it fails because duration is currently only accumulated after completion.
- [ ] Implement atomic duration reservation and completion conversion, including context deadline handling for the fixed budget window.
- [ ] Run the focused concurrency test and `go test -race` for the package.

### Task 4: Full formatting and verification

**Files:**
- Modify: all touched Go files as needed for formatting.

- [ ] Run `gofmt` on touched Go files.
- [ ] Run `GOCACHE=/tmp/hello-agent-gocache go vet ./...`.
- [ ] Run `go test ./... -count=1`.
- [ ] Run `go test -race ./... -count=1`.
- [ ] Run `git diff --check` and inspect the final diff for scope and compatibility.
