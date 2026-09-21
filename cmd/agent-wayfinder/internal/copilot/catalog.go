package copilot

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"agent-wayfinder/index"
)

type CatalogConfiguration struct {
	Path         string
	MaxAICredits int
}

type Runner func(context.Context, string, ...string) *exec.Cmd

type CatalogSynopsisGenerator struct {
	configuration CatalogConfiguration
	runner        Runner
}

func NewCatalogSynopsisGenerator(path string, maxAICredits int, runner Runner) CatalogSynopsisGenerator {
	if maxAICredits <= 0 {
		maxAICredits = DefaultAICredits
	}
	return CatalogSynopsisGenerator{
		configuration: CatalogConfiguration{Path: path, MaxAICredits: maxAICredits},
		runner:        runner,
	}
}

func (generator CatalogSynopsisGenerator) GenerateCatalogSynopsis(ctx context.Context, input index.CatalogSynopsisInput) (string, error) {
	return RunCatalogSynopsis(ctx, generator.configuration, input, generator.runner)
}

func RunCatalogSynopsis(parent context.Context, configuration CatalogConfiguration, input index.CatalogSynopsisInput, runner Runner) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()

	commandPath := configuration.Path
	if commandPath == "" {
		commandPath = "copilot"
	}
	maxAICredits := configuration.MaxAICredits
	if maxAICredits <= 0 {
		maxAICredits = DefaultAICredits
	}
	prompt := catalogSynopsisPrompt(input)
	arguments := []string{
		"--silent",
		"--output-format", "text",
		"--max-ai-credits", strconv.Itoa(maxAICredits),
		"--available-tools=",
		"--disable-builtin-mcps",
		"--no-ask-user",
		"--no-custom-instructions",
		"--prompt", prompt,
	}
	command := runner(ctx, commandPath, arguments...)
	response, err := command.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("open Copilot catalog synopsis output: %w", err)
	}
	if err := command.Start(); err != nil {
		return "", fmt.Errorf("start Copilot catalog synopsis: %w", err)
	}
	contents, readErr := ReadResponse(ctx, prompt, 4096, response)
	waitErr := command.Wait()
	if readErr != nil {
		return "", readErr
	}
	if waitErr != nil {
		return "", fmt.Errorf("run Copilot catalog synopsis: %w", waitErr)
	}
	return strings.TrimSpace(string(contents)), nil
}

func catalogSynopsisPrompt(unit index.CatalogSynopsisInput) string {
	return "Write one concise factual capability synopsis based only on the supplied catalog unit. Do not make claims that the input does not support. Do not use tools. Return synopsis text only.\n" +
		"Name: " + unit.Name + "\n" +
		"Kind: " + string(unit.Kind) + "\n" +
		"Owner: " + unit.Owner + "\n" +
		"Signature: " + unit.Signature + "\n" +
		"Comments: " + strings.Join(unit.Comments, " ") + "\n" +
		"Identifiers: " + strings.Join(unit.IdentifierTokens, " ") + "\n" +
		"Declaration source:\n" + unit.DeclarationSource
}
