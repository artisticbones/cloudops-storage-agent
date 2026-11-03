package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Identity identifies the caller and the runtime scope of a Tool call.
// UserID is intentionally optional until authentication is introduced by a
// later service layer; SessionID and RunID are always supplied by Agent.
type Identity struct {
	UserID    string   `json:"userId,omitempty"`
	SessionID string   `json:"sessionId,omitempty"`
	RunID     string   `json:"runId,omitempty"`
	Roles     []string `json:"roles,omitempty"`
}

type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

// ToolSpec adds runtime safety metadata without changing Tool or
// ToolDefinition. Validate is deliberately a second, runtime-side check and
// should reject scope violations that a model-facing JSON schema cannot see.
type ToolSpec struct {
	Name                string
	Definition          ToolDefinition
	Permissions         []string
	Risk                RiskLevel
	SideEffect          bool
	Sensitive           bool
	SensitiveParameters []string
	Validate            func(json.RawMessage) error
}

// ToolSpecProvider is optional so existing Tool implementations remain
// source-compatible. Tools without a provider receive a safe, read-only
// default spec derived from their existing ToolDefinition.
type ToolSpecProvider interface {
	ToolSpec() ToolSpec
}

type ToolPolicy interface {
	Authorize(context.Context, Identity, ToolSpec, json.RawMessage) error
}

type ToolPolicyDecision string

const (
	ToolPolicyAllowed         ToolPolicyDecision = "allowed"
	ToolPolicyPendingApproval ToolPolicyDecision = "pending_approval"
	ToolPolicyDenied          ToolPolicyDecision = "denied"
)

var (
	ErrToolUnauthorized     = errors.New("tool unauthorized")
	ErrToolApprovalRequired = errors.New("tool approval required")
)

// ToolPolicyError lets the runtime distinguish a denial from a request that
// needs human approval while preserving errors.Is compatibility for callers.
type ToolPolicyError struct {
	Decision ToolPolicyDecision
	Message  string
	Err      error
}

func (e *ToolPolicyError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return string(e.Decision)
}

func (e *ToolPolicyError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// AllowAllToolPolicy preserves the pre-Task-3 behavior for ordinary tools,
// but still requires approval for a tool explicitly marked high risk.
type AllowAllToolPolicy struct{}

var _ ToolPolicy = (*AllowAllToolPolicy)(nil)

func (p *AllowAllToolPolicy) Authorize(_ context.Context, _ Identity, spec ToolSpec, args json.RawMessage) error {
	if effectiveToolRisk(spec, args) == RiskHigh {
		return &ToolPolicyError{
			Decision: ToolPolicyPendingApproval,
			Message:  fmt.Sprintf("tool %q is pending approval", toolSpecName(spec)),
			Err:      ErrToolApprovalRequired,
		}
	}
	return nil
}

func toolSpecFor(tool Tool) ToolSpec {
	definition := tool.Definition()
	spec := ToolSpec{
		Name:       definition.Name,
		Definition: definition,
		Risk:       RiskLow,
	}
	if provider, ok := tool.(ToolSpecProvider); ok {
		provided := provider.ToolSpec()
		if provided.Name != "" {
			spec.Name = provided.Name
		}
		if provided.Definition.Name != "" || provided.Definition.Parameters != nil {
			spec.Definition = provided.Definition
		}
		spec.Permissions = append([]string(nil), provided.Permissions...)
		spec.Risk = provided.Risk
		spec.SideEffect = provided.SideEffect
		spec.Sensitive = provided.Sensitive
		spec.SensitiveParameters = append([]string(nil), provided.SensitiveParameters...)
		spec.Validate = provided.Validate
	}
	if spec.Name == "" {
		spec.Name = spec.Definition.Name
	}
	if spec.Definition.Name == "" {
		spec.Definition = definition
	}
	if spec.Risk == "" {
		spec.Risk = RiskLow
	}
	return spec
}

func toolSpecName(spec ToolSpec) string {
	if spec.Name != "" {
		return spec.Name
	}
	return spec.Definition.Name
}

func effectiveToolRisk(spec ToolSpec, args json.RawMessage) RiskLevel {
	if spec.Risk == "" {
		return RiskLow
	}
	return spec.Risk
}

type ToolExecutionStatus string

const (
	ToolExecuted        ToolExecutionStatus = "executed"
	ToolReused          ToolExecutionStatus = "reused"
	ToolPendingApproval ToolExecutionStatus = "pending_approval"
	ToolDenied          ToolExecutionStatus = "denied"
)

type toolExecution struct {
	Content        string
	Status         ToolExecutionStatus
	IdempotencyKey string
}

type preparedToolCall struct {
	tool           Tool
	spec           ToolSpec
	args           json.RawMessage
	identity       Identity
	checkpointKey  string
	idempotencyKey string
}

func prepareToolCall(ctx context.Context, registry ToolRegistry, policy ToolPolicy, identity Identity, call ToolCall) (preparedToolCall, toolExecution, error) {
	tool, ok := registry.Find(call.Name)
	if !ok {
		return preparedToolCall{}, toolExecution{}, &AgentError{Kind: ModelError, Message: "unknown tool: " + call.Name}
	}
	spec := toolSpecFor(tool)
	args := json.RawMessage(call.Arguments)
	if err := validateToolSpecArguments(spec, args); err != nil {
		return preparedToolCall{}, toolExecution{}, err
	}
	if policy == nil {
		policy = &AllowAllToolPolicy{}
	}
	if err := policy.Authorize(ctx, identity, spec, args); err != nil {
		if outcome, handled := policyOutcome(err, spec); handled {
			return preparedToolCall{}, outcome, nil
		}
		return preparedToolCall{}, toolExecution{}, sanitizeToolError(err, spec, args)
	}
	// There is no implicit approval channel in this runtime. A custom policy
	// may grant ordinary access, but it cannot turn a high-risk model request
	// into an immediately executable side effect.
	if effectiveToolRisk(spec, args) == RiskHigh {
		return preparedToolCall{}, toolExecution{
			Content: fmt.Sprintf("工具 %q 待审批，未执行", toolSpecName(spec)),
			Status:  ToolPendingApproval,
		}, nil
	}
	prepared := preparedToolCall{
		tool:          tool,
		spec:          spec,
		args:          args,
		identity:      identity,
		checkpointKey: checkpointToolKey(identity, spec, call),
	}
	if spec.SideEffect {
		prepared.idempotencyKey = stableIdempotencyKey(identity, spec, call)
	}
	return prepared, toolExecution{}, nil
}

func dispatchToolWithPolicy(ctx context.Context, registry ToolRegistry, policy ToolPolicy, store IdempotencyStore, identity Identity, call ToolCall) (toolExecution, error) {
	prepared, outcome, err := prepareToolCall(ctx, registry, policy, identity, call)
	if err != nil || outcome.Status != "" {
		return outcome, err
	}
	return executePreparedToolCall(ctx, prepared, store)
}

func executePreparedToolCall(ctx context.Context, prepared preparedToolCall, store IdempotencyStore) (toolExecution, error) {
	if !prepared.spec.SideEffect {
		result, err := prepared.tool.Execute(ctx, prepared.args)
		if err != nil {
			return toolExecution{}, sanitizeToolError(err, prepared.spec, prepared.args)
		}
		return toolExecution{Content: result.Content, Status: ToolExecuted}, nil
	}
	if store == nil {
		return toolExecution{}, fmt.Errorf("idempotency store is not configured")
	}
	execute := func(callCtx context.Context) (string, error) {
		result, err := prepared.tool.Execute(callCtx, prepared.args)
		if err != nil {
			return "", sanitizeToolError(err, prepared.spec, prepared.args)
		}
		return result.Content, nil
	}
	if executor, ok := store.(IdempotencyExecutor); ok {
		value, reused, err := executor.ExecuteOnce(ctx, prepared.idempotencyKey, execute)
		if err != nil {
			return toolExecution{}, err
		}
		status := ToolExecuted
		if reused {
			status = ToolReused
		}
		return toolExecution{Content: value, Status: status, IdempotencyKey: prepared.idempotencyKey}, nil
	}

	// Preserve compatibility with Get/Put-only stores while serializing the
	// complete check/execute/complete sequence for all Agents in this process.
	unlock := lockFallbackIdempotencyKey(prepared.idempotencyKey)
	defer unlock()
	if saved, found, err := store.Get(ctx, prepared.idempotencyKey); err != nil {
		return toolExecution{}, fmt.Errorf("get idempotent tool result: %w", err)
	} else if found {
		return toolExecution{Content: saved, Status: ToolReused, IdempotencyKey: prepared.idempotencyKey}, nil
	}
	value, err := execute(ctx)
	if err != nil {
		return toolExecution{}, err
	}
	if err := store.Put(ctx, prepared.idempotencyKey, value); err != nil {
		return toolExecution{}, fmt.Errorf("store idempotent tool result: %w", err)
	}
	return toolExecution{Content: value, Status: ToolExecuted, IdempotencyKey: prepared.idempotencyKey}, nil
}

func validateToolSpecArguments(spec ToolSpec, rawArgs json.RawMessage) error {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(rawArgs)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return &AgentError{Kind: InvalidArgument, Message: "invalid tool arguments", Err: err}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return &AgentError{Kind: InvalidArgument, Message: "tool arguments must contain one JSON value"}
	}
	if object, ok := value.(map[string]any); !ok || object == nil {
		return &AgentError{Kind: InvalidArgument, Message: "tool arguments must be a JSON object"}
	}
	if err := validateJSONSchema(spec.Definition.Parameters, value); err != nil {
		return &AgentError{Kind: InvalidArgument, Message: "tool arguments do not match tool schema", Err: err}
	}
	if spec.Validate != nil {
		if err := spec.Validate(rawArgs); err != nil {
			return sanitizeToolError(err, spec, rawArgs)
		}
	}
	return nil
}

func validateJSONSchema(schema map[string]any, value any) error {
	if len(schema) == 0 {
		return nil
	}
	if enum, ok := schema["enum"]; ok && !jsonSchemaEnumContains(enum, value) {
		return fmt.Errorf("value is not one of the allowed enum values")
	}
	if typeName, ok := schema["type"].(string); ok {
		if !jsonTypeMatches(typeName, value) {
			return fmt.Errorf("expected %s", typeName)
		}
	}
	object, isObject := value.(map[string]any)
	if isObject {
		switch required := schema["required"].(type) {
		case []string:
			for _, name := range required {
				if _, exists := object[name]; !exists {
					return fmt.Errorf("missing required property %q", name)
				}
			}
		case []any:
			for _, item := range required {
				name, ok := item.(string)
				if ok {
					if _, exists := object[name]; !exists {
						return fmt.Errorf("missing required property %q", name)
					}
				}
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		if additional, ok := schema["additionalProperties"].(bool); ok && !additional {
			for name := range object {
				if _, exists := properties[name]; !exists {
					return fmt.Errorf("additional property %q is not allowed", name)
				}
			}
		}
		for name, propertySchema := range properties {
			property, exists := object[name]
			if !exists {
				continue
			}
			propertyMap, ok := propertySchema.(map[string]any)
			if ok {
				if err := validateJSONSchema(propertyMap, property); err != nil {
					return fmt.Errorf("property %q: %w", name, err)
				}
			}
		}
	}
	return nil
}

func jsonSchemaEnumContains(enum, value any) bool {
	switch values := enum.(type) {
	case []any:
		for _, candidate := range values {
			if jsonSchemaValuesEqual(candidate, value) {
				return true
			}
		}
	case []string:
		for _, candidate := range values {
			if candidate == value {
				return true
			}
		}
	}
	return false
}

func jsonSchemaValuesEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return string(leftJSON) == string(rightJSON)
}

func jsonTypeMatches(typeName string, value any) bool {
	switch typeName {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		return !strings.Contains(number.String(), ".")
	case "boolean":
		_, ok := value.(bool)
		return ok
	default:
		return true
	}
}

func stableIdempotencyKey(identity Identity, spec ToolSpec, call ToolCall) string {
	return fmt.Sprintf("tool:v2:session=%s:run=%s:spec=%s:args=%s", identity.SessionID, identity.RunID, toolSpecHash(spec), argumentHash(call.Arguments))
}

func checkpointToolKey(identity Identity, spec ToolSpec, call ToolCall) string {
	return "checkpoint:" + stableIdempotencyKey(identity, spec, call)
}

func toolSpecHash(spec ToolSpec) string {
	permissions := append([]string(nil), spec.Permissions...)
	sensitiveParameters := append([]string(nil), spec.SensitiveParameters...)
	sort.Strings(permissions)
	sort.Strings(sensitiveParameters)
	semantic := map[string]any{
		"name":                toolSpecName(spec),
		"definition":          spec.Definition,
		"permissions":         permissions,
		"risk":                spec.Risk,
		"sideEffect":          spec.SideEffect,
		"sensitive":           spec.Sensitive,
		"sensitiveParameters": sensitiveParameters,
	}
	encoded, _ := json.Marshal(semantic)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func argumentHash(raw string) string {
	digest := sha256.Sum256([]byte(canonicalJSON(raw)))
	return hex.EncodeToString(digest[:])
}

func canonicalJSON(raw string) string {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return raw
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return string(canonical)
}

func summarizeToolArguments(spec ToolSpec, raw string) string {
	if !spec.Sensitive && len(spec.SensitiveParameters) == 0 {
		return raw
	}
	digest := sha256.Sum256([]byte(canonicalJSON(raw)))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func sanitizeTraceText(spec ToolSpec, rawArgs, text string) string {
	if spec.Sensitive {
		return "[sensitive tool output omitted]"
	}
	if rawArgs != "" {
		text = strings.ReplaceAll(text, rawArgs, summarizeToolArguments(spec, rawArgs))
	}
	for _, value := range sensitiveArgumentValues(rawArgs, spec.SensitiveParameters) {
		text = strings.ReplaceAll(text, value, "[REDACTED]")
	}
	return text
}

func sanitizeToolError(err error, spec ToolSpec, rawArgs json.RawMessage) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	needsRedaction := spec.Sensitive
	if spec.Sensitive {
		message = fmt.Sprintf("tool %q failed", toolSpecName(spec))
	} else {
		raw := string(rawArgs)
		if raw != "" && strings.Contains(message, raw) {
			message = strings.ReplaceAll(message, raw, summarizeToolArguments(spec, raw))
			needsRedaction = true
		}
		for _, value := range sensitiveArgumentValues(raw, spec.SensitiveParameters) {
			if value != "" && strings.Contains(message, value) {
				message = strings.ReplaceAll(message, value, "[REDACTED]")
				needsRedaction = true
			}
		}
	}
	if !needsRedaction {
		return err
	}
	kind := BusinessError
	var agentErr *AgentError
	if errors.As(err, &agentErr) {
		kind = agentErr.Kind
	}
	return &AgentError{Kind: kind, Message: message}
}

func sensitiveArgumentValues(raw string, names []string) []string {
	if len(names) == 0 {
		return nil
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		return nil
	}
	values := make([]string, 0, len(names))
	for _, name := range names {
		value, ok := object[name]
		if !ok || value == nil {
			continue
		}
		if text, ok := value.(string); ok {
			values = append(values, text)
		}
		if encoded, err := json.Marshal(value); err == nil {
			values = append(values, string(encoded))
		}
	}
	return values
}

func policyOutcome(err error, spec ToolSpec) (toolExecution, bool) {
	var policyErr *ToolPolicyError
	if !errors.As(err, &policyErr) {
		if errors.Is(err, ErrToolApprovalRequired) {
			policyErr = &ToolPolicyError{Decision: ToolPolicyPendingApproval, Err: err}
		} else if errors.Is(err, ErrToolUnauthorized) {
			policyErr = &ToolPolicyError{Decision: ToolPolicyDenied, Err: err}
		} else {
			return toolExecution{}, false
		}
	}
	name := toolSpecName(spec)
	switch policyErr.Decision {
	case ToolPolicyPendingApproval:
		return toolExecution{Content: fmt.Sprintf("工具 %q 待审批，未执行", name), Status: ToolPendingApproval}, true
	case ToolPolicyDenied:
		return toolExecution{Content: fmt.Sprintf("工具 %q 已拒绝，未执行", name), Status: ToolDenied}, true
	default:
		return toolExecution{}, false
	}
}
