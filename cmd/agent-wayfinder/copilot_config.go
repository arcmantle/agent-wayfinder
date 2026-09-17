package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	defaultCopilotModel       = "auto"
	defaultCopilotAICredits   = 1
	defaultCopilotTokenBudget = 4096
	defaultCopilotTimeout     = 30 * time.Second
	maximumCopilotTimeout     = 30 * time.Second
	copilotConfigurationName  = ".wayfinder"
)

var copilotModelPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

type copilotConfiguration struct {
	Enabled      bool
	Model        string
	MaxAICredits int
	TokenBudget  int
	Timeout      time.Duration
}

type copilotConfigurationFile struct {
	Enabled      *bool           `json:"enabled"`
	Model        *string         `json:"model"`
	MaxAICredits *int            `json:"maxAiCredits"`
	TokenBudget  *int            `json:"tokenBudget"`
	Timeout      json.RawMessage `json:"timeout"`
}

type copilotConfigurationReport struct {
	Enabled      bool                        `json:"enabled"`
	Model        string                      `json:"model"`
	MaxAICredits int                         `json:"maxAiCredits"`
	TokenBudget  int                         `json:"tokenBudget"`
	Timeout      string                      `json:"timeout"`
	Sources      copilotConfigurationSources `json:"sources"`
}

func (report copilotConfigurationReport) plannerConfiguration() (copilotConfiguration, error) {
	timeout, err := time.ParseDuration(report.Timeout)
	if err != nil {
		return copilotConfiguration{}, fmt.Errorf("parse effective Copilot timeout: %w", err)
	}
	return copilotConfiguration{
		Enabled:      report.Enabled,
		Model:        report.Model,
		MaxAICredits: report.MaxAICredits,
		TokenBudget:  report.TokenBudget,
		Timeout:      timeout,
	}, nil
}

type copilotConfigurationSources struct {
	Enabled      string `json:"enabled"`
	Model        string `json:"model"`
	MaxAICredits string `json:"maxAiCredits"`
	TokenBudget  string `json:"tokenBudget"`
	Timeout      string `json:"timeout"`
}

func resolveCopilotConfiguration(command *cobra.Command, workspaceRoot string) (copilotConfigurationReport, error) {
	configuration, err := readCopilotConfiguration(workspaceRoot)
	if err != nil {
		return copilotConfigurationReport{}, err
	}
	sources, err := copilotSources(workspaceRoot)
	if err != nil {
		return copilotConfigurationReport{}, err
	}
	flags := command.Flags()
	if flags.Changed("copilot") {
		configuration.Enabled, err = flags.GetBool("copilot")
		sources.Enabled = "flag"
	}
	if err == nil && flags.Changed("copilot-model") {
		configuration.Model, err = flags.GetString("copilot-model")
		sources.Model = "flag"
	}
	if err == nil && flags.Changed("copilot-max-ai-credits") {
		configuration.MaxAICredits, err = flags.GetInt("copilot-max-ai-credits")
		sources.MaxAICredits = "flag"
	}
	if err == nil && flags.Changed("copilot-token-budget") {
		configuration.TokenBudget, err = flags.GetInt("copilot-token-budget")
		sources.TokenBudget = "flag"
	}
	if err == nil && flags.Changed("copilot-timeout") {
		var timeout string
		timeout, err = flags.GetString("copilot-timeout")
		if err == nil {
			configuration.Timeout, err = parseCopilotTimeout(timeout)
			sources.Timeout = "flag"
		}
	}
	if err != nil {
		return copilotConfigurationReport{}, err
	}
	if err := validateCopilotConfiguration(configuration); err != nil {
		return copilotConfigurationReport{}, err
	}
	return copilotConfigurationReport{
		Enabled:      configuration.Enabled,
		Model:        configuration.Model,
		MaxAICredits: configuration.MaxAICredits,
		TokenBudget:  configuration.TokenBudget,
		Timeout:      configuration.Timeout.String(),
		Sources:      sources,
	}, nil
}

func copilotSources(workspaceRoot string) (copilotConfigurationSources, error) {
	sources := copilotConfigurationSources{Enabled: "default", Model: "default", MaxAICredits: "default", TokenBudget: "default", Timeout: "default"}
	configuration, err := readCopilotConfigurationFile(filepath.Join(workspaceRoot, copilotConfigurationName))
	if err != nil {
		return copilotConfigurationSources{}, err
	}
	if configuration.Enabled != nil {
		sources.Enabled = "workspace"
	}
	if configuration.Model != nil {
		sources.Model = "workspace"
	}
	if configuration.MaxAICredits != nil {
		sources.MaxAICredits = "workspace"
	}
	if configuration.TokenBudget != nil {
		sources.TokenBudget = "workspace"
	}
	if len(configuration.Timeout) > 0 {
		sources.Timeout = "workspace"
	}
	for variable, apply := range map[string]func(){
		"WAYFINDER_COPILOT_ENABLED":        func() { sources.Enabled = "environment" },
		"WAYFINDER_COPILOT_MODEL":          func() { sources.Model = "environment" },
		"WAYFINDER_COPILOT_MAX_AI_CREDITS": func() { sources.MaxAICredits = "environment" },
		"WAYFINDER_COPILOT_TOKEN_BUDGET":   func() { sources.TokenBudget = "environment" },
		"WAYFINDER_COPILOT_TIMEOUT":        func() { sources.Timeout = "environment" },
	} {
		if _, exists := os.LookupEnv(variable); exists {
			apply()
		}
	}
	return sources, nil
}

func readCopilotConfiguration(workspaceRoot string) (copilotConfiguration, error) {
	configuration := copilotConfiguration{
		Model:        defaultCopilotModel,
		MaxAICredits: defaultCopilotAICredits,
		TokenBudget:  defaultCopilotTokenBudget,
		Timeout:      defaultCopilotTimeout,
	}

	fileConfiguration, err := readCopilotConfigurationFile(filepath.Join(workspaceRoot, copilotConfigurationName))
	if err != nil {
		return copilotConfiguration{}, err
	}
	if fileConfiguration.Enabled != nil {
		configuration.Enabled = *fileConfiguration.Enabled
	}
	if fileConfiguration.Model != nil {
		configuration.Model = *fileConfiguration.Model
	}
	if fileConfiguration.MaxAICredits != nil {
		configuration.MaxAICredits = *fileConfiguration.MaxAICredits
	}
	if fileConfiguration.TokenBudget != nil {
		configuration.TokenBudget = *fileConfiguration.TokenBudget
	}
	if len(fileConfiguration.Timeout) != 0 {
		configuration.Timeout, err = parseCopilotJSONTimeout(fileConfiguration.Timeout)
		if err != nil {
			return copilotConfiguration{}, fmt.Errorf("invalid copilot.timeout: %w", err)
		}
	}
	if err := applyCopilotEnvironment(&configuration); err != nil {
		return copilotConfiguration{}, err
	}
	if err := validateCopilotConfiguration(configuration); err != nil {
		return copilotConfiguration{}, err
	}
	return configuration, nil
}

func readCopilotConfigurationFile(path string) (copilotConfigurationFile, error) {
	contents, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return copilotConfigurationFile{}, nil
	}
	if err != nil {
		return copilotConfigurationFile{}, fmt.Errorf("read Copilot configuration: %w", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(contents, &root); err != nil {
		return copilotConfigurationFile{}, fmt.Errorf("parse Copilot configuration: %w", err)
	}
	copilot, exists := root["copilot"]
	if !exists {
		return copilotConfigurationFile{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(copilot))
	decoder.DisallowUnknownFields()
	var configuration copilotConfigurationFile
	if err := decoder.Decode(&configuration); err != nil {
		return copilotConfigurationFile{}, fmt.Errorf("parse copilot configuration: %w", err)
	}
	return configuration, nil
}

func applyCopilotEnvironment(configuration *copilotConfiguration) error {
	if value, exists := os.LookupEnv("WAYFINDER_COPILOT_ENABLED"); exists {
		if value != "true" && value != "false" {
			return fmt.Errorf("invalid WAYFINDER_COPILOT_ENABLED: use true or false")
		}
		configuration.Enabled = value == "true"
	}
	if value, exists := os.LookupEnv("WAYFINDER_COPILOT_MODEL"); exists {
		configuration.Model = value
	}
	if value, exists := os.LookupEnv("WAYFINDER_COPILOT_MAX_AI_CREDITS"); exists {
		parsed, err := parsePositiveInteger(value)
		if err != nil {
			return fmt.Errorf("invalid WAYFINDER_COPILOT_MAX_AI_CREDITS: %w", err)
		}
		configuration.MaxAICredits = parsed
	}
	if value, exists := os.LookupEnv("WAYFINDER_COPILOT_TOKEN_BUDGET"); exists {
		parsed, err := parsePositiveInteger(value)
		if err != nil {
			return fmt.Errorf("invalid WAYFINDER_COPILOT_TOKEN_BUDGET: %w", err)
		}
		configuration.TokenBudget = parsed
	}
	if value, exists := os.LookupEnv("WAYFINDER_COPILOT_TIMEOUT"); exists {
		parsed, err := parseCopilotTimeout(value)
		if err != nil {
			return fmt.Errorf("invalid WAYFINDER_COPILOT_TIMEOUT: %w", err)
		}
		configuration.Timeout = parsed
	}
	return nil
}

func validateCopilotConfiguration(configuration copilotConfiguration) error {
	if configuration.Model != defaultCopilotModel && !copilotModelPattern.MatchString(configuration.Model) {
		return fmt.Errorf("invalid Copilot model %q", configuration.Model)
	}
	if configuration.MaxAICredits <= 0 {
		return fmt.Errorf("invalid max AI credits: use a positive integer")
	}
	if configuration.TokenBudget <= 0 {
		return fmt.Errorf("invalid token budget: use a positive integer")
	}
	if configuration.Timeout <= 0 || configuration.Timeout > maximumCopilotTimeout {
		return fmt.Errorf("invalid timeout: use a duration from 1ns through 30s")
	}
	return nil
}

func parsePositiveInteger(value string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("use a positive integer")
	}
	return parsed, nil
}

func parseCopilotTimeout(value string) (time.Duration, error) {
	if strings.TrimSpace(value) != value || value == "" {
		return 0, fmt.Errorf("use a duration or integer seconds")
	}
	if seconds, err := parsePositiveInteger(value); err == nil {
		return time.Duration(seconds) * time.Second, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("use a duration or integer seconds")
	}
	return parsed, nil
}

func parseCopilotJSONTimeout(value json.RawMessage) (time.Duration, error) {
	var duration string
	if err := json.Unmarshal(value, &duration); err == nil {
		return parseCopilotTimeout(duration)
	}
	var seconds int
	if err := json.Unmarshal(value, &seconds); err != nil {
		return 0, fmt.Errorf("use a duration string or integer seconds")
	}
	return parseCopilotTimeout(strconv.Itoa(seconds))
}
