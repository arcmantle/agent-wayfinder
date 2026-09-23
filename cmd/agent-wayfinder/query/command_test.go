package query

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-wayfinder/cmd/agent-wayfinder/internal/claude"
	"agent-wayfinder/graph"
	"agent-wayfinder/query"
	"agent-wayfinder/storage"
	"agent-wayfinder/storage/sqlite"
	"agent-wayfinder/testkit"
)

var cliBinary string

func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "agent-wayfinder-test-home-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("HOME", home); err != nil {
		panic(err)
	}
	if err := os.Setenv("USERPROFILE", home); err != nil {
		panic(err)
	}
	if err := os.Chdir(".."); err != nil {
		panic(err)
	}
	binaryDirectory, err := os.MkdirTemp("", "agent-wayfinder-test-cli-")
	if err != nil {
		panic(err)
	}
	cliBinary = filepath.Join(binaryDirectory, "agent-wayfinder")
	if output, err := exec.Command("go", "build", "-o", cliBinary, ".").CombinedOutput(); err != nil {
		panic(string(output))
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	_ = os.RemoveAll(binaryDirectory)
	os.Exit(code)
}

func cliCommand(arguments ...string) *exec.Cmd {
	return exec.Command(cliBinary, arguments...)
}

func runQueryCommand(t *testing.T, arguments []string, standardOutput, standardError *strings.Builder) int {
	t.Helper()
	if len(arguments) == 0 || arguments[0] != "query" {
		t.Fatalf("query command arguments = %v, want query command", arguments)
	}
	exitCode := 0
	command := New(standardOutput, standardError, &exitCode)
	command.SetOut(standardOutput)
	command.SetErr(standardError)
	command.SetArgs(arguments[1:])
	if err := command.Execute(); err != nil {
		t.Fatalf("run query command: %v", err)
	}
	return exitCode
}

func TestQueryCommandHelpDescribesQuestionAndTermModes(t *testing.T) {
	standardOutput := &strings.Builder{}
	standardError := &strings.Builder{}

	if exitCode := runQueryCommand(t, []string{"query", "--help"}, standardOutput, standardError); exitCode != 0 {
		t.Fatalf("run query help command: exit code %d, error %s", exitCode, standardError.String())
	}

	output := standardOutput.String()
	for _, expected := range []string{"QUESTION", "TERM...", "--question", "--terms", "--show-plan"} {
		if !strings.Contains(output, expected) {
			t.Errorf("query help output = %q, want %q", output, expected)
		}
	}
}

func TestCopilotQueryExecutesValidatedPlanAgainstPublishedGraph(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"copilot","copilot":{}}}`,
		"package.json":                 `{"name":"fixture"}`,
		"src/helper.ts":                "export function helper() { return 1; }",
		"src/main.ts":                  "import { helper } from './helper'; export function main() { return helper(); }",
	})
	database := filepath.Join(t.TempDir(), "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("index workspace: %v\n%s", err, output)
	}

	installCopilotPlanner(t, `{"schemaVersion":1,"intent":"calls","entities":["main"]}`)

	output, err := cliCommand("query", "--database", database, "--format", "json", workspace.Root, "Which code invokes main?").CombinedOutput()
	if err != nil {
		t.Fatalf("query with Copilot planner: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			Interpretation *query.QueryPlan      `json:"interpretation"`
			Plan           query.QueryPlan       `json:"plan"`
			Edges          []graph.Edge          `json:"edges"`
			Evidence       []query.EvidenceGroup `json:"evidence"`
			Limits         []query.StageLimit    `json:"limits"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode query result: %v\n%s", err, output)
	}
	if result.Result.Plan.Intent != query.IntentCalls {
		t.Errorf("executed plan intent = %q, want %q", result.Result.Plan.Intent, query.IntentCalls)
	}
	if result.Result.Plan.Question != "Which code invokes main?" || result.Result.Interpretation == nil {
		t.Errorf("executed plan = %+v, want the original question and interpretation", result.Result.Plan)
	}
	if len(result.Result.Edges) != 1 || result.Result.Edges[0].Relation != "typescript:calls" {
		t.Errorf("executed evidence = %+v, want the planned calls relation", result.Result.Edges)
	}
	if len(result.Result.Evidence) == 0 || len(result.Result.Evidence[0].Nodes) == 0 || len(result.Result.Limits) == 0 {
		t.Errorf("query result evidence = %+v, limits = %+v; want source-backed evidence and limits", result.Result.Evidence, result.Result.Limits)
	}
}

func TestQueryUsesDeterministicPlanAfterDailyCopilotCreditLimit(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"copilot","copilot":{"maxAiCredits":30}},"spending":{"copilot":{"dailyAiCredits":30}}}`,
		"package.json":                 `{"name":"fixture"}`,
		"src/helper.ts":                "export function helper() { return 1; }",
		"src/main.ts":                  "import { helper } from './helper'; export function main() { return helper(); }",
	})
	database := filepath.Join(t.TempDir(), "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("index workspace: %v\n%s", err, output)
	}
	installCopilotPlanner(t, `{"schemaVersion":1,"intent":"calls","entities":["main"]}`)

	for request := 0; request < 2; request++ {
		output, err := cliCommand("query", "--database", database, "--format", "json", workspace.Root, "Which code invokes main?").CombinedOutput()
		if err != nil {
			t.Fatalf("query request %d: %v\n%s", request+1, err, output)
		}
		var result struct {
			Result struct {
				Planning struct {
					Method string `json:"method"`
				} `json:"planning"`
			} `json:"result"`
		}
		if err := json.Unmarshal(output, &result); err != nil {
			t.Fatalf("decode query request %d: %v\n%s", request+1, err, output)
		}
		if request == 0 && result.Result.Planning.Method != "copilot" {
			t.Errorf("first planning method = %q, want copilot", result.Result.Planning.Method)
		}
		if request == 1 && result.Result.Planning.Method != "fallback" {
			t.Errorf("second planning method = %q, want fallback after daily limit", result.Result.Planning.Method)
		}
	}
}

func TestQueryUsesDeterministicPlanAfterDailyClaudeDollarLimit(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"claude","claude":{}},"spending":{"claude":{"dailyUsd":1}}}`,
		"package.json":                 `{"name":"fixture"}`,
		"src/helper.ts":                "export function helper() { return 1; }",
		"src/main.ts":                  "import { helper } from './helper'; export function main() { return helper(); }",
	})
	database := filepath.Join(t.TempDir(), "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("index workspace: %v\n%s", err, output)
	}
	installClaudePlanner(t, `{"schemaVersion":1,"intent":"calls","entities":["main"]}`)

	for request := 0; request < 2; request++ {
		output, err := cliCommand("query", "--database", database, "--format", "json", workspace.Root, "Which code invokes main?").CombinedOutput()
		if err != nil {
			t.Fatalf("query request %d: %v\n%s", request+1, err, output)
		}
		var result struct {
			Result struct {
				Planning struct {
					Method string `json:"method"`
				} `json:"planning"`
			} `json:"result"`
		}
		if err := json.Unmarshal(output, &result); err != nil {
			t.Fatalf("decode query request %d: %v\n%s", request+1, err, output)
		}
		if request == 0 && result.Result.Planning.Method != "claude" {
			t.Errorf("first planning method = %q, want claude", result.Result.Planning.Method)
		}
		if request == 1 && result.Result.Planning.Method != "fallback" {
			t.Errorf("second planning method = %q, want fallback after daily limit", result.Result.Planning.Method)
		}
	}
}

func TestQueryUsesOllamaPlanBeforeCopilotFallback(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"ollama","ollama":{}}}`,
		"package.json":                 `{"name":"fixture"}`,
		"src/helper.ts":                "export function helper() { return 1; }",
		"src/main.ts":                  "import { helper } from './helper'; export function main() { return helper(); }",
	})
	database := filepath.Join(t.TempDir(), "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("index workspace: %v\n%s", err, output)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = response.Write([]byte(`{"message":{"content":"{\"schemaVersion\":1,\"intent\":\"called_by\",\"entities\":[\"helper\"],\"confidence\":\"high\"}"}}`))
	}))
	defer server.Close()
	t.Setenv("OLLAMA_HOST", server.URL)
	installFailingCopilotPlanner(t)

	output, err := cliCommand("query", "--database", database, "--format", "json", workspace.Root, "Which code invokes helper?").CombinedOutput()
	if err != nil {
		t.Fatalf("query with Ollama planner: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			Plan     query.QueryPlan `json:"plan"`
			Planning struct {
				Method string `json:"method"`
			} `json:"planning"`
			Ollama struct {
				Enabled   bool   `json:"enabled"`
				Model     string `json:"model"`
				Timeout   string `json:"timeout"`
				Available bool   `json:"available"`
			} `json:"ollama"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode Ollama query result: %v\n%s", err, output)
	}
	if result.Result.Plan.Intent != query.IntentCalledBy || result.Result.Planning.Method != "ollama" {
		t.Errorf("Ollama query result = %+v, want Ollama called-by plan", result.Result)
	}
	if !result.Result.Ollama.Enabled || result.Result.Ollama.Model != "qwen3:8b" || result.Result.Ollama.Timeout != "30s" || !result.Result.Ollama.Available {
		t.Errorf("Ollama query metadata = %+v, want enabled available qwen3:8b with a 30s timeout", result.Result.Ollama)
	}
}

func TestQueryUsesClaudePlanBeforeCopilotFallback(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"claude","claude":{}}}`,
		"package.json":                 `{"name":"fixture"}`,
		"src/helper.ts":                "export function helper() { return 1; }",
		"src/main.ts":                  "import { helper } from './helper'; export function main() { return helper(); }",
	})
	database := filepath.Join(t.TempDir(), "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("index workspace: %v\n%s", err, output)
	}
	installClaudePlanner(t, `{"schemaVersion":1,"intent":"called_by","entities":["helper"]}`)
	installFailingCopilotPlanner(t)

	output, err := cliCommand("query", "--database", database, "--format", "json", workspace.Root, "Which code invokes helper?").CombinedOutput()
	if err != nil {
		t.Fatalf("query with Claude planner: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			Plan   query.QueryPlan `json:"plan"`
			Claude struct {
				Sources claude.ConfigurationSources `json:"sources"`
			} `json:"claude"`
			Planning struct {
				Method string `json:"method"`
			} `json:"planning"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode Claude query result: %v\n%s", err, output)
	}
	if result.Result.Plan.Intent != query.IntentCalledBy || result.Result.Planning.Method != "claude" {
		t.Errorf("Claude query result = %+v, want Claude called-by plan", result.Result)
	}
	if sources := result.Result.Claude.Sources; sources.Enabled != "workspace" || sources.Path != "default" || sources.Model != "default" || sources.FallbackModel != "default" || sources.MaxBudgetUSD != "default" || sources.Effort != "default" || sources.Timeout != "default" {
		t.Errorf("Claude query configuration sources = %+v, want workspace enablement and defaults", sources)
	}
}

func TestQueryPreservesFallbackAfterRejectedClaudePlan(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"claude","claude":{}}}`,
		"package.json":                 `{"name":"fixture"}`,
		"src/main.ts":                  "export function main() { return 1; }",
	})
	database := filepath.Join(t.TempDir(), "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("index workspace: %v\n%s", err, output)
	}
	installClaudePlanner(t, `{"schemaVersion":1,"intent":"delete","entities":["main"]}`)

	output, err := cliCommand("query", "--database", database, "--format", "json", workspace.Root, "Which code handles main work?").CombinedOutput()
	if err != nil {
		t.Fatalf("query with rejected Claude plan: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			Plan     query.QueryPlan `json:"plan"`
			Planning struct {
				Method string `json:"method"`
			} `json:"planning"`
			Claude struct {
				UnavailableReason string `json:"unavailableReason"`
			} `json:"claude"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode Claude fallback result: %v\n%s", err, output)
	}
	if result.Result.Plan.Intent != query.IntentUnknown || result.Result.Planning.Method != "fallback" || result.Result.Claude.UnavailableReason == "" {
		t.Errorf("Claude fallback result = %+v, want unknown fallback with reason", result.Result)
	}
}

func TestQueryDoesNotCallClaudeForDeterministicPlan(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"claude","claude":{}}}`,
		"package.json":                 `{"name":"fixture"}`,
		"src/main.ts":                  "export function main() { return 1; }",
	})
	database := filepath.Join(t.TempDir(), "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("index workspace: %v\n%s", err, output)
	}
	installFailingClaudePlanner(t)

	output, err := cliCommand("query", "--database", database, "--format", "json", workspace.Root, "Where is main?").CombinedOutput()
	if err != nil {
		t.Fatalf("query with deterministic plan: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			Planning struct {
				Method string `json:"method"`
			} `json:"planning"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode deterministic query result: %v\n%s", err, output)
	}
	if result.Result.Planning.Method != "deterministic" {
		t.Errorf("planning method = %q, want deterministic", result.Result.Planning.Method)
	}
}

func TestQueryDoesNotCallOllamaForDeterministicPlan(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"ollama","ollama":{}}}`,
		"package.json":                 `{"name":"fixture"}`,
		"src/main.ts":                  "export function main() { return 1; }",
	})
	database := filepath.Join(t.TempDir(), "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("index workspace: %v\n%s", err, output)
	}
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("Ollama planner was called for a deterministic question")
	}))
	defer server.Close()
	t.Setenv("OLLAMA_HOST", server.URL)

	output, err := cliCommand("query", "--database", database, "--format", "json", workspace.Root, "Where is main?").CombinedOutput()
	if err != nil {
		t.Fatalf("query with deterministic plan: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			Planning struct {
				Method string `json:"method"`
			} `json:"planning"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode deterministic query result: %v\n%s", err, output)
	}
	if result.Result.Planning.Method != "deterministic" {
		t.Errorf("planning method = %q, want deterministic", result.Result.Planning.Method)
	}
}

func TestQueryPreservesFallbackForRejectedOllamaPlan(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "unsupported intent", content: `{"schemaVersion":1,"intent":"delete","entities":["main"],"confidence":"high"}`},
		{name: "low confidence", content: `{"schemaVersion":1,"intent":"called_by","entities":["main"],"confidence":"low"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := testkit.NewWorkspace(t, map[string]string{
				".agent-wayfinder/config.json": `{"planning":{"provider":"ollama","ollama":{"timeout":"1s"}}}`,
				"package.json":                 `{"name":"fixture"}`,
				"src/main.ts":                  "export function main() { return 1; }",
			})
			database := filepath.Join(t.TempDir(), "graph.db")
			if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
				t.Fatalf("index workspace: %v\n%s", err, output)
			}
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(response).Encode(map[string]any{"message": map[string]string{"content": test.content}})
			}))
			defer server.Close()
			t.Setenv("OLLAMA_HOST", server.URL)

			output, err := cliCommand("query", "--database", database, "--format", "json", workspace.Root, "Which code handles main work?").CombinedOutput()
			if err != nil {
				t.Fatalf("query with rejected local plan: %v\n%s", err, output)
			}
			var result struct {
				Result struct {
					Plan     query.QueryPlan `json:"plan"`
					Planning struct {
						Method string `json:"method"`
					} `json:"planning"`
					Ollama struct {
						UnavailableReason string `json:"unavailableReason"`
					} `json:"ollama"`
				} `json:"result"`
			}
			if err := json.Unmarshal(output, &result); err != nil {
				t.Fatalf("decode fallback query result: %v\n%s", err, output)
			}
			if result.Result.Plan.Intent != query.IntentUnknown || result.Result.Planning.Method != "fallback" || result.Result.Ollama.UnavailableReason == "" {
				t.Errorf("rejected local plan result = %+v, want unknown fallback with reason", result.Result)
			}
		})
	}
}

func TestCopilotQueryOutputJSONSeparatesPlannerMetadataFromEvidence(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"copilot","copilot":{}}}`,
		"package.json":                 `{"name":"fixture"}`,
		"src/main.ts":                  "export function main() { return 1; }",
	})
	database := filepath.Join(t.TempDir(), "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("index workspace: %v\n%s", err, output)
	}

	const plannerResponse = `{"schemaVersion":1,"intent":"lookup","entities":["main"]}`
	installCopilotPlanner(t, plannerResponse)

	output, err := cliCommand("query", "--database", database, "--format", "json", workspace.Root, "Which code locates main?").CombinedOutput()
	if err != nil {
		t.Fatalf("query with Copilot planner: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			Plan     *query.QueryPlan      `json:"plan"`
			Evidence []query.EvidenceGroup `json:"evidence"`
			Limits   []query.StageLimit    `json:"limits"`
			Warnings []query.PlanWarning   `json:"warnings"`
			Copilot  struct {
				Available bool            `json:"available"`
				Response  json.RawMessage `json:"response"`
			} `json:"copilot"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode query result: %v\n%s", err, output)
	}
	if result.Result.Plan == nil || len(result.Result.Evidence) == 0 || len(result.Result.Limits) == 0 {
		t.Errorf("deterministic result = %+v, want separate plan, evidence, and limits", result.Result)
	}
	if !result.Result.Copilot.Available {
		t.Error("Copilot metadata available = false, want true")
	}
	if string(result.Result.Copilot.Response) != plannerResponse {
		t.Errorf("Copilot response = %s, want %s", result.Result.Copilot.Response, plannerResponse)
	}
	store, err := sqlite.Open(context.Background(), database)
	if err != nil {
		t.Fatalf("open metrics database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	metrics, err := store.ReadCopilotPlannerDailyMetrics(context.Background(), storage.CopilotPlannerDailyMetricsRequest{Day: time.Now().UTC()})
	if err != nil {
		t.Fatalf("read planner metrics: %v", err)
	}
	if len(metrics) != 1 || metrics[0].Successes != 1 || metrics[0].OutputTokens.Availability != storage.MetricValueExact || metrics[0].OutputTokens.Value != 12 || metrics[0].SessionTotalNanoAiu.Availability != storage.MetricValueExact || metrics[0].SessionTotalNanoAiu.Value != 80 {
		t.Errorf("planner metrics = %+v, want one exact successful request", metrics)
	}
}

func TestCopilotQueryOutputTextReportsPlannerAvailability(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"copilot","copilot":{}}}`,
		"package.json":                 `{"name":"fixture"}`,
		"src/main.ts":                  "export function main() { return 1; }",
	})
	database := filepath.Join(t.TempDir(), "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("index workspace: %v\n%s", err, output)
	}
	installCopilotPlanner(t, `{"schemaVersion":1,"intent":"lookup","entities":["main"]}`)

	output, err := cliCommand("query", "--database", database, workspace.Root, "Which code locates main?").CombinedOutput()
	if err != nil {
		t.Fatalf("query with Copilot planner: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "Copilot planner: available.") {
		t.Errorf("Copilot text output = %q, want planner availability", output)
	}
}

func TestQueryCommandReturnsRankedSeedsAndBoundedEvidence(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}

	command := cliCommand("query", "--database", database, "--format", "json", "--max-depth", "1", workspace.Root, "main")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run query command: %v\n%s", err, output)
	}

	var result struct {
		GraphVersion int    `json:"graphVersion"`
		PublishedAt  string `json:"publishedAt"`
		Result       struct {
			Seeds []struct {
				Term  string `json:"term"`
				Nodes []struct {
					QualifiedName string `json:"qualifiedName"`
				} `json:"nodes"`
			} `json:"seeds"`
			Nodes []struct {
				QualifiedName string `json:"qualifiedName"`
			} `json:"nodes"`
			Edges []struct {
				Relation string `json:"relation"`
			} `json:"edges"`
			MaxDepth int `json:"maxDepth"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode query result: %v\n%s", err, output)
	}
	if result.GraphVersion != 1 || result.PublishedAt == "" {
		t.Errorf("query envelope = %+v, want published graph metadata", result)
	}
	if len(result.Result.Seeds) != 1 || result.Result.Seeds[0].Term != "main" || len(result.Result.Seeds[0].Nodes) == 0 || result.Result.Seeds[0].Nodes[0].QualifiedName != "src/main.ts::main" {
		t.Errorf("query seeds = %+v, want ranked main seed", result.Result.Seeds)
	}
	if result.Result.MaxDepth != 1 {
		t.Errorf("maximum depth = %d, want 1", result.Result.MaxDepth)
	}
	hasCallEdge := false
	for _, edge := range result.Result.Edges {
		if edge.Relation == "typescript:calls" {
			hasCallEdge = true
			break
		}
	}
	if len(result.Result.Nodes) == 0 || !hasCallEdge {
		t.Errorf("query facts = nodes %+v, edges %+v, want bounded call evidence", result.Result.Nodes, result.Result.Edges)
	}
}

func TestQueryCommandSelectsQuestionModeAndReturnsPlan(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/main.ts":  "export function main() { return 1; }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}

	output, err := cliCommand("query", "--database", database, "--format", "json", "--max-depth", "1", "--max-nodes", "25", workspace.Root, "Where is main?").CombinedOutput()
	if err != nil {
		t.Fatalf("run question query: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			SchemaVersion  int `json:"schemaVersion"`
			Interpretation struct {
				SchemaVersion int     `json:"schemaVersion"`
				Question      string  `json:"question"`
				Intent        string  `json:"intent"`
				Confidence    float64 `json:"confidence"`
				Operator      string  `json:"operator"`
				MaxDepth      int     `json:"maxDepth"`
				MaxNodes      int     `json:"maxNodes"`
				EntitySlots   []struct {
					Role string `json:"role"`
					Text string `json:"text"`
				} `json:"entitySlots"`
			} `json:"interpretation"`
			Seeds []struct {
				Role     string `json:"role"`
				Term     string `json:"term"`
				Rankings []struct {
					NodeID     string  `json:"nodeId"`
					Score      float64 `json:"score"`
					Components struct {
						Exact          float64 `json:"exact"`
						Lexical        float64 `json:"lexical"`
						ReciprocalRank float64 `json:"reciprocalRank"`
					} `json:"components"`
				} `json:"rankings"`
			} `json:"seeds"`
			Evidence    []query.EvidenceGroup `json:"evidence"`
			Limits      []query.StageLimit    `json:"limits"`
			Warnings    []query.PlanWarning   `json:"warnings"`
			Suggestions []string              `json:"suggestions"`
			Catalog     *query.CatalogResult  `json:"catalog"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode question query: %v\n%s", err, output)
	}
	if result.Result.SchemaVersion != 1 {
		t.Errorf("question result schema version = %d, want 1", result.Result.SchemaVersion)
	}
	plan := result.Result.Interpretation
	if plan.SchemaVersion != 1 || plan.Question != "Where is main?" || plan.Intent != "lookup" || plan.Confidence <= 0 || plan.Operator != "lookup" {
		t.Errorf("question plan = %+v, want stable lookup plan", plan)
	}
	if plan.MaxDepth != 1 || plan.MaxNodes != 25 {
		t.Errorf("question plan limits = {%d, %d}, want {1, 25}", plan.MaxDepth, plan.MaxNodes)
	}
	if len(plan.EntitySlots) != 1 || plan.EntitySlots[0].Role != "entity" || plan.EntitySlots[0].Text != "main" {
		t.Errorf("question slots = %+v, want main entity slot", plan.EntitySlots)
	}
	if len(result.Result.Seeds) != 1 || result.Result.Seeds[0].Term != "main" {
		t.Errorf("question seeds = %+v, want current lookup for main", result.Result.Seeds)
	}
	seed := result.Result.Seeds[0]
	if seed.Role != "entity" || len(seed.Rankings) == 0 || seed.Rankings[0].NodeID == "" || seed.Rankings[0].Score <= 0 {
		t.Fatalf("question seed rankings = %+v, want role-bound scored candidates", seed)
	}
	components := seed.Rankings[0].Components
	if components.Exact <= 0 && (components.Lexical <= 0 || components.ReciprocalRank <= 0) {
		t.Errorf("question seed components = %+v, want exact or fused lexical score", components)
	}
	if len(result.Result.Evidence) == 0 || result.Result.Evidence[0].Rank != 1 || result.Result.Evidence[0].SlotRole != "entity" || len(result.Result.Evidence[0].Nodes) != 1 || result.Result.Evidence[0].Nodes[0].Evidence.Span.Path == "" {
		t.Errorf("question evidence = %+v, want ranked entity evidence", result.Result.Evidence)
	}
	if len(result.Result.Limits) != 1 || result.Result.Limits[0].Stage != "retrieval" || result.Result.Limits[0].SlotRole != "entity" {
		t.Errorf("question limits = %+v, want entity retrieval limit", result.Result.Limits)
	}
	if result.Result.Catalog != nil {
		t.Errorf("named-unit catalog result = %+v, want graph-only lookup", result.Result.Catalog)
	}

	repeatedOutput, err := cliCommand("query", "--database", database, "--format", "json", "--max-depth", "1", "--max-nodes", "25", workspace.Root, "Where is main?").CombinedOutput()
	if err != nil {
		t.Fatalf("repeat question query: %v\n%s", err, repeatedOutput)
	}
	if !bytes.Equal(output, repeatedOutput) {
		t.Errorf("repeated question JSON differs\nfirst: %s\nsecond: %s", output, repeatedOutput)
	}

	textOutput, err := cliCommand("query", "--show-plan", "--database", database, workspace.Root, "Where is main?").CombinedOutput()
	if err != nil {
		t.Fatalf("run text question query: %v\n%s", err, textOutput)
	}
	if !strings.Contains(string(textOutput), "Interpreted as: lookup (lookup, confidence 1.00)") {
		t.Errorf("question text = %q, want interpreted plan", textOutput)
	}
}

func TestQueryCommandRoutesCapabilityQuestionToCatalog(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/token.ts": "export function validateAccessToken(token: string) { return token.length > 0; }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}
	if output, err := cliCommand("catalog", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("run catalog command: %v\n%s", err, output)
	}

	output, err := cliCommand("query", "--database", database, "--format", "json", workspace.Root, "Does this workspace validate access tokens?").CombinedOutput()
	if err != nil {
		t.Fatalf("run capability question: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			Interpretation query.QueryPlan `json:"interpretation"`
			Catalog        struct {
				Matches []struct {
					Node     graph.Node `json:"node"`
					Evidence []struct {
						Generator string `json:"generator"`
						Synopsis  string `json:"synopsis"`
					} `json:"evidence"`
				} `json:"matches"`
			} `json:"catalog"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode capability query: %v\n%s", err, output)
	}
	if result.Result.Interpretation.Intent != "capability" {
		t.Errorf("capability intent = %q, want capability", result.Result.Interpretation.Intent)
	}
	if len(result.Result.Catalog.Matches) == 0 || result.Result.Catalog.Matches[0].Node.Label != "validateAccessToken" {
		t.Errorf("catalog matches = %+v, want validateAccessToken", result.Result.Catalog.Matches)
	}
	if len(result.Result.Catalog.Matches) == 0 || len(result.Result.Catalog.Matches[0].Evidence) != 1 || result.Result.Catalog.Matches[0].Evidence[0].Generator != "deterministic" || result.Result.Catalog.Matches[0].Evidence[0].Synopsis == "" {
		t.Errorf("catalog evidence = %+v, want one deterministic synopsis", result.Result.Catalog.Matches)
	}

	textOutput, err := cliCommand("query", "--database", database, workspace.Root, "Does this workspace validate access tokens?").CombinedOutput()
	if err != nil {
		t.Fatalf("run capability text query: %v\n%s", err, textOutput)
	}
	if strings.Contains(string(textOutput), "No answer-ready evidence was found.") || !strings.Contains(string(textOutput), "Catalog:") {
		t.Errorf("capability text = %q, want catalog evidence without an empty-evidence claim", textOutput)
	}
}

func TestQueryCommandReportsLowConfidenceEmptyQuestionWithoutAnAnswerClaim(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/main.ts":  "export function main() { return 1; }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}

	question := "Why do lunar widgets shimmer?"
	output, err := cliCommand("query", "--database", database, "--format", "json", workspace.Root, question).CombinedOutput()
	if err != nil {
		t.Fatalf("run low-confidence question: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			SchemaVersion  int                   `json:"schemaVersion"`
			Interpretation query.QueryPlan       `json:"interpretation"`
			Evidence       []query.EvidenceGroup `json:"evidence"`
			Warnings       []query.PlanWarning   `json:"warnings"`
			Suggestions    []string              `json:"suggestions"`
			EvidenceGroups []struct {
				Label   string               `json:"label"`
				Graph   *query.Result        `json:"graph"`
				Catalog *query.CatalogResult `json:"catalog"`
			} `json:"evidenceGroups"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode low-confidence question: %v\n%s", err, output)
	}
	if result.Result.SchemaVersion != 1 || result.Result.Interpretation.Confidence >= 0.5 {
		t.Errorf("low-confidence result = %+v, want versioned low-confidence interpretation", result.Result)
	}
	if len(result.Result.Evidence) != 0 || len(result.Result.Warnings) == 0 || len(result.Result.Suggestions) == 0 {
		t.Errorf("low-confidence result = %+v, want no evidence plus warnings and suggestions", result.Result)
	}
	if len(result.Result.EvidenceGroups) != 2 || result.Result.EvidenceGroups[0].Label != "graph" || result.Result.EvidenceGroups[0].Graph == nil || result.Result.EvidenceGroups[1].Label != "catalog" || result.Result.EvidenceGroups[1].Catalog == nil {
		t.Errorf("low-confidence evidence groups = %+v, want labeled graph and catalog results", result.Result.EvidenceGroups)
	}

	textOutput, err := cliCommand("query", "--database", database, workspace.Root, question).CombinedOutput()
	if err != nil {
		t.Fatalf("run low-confidence text question: %v\n%s", err, textOutput)
	}
	text := string(textOutput)
	if !strings.Contains(text, "No answer-ready evidence was found.") || !strings.Contains(text, "Next: Try a supported question such as: where is <entity>?") {
		t.Errorf("low-confidence text = %q, want empty-result text and a supported next question", text)
	}
	if got := strings.Count(text, "Warning: The question grammar is not in the supported intent set."); got != 1 {
		t.Errorf("unknown-intent warning count = %d, want 1 in %q", got, text)
	}
}

func TestQueryResultDataIncludesImpactEvidence(t *testing.T) {
	node := graph.Node{ID: "function:dependent", Kind: "function", Label: "dependent"}
	result := resultData(query.Result{Impact: []query.ImpactEvidence{{
		Node:     node,
		Relation: "calls",
		Distance: 1,
		Score:    1,
	}}}, nil, nil, 2, 100, ollamaQueryMetadata{}, copilotQueryMetadata{}, claudeQueryMetadata{})

	if len(result.Impact) != 1 || result.Impact[0].Node.ID != node.ID || result.Impact[0].Relation != "calls" || result.Impact[0].Distance != 1 || result.Impact[0].Score != 1 {
		t.Errorf("impact result = %+v, want visible node, relation, distance, and score", result.Impact)
	}
}

func TestQueryCommandReportsAmbiguousExplainWithoutNeighborhoodEvidence(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/first.ts":  "export function helper() { return 1; }",
		"src/second.ts": "export function helper() { return 2; }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}

	output, err := cliCommand("query", "--database", database, "--format", "json", workspace.Root, "Explain helper").CombinedOutput()
	if err != nil {
		t.Fatalf("run explain question: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			Warnings []struct {
				Code        string   `json:"code"`
				Suggestions []string `json:"suggestions"`
			} `json:"warnings"`
			Seeds []struct {
				Nodes []graph.Node `json:"nodes"`
			} `json:"seeds"`
			Nodes []graph.Node `json:"nodes"`
			Edges []graph.Edge `json:"edges"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode explain question: %v\n%s", err, output)
	}
	if len(result.Result.Warnings) != 1 || result.Result.Warnings[0].Code != "ambiguous_entity" || len(result.Result.Warnings[0].Suggestions) < 2 {
		t.Fatalf("warnings = %+v, want ambiguous_entity with exact follow-up commands", result.Result.Warnings)
	}
	if len(result.Result.Seeds) != 1 || len(result.Result.Seeds[0].Nodes) < 2 {
		t.Errorf("candidates = %+v, want ranked ambiguous candidates", result.Result.Seeds)
	}
	if len(result.Result.Nodes) != 0 || len(result.Result.Edges) != 0 {
		t.Errorf("neighborhood = {%+v, %+v}, want no answer evidence", result.Result.Nodes, result.Result.Edges)
	}
}

func TestQueryCommandReturnsDirectedPathEvidenceForAQuestion(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}

	output, err := cliCommand("query", "--database", database, "--format", "json", workspace.Root, "How does src/main.ts::main reach src/helper.ts::helper?").CombinedOutput()
	if err != nil {
		t.Fatalf("run path question: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			Plan struct {
				Intent   string `json:"intent"`
				Operator string `json:"operator"`
			} `json:"plan"`
			Nodes []graph.Node `json:"nodes"`
			Edges []graph.Edge `json:"edges"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode path question: %v\n%s", err, output)
	}
	if result.Result.Plan.Intent != "path" || result.Result.Plan.Operator != "path" {
		t.Errorf("plan = %+v, want path intent and operator", result.Result.Plan)
	}
	pathNodeNames := make([]string, len(result.Result.Nodes))
	for index, node := range result.Result.Nodes {
		pathNodeNames[index] = node.QualifiedName
	}
	if got, want := pathNodeNames, []string{"src/main.ts::main", "src/helper.ts::helper"}; !reflect.DeepEqual(got, want) {
		t.Errorf("path node names = %v, want %v", got, want)
	}
	if len(result.Result.Edges) != 1 || result.Result.Edges[0].Relation != "typescript:calls" {
		t.Errorf("path edges = %+v, want one TypeScript call edge", result.Result.Edges)
	}
}

func TestQueryCommandModeFlagsPreserveTermsAndRejectConflict(t *testing.T) {
	standardOutput := &strings.Builder{}
	standardError := &strings.Builder{}
	if exitCode := runQueryCommand(t, []string{"query", "--question", "--terms", ".", "main"}, standardOutput, standardError); exitCode != 2 {
		t.Fatalf("conflicting mode exit code = %d, want 2; error %s", exitCode, standardError.String())
	}
	if !strings.Contains(standardError.String(), "--question and --terms cannot be used together") {
		t.Errorf("conflicting mode error = %q, want mode conflict", standardError.String())
	}
	standardOutput.Reset()
	standardError.Reset()
	if exitCode := runQueryCommand(t, []string{"query", "--question", ".", "Where is main?", "Where is helper?"}, standardOutput, standardError); exitCode != 2 {
		t.Fatalf("multiple questions exit code = %d, want 2; error %s", exitCode, standardError.String())
	}
	if !strings.Contains(standardError.String(), "question mode requires exactly one question argument") {
		t.Errorf("multiple questions error = %q, want argument count error", standardError.String())
	}

	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/main.ts":  "export function main() { return 1; }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}
	output, err := cliCommand("query", "--terms", "--database", database, "--format", "json", workspace.Root, "Where is main?").CombinedOutput()
	if err != nil {
		t.Fatalf("run explicit terms query: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			Plan  json.RawMessage `json:"plan"`
			Seeds []struct {
				Term string `json:"term"`
			} `json:"seeds"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode explicit terms query: %v\n%s", err, output)
	}
	if result.Result.Plan != nil {
		t.Errorf("legacy terms plan = %s, want no question plan", result.Result.Plan)
	}
	if len(result.Result.Seeds) != 1 || result.Result.Seeds[0].Term != "Where is main?" {
		t.Errorf("legacy terms seeds = %+v, want unchanged literal term", result.Result.Seeds)
	}
}

func TestQueryCommandReportsLimitsAndFilteredEvidence(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}

	command := cliCommand("query", "--database", database, "--format", "json", "--max-depth", "0", "--relation", "typescript:calls", workspace.Root, "src/main.ts::main")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run query command: %v\n%s", err, output)
	}

	var result struct {
		Result struct {
			Nodes []struct {
				QualifiedName string `json:"qualifiedName"`
				Evidence      struct {
					Confidence string `json:"confidence"`
					Span       struct {
						Path string `json:"path"`
					} `json:"span"`
				} `json:"evidence"`
			} `json:"nodes"`
			Edges []struct {
				Relation string `json:"relation"`
			} `json:"edges"`
			TruncationReasons []string `json:"truncationReasons"`
			MaxDepth          int      `json:"maxDepth"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode query result: %v\n%s", err, output)
	}
	if result.Result.MaxDepth != 0 {
		t.Errorf("maximum depth = %d, want 0", result.Result.MaxDepth)
	}
	if len(result.Result.Nodes) != 1 || result.Result.Nodes[0].QualifiedName != "src/main.ts::main" || result.Result.Nodes[0].Evidence.Confidence == "" || result.Result.Nodes[0].Evidence.Span.Path != "src/main.ts" {
		t.Errorf("query nodes = %+v, want evidenced main seed only", result.Result.Nodes)
	}
	if len(result.Result.Edges) != 0 {
		t.Errorf("query edges = %+v, want no edges at depth zero", result.Result.Edges)
	}
	if len(result.Result.TruncationReasons) != 1 || result.Result.TruncationReasons[0] != "depth_limit" {
		t.Errorf("truncation reasons = %v, want depth limit", result.Result.TruncationReasons)
	}
}

func TestQueryPreservesDeterministicFallbackAfterCopilotFailure(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"copilot","copilot":{}}}`,
		"package.json":                 `{"name":"fixture"}`,
		"src/main.ts":                  "export function main() { return 1; }",
	})
	database := filepath.Join(t.TempDir(), "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("index workspace: %v\n%s", err, output)
	}
	installFailingCopilotPlanner(t)

	output, err := cliCommand("query", "--database", database, "--format", "json", workspace.Root, "Which code handles main work?").CombinedOutput()
	if err != nil {
		t.Fatalf("query with failed Copilot planner: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			Plan     *query.QueryPlan    `json:"plan"`
			Warnings []query.PlanWarning `json:"warnings"`
			Copilot  struct {
				Available         bool            `json:"available"`
				UnavailableReason string          `json:"unavailableReason"`
				Response          json.RawMessage `json:"response"`
			} `json:"copilot"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode fallback result: %v\n%s", err, output)
	}
	if result.Result.Plan == nil || result.Result.Plan.Intent != query.IntentUnknown || result.Result.Plan.Question != "Which code handles main work?" {
		t.Errorf("fallback result = %+v, want the deterministic query plan", result.Result)
	}
	if !hasWarningCode(result.Result.Warnings, "copilot_unavailable") || result.Result.Copilot.Available || result.Result.Copilot.UnavailableReason == "" || len(result.Result.Copilot.Response) != 0 {
		t.Errorf("fallback result = %+v, want unavailable Copilot metadata and warning", result.Result)
	}
	store, err := sqlite.Open(context.Background(), database)
	if err != nil {
		t.Fatalf("open metrics database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	metrics, err := store.ReadCopilotPlannerDailyMetrics(context.Background(), storage.CopilotPlannerDailyMetricsRequest{Day: time.Now().UTC()})
	if err != nil {
		t.Fatalf("read planner metrics: %v", err)
	}
	if len(metrics) != 1 || metrics[0].Fallbacks != 1 || metrics[0].OutputTokens.Availability != storage.MetricValueUnavailable || metrics[0].SessionTotalNanoAiu.Availability != storage.MetricValueUnavailable {
		t.Errorf("fallback planner metrics = %+v, want one fallback with unavailable event values", metrics)
	}
}

func TestQueryPersistsCopilotTimeoutMetric(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"copilot","copilot":{"timeout":"10ms"}}}`,
		"package.json":                 `{"name":"fixture"}`,
		"src/main.ts":                  "export function main() { return 1; }",
	})
	database := filepath.Join(t.TempDir(), "graph.db")
	if output, err := cliCommand("index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("index workspace: %v\n%s", err, output)
	}
	installBlockingCopilotPlanner(t)
	if output, err := cliCommand("query", "--database", database, "--format", "json", workspace.Root, "Which code handles main work?").CombinedOutput(); err != nil {
		t.Fatalf("query with timed out Copilot planner: %v\n%s", err, output)
	}
	store, err := sqlite.Open(context.Background(), database)
	if err != nil {
		t.Fatalf("open metrics database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	metrics, err := store.ReadCopilotPlannerDailyMetrics(context.Background(), storage.CopilotPlannerDailyMetricsRequest{Day: time.Now().UTC()})
	if err != nil {
		t.Fatalf("read planner metrics: %v", err)
	}
	if len(metrics) != 1 || metrics[0].Timeouts != 1 || metrics[0].OutputTokens.Availability != storage.MetricValueUnavailable || metrics[0].SessionTotalNanoAiu.Availability != storage.MetricValueUnavailable {
		t.Errorf("timeout planner metrics = %+v, want one timeout with unavailable event values", metrics)
	}
}

func hasWarningCode(warnings []query.PlanWarning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}

func installCopilotPlanner(t *testing.T, response string) {
	t.Helper()
	plannerDirectory := t.TempDir()
	plannerPath := filepath.Join(plannerDirectory, "copilot")
	event, err := json.Marshal(map[string]any{"type": "assistant.message", "data": map[string]string{"content": response}})
	if err != nil {
		t.Fatalf("encode Copilot planner event: %v", err)
	}
	plannerScript := "#!/bin/sh\nprintf '%s\\n' '" + string(event) + "' '{\"type\":\"session.idle\",\"data\":{\"outputTokens\":12,\"totalNanoAiu\":80}}'\n"
	if err := os.WriteFile(plannerPath, []byte(plannerScript), 0o755); err != nil {
		t.Fatalf("write Copilot planner: %v", err)
	}
	t.Setenv("PATH", plannerDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func installCopilotPlannerWithUsage(t *testing.T, response string, credits int) {
	t.Helper()
	plannerDirectory := t.TempDir()
	plannerPath := filepath.Join(plannerDirectory, "copilot")
	event, err := json.Marshal(map[string]any{"type": "assistant.message", "data": map[string]string{"content": response}})
	if err != nil {
		t.Fatalf("encode Copilot planner event: %v", err)
	}
	plannerScript := "#!/bin/sh\nusage_file=''\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"--usage-output-file\" ]; then usage_file=$2; shift 2; continue; fi\n  shift\ndone\nprintf '%s\\n' '" + string(event) + "'\nprintf '{\"currentModel\":\"gpt-5\",\"totalPremiumRequestCost\":" + strconv.Itoa(credits) + ",\"modelMetrics\":{\"gpt-5\":{\"usage\":{}}}}' > \"$usage_file\"\n"
	if err := os.WriteFile(plannerPath, []byte(plannerScript), 0o755); err != nil {
		t.Fatalf("write Copilot planner with usage: %v", err)
	}
	t.Setenv("PATH", plannerDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func installFailingCopilotPlanner(t *testing.T) {
	t.Helper()
	plannerDirectory := t.TempDir()
	plannerPath := filepath.Join(plannerDirectory, "copilot")
	if err := os.WriteFile(plannerPath, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write failing Copilot planner: %v", err)
	}
	t.Setenv("PATH", plannerDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func installClaudePlanner(t *testing.T, response string) {
	t.Helper()
	plannerDirectory := t.TempDir()
	plannerPath := filepath.Join(plannerDirectory, "claude")
	envelope, err := json.Marshal(map[string]string{"type": "result", "result": response})
	if err != nil {
		t.Fatalf("encode Claude planner response: %v", err)
	}
	plannerScript := "#!/bin/sh\nprintf '%s\\n' '" + string(envelope) + "'\n"
	if err := os.WriteFile(plannerPath, []byte(plannerScript), 0o755); err != nil {
		t.Fatalf("write Claude planner: %v", err)
	}
	t.Setenv("PATH", plannerDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func installFailingClaudePlanner(t *testing.T) {
	t.Helper()
	plannerDirectory := t.TempDir()
	plannerPath := filepath.Join(plannerDirectory, "claude")
	if err := os.WriteFile(plannerPath, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write failing Claude planner: %v", err)
	}
	t.Setenv("PATH", plannerDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func installBlockingCopilotPlanner(t *testing.T) {
	t.Helper()
	plannerDirectory := t.TempDir()
	plannerPath := filepath.Join(plannerDirectory, "copilot")
	if err := os.WriteFile(plannerPath, []byte("#!/bin/sh\nsleep 1\n"), 0o755); err != nil {
		t.Fatalf("write blocking Copilot planner: %v", err)
	}
	t.Setenv("PATH", plannerDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
}
