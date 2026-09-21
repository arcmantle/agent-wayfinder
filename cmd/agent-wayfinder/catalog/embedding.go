package catalog

import (
	"net/http"
	"os"
	"strings"

	"agent-wayfinder/index"
)

func newCatalogEmbeddingGenerator(configuration catalogConfiguration, client *http.Client) (index.CatalogEmbeddingGenerator, error) {
	if !configuration.EmbeddingEnabled {
		return nil, nil
	}
	endpoint := strings.TrimSpace(os.Getenv("OLLAMA_HOST"))
	if endpoint != "" && !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}
	return index.NewOllamaCatalogEmbeddingGenerator(configuration.EmbeddingModel, endpoint, client)
}

func NewEmbeddingGenerator(workspaceRoot string, client *http.Client) (index.CatalogEmbeddingGenerator, error) {
	configuration, err := readCatalogConfiguration(workspaceRoot)
	if err != nil {
		return nil, err
	}
	return newCatalogEmbeddingGenerator(configuration, client)
}
