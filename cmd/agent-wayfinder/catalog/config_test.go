package catalog

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent-wayfinder/extractor"
	"agent-wayfinder/index"
	"agent-wayfinder/testkit"

	"github.com/spf13/cobra"
)

func TestReadCatalogConfigurationReadsWorkspaceAndUsesConservativeDefaults(t *testing.T) {
	configured := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"synopsis":{"provider":"copilot","sourceLimit":512},"copilot":{"enabled":true,"maxAiCredits":42,"processLimit":3,"path":"/opt/bin/copilot"},"ollama":{"model":"catalog-generation:8b","endpoint":"http://localhost:11435"},"embeddingEnabled":true,"embeddingModel":"qwen3-embedding:8b","embeddingProcessLimit":1}`,
	})
	configuration, err := readCatalogConfiguration(configured.Root)
	if err != nil {
		t.Fatalf("read configured catalog configuration: %v", err)
	}
	if configuration.Synopsis.Provider != index.CatalogSynopsisProviderCopilot || configuration.Synopsis.SourceLimit != 512 || !configuration.Copilot.Enabled || configuration.Copilot.MaxAICredits != 42 || configuration.Copilot.ProcessLimit != 3 || configuration.Copilot.Path != "/opt/bin/copilot" || configuration.Ollama.Model != "catalog-generation:8b" || configuration.Ollama.Endpoint != "http://localhost:11435" || !configuration.EmbeddingEnabled || configuration.EmbeddingModel != "qwen3-embedding:8b" || configuration.EmbeddingProcessLimit != 1 {
		t.Errorf("configured catalog configuration = %+v, want workspace values", configuration)
	}

	defaults := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
	})
	configuration, err = readCatalogConfiguration(defaults.Root)
	if err != nil {
		t.Fatalf("read default catalog configuration: %v", err)
	}
	if configuration.Synopsis.Provider != "" || configuration.Synopsis.SourceLimit != extractor.DefaultCatalogDeclarationSourceLimit || configuration.Copilot.Enabled || configuration.Copilot.MaxAICredits != 30 || configuration.Copilot.ProcessLimit != 1 || configuration.Copilot.Path != "" || configuration.Ollama.Model != "qwen3:8b" || configuration.Ollama.Endpoint != index.DefaultOllamaHost || configuration.EmbeddingEnabled || configuration.EmbeddingModel != "qwen3-embedding:4b" || configuration.EmbeddingProcessLimit != index.MaximumEmbeddingProcessLimit {
		t.Errorf("default catalog configuration = %+v, want disabled Copilot and default embedding model", configuration)
	}
}

func TestReadCatalogConfigurationMergesUserAndWorkspaceValues(t *testing.T) {
	user := testkit.NewWorkspace(t, map[string]string{})
	user.WriteFile(t, ".agent-wayfinder/config.json", `{"synopsis":{"provider":"copilot","sourceLimit":512},"copilot":{"enabled":true,"maxAiCredits":42,"processLimit":3,"path":"user-copilot"},"ollama":{"model":"user-model","endpoint":"http://localhost:11435"},"embeddingEnabled":true,"embeddingModel":"user-embedding","embeddingProcessLimit":1}`)
	t.Setenv("HOME", user.Root)
	t.Setenv("USERPROFILE", user.Root)
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"synopsis":{"sourceLimit":1024},"copilot":{"path":"workspace-copilot"},"ollama":{"model":"workspace-model"},"embeddingModel":"workspace-embedding"}`,
	})

	configuration, err := readCatalogConfiguration(workspace.Root)
	if err != nil {
		t.Fatalf("read layered catalog configuration: %v", err)
	}
	if configuration.Synopsis.Provider != index.CatalogSynopsisProviderCopilot || configuration.Synopsis.SourceLimit != 1024 || !configuration.Copilot.Enabled || configuration.Copilot.MaxAICredits != 42 || configuration.Copilot.ProcessLimit != 3 || configuration.Copilot.Path != "workspace-copilot" || configuration.Ollama.Model != "workspace-model" || configuration.Ollama.Endpoint != "http://localhost:11435" || !configuration.EmbeddingEnabled || configuration.EmbeddingModel != "workspace-embedding" || configuration.EmbeddingProcessLimit != 1 {
		t.Errorf("catalog configuration = %+v, want workspace values to override user values", configuration)
	}
}

func TestCatalogFlagsOverrideWorkspaceConfiguration(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"synopsis":{"provider":"copilot","sourceLimit":512},"copilot":{"enabled":true,"maxAiCredits":42,"processLimit":2,"path":"workspace-copilot"}}`,
	})
	command := &cobra.Command{}
	ConfigureFlags(command)
	for name, value := range map[string]string{
		"catalog-synopsis-provider":       "claude",
		"catalog-synopsis-source-limit":   "1024",
		"catalog-copilot":                 "false",
		"catalog-copilot-max-ai-credits":  "60",
		"catalog-copilot-process-limit":   "4",
		"catalog-copilot-path":            "flag-copilot",
		"catalog-ollama-model":            "flag-generation:8b",
		"catalog-ollama-endpoint":         "http://localhost:11435",
		"catalog-embeddings":              "true",
		"catalog-embedding-model":         "qwen3-embedding:8b",
		"catalog-embedding-process-limit": "1",
	} {
		if err := command.Flags().Set(name, value); err != nil {
			t.Fatalf("set --%s: %v", name, err)
		}
	}

	configuration, err := resolveCatalogConfiguration(command, workspace.Root)
	if err != nil {
		t.Fatalf("resolve catalog configuration: %v", err)
	}
	if configuration.Synopsis.Provider != index.CatalogSynopsisProviderClaude || configuration.Synopsis.SourceLimit != 1024 || configuration.Copilot.Enabled || configuration.Copilot.MaxAICredits != 60 || configuration.Copilot.ProcessLimit != 4 || configuration.Copilot.Path != "flag-copilot" || configuration.Ollama.Model != "flag-generation:8b" || configuration.Ollama.Endpoint != "http://localhost:11435" || !configuration.EmbeddingEnabled || configuration.EmbeddingModel != "qwen3-embedding:8b" || configuration.EmbeddingProcessLimit != 1 {
		t.Errorf("catalog configuration = %+v, want flag overrides", configuration)
	}
}

func TestReadCatalogConfigurationRejectsInvalidSynopsisValues(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		contents string
		want     string
	}{
		{
			name:     "provider",
			contents: `{"synopsis":{"provider":"unknown","sourceLimit":512}}`,
			want:     "invalid catalog synopsis provider",
		},
		{
			name:     "source limit",
			contents: `{"synopsis":{"provider":"copilot","sourceLimit":0}}`,
			want:     "invalid catalog synopsis source limit",
		},
		{
			name:     "Copilot max AI credits",
			contents: `{"copilot":{"maxAiCredits":29}}`,
			want:     "invalid catalog Copilot max AI credits",
		},
		{
			name:     "Ollama model",
			contents: `{"ollama":{"model":" "}}`,
			want:     "invalid catalog Ollama model",
		},
		{
			name:     "Ollama endpoint",
			contents: `{"ollama":{"endpoint":"localhost:11434"}}`,
			want:     "invalid catalog Ollama endpoint",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workspace := testkit.NewWorkspace(t, map[string]string{
				".agent-wayfinder/config.json": testCase.contents,
			})
			_, err := readCatalogConfiguration(workspace.Root)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("read catalog configuration error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestCatalogFlagsRejectInvalidSynopsisValues(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		flag  string
		value string
		want  string
	}{
		{name: "provider", flag: "catalog-synopsis-provider", value: "unknown", want: "invalid catalog synopsis provider"},
		{name: "source limit", flag: "catalog-synopsis-source-limit", value: "0", want: "invalid catalog synopsis source limit"},
		{name: "Copilot max AI credits", flag: "catalog-copilot-max-ai-credits", value: "29", want: "invalid catalog Copilot max AI credits"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			command := &cobra.Command{}
			ConfigureFlags(command)
			if err := command.Flags().Set(testCase.flag, testCase.value); err != nil {
				t.Fatalf("set --%s: %v", testCase.flag, err)
			}
			_, err := resolveCatalogConfiguration(command, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("resolve catalog configuration error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestNewCatalogEmbeddingGeneratorUsesConfiguredModelAndOllamaHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/embed" {
			t.Errorf("Ollama request path = %q, want /api/embed", request.URL.Path)
		}
		contents, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read Ollama request: %v", err)
		}
		if string(contents) != `{"model":"qwen3-embedding:8b","input":["validates an access token"],"options":{"num_thread":1},"keep_alive":"0"}` {
			t.Errorf("Ollama request = %s, want configured bounded model and input", contents)
		}
		_, _ = response.Write([]byte(`{"embeddings":[[0.1,0.2]]}`))
	}))
	t.Cleanup(server.Close)
	t.Setenv("OLLAMA_HOST", server.URL)

	generator, err := newCatalogEmbeddingGenerator(catalogConfiguration{EmbeddingEnabled: true, EmbeddingModel: "qwen3-embedding:8b"}, server.Client())
	if err != nil {
		t.Fatalf("create catalog embedding generator: %v", err)
	}
	if _, err := generator.GenerateCatalogEmbedding(context.Background(), "validates an access token"); err != nil {
		t.Fatalf("generate catalog embedding: %v", err)
	}
}

func TestNewCatalogEmbeddingGeneratorUsesDefaultsWhenConfigurationIsUnset(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "")
	generator, err := newCatalogEmbeddingGenerator(catalogConfiguration{}, nil)
	if err != nil {
		t.Fatalf("create default catalog embedding generator: %v", err)
	}
	if generator != nil {
		t.Errorf("default catalog embedding generator = %T, want disabled", generator)
	}
}
