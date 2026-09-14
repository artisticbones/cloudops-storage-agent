package main

type ErrorKind string

const (
	InvalidArgument ErrorKind = "invalid_argument"
	BusinessError   ErrorKind = "business_error"
	ModelError      ErrorKind = "model_error"
	NetworkError    ErrorKind = "network_error"
	TimeoutError    ErrorKind = "timeout"
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
