package main

import (
	"fmt"
)

type ErrorKind string

const (
	InvalidArgument ErrorKind = "invalid_argument"
	BusinessError   ErrorKind = "business_error"
	ModelError      ErrorKind = "model_error"
	NetworkError    ErrorKind = "network_error"
	TimeoutError    ErrorKind = "timeout"
	RateLimitError  ErrorKind = "rate_limit"
	QuotaError      ErrorKind = "quota_exhausted"
	BudgetError     ErrorKind = "budget_exceeded"
)

type AgentError struct {
	Kind    ErrorKind
	Message string
	Err     error
}

func (e *AgentError) Error() string {
	if e.Err != nil {
		return string(e.Kind) + ": " + e.Message + ": " + e.Err.Error()
	}
	return string(e.Kind) + ": " + e.Message
}

func (e *AgentError) Unwrap() error {
	return e.Err
}

// ModelHTTPError preserves an HTTP status returned by an OpenAI-compatible
// adapter without making the Agent depend on a particular SDK error type.
type ModelHTTPError struct {
	StatusCode int
	Err        error
}

func NewModelHTTPError(statusCode int, err error) *ModelHTTPError {
	return &ModelHTTPError{StatusCode: statusCode, Err: err}
}

func (e *ModelHTTPError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("model HTTP status %d", e.StatusCode)
	}
	return fmt.Sprintf("model HTTP status %d: %v", e.StatusCode, e.Err)
}

func (e *ModelHTTPError) Unwrap() error { return e.Err }

type modelQuotaError struct{ message string }

func (e *modelQuotaError) Error() string { return e.message }

func NewQuotaError(message string) error { return &modelQuotaError{message: message} }
