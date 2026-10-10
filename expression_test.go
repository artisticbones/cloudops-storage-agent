package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestEvaluateExpressionRespectsPrecedence(t *testing.T) {
	tests := []struct {
		expression string
		want       float64
	}{
		{expression: "123 + 23 / 23", want: 124},
		{expression: "2 + 3 * 4", want: 14},
		{expression: "(2 + 3) * 4", want: 20},
		{expression: "-2 * (3 + 4)", want: -14},
	}

	for _, tt := range tests {
		t.Run(tt.expression, func(t *testing.T) {
			got, err := evaluateExpression(context.Background(), tt.expression)
			if err != nil {
				t.Fatalf("evaluateExpression() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("result = %g, want %g", got, tt.want)
			}
		})
	}
}

func TestExecuteExpressionCalculator(t *testing.T) {
	result, err := executeExpressionCalculator(context.Background(), `{"expression":"123 + 23 / 23"}`)
	if err != nil {
		t.Fatalf("executeExpressionCalculator() error = %v", err)
	}
	if !strings.Contains(result, "result=124") {
		t.Fatalf("result = %q, want result=124", result)
	}
}

func TestEvaluateExpressionRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "division by zero", input: "1 / 0", want: "division by zero"},
		{name: "trailing operator", input: "1 +", want: "expected number"},
		{name: "missing parenthesis", input: "(1 + 2", want: "missing closing parenthesis"},
		{name: "unexpected character", input: "1 & 2", want: "unexpected character"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := evaluateExpression(context.Background(), tt.input)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestEvaluateExpressionHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := evaluateExpression(ctx, "1 + 2")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
