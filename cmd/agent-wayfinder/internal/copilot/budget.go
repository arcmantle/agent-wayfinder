package copilot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
)

var (
	ErrPromptBudgetExceeded   = errors.New("Copilot prompt exceeds token budget")
	ErrResponseBudgetExceeded = errors.New("Copilot response exceeds token budget")
	ErrPlannerTimeout         = errors.New("Copilot planner timed out")
)

func ReadResponse(ctx context.Context, prompt string, tokenBudget int, response io.ReadCloser) ([]byte, error) {
	promptTokens := EstimateTokens(prompt)
	if promptTokens >= tokenBudget {
		return nil, ErrPromptBudgetExceeded
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = response.Close()
		case <-done:
		}
	}()
	defer close(done)
	defer response.Close()

	remainingTokens := tokenBudget - promptTokens
	contents := make([]byte, 0, remainingTokens)
	buffer := make([]byte, 1024)
	for {
		count, err := response.Read(buffer)
		if count > remainingTokens {
			return nil, ErrResponseBudgetExceeded
		}
		if count > 0 {
			contents = append(contents, buffer[:count]...)
			remainingTokens -= count
		}
		if err == io.EOF {
			return contents, nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("%w: %w", ErrPlannerTimeout, ctx.Err())
			}
			return nil, fmt.Errorf("read Copilot planner response: %w", err)
		}
	}
}

func EstimateTokens(text string) int {
	return len(text)
}

func Prompt(question string) string {
	return "Return only one JSON object in this shape: {\"schemaVersion\":1,\"intent\":\"intent\",\"entities\":[\"entity\"]}. " +
		"Intent must be one of lookup, explain, calls, called_by, dependencies, dependents, path, reachability, shared_contract, or impact. " +
		"Use one entity for lookup, explain, calls, called_by, dependencies, dependents, or impact. " +
		"Use two entities for path, reachability, or shared_contract. " +
		"Do not use tools or add prose.\nQuestion: " + strconv.Quote(question)
}
