package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-wayfinder/index"
	"agent-wayfinder/testkit"
)

func TestReadConfigurationUsesWorkspaceValuesAndDisabledDefaults(t *testing.T) {
	user := testkit.NewWorkspace(t, map[string]string{})
	t.Setenv("HOME", user.Root)
	t.Setenv("USERPROFILE", user.Root)
	t.Setenv("OLLAMA_HOST", "")

	configured := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"ollama","ollama":{"model":"qwen3:8b","endpoint":"http://localhost:11435","timeout":"12s"}}}`,
	})
	configuration, err := ReadConfiguration(configured.Root)
	if err != nil {
		t.Fatalf("read configured Ollama planner configuration: %v", err)
	}
	if !configuration.Enabled || configuration.Model != "qwen3:8b" || configuration.Endpoint != "http://localhost:11435" || configuration.Timeout != 12*time.Second {
		t.Errorf("Ollama planner configuration = %+v, want workspace values", configuration)
	}

	defaults := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
	})
	configuration, err = ReadConfiguration(defaults.Root)
	if err != nil {
		t.Fatalf("read default Ollama planner configuration: %v", err)
	}
	if configuration.Enabled || configuration.Model != "qwen3:8b" || configuration.Endpoint != defaultEndpoint || configuration.Timeout != 30*time.Second {
		t.Errorf("default Ollama planner configuration = %+v, want disabled qwen3:8b defaults", configuration)
	}

	legacy := testkit.NewWorkspace(t, map[string]string{
		".wayfinder": `{"planning":{"provider":"ollama","ollama":{"model":"qwen3:8b"}}}`,
	})
	configuration, err = ReadConfiguration(legacy.Root)
	if err != nil {
		t.Fatalf("read legacy Ollama planner configuration: %v", err)
	}
	if configuration.Enabled || configuration.Model != "qwen3:8b" || configuration.Endpoint != defaultEndpoint || configuration.Timeout != 30*time.Second {
		t.Errorf("legacy Ollama configuration = %+v, want ignored old configuration file", configuration)
	}
}

func TestReadConfigurationMergesUserAndWorkspaceValues(t *testing.T) {
	user := testkit.NewWorkspace(t, map[string]string{})
	user.WriteFile(t, ".agent-wayfinder/config.json", `{"planning":{"provider":"ollama","ollama":{"model":"user-model","timeout":"7s"}}}`)
	t.Setenv("HOME", user.Root)
	t.Setenv("USERPROFILE", user.Root)
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"ollama":{"model":"workspace-model","timeout":"8s"}}}`,
	})

	configuration, err := ReadConfiguration(workspace.Root)
	if err != nil {
		t.Fatalf("read layered Ollama configuration: %v", err)
	}
	if !configuration.Enabled || configuration.Model != "workspace-model" || configuration.Timeout != 8*time.Second {
		t.Errorf("Ollama configuration = %+v, want workspace values to override user values", configuration)
	}
}

func TestRunSendsStructuredQuestionRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/chat" {
			t.Errorf("Ollama planner path = %q, want /api/chat", request.URL.Path)
		}
		var body struct {
			Model     string          `json:"model"`
			Stream    bool            `json:"stream"`
			Think     bool            `json:"think"`
			KeepAlive string          `json:"keep_alive"`
			Format    json.RawMessage `json:"format"`
			Options   struct {
				NumCtx     int `json:"num_ctx"`
				NumPredict int `json:"num_predict"`
				NumThread  int `json:"num_thread"`
			} `json:"options"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode Ollama planner request: %v", err)
		}
		if body.Model != "qwen3:8b" || body.Stream || body.Think || body.KeepAlive != "0" || body.Options.NumCtx != PlannerContextTokens || body.Options.NumPredict != PlannerPredictionTokens || body.Options.NumThread != PlannerThreadLimit || len(body.Format) == 0 || len(body.Messages) != 2 || body.Messages[1].Content != "Which code invokes runQuery?" {
			t.Errorf("Ollama planner request = %+v, want structured non-streaming qwen3 question request", body)
		}
		_, _ = response.Write([]byte(`{"message":{"content":"{\"schemaVersion\":1,\"intent\":\"called_by\",\"entities\":[\"runQuery\"]}"}}`))
	}))
	defer server.Close()

	contents, err := Run(context.Background(), Configuration{Enabled: true, Model: "qwen3:8b", Timeout: time.Second}, "Which code invokes runQuery?", server.URL, server.Client())
	if err != nil {
		t.Fatalf("run Ollama planner: %v", err)
	}
	if got, want := string(contents), `{"schemaVersion":1,"intent":"called_by","entities":["runQuery"]}`; got != want {
		t.Errorf("Ollama planner contents = %s, want %s", got, want)
	}
}

func TestRunReturnsErrorForUnavailableService(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	_, err := Run(context.Background(), Configuration{Model: "qwen3:4b", Timeout: time.Second}, "Which code invokes runQuery?", server.URL, server.Client())
	if err == nil || !strings.Contains(err.Error(), "unexpected HTTP status 503") {
		t.Errorf("Ollama unavailable error = %v, want HTTP 503 error", err)
	}
}

func TestRunRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(response, strings.Repeat("x", PlannerMaxResponseBytes+1))
	}))
	defer server.Close()

	_, err := Run(context.Background(), Configuration{Model: "qwen3:4b", Timeout: time.Second}, "Which code invokes runQuery?", server.URL, server.Client())
	if err == nil || !strings.Contains(err.Error(), "exceeds 8192 bytes") {
		t.Errorf("oversized Ollama response error = %v, want response size error", err)
	}
}

func TestCatalogSynopsisUsesBoundedLocalGeneration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/generate" {
			t.Errorf("Ollama catalog path = %q, want /api/generate", request.URL.Path)
		}
		var body struct {
			Model     string `json:"model"`
			Stream    bool   `json:"stream"`
			Think     bool   `json:"think"`
			KeepAlive string `json:"keep_alive"`
			Prompt    string `json:"prompt"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode Ollama catalog request: %v", err)
		}
		if body.Model != "qwen3:8b" || body.Stream || body.Think || body.KeepAlive != "0" || !strings.Contains(body.Prompt, "Name: ValidateToken") || !strings.Contains(body.Prompt, "Declaration source: func ValidateToken(token string) error { return nil }") {
			t.Errorf("Ollama catalog request = %+v, want bounded synopsis request", body)
		}
		_, _ = response.Write([]byte(`{"response":"Validates an access token."}`))
	}))
	defer server.Close()

	generator := NewCatalogSynopsisGenerator(CatalogSynopsisConfiguration{Model: "qwen3:8b", Timeout: time.Second}, server.URL, server.Client())
	synopsis, err := generator.GenerateCatalogSynopsis(context.Background(), index.CatalogSynopsisInput{
		Name:              "ValidateToken",
		DeclarationSource: "func ValidateToken(token string) error { return nil }",
	})
	if err != nil {
		t.Fatalf("generate Ollama catalog synopsis: %v", err)
	}
	if synopsis != "Validates an access token." {
		t.Errorf("Ollama catalog synopsis = %q, want generated text", synopsis)
	}
}

func TestCatalogSynopsisRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(response, strings.Repeat("x", catalogSynopsisMaxResponseBytes+1))
	}))
	t.Cleanup(server.Close)

	generator := NewCatalogSynopsisGenerator(CatalogSynopsisConfiguration{Model: "qwen3:8b", Timeout: time.Second}, server.URL, server.Client())
	_, err := generator.GenerateCatalogSynopsis(context.Background(), index.CatalogSynopsisInput{Name: "ValidateToken"})
	if err == nil || !strings.Contains(err.Error(), "exceeds 4096 bytes") {
		t.Errorf("Ollama catalog synopsis error = %v, want response size error", err)
	}
}
