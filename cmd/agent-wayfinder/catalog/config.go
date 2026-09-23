package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"agent-wayfinder/cmd/agent-wayfinder/internal/claude"
	configpath "agent-wayfinder/cmd/agent-wayfinder/internal/configuration"
	"agent-wayfinder/cmd/agent-wayfinder/internal/copilot"
	"agent-wayfinder/extractor"
	"agent-wayfinder/index"

	"github.com/spf13/cobra"
)

type catalogConfiguration struct {
	Copilot   catalogCopilotConfiguration
	Ollama    catalogOllamaConfiguration
	Claude    claude.Configuration
	Synopsis  catalogSynopsisConfiguration
	Embedding catalogEmbeddingConfiguration
}

type catalogCopilotConfiguration struct {
	Model        string
	MaxAICredits int
	ProcessLimit int
	Path         string
}

type catalogOllamaConfiguration struct {
	Model    string
	Endpoint string
}

type catalogSynopsisConfiguration struct {
	Provider    index.CatalogSynopsisProvider
	SourceLimit int
}

type catalogEmbeddingConfiguration struct {
	Enabled      bool
	Model        string
	ProcessLimit int
}

type catalogConfigurationFileContents struct {
	Planning  json.RawMessage                            `json:"planning"`
	Spending  json.RawMessage                            `json:"spending"`
	Sources   json.RawMessage                            `json:"sources"`
	Synopsis  *catalogSynopsisConfigurationFileContents  `json:"synopsis"`
	Embedding *catalogEmbeddingConfigurationFileContents `json:"embedding"`
}

type catalogCopilotConfigurationFileContents struct {
	Model        *string `json:"model"`
	MaxAICredits *int    `json:"maxAiCredits"`
	ProcessLimit *int    `json:"processLimit"`
	Path         *string `json:"path"`
}

type catalogOllamaConfigurationFileContents struct {
	Model    *string `json:"model"`
	Endpoint *string `json:"endpoint"`
}

type catalogSynopsisConfigurationFileContents struct {
	Provider    *string                                  `json:"provider"`
	SourceLimit *int                                     `json:"sourceLimit"`
	Copilot     *catalogCopilotConfigurationFileContents `json:"copilot"`
	Ollama      *catalogOllamaConfigurationFileContents  `json:"ollama"`
	Claude      *catalogClaudeConfigurationFileContents  `json:"claude"`
}

type catalogClaudeConfigurationFileContents struct {
	Path          *string         `json:"path"`
	Model         *string         `json:"model"`
	FallbackModel *string         `json:"fallbackModel"`
	MaxBudgetUSD  *float64        `json:"maxBudgetUsd"`
	Effort        *string         `json:"effort"`
	Timeout       json.RawMessage `json:"timeout"`
}

type catalogEmbeddingConfigurationFileContents struct {
	Enabled      *bool   `json:"enabled"`
	Model        *string `json:"model"`
	ProcessLimit *int    `json:"processLimit"`
}

func readCatalogConfiguration(workspaceRoot string) (catalogConfiguration, error) {
	configuration := catalogConfiguration{
		Copilot:   catalogCopilotConfiguration{MaxAICredits: copilot.DefaultAICredits, ProcessLimit: 1},
		Ollama:    catalogOllamaConfiguration{Model: "qwen3:8b", Endpoint: index.DefaultOllamaHost},
		Claude:    claude.Configuration{Path: claude.DefaultPlannerPath, Model: claude.DefaultPlannerModel, Timeout: 30 * time.Second},
		Synopsis:  catalogSynopsisConfiguration{SourceLimit: extractor.DefaultCatalogDeclarationSourceLimit},
		Embedding: catalogEmbeddingConfiguration{Model: index.DefaultOllamaCatalogEmbeddingModel, ProcessLimit: index.MaximumEmbeddingProcessLimit},
	}
	paths := configpath.Paths(workspaceRoot)
	for _, path := range paths {
		fileConfiguration, err := readCatalogConfigurationFile(path)
		if err != nil {
			return catalogConfiguration{}, err
		}
		if err := applyCatalogConfiguration(&configuration, fileConfiguration); err != nil {
			return catalogConfiguration{}, err
		}
	}
	if configuration.Copilot.ProcessLimit <= 0 {
		return catalogConfiguration{}, fmt.Errorf("invalid catalog Copilot process limit: use a positive integer")
	}
	if configuration.Copilot.MaxAICredits < copilot.MinimumAICredits {
		return catalogConfiguration{}, fmt.Errorf("invalid catalog Copilot max AI credits: use an integer of at least %d", copilot.MinimumAICredits)
	}
	if err := validateCatalogSynopsisConfiguration(configuration.Synopsis); err != nil {
		return catalogConfiguration{}, err
	}
	if err := validateCatalogOllamaConfiguration(configuration.Ollama); err != nil {
		return catalogConfiguration{}, err
	}
	if err := claude.ValidateConfiguration(configuration.Claude); err != nil {
		return catalogConfiguration{}, fmt.Errorf("invalid catalog Claude configuration: %w", err)
	}
	if configuration.Embedding.ProcessLimit <= 0 || configuration.Embedding.ProcessLimit > index.MaximumEmbeddingProcessLimit {
		return catalogConfiguration{}, fmt.Errorf("invalid catalog embedding process limit: use an integer from 1 through %d", index.MaximumEmbeddingProcessLimit)
	}
	return configuration, nil
}

func readCatalogConfigurationFile(path string) (catalogConfigurationFileContents, error) {
	contents, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return catalogConfigurationFileContents{}, nil
	}
	if err != nil {
		return catalogConfigurationFileContents{}, fmt.Errorf("read catalog configuration: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var configuration catalogConfigurationFileContents
	if err := decoder.Decode(&configuration); err != nil {
		return catalogConfigurationFileContents{}, fmt.Errorf("parse catalog configuration: %w", err)
	}
	return configuration, nil
}

func applyCatalogConfiguration(configuration *catalogConfiguration, fileConfiguration catalogConfigurationFileContents) error {
	if fileConfiguration.Synopsis != nil {
		if fileConfiguration.Synopsis.Provider != nil {
			configuration.Synopsis.Provider = index.CatalogSynopsisProvider(*fileConfiguration.Synopsis.Provider)
		}
		if fileConfiguration.Synopsis.SourceLimit != nil {
			configuration.Synopsis.SourceLimit = *fileConfiguration.Synopsis.SourceLimit
		}
		if fileConfiguration.Synopsis.Copilot != nil {
			if fileConfiguration.Synopsis.Copilot.Model != nil {
				configuration.Copilot.Model = *fileConfiguration.Synopsis.Copilot.Model
			}
			if fileConfiguration.Synopsis.Copilot.MaxAICredits != nil {
				configuration.Copilot.MaxAICredits = *fileConfiguration.Synopsis.Copilot.MaxAICredits
			}
			if fileConfiguration.Synopsis.Copilot.ProcessLimit != nil {
				configuration.Copilot.ProcessLimit = *fileConfiguration.Synopsis.Copilot.ProcessLimit
			}
			if fileConfiguration.Synopsis.Copilot.Path != nil {
				configuration.Copilot.Path = *fileConfiguration.Synopsis.Copilot.Path
			}
		}
		if fileConfiguration.Synopsis.Ollama != nil {
			if fileConfiguration.Synopsis.Ollama.Model != nil {
				configuration.Ollama.Model = *fileConfiguration.Synopsis.Ollama.Model
			}
			if fileConfiguration.Synopsis.Ollama.Endpoint != nil {
				configuration.Ollama.Endpoint = *fileConfiguration.Synopsis.Ollama.Endpoint
			}
		}
		if fileConfiguration.Synopsis.Claude != nil {
			if fileConfiguration.Synopsis.Claude.Path != nil {
				configuration.Claude.Path = *fileConfiguration.Synopsis.Claude.Path
			}
			if fileConfiguration.Synopsis.Claude.Model != nil {
				configuration.Claude.Model = *fileConfiguration.Synopsis.Claude.Model
			}
			if fileConfiguration.Synopsis.Claude.FallbackModel != nil {
				configuration.Claude.FallbackModel = *fileConfiguration.Synopsis.Claude.FallbackModel
			}
			if fileConfiguration.Synopsis.Claude.MaxBudgetUSD != nil {
				configuration.Claude.MaxBudgetUSD = *fileConfiguration.Synopsis.Claude.MaxBudgetUSD
			}
			if fileConfiguration.Synopsis.Claude.Effort != nil {
				configuration.Claude.Effort = *fileConfiguration.Synopsis.Claude.Effort
			}
			if len(fileConfiguration.Synopsis.Claude.Timeout) != 0 {
				timeout, err := parseCatalogClaudeTimeout(fileConfiguration.Synopsis.Claude.Timeout)
				if err != nil {
					return fmt.Errorf("invalid synopsis.claude.timeout: %w", err)
				}
				configuration.Claude.Timeout = timeout
			}
		}
	}
	if fileConfiguration.Embedding != nil {
		if fileConfiguration.Embedding.Enabled != nil {
			configuration.Embedding.Enabled = *fileConfiguration.Embedding.Enabled
		}
		if fileConfiguration.Embedding.Model != nil {
			configuration.Embedding.Model = *fileConfiguration.Embedding.Model
		}
		if fileConfiguration.Embedding.ProcessLimit != nil {
			configuration.Embedding.ProcessLimit = *fileConfiguration.Embedding.ProcessLimit
		}
	}
	return nil
}

func parseCatalogClaudeTimeout(value json.RawMessage) (time.Duration, error) {
	var duration string
	if err := json.Unmarshal(value, &duration); err == nil {
		return time.ParseDuration(duration)
	}
	var seconds int
	if err := json.Unmarshal(value, &seconds); err != nil {
		return 0, fmt.Errorf("use a duration string or integer seconds")
	}
	return time.Duration(seconds) * time.Second, nil
}

func ConfigureFlags(command *cobra.Command) {
	command.Flags().String("catalog-synopsis-provider", "", "catalog synopsis provider: copilot, ollama, or claude")
	command.Flags().Int("catalog-synopsis-source-limit", 0, "maximum declaration source bytes per catalog synopsis")
	command.Flags().Bool("catalog-copilot", false, "enable Copilot catalog synopses")
	command.Flags().String("catalog-copilot-model", "", "Copilot model for catalog synopses")
	command.Flags().Int("catalog-copilot-max-ai-credits", 0, "maximum Copilot AI credits per catalog synopsis")
	command.Flags().Int("catalog-copilot-process-limit", 0, "maximum concurrent Copilot catalog processes")
	command.Flags().String("catalog-copilot-path", "", "Copilot command path for catalog synopses")
	command.Flags().String("catalog-ollama-model", "", "Ollama model for catalog synopses")
	command.Flags().String("catalog-ollama-endpoint", "", "Ollama endpoint for catalog synopses")
	command.Flags().Bool("catalog-embeddings", false, "enable local Ollama catalog embeddings")
	command.Flags().String("catalog-embedding-model", "", "Ollama model for catalog embeddings")
	command.Flags().Int("catalog-embedding-process-limit", 0, "maximum concurrent Ollama catalog embedding requests")
}

func resolveCatalogConfiguration(command *cobra.Command, workspaceRoot string) (catalogConfiguration, error) {
	configuration, err := readCatalogConfiguration(workspaceRoot)
	if err != nil {
		return catalogConfiguration{}, err
	}
	flags := command.Flags()
	if flags.Changed("catalog-synopsis-provider") {
		provider, providerErr := flags.GetString("catalog-synopsis-provider")
		if providerErr != nil {
			return catalogConfiguration{}, providerErr
		}
		configuration.Synopsis.Provider = index.CatalogSynopsisProvider(provider)
	}
	if flags.Changed("catalog-synopsis-source-limit") {
		configuration.Synopsis.SourceLimit, err = flags.GetInt("catalog-synopsis-source-limit")
	}
	if err == nil && flags.Changed("catalog-copilot") {
		var enabled bool
		enabled, err = flags.GetBool("catalog-copilot")
		if enabled {
			configuration.Synopsis.Provider = index.CatalogSynopsisProviderCopilot
		}
	}
	if err == nil && flags.Changed("catalog-copilot-model") {
		configuration.Copilot.Model, err = flags.GetString("catalog-copilot-model")
	}
	if err == nil && flags.Changed("catalog-copilot-max-ai-credits") {
		configuration.Copilot.MaxAICredits, err = flags.GetInt("catalog-copilot-max-ai-credits")
	}
	if err == nil && flags.Changed("catalog-copilot-process-limit") {
		configuration.Copilot.ProcessLimit, err = flags.GetInt("catalog-copilot-process-limit")
	}
	if err == nil && flags.Changed("catalog-copilot-path") {
		configuration.Copilot.Path, err = flags.GetString("catalog-copilot-path")
	}
	if err == nil && flags.Changed("catalog-ollama-model") {
		configuration.Ollama.Model, err = flags.GetString("catalog-ollama-model")
	}
	if err == nil && flags.Changed("catalog-ollama-endpoint") {
		configuration.Ollama.Endpoint, err = flags.GetString("catalog-ollama-endpoint")
	}
	if err == nil && flags.Changed("catalog-embeddings") {
		configuration.Embedding.Enabled, err = flags.GetBool("catalog-embeddings")
	}
	if err == nil && flags.Changed("catalog-embedding-model") {
		configuration.Embedding.Model, err = flags.GetString("catalog-embedding-model")
	}
	if err == nil && flags.Changed("catalog-embedding-process-limit") {
		configuration.Embedding.ProcessLimit, err = flags.GetInt("catalog-embedding-process-limit")
	}
	if err != nil {
		return catalogConfiguration{}, err
	}
	if configuration.Copilot.ProcessLimit <= 0 {
		return catalogConfiguration{}, fmt.Errorf("invalid catalog Copilot process limit %s: use a positive integer", strconv.Itoa(configuration.Copilot.ProcessLimit))
	}
	if configuration.Copilot.MaxAICredits < copilot.MinimumAICredits {
		return catalogConfiguration{}, fmt.Errorf("invalid catalog Copilot max AI credits %s: use an integer of at least %d", strconv.Itoa(configuration.Copilot.MaxAICredits), copilot.MinimumAICredits)
	}
	if err := validateCatalogSynopsisConfiguration(configuration.Synopsis); err != nil {
		return catalogConfiguration{}, err
	}
	if err := validateCatalogOllamaConfiguration(configuration.Ollama); err != nil {
		return catalogConfiguration{}, err
	}
	if configuration.Embedding.ProcessLimit <= 0 || configuration.Embedding.ProcessLimit > index.MaximumEmbeddingProcessLimit {
		return catalogConfiguration{}, fmt.Errorf("invalid catalog embedding process limit %s: use an integer from 1 through %d", strconv.Itoa(configuration.Embedding.ProcessLimit), index.MaximumEmbeddingProcessLimit)
	}
	return configuration, nil
}

func validateCatalogSynopsisConfiguration(configuration catalogSynopsisConfiguration) error {
	switch configuration.Provider {
	case "", index.CatalogSynopsisProviderCopilot, index.CatalogSynopsisProviderOllama, index.CatalogSynopsisProviderClaude:
	default:
		return fmt.Errorf("invalid catalog synopsis provider %q: use copilot, ollama, or claude", configuration.Provider)
	}
	if configuration.SourceLimit <= 0 {
		return fmt.Errorf("invalid catalog synopsis source limit: use a positive integer")
	}
	return nil
}

func validateCatalogOllamaConfiguration(configuration catalogOllamaConfiguration) error {
	if strings.TrimSpace(configuration.Model) == "" {
		return fmt.Errorf("invalid catalog Ollama model: use a nonempty model name")
	}
	endpoint, err := url.ParseRequestURI(configuration.Endpoint)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return fmt.Errorf("invalid catalog Ollama endpoint: use an absolute URL")
	}
	return nil
}
