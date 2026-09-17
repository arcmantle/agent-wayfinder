package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
)

var (
	errCopilotPromptBudgetExceeded   = errors.New("Copilot prompt exceeds token budget")
	errCopilotResponseBudgetExceeded = errors.New("Copilot response exceeds token budget")
	errCopilotPlannerTimeout         = errors.New("Copilot planner timed out")
)

func readCopilotPlannerResponse(ctx context.Context, prompt string, tokenBudget int, response io.ReadCloser) ([]byte, error) {
	promptTokens := estimateCopilotTokens(prompt)
	if promptTokens >= tokenBudget {
		return nil, errCopilotPromptBudgetExceeded
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
			return nil, errCopilotResponseBudgetExceeded
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
				return nil, fmt.Errorf("%w: %w", errCopilotPlannerTimeout, ctx.Err())
			}
			return nil, fmt.Errorf("read Copilot planner response: %w", err)
		}
	}
}

func estimateCopilotTokens(text string) int {
	return len(text)
}

func copilotPlannerPrompt(question string) string {
	return "Return one JSON object in this shape: {\"schemaVersion\":1,\"intent\":\"calls\",\"entities\":[\"entity\"]}. " +
		"Do not include source code or explanation.\nQuestion: " + strconv.Quote(question)
}
