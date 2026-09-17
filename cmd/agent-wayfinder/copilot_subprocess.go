package main

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
)

type copilotCommandRunner func(context.Context, string, ...string) *exec.Cmd

func runCopilotPlanner(parent context.Context, configuration copilotConfiguration, question string, runner copilotCommandRunner) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, configuration.Timeout)
	defer cancel()

	prompt := copilotPlannerPrompt(question)
	arguments := []string{"--silent", "--output-format", "json"}
	if configuration.Model != defaultCopilotModel {
		arguments = append(arguments, "--model", configuration.Model)
	}
	arguments = append(arguments, "--max-ai-credits", strconv.Itoa(configuration.MaxAICredits), prompt)
	command := runner(ctx, "copilot", arguments...)
	response, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open Copilot planner output: %w", err)
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start Copilot planner: %w", err)
	}
	contents, readErr := readCopilotPlannerResponse(ctx, prompt, configuration.TokenBudget, response)
	waitErr := command.Wait()
	if readErr != nil {
		return nil, readErr
	}
	if waitErr != nil {
		return nil, fmt.Errorf("run Copilot planner: %w", waitErr)
	}
	return contents, nil
}
