package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"agent-wayfinder/index"
)

const catalogSynopsisMaxResponseBytes = 4096

type CatalogSynopsisGenerator struct {
	configuration Configuration
	runner        Runner
}

func NewCatalogSynopsisGenerator(configuration Configuration, runner Runner) CatalogSynopsisGenerator {
	return CatalogSynopsisGenerator{configuration: configuration, runner: runner}
}

func (generator CatalogSynopsisGenerator) GenerateCatalogSynopsis(parent context.Context, unit index.CatalogSynopsisInput) (string, error) {
	ctx, cancel := context.WithTimeout(parent, generator.configuration.Timeout)
	defer cancel()

	prompt := catalogSynopsisPrompt(unit)
	arguments := []string{"-p", prompt, "--output-format", "json", "--bare", "--tools", "", "--model", generator.configuration.Model}
	if generator.configuration.FallbackModel != "" {
		arguments = append(arguments, "--fallback-model", generator.configuration.FallbackModel)
	}
	if generator.configuration.MaxBudgetUSD > 0 {
		arguments = append(arguments, "--max-budget-usd", strconv.FormatFloat(generator.configuration.MaxBudgetUSD, 'f', -1, 64))
	}
	if generator.configuration.Effort != "" {
		arguments = append(arguments, "--effort", generator.configuration.Effort)
	}
	arguments = append(arguments, "--no-session-persistence")
	command := generator.runner(ctx, generator.configuration.Path, arguments...)
	var standardError bytes.Buffer
	command.Stderr = &standardError
	response, err := command.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("open Claude catalog synopsis output: %w", err)
	}
	if err := command.Start(); err != nil {
		return "", fmt.Errorf("start Claude catalog synopsis: %w", err)
	}
	contents, readErr := io.ReadAll(io.LimitReader(response, catalogSynopsisMaxResponseBytes+1))
	closeErr := response.Close()
	waitErr := command.Wait()
	if readErr != nil {
		return "", fmt.Errorf("read Claude catalog synopsis: %w", readErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close Claude catalog synopsis output: %w", closeErr)
	}
	if len(contents) > catalogSynopsisMaxResponseBytes {
		return "", fmt.Errorf("read Claude catalog synopsis: exceeds %d bytes", catalogSynopsisMaxResponseBytes)
	}
	if waitErr != nil {
		message := strings.TrimSpace(standardError.String())
		if message == "" {
			return "", fmt.Errorf("run Claude catalog synopsis: %w", waitErr)
		}
		return "", fmt.Errorf("run Claude catalog synopsis: %w: %s", waitErr, message)
	}
	var result struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(contents, &result); err != nil {
		return "", fmt.Errorf("parse Claude catalog synopsis: %w", err)
	}
	if result.IsError || strings.TrimSpace(result.Result) == "" {
		return "", fmt.Errorf("parse Claude catalog synopsis: missing result")
	}
	return strings.TrimSpace(result.Result), nil
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
