package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"agent-wayfinder/cmd/agent-wayfinder/internal/copilot"
	"agent-wayfinder/extractor"
	"agent-wayfinder/index"

	"github.com/spf13/cobra"
)

const catalogConfigurationFile = ".agent-wayfinder/config.json"

type catalogConfiguration struct {
	Copilot               catalogCopilotConfiguration
	Ollama                catalogOllamaConfiguration
	Synopsis              catalogSynopsisConfiguration
	EmbeddingEnabled      bool
	EmbeddingModel        string
	EmbeddingProcessLimit int
}

type catalogCopilotConfiguration struct {
	Enabled      bool
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

type catalogConfigurationFileContents struct {
	Planning              json.RawMessage                           `json:"planning"`
	Synopsis              *catalogSynopsisConfigurationFileContents `json:"synopsis"`
	Copilot               *catalogCopilotConfigurationFileContents  `json:"copilot"`
	Ollama                *catalogOllamaConfigurationFileContents   `json:"ollama"`
	EmbeddingEnabled      *bool                                     `json:"embeddingEnabled"`
	EmbeddingModel        *string                                   `json:"embeddingModel"`
	EmbeddingProcessLimit *int                                      `json:"embeddingProcessLimit"`
}

type catalogCopilotConfigurationFileContents struct {
	Enabled      *bool   `json:"enabled"`
	MaxAICredits *int    `json:"maxAiCredits"`
	ProcessLimit *int    `json:"processLimit"`
	Path         *string `json:"path"`
}

type catalogOllamaConfigurationFileContents struct {
	Model    *string `json:"model"`
	Endpoint *string `json:"endpoint"`
}

type catalogSynopsisConfigurationFileContents struct {
	Provider    *string `json:"provider"`
	SourceLimit *int    `json:"sourceLimit"`
}

func readCatalogConfiguration(workspaceRoot string) (catalogConfiguration, error) {
	configuration := catalogConfiguration{
		Copilot:               catalogCopilotConfiguration{MaxAICredits: copilot.DefaultAICredits, ProcessLimit: 1},
		Ollama:                catalogOllamaConfiguration{Model: "qwen3:8b", Endpoint: index.DefaultOllamaHost},
		Synopsis:              catalogSynopsisConfiguration{SourceLimit: extractor.DefaultCatalogDeclarationSourceLimit},
		EmbeddingModel:        index.DefaultOllamaCatalogEmbeddingModel,
		EmbeddingProcessLimit: index.MaximumEmbeddingProcessLimit,
	}
	contents, err := os.ReadFile(filepath.Join(workspaceRoot, catalogConfigurationFile))
	if os.IsNotExist(err) {
		return configuration, nil
	}
	if err != nil {
		return catalogConfiguration{}, fmt.Errorf("read catalog configuration: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var fileConfiguration catalogConfigurationFileContents
	if err := decoder.Decode(&fileConfiguration); err != nil {
		return catalogConfiguration{}, fmt.Errorf("parse catalog configuration: %w", err)
	}
	if fileConfiguration.Synopsis != nil {
		if fileConfiguration.Synopsis.Provider != nil {
			configuration.Synopsis.Provider = index.CatalogSynopsisProvider(*fileConfiguration.Synopsis.Provider)
		}
		if fileConfiguration.Synopsis.SourceLimit != nil {
			configuration.Synopsis.SourceLimit = *fileConfiguration.Synopsis.SourceLimit
		}
	}
	if fileConfiguration.Copilot != nil {
		if fileConfiguration.Copilot.Enabled != nil {
			configuration.Copilot.Enabled = *fileConfiguration.Copilot.Enabled
		}
		if fileConfiguration.Copilot.MaxAICredits != nil {
			configuration.Copilot.MaxAICredits = *fileConfiguration.Copilot.MaxAICredits
		}
		if fileConfiguration.Copilot.ProcessLimit != nil {
			configuration.Copilot.ProcessLimit = *fileConfiguration.Copilot.ProcessLimit
		}
		if fileConfiguration.Copilot.Path != nil {
			configuration.Copilot.Path = *fileConfiguration.Copilot.Path
		}
	}
	if fileConfiguration.Ollama != nil {
		if fileConfiguration.Ollama.Model != nil {
			configuration.Ollama.Model = *fileConfiguration.Ollama.Model
		}
		if fileConfiguration.Ollama.Endpoint != nil {
			configuration.Ollama.Endpoint = *fileConfiguration.Ollama.Endpoint
		}
	}
	if fileConfiguration.EmbeddingModel != nil {
		configuration.EmbeddingModel = *fileConfiguration.EmbeddingModel
	}
	if fileConfiguration.EmbeddingEnabled != nil {
		configuration.EmbeddingEnabled = *fileConfiguration.EmbeddingEnabled
	}
	if fileConfiguration.EmbeddingProcessLimit != nil {
		configuration.EmbeddingProcessLimit = *fileConfiguration.EmbeddingProcessLimit
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
	if configuration.EmbeddingProcessLimit <= 0 || configuration.EmbeddingProcessLimit > index.MaximumEmbeddingProcessLimit {
		return catalogConfiguration{}, fmt.Errorf("invalid catalog embedding process limit: use an integer from 1 through %d", index.MaximumEmbeddingProcessLimit)
	}
	return configuration, nil
}

func ConfigureFlags(command *cobra.Command) {
	command.Flags().String("catalog-synopsis-provider", "", "catalog synopsis provider: copilot, ollama, or claude")
	command.Flags().Int("catalog-synopsis-source-limit", 0, "maximum declaration source bytes per catalog synopsis")
	command.Flags().Bool("catalog-copilot", false, "enable Copilot catalog synopses")
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
		configuration.Copilot.Enabled, err = flags.GetBool("catalog-copilot")
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
		configuration.EmbeddingEnabled, err = flags.GetBool("catalog-embeddings")
	}
	if err == nil && flags.Changed("catalog-embedding-model") {
		configuration.EmbeddingModel, err = flags.GetString("catalog-embedding-model")
	}
	if err == nil && flags.Changed("catalog-embedding-process-limit") {
		configuration.EmbeddingProcessLimit, err = flags.GetInt("catalog-embedding-process-limit")
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
	if configuration.EmbeddingProcessLimit <= 0 || configuration.EmbeddingProcessLimit > index.MaximumEmbeddingProcessLimit {
		return catalogConfiguration{}, fmt.Errorf("invalid catalog embedding process limit %s: use an integer from 1 through %d", strconv.Itoa(configuration.EmbeddingProcessLimit), index.MaximumEmbeddingProcessLimit)
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
