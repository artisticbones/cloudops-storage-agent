package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

const expressionCalculatorToolName = "expression_calculator"

type expressionCalculatorArgs struct {
	Expression string `json:"expression"`
}

// expressionCalculatorTool keeps expression parsing deterministic. The model
// supplies the expression text; Go, not the model, applies precedence rules.
func expressionCalculatorTool() Tool {
	return &functionTool{
		definition: ToolDefinition{
			Name:        expressionCalculatorToolName,
			Description: "计算包含 +、-、*、/ 和括号的数学表达式。必须把用户的原始表达式完整放入 expression，不要自行拆分或改变运算符；程序会确定性地处理运算符优先级。",
			Strict:      true,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"expression": map[string]any{
						"type":        "string",
						"description": "用户原始数学表达式，例如 123 + 23 / 23；不要在末尾添加运算符。",
					},
				},
				"required":             []string{"expression"},
				"additionalProperties": false,
			},
		},
		handler: executeExpressionCalculator,
	}
}

func executeExpressionCalculator(ctx context.Context, rawArgs string) (string, error) {
	if err := contextErr(ctx); err != nil {
		return "", err
	}
	var args expressionCalculatorArgs
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", &AgentError{Kind: InvalidArgument, Message: "invalid expression calculator arguments", Err: err}
	}
	if strings.TrimSpace(args.Expression) == "" {
		return "", &AgentError{Kind: InvalidArgument, Message: "expression cannot be empty"}
	}

	result, err := evaluateExpression(ctx, args.Expression)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("expression=%s, result=%g", args.Expression, result), nil
}

type expressionParser struct {
	ctx context.Context
	s   string
	pos int
}

func evaluateExpression(ctx context.Context, expression string) (float64, error) {
	p := &expressionParser{ctx: ctx, s: expression}
	value, err := p.parseExpression()
	if err != nil {
		return 0, err
	}
	p.skipSpaces()
	if p.pos != len(p.s) {
		return 0, p.invalid("unexpected character")
	}
	return value, nil
}

func (p *expressionParser) parseExpression() (float64, error) {
	if err := contextErr(p.ctx); err != nil {
		return 0, err
	}
	left, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpaces()
		if p.pos >= len(p.s) || (p.s[p.pos] != '+' && p.s[p.pos] != '-') {
			return left, nil
		}
		op := p.s[p.pos]
		p.pos++
		right, err := p.parseTerm()
		if err != nil {
			return 0, err
		}
		if op == '+' {
			left += right
		} else {
			left -= right
		}
	}
}

func (p *expressionParser) parseTerm() (float64, error) {
	left, err := p.parseFactor()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpaces()
		if p.pos >= len(p.s) || (p.s[p.pos] != '*' && p.s[p.pos] != '/') {
			return left, nil
		}
		op := p.s[p.pos]
		p.pos++
		right, err := p.parseFactor()
		if err != nil {
			return 0, err
		}
		if op == '*' {
			left *= right
			continue
		}
		if right == 0 {
			return 0, &AgentError{Kind: BusinessError, Message: "division by zero"}
		}
		left /= right
	}
}

func (p *expressionParser) parseFactor() (float64, error) {
	if err := contextErr(p.ctx); err != nil {
		return 0, err
	}
	p.skipSpaces()
	if p.pos >= len(p.s) {
		return 0, p.invalid("expected number or parentheses")
	}

	if p.s[p.pos] == '+' || p.s[p.pos] == '-' {
		sign := p.s[p.pos]
		p.pos++
		value, err := p.parseFactor()
		if err != nil {
			return 0, err
		}
		if sign == '-' {
			return -value, nil
		}
		return value, nil
	}

	if p.s[p.pos] == '(' {
		p.pos++
		value, err := p.parseExpression()
		if err != nil {
			return 0, err
		}
		p.skipSpaces()
		if p.pos >= len(p.s) || p.s[p.pos] != ')' {
			return 0, p.invalid("missing closing parenthesis")
		}
		p.pos++
		return value, nil
	}

	start := p.pos
	for p.pos < len(p.s) && (unicode.IsDigit(rune(p.s[p.pos])) || p.s[p.pos] == '.') {
		p.pos++
	}
	if start == p.pos {
		return 0, p.invalid("expected number")
	}
	value, err := strconv.ParseFloat(p.s[start:p.pos], 64)
	if err != nil {
		return 0, p.invalid("invalid number")
	}
	return value, nil
}

func (p *expressionParser) skipSpaces() {
	for p.pos < len(p.s) && unicode.IsSpace(rune(p.s[p.pos])) {
		p.pos++
	}
}

func (p *expressionParser) invalid(message string) error {
	return &AgentError{Kind: InvalidArgument, Message: fmt.Sprintf("invalid expression at position %d: %s", p.pos, message)}
}
