# Task9 Validation Gaps Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Close Task9 validation gaps without changing Task1–8 contracts: make eval citations retrieval-backed, enforce executable eval budgets, prove security refusals, and redact sensitive data at persistence boundaries.

**Architecture:** Extend the existing `EvalCase`/`EvalRunner` and `DefaultContextBuilder` path rather than creating a second evaluation runtime. Add a persistence sanitization pass that uses registered `ToolSpec` metadata before updating `Session` or saving `Checkpoint`; keep Trace redaction in place and make restored placeholders safe model-visible content.

**Tech Stack:** Go, standard library, existing Fake Model/Tool, Memory Retriever/CheckpointStore, `go test`, `go vet`, race detector.

**Spec:** `docs/data-governance.md` and the confirmed Task9 request.

## Global Constraints

- Preserve Task1–8 behavior and public constructors unless an additive field/helper is required.
- Eval cases use deterministic fakes and must not call network services or use production secrets.
- Sensitive tool arguments/results must not enter Session, Checkpoint, or Trace; non-sensitive tool data remains unchanged.
- Run `gofmt`, `GOCACHE=/tmp/hello-agent-gocache go vet ./...`, `go test ./... -count=1`, `go test -race ./... -count=1`, and `git diff --check` before completion.

## Review Focus

- A citation must be sourced from the Retriever context, not merely appear in a hard-coded expected answer; test Fake Model requests for Source IDs.
- Tenant/version must reach retrieval and select only matching documents; test the request context includes the selected source.
- MaxCalls, output tokens, and cost must fail with clear budget results at the EvalRunner boundary while Router regression remains a lower-level routing test.
- High-risk refusal must leave handler `CallCount` at zero and emit a refusal event in Trace.
- Sensitive parameters, full sensitive tools, and sensitive tool results must be redacted in Session/Checkpoint/Trace while safe placeholders remain usable after restore.

### Task 1: Retrieval-backed eval citations and budget assertions

**Files:** `eval.go`, `eval_test.go`, `fake_model_client.go` or test helpers, `docs/evaluation.md`

- [ ] Add failing cases with `Retriever`, tenant/version, expected Source, and a Fake Model request assertion; add executable MaxCalls/output/cost over-budget cases and clear `EvalResult` budget fields.
- [ ] Run focused eval tests and confirm they fail because the current runner neither injects retrieval nor checks those budgets/requests.
- [ ] Extend `EvalCase`/`EvalRunner` additively: build context with retrieval scope, assert source presence in received model requests, and expose/validate budget errors without replacing Router tests.
- [ ] Run focused eval/retrieval/router tests and refactor only after green.

### Task 2: Security refusal and persistence-boundary redaction

**Files:** `eval.go`, `eval_test.go`, `agent.go`, `checkpoint.go`, `session_store.go`, `tool_policy_test.go`, `session_test.go`

- [ ] Add failing assertions for denied handler `CallCount == 0`, refusal Trace event, and raw secret absence from agent Session, stored Checkpoint, and Trace; include restored placeholder and unchanged non-sensitive result cases.
- [ ] Run focused tests to confirm the current persistence path retains raw tool content and eval does not require zero handler calls.
- [ ] Implement a metadata-aware redaction helper for conversation messages and completed tool results, apply it before Session/checkpoint persistence, and ensure restored redacted tool content is passed back as a safe placeholder; preserve existing Trace sanitization and non-sensitive values.
- [ ] Run focused security/session tests and then the full suite.

### Task 3: Governance documentation and final verification

**Files:** `docs/data-governance.md`, `docs/evaluation.md`

- [ ] Update documentation to distinguish implemented in-memory behavior from remaining tenant ACL, durable storage, backup/restore, and real-model evaluation limitations; accurately define EvalRunner versus Router regression scope.
- [ ] Run formatting, vet, normal tests, race tests, and diff checks; inspect the final diff for Task1–8 regressions.

