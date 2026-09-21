package copilot

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-wayfinder/index"
	"agent-wayfinder/storage"
	"agent-wayfinder/testkit"

	"github.com/spf13/cobra"
)

func TestBudgetRejectsPromptThatConsumesFullBudgetBeforeReadingResponse(t *testing.T) {
	response := &budgetTestResponse{}
	_, err := ReadResponse(context.Background(), "three", 5, response)
	if !errors.Is(err, ErrPromptBudgetExceeded) {
		t.Fatalf("read planner response error = %v, want prompt-budget error", err)
	}
	if response.read {
		t.Error("read planner response read output after the prompt used the full budget")
	}
}

func TestBudgetRejectsExcessResponse(t *testing.T) {
	contents, err := ReadResponse(context.Background(), "one", 6, io.NopCloser(strings.NewReader("four")))
	if !errors.Is(err, ErrResponseBudgetExceeded) {
		t.Fatalf("read planner response error = %v, want response-budget error", err)
	}
	if contents != nil {
		t.Errorf("read planner response contents = %q, want no excess output", contents)
	}
}

func TestBudgetReturnsTimeoutForBlockedResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := ReadResponse(ctx, "one", 10, newBudgetBlockingResponse())
	if !errors.Is(err, ErrPlannerTimeout) {
		t.Fatalf("read planner response error = %v, want timeout error", err)
	}
}

func TestPromptBuildsMetadataOnlyPlannerPrompt(t *testing.T) {
	prompt := Prompt("which functions call runQuery?")
	if !strings.Contains(prompt, `"schemaVersion":1`) || !strings.Contains(prompt, `"intent"`) || !strings.Contains(prompt, `"entities"`) {
		t.Errorf("planner prompt = %q, want the response schema", prompt)
	}
	if !strings.Contains(prompt, strconv.Quote("which functions call runQuery?")) {
		t.Errorf("planner prompt = %q, want the encoded question", prompt)
	}
}

func TestRunPlannerUsesOnlyConfiguredArguments(t *testing.T) {
	var gotName string
	var gotArguments []string
	runner := func(_ context.Context, name string, arguments ...string) *exec.Cmd {
		gotName = name
		gotArguments = append([]string(nil), arguments...)
		return exec.Command("sh", "-c", `printf '%s\n' '{"type":"assistant.message","data":{"content":"planner response"}}' '{"type":"session.idle","data":{"outputTokens":12,"totalNanoAiu":80}}'`)
	}

	response, err := RunPlanner(context.Background(), Configuration{Model: "gpt-5", MaxAICredits: MinimumAICredits, TokenBudget: 4096, Timeout: 30 * time.Second}, "which functions call runQuery?", runner)
	if err != nil {
		t.Fatalf("run Copilot planner: %v", err)
	}
	if got := string(response); got != "planner response" {
		t.Errorf("planner response = %q, want text from the assistant event", got)
	}
	if gotName != "copilot" {
		t.Errorf("planner command = %q, want copilot", gotName)
	}
	usageOutputFile := argumentValue(gotArguments, "--usage-output-file")
	if usageOutputFile == "" {
		t.Fatalf("planner arguments = %q, want usage output file", gotArguments)
	}
	if _, err := os.Stat(usageOutputFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("usage output file = %q, stat error %v, want removal", usageOutputFile, err)
	}
	filteredArguments := removeArgument(gotArguments, "--usage-output-file")
	wantArguments := []string{"--silent", "--output-format", "json", "--available-tools=", "--disable-builtin-mcps", "--no-ask-user", "--no-custom-instructions", "--model", "gpt-5", "--max-ai-credits", "30", "--prompt", Prompt("which functions call runQuery?")}
	if !reflect.DeepEqual(filteredArguments, wantArguments) {
		t.Errorf("planner arguments = %q, want %q", filteredArguments, wantArguments)
	}
}

func TestRunPlannerOmitsAutomaticModelOverride(t *testing.T) {
	var gotArguments []string
	runner := func(_ context.Context, _ string, arguments ...string) *exec.Cmd {
		gotArguments = append([]string(nil), arguments...)
		return exec.Command("sh", "-c", `printf '%s\n' '{"type":"assistant.message","data":{"content":"planner response"}}'`)
	}

	_, err := RunPlanner(context.Background(), Configuration{Model: AutomaticModel, MaxAICredits: MinimumAICredits, TokenBudget: 4096, Timeout: 30 * time.Second}, "where is runQuery?", runner)
	if err != nil {
		t.Fatalf("run Copilot planner: %v", err)
	}
	for _, argument := range gotArguments {
		if argument == "--model" {
			t.Errorf("planner arguments = %q, must omit automatic model override", gotArguments)
		}
	}
}

func TestRunPlannerUsesHighReasoningForDefaultLunaModel(t *testing.T) {
	var gotArguments []string
	runner := func(_ context.Context, _ string, arguments ...string) *exec.Cmd {
		gotArguments = append([]string(nil), arguments...)
		return exec.Command("sh", "-c", `printf '%s\n' '{"type":"assistant.message","data":{"content":"planner response"}}'`)
	}

	_, err := RunPlanner(context.Background(), Configuration{Model: DefaultModel, MaxAICredits: MinimumAICredits, TokenBudget: DefaultTokenBudget, Timeout: DefaultTimeout}, "where is runQuery?", runner)
	if err != nil {
		t.Fatalf("run Copilot planner: %v", err)
	}
	if reasoningEffort := argumentValue(gotArguments, "--reasoning-effort"); reasoningEffort != "high" {
		t.Errorf("planner reasoning effort = %q, want high", reasoningEffort)
	}
}

func TestRunPlannerFallsBackToAutomaticModelWhenDefaultLunaIsUnavailable(t *testing.T) {
	var models []string
	var reasoningEfforts []string
	runner := func(_ context.Context, _ string, arguments ...string) *exec.Cmd {
		model := argumentValue(arguments, "--model")
		if model == "" {
			model = AutomaticModel
		}
		models = append(models, model)
		reasoningEfforts = append(reasoningEfforts, argumentValue(arguments, "--reasoning-effort"))
		if len(models) == 1 {
			return exec.Command("sh", "-c", `printf '%s\n' 'model gpt-5.6-luna is not available' >&2; exit 1`)
		}
		return exec.Command("sh", "-c", `printf '%s\n' '{"type":"assistant.message","data":{"content":"planner response"}}'`)
	}

	response, err := RunPlanner(context.Background(), Configuration{Model: DefaultModel, UseAutomaticFallback: true, MaxAICredits: MinimumAICredits, TokenBudget: DefaultTokenBudget, Timeout: DefaultTimeout}, "where is runQuery?", runner)
	if err != nil {
		t.Fatalf("run Copilot planner: %v", err)
	}
	if string(response) != "planner response" {
		t.Errorf("planner response = %q, want automatic-model response", response)
	}
	if want := []string{DefaultModel, AutomaticModel}; !reflect.DeepEqual(models, want) {
		t.Errorf("planner models = %q, want %q", models, want)
	}
	if want := []string{defaultLunaReasoningEffort, ""}; !reflect.DeepEqual(reasoningEfforts, want) {
		t.Errorf("planner reasoning efforts = %q, want %q", reasoningEfforts, want)
	}
}

func TestRunPlannerDoesNotFallbackForExplicitModel(t *testing.T) {
	var models []string
	runner := func(_ context.Context, _ string, arguments ...string) *exec.Cmd {
		models = append(models, argumentValue(arguments, "--model"))
		return exec.Command("sh", "-c", `printf '%s\n' 'model configured-model is not available' >&2; exit 1`)
	}

	_, err := RunPlanner(context.Background(), Configuration{Model: "configured-model", MaxAICredits: MinimumAICredits, TokenBudget: DefaultTokenBudget, Timeout: DefaultTimeout}, "where is runQuery?", runner)
	if !errors.Is(err, ErrPlannerModelUnavailable) {
		t.Fatalf("run Copilot planner error = %v, want unavailable model error", err)
	}
	if want := []string{"configured-model"}; !reflect.DeepEqual(models, want) {
		t.Errorf("planner models = %q, want %q", models, want)
	}
}

func TestRunPlannerCollectsExactUsageFromEventStream(t *testing.T) {
	runner := func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.Command("sh", "-c", `printf '%s\n' '{"type":"assistant.message","data":{"content":"planner response"}}' '{"type":"session.idle","data":{"outputTokens":12,"totalNanoAiu":80}}'`)
	}

	result, err := RunPlannerWithMetrics(context.Background(), Configuration{Model: "gpt-5", MaxAICredits: MinimumAICredits, TokenBudget: DefaultTokenBudget, Timeout: DefaultTimeout}, "where is runQuery?", runner)
	if err != nil {
		t.Fatalf("run Copilot planner with metrics: %v", err)
	}
	if string(result.Response) != "planner response" || result.ResponseBytes == 0 || result.OutputTokens.Availability != storage.MetricValueExact || result.OutputTokens.Value != 12 || result.SessionTotalNanoAiu.Availability != storage.MetricValueExact || result.SessionTotalNanoAiu.Value != 80 {
		t.Errorf("planner metric result = %+v, want assistant response and exact event metrics", result)
	}
}

func TestParseUsageReportReadsSafeUsageMetrics(t *testing.T) {
	usage, err := ParseUsageReport(strings.NewReader(`{"currentModel":"gpt-5.3-codex","totalNanoAiu":534765000,"totalPremiumRequestCost":1,"totalUserRequests":1,"totalApiDurationMs":2592,"modelMetrics":{"gpt-5.3-codex":{"usage":{"inputTokens":15667,"outputTokens":94,"cacheReadTokens":14848,"cacheWriteTokens":0,"reasoningTokens":72}}}}`))
	if err != nil {
		t.Fatalf("parse Copilot usage report: %v", err)
	}
	if usage.ActualModel != "gpt-5.3-codex" || usage.InputTokens != 15667 || usage.OutputTokens != 94 || usage.CacheReadTokens != 14848 || usage.CacheWriteTokens != 0 || usage.ReasoningTokens != 72 || usage.TotalNanoAiu != 534765000 || usage.PremiumRequestCredits != 1 || usage.UserRequests != 1 || usage.APIDuration != 2592*time.Millisecond {
		t.Errorf("Copilot usage = %+v, want exact report metrics", usage)
	}
}

func TestRunPlannerReadsFinalUsageFileAndBuildsMetric(t *testing.T) {
	runner := func(_ context.Context, _ string, arguments ...string) *exec.Cmd {
		for index, argument := range arguments {
			if argument == "--usage-output-file" && index+1 < len(arguments) {
				usage := `{"currentModel":"gpt-5.3-codex","totalNanoAiu":534765000,"totalPremiumRequestCost":1,"totalUserRequests":1,"totalApiDurationMs":2592,"modelMetrics":{"gpt-5.3-codex":{"usage":{"inputTokens":15667,"outputTokens":94,"cacheReadTokens":14848,"cacheWriteTokens":0,"reasoningTokens":72}}}}`
				if err := os.WriteFile(arguments[index+1], []byte(usage), 0o600); err != nil {
					t.Fatalf("write Copilot usage report: %v", err)
				}
			}
		}
		return exec.Command("sh", "-c", `printf '%s\n' '{"type":"assistant.message","data":{"content":"planner response"}}'`)
	}

	result, err := RunPlannerWithMetrics(context.Background(), Configuration{Model: AutomaticModel, MaxAICredits: MinimumAICredits, TokenBudget: DefaultTokenBudget, Timeout: DefaultTimeout}, "where is runQuery?", runner)
	if err != nil {
		t.Fatalf("run Copilot planner with final usage report: %v", err)
	}
	if result.Usage.ActualModel != "gpt-5.3-codex" || result.Usage.OutputTokens != 94 || result.Usage.PremiumRequestCredits != 1 || result.Usage.TotalNanoAiu != 534765000 {
		t.Errorf("planner usage = %+v, want exact final usage report", result.Usage)
	}
	metric := NewPlannerMetric(Configuration{Model: AutomaticModel, MaxAICredits: 30}, result, storage.CopilotPlannerOutcomeSuccess)
	if metric.ActualModel != "gpt-5.3-codex" || metric.InputTokens.Value != 15667 || metric.InputTokens.Availability != storage.MetricValueExact || metric.OutputTokens.Value != 94 || metric.CacheReadTokens.Value != 14848 || metric.ReasoningTokens.Value != 72 || metric.SessionTotalNanoAiu.Value != 534765000 || metric.PremiumRequestCredits.Value != 1 || metric.UserRequests.Value != 1 || metric.APIDurationMilliseconds.Value != 2592 {
		t.Errorf("planner metric = %+v, want exact final usage metrics", metric)
	}
}

func TestNewPlannerMetricUsesRequestStartTime(t *testing.T) {
	started := time.Date(2026, time.September, 19, 23, 59, 59, 0, time.UTC)
	metric := NewPlannerMetric(Configuration{Model: "gpt-5", MaxAICredits: 30}, PlannerRun{RecordedAt: started}, storage.CopilotPlannerOutcomeSuccess)
	if !metric.RecordedAt.Equal(started) {
		t.Errorf("metric timestamp = %s, want request start %s", metric.RecordedAt, started)
	}
}

func TestRunCatalogSynopsisUsesConfiguredPathForOneUnitWithoutTools(t *testing.T) {
	var gotName string
	var gotArguments []string
	runner := func(_ context.Context, name string, arguments ...string) *exec.Cmd {
		gotName = name
		gotArguments = append([]string(nil), arguments...)
		return exec.Command("true")
	}
	unit := index.CatalogSynopsisInput{
		Name:              "ValidateToken",
		Kind:              "go:function",
		Signature:         "func ValidateToken(token string) error",
		DeclarationSource: "func ValidateToken(token string) error { return verify(token) }",
		Comments:          []string{"ValidateToken checks a signed access token."},
	}

	synopsis, err := RunCatalogSynopsis(context.Background(), CatalogConfiguration{Path: "configured-copilot", MaxAICredits: 42}, unit, runner)
	if err != nil {
		t.Fatalf("run Copilot catalog synopsis: %v", err)
	}
	if synopsis != "" || gotName != "configured-copilot" {
		t.Errorf("catalog synopsis = %q, command = %q; want empty configured command output", synopsis, gotName)
	}
	if !containsArguments(gotArguments, "--max-ai-credits") || !containsArguments(gotArguments, "42") || !containsArguments(gotArguments, "--available-tools=") || !containsArguments(gotArguments, "--disable-builtin-mcps") || !containsArguments(gotArguments, "--prompt") || !containsArguments(gotArguments, "Do not make claims that the input does not support.") || !containsArguments(gotArguments, "ValidateToken") || !containsArguments(gotArguments, "func ValidateToken(token string) error") || !containsArguments(gotArguments, "return verify(token)") {
		t.Errorf("catalog arguments = %q, want an evidence-only prompt with tools disabled and a complete catalog unit", gotArguments)
	}
}

func TestReadConfigurationReadsWorkspaceAndEnvironment(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{".agent-wayfinder/config.json": `{"planning":{"copilot":{"enabled":true,"model":"gpt-5","maxAiCredits":30,"tokenBudget":2048,"timeout":"12s"}}}`})
	t.Setenv("WAYFINDER_COPILOT_TOKEN_BUDGET", "3072")

	configuration, err := ReadConfiguration(workspace.Root)
	if err != nil {
		t.Fatalf("read Copilot configuration: %v", err)
	}
	if !configuration.Enabled || configuration.Model != "gpt-5" || configuration.MaxAICredits != 30 || configuration.TokenBudget != 3072 || configuration.Timeout != 12*time.Second {
		t.Errorf("Copilot configuration = %+v, want workspace values with environment token budget", configuration)
	}
}

func TestReadConfigurationUsesDisabledDefaultsAndIgnoresLegacyKey(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{"package.json": `{"name":"fixture"}`})
	configuration, err := ReadConfiguration(workspace.Root)
	if err != nil {
		t.Fatalf("read Copilot configuration: %v", err)
	}
	want := Configuration{Model: DefaultModel, MaxAICredits: DefaultAICredits, TokenBudget: DefaultTokenBudget, Timeout: DefaultTimeout}
	if !reflect.DeepEqual(configuration, want) {
		t.Errorf("Copilot configuration = %+v, want %+v", configuration, want)
	}

	legacy := testkit.NewWorkspace(t, map[string]string{".wayfinder": `{"planning":{"copilot":{"enabled":true,"model":"gpt-5"}}}`})
	configuration, err = ReadConfiguration(legacy.Root)
	if err != nil || !reflect.DeepEqual(configuration, want) {
		t.Errorf("legacy Copilot configuration = %+v, %v; want ignored old configuration file", configuration, err)
	}
}

func TestReadConfigurationAcceptsIntegerSecondTimeout(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{".agent-wayfinder/config.json": `{"planning":{"copilot":{"timeout":12}}}`})
	configuration, err := ReadConfiguration(workspace.Root)
	if err != nil || configuration.Timeout != 12*time.Second {
		t.Errorf("Copilot timeout = %s, %v; want 12s", configuration.Timeout, err)
	}
}

func TestReadConfigurationRejectsInvalidInputWithoutSecretValues(t *testing.T) {
	for _, testCase := range []struct{ name, configuration, environment, value, wantError string }{
		{name: "unknown copilot property", configuration: `{"planning":{"copilot":{"apiKey":"not-a-secret"}}}`, wantError: "unknown field"},
		{name: "invalid enabled environment value", environment: "WAYFINDER_COPILOT_ENABLED", value: "yes", wantError: "WAYFINDER_COPILOT_ENABLED"},
		{name: "timeout exceeds maximum", configuration: `{"planning":{"copilot":{"timeout":"31s"}}}`, wantError: "invalid timeout"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			configuration := testCase.configuration
			if configuration == "" {
				configuration = `{}`
			}
			workspace := testkit.NewWorkspace(t, map[string]string{".agent-wayfinder/config.json": configuration})
			if testCase.environment != "" {
				t.Setenv(testCase.environment, testCase.value)
			}
			_, err := ReadConfiguration(workspace.Root)
			if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
				t.Fatalf("configuration error = %v, want %q", err, testCase.wantError)
			}
			if strings.Contains(err.Error(), "not-a-secret") {
				t.Errorf("configuration error = %q, must not expose configuration values", err)
			}
		})
	}
}

func TestReadConfigurationValidatesModelAndBounds(t *testing.T) {
	for _, testCase := range []struct{ name, configuration, wantError string }{
		{name: "valid model identifier", configuration: `{"planning":{"copilot":{"model":"github/gpt-5.1_mini"}}}`},
		{name: "empty model", configuration: `{"planning":{"copilot":{"model":""}}}`, wantError: "invalid Copilot model"},
		{name: "model with space", configuration: `{"planning":{"copilot":{"model":"gpt 5"}}}`, wantError: "invalid Copilot model"},
		{name: "zero AI credits", configuration: `{"planning":{"copilot":{"maxAiCredits":0}}}`, wantError: "invalid max AI credits"},
		{name: "AI credits below CLI minimum", configuration: `{"planning":{"copilot":{"maxAiCredits":29}}}`, wantError: "invalid max AI credits"},
		{name: "negative token budget", configuration: `{"planning":{"copilot":{"tokenBudget":-1}}}`, wantError: "invalid token budget"},
		{name: "zero timeout", configuration: `{"planning":{"copilot":{"timeout":"0s"}}}`, wantError: "invalid timeout"},
		{name: "negative timeout", configuration: `{"planning":{"copilot":{"timeout":-1}}}`, wantError: "invalid planning.copilot.timeout"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workspace := testkit.NewWorkspace(t, map[string]string{".agent-wayfinder/config.json": testCase.configuration})
			configuration, err := ReadConfiguration(workspace.Root)
			if testCase.wantError == "" {
				if err != nil || configuration.Model != "github/gpt-5.1_mini" {
					t.Fatalf("Copilot configuration = %+v, %v; want valid model", configuration, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
				t.Errorf("configuration error = %v, want %q", err, testCase.wantError)
			}
		})
	}
}

func TestResolveConfigurationUsesFlagPrecedence(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{".agent-wayfinder/config.json": `{"planning":{"copilot":{"enabled":false,"model":"workspace-model","maxAiCredits":30,"tokenBudget":100,"timeout":"5s"}}}`})
	t.Setenv("WAYFINDER_COPILOT_ENABLED", "false")
	t.Setenv("WAYFINDER_COPILOT_MODEL", "environment-model")
	t.Setenv("WAYFINDER_COPILOT_MAX_AI_CREDITS", "31")
	t.Setenv("WAYFINDER_COPILOT_TOKEN_BUDGET", "200")
	t.Setenv("WAYFINDER_COPILOT_TIMEOUT", "10s")
	command := newConfigurationCommand()
	for name, value := range map[string]string{"copilot": "true", "copilot-model": "flag-model", "copilot-max-ai-credits": "32", "copilot-token-budget": "300", "copilot-timeout": "15s"} {
		if err := command.Flags().Set(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}

	report, err := ResolveConfiguration(command, workspace.Root)
	if err != nil {
		t.Fatalf("resolve Copilot configuration: %v", err)
	}
	if !report.Enabled || report.Model != "flag-model" || report.MaxAICredits != 32 || report.TokenBudget != 300 || report.Timeout != "15s" || report.Sources.Enabled != "flag" || report.Sources.Model != "flag" || report.Sources.MaxAICredits != "flag" || report.Sources.TokenBudget != "flag" || report.Sources.Timeout != "flag" {
		t.Errorf("Copilot configuration report = %+v, want flag configuration", report)
	}
}

func newConfigurationCommand() *cobra.Command {
	command := &cobra.Command{}
	flags := command.Flags()
	flags.Bool("copilot", false, "")
	flags.String("copilot-model", "", "")
	flags.Int("copilot-max-ai-credits", 0, "")
	flags.Int("copilot-token-budget", 0, "")
	flags.String("copilot-timeout", "", "")
	return command
}

func argumentValue(arguments []string, name string) string {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name {
			return arguments[index+1]
		}
	}
	return ""
}

func removeArgument(arguments []string, name string) []string {
	filtered := make([]string, 0, len(arguments))
	for index := 0; index < len(arguments); index++ {
		if arguments[index] == name {
			index++
			continue
		}
		filtered = append(filtered, arguments[index])
	}
	return filtered
}

func containsArguments(arguments []string, want string) bool {
	for _, argument := range arguments {
		if argument == want || strings.Contains(argument, want) {
			return true
		}
	}
	return false
}

type budgetTestResponse struct{ read bool }

func (response *budgetTestResponse) Read([]byte) (int, error) {
	response.read = true
	return 0, io.EOF
}

func (response *budgetTestResponse) Close() error { return nil }

type budgetBlockingResponse struct {
	closed chan struct{}
	once   sync.Once
}

func newBudgetBlockingResponse() *budgetBlockingResponse {
	return &budgetBlockingResponse{closed: make(chan struct{})}
}

func (response *budgetBlockingResponse) Read([]byte) (int, error) {
	<-response.closed
	return 0, io.ErrClosedPipe
}

func (response *budgetBlockingResponse) Close() error {
	response.once.Do(func() { close(response.closed) })
	return nil
}
