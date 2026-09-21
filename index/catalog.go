package index

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"agent-wayfinder/extractor"
	"agent-wayfinder/storage"
)

type CatalogSynopsisGenerator interface {
	GenerateCatalogSynopsis(context.Context, CatalogSynopsisInput) (string, error)
}

type CatalogSynopsisInput struct {
	Name              string
	Kind              string
	Owner             string
	Signature         string
	DeclarationSource string
	Comments          []string
	IdentifierTokens  []string
}

type CatalogSynopsisProvider string

const (
	CatalogSynopsisProviderCopilot CatalogSynopsisProvider = "copilot"
	CatalogSynopsisProviderOllama  CatalogSynopsisProvider = "ollama"
	CatalogSynopsisProviderClaude  CatalogSynopsisProvider = "claude"
)

type CatalogEmbeddingGenerator interface {
	GenerateCatalogEmbedding(context.Context, string) ([]float32, error)
}

type CatalogEmbeddingBatchGenerator interface {
	GenerateCatalogEmbeddings(context.Context, []string) ([][]float32, error)
	ReleaseCatalogEmbeddingModel(context.Context) error
}

const (
	DefaultOllamaCatalogEmbeddingModel = "qwen3-embedding:4b"
	DefaultOllamaHost                  = "http://127.0.0.1:11434"
	ollamaEmbeddingTimeout             = 10 * time.Second
	ollamaEmbeddingThreadLimit         = 1
	ollamaEmbeddingBatchSize           = 32
	ollamaEmbeddingKeepAlive           = "5m"
	ollamaEmbeddingMaxResponseBytes    = 4 * 1024 * 1024
	MaximumEmbeddingProcessLimit       = 2
)

type OllamaCatalogEmbeddingGenerator struct {
	model    string
	endpoint string
	client   *http.Client
}

func NewOllamaCatalogEmbeddingGenerator(model, endpoint string, client *http.Client) (*OllamaCatalogEmbeddingGenerator, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultOllamaCatalogEmbeddingModel
	}
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		endpoint = DefaultOllamaHost
	}
	parsed, err := url.ParseRequestURI(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("create Ollama embedding generator: endpoint must be an absolute URL")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &OllamaCatalogEmbeddingGenerator{model: model, endpoint: strings.TrimRight(endpoint, "/"), client: client}, nil
}

func (generator *OllamaCatalogEmbeddingGenerator) GenerateCatalogEmbedding(ctx context.Context, text string) ([]float32, error) {
	vectors, err := generator.generateCatalogEmbeddings(ctx, []string{text}, "0")
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (generator *OllamaCatalogEmbeddingGenerator) GenerateCatalogEmbeddings(ctx context.Context, texts []string) ([][]float32, error) {
	return generator.generateCatalogEmbeddings(ctx, texts, ollamaEmbeddingKeepAlive)
}

func (generator *OllamaCatalogEmbeddingGenerator) ReleaseCatalogEmbeddingModel(ctx context.Context) error {
	requestContext, cancel := context.WithTimeout(ctx, ollamaEmbeddingTimeout)
	defer cancel()
	contents, err := json.Marshal(struct {
		Model     string `json:"model"`
		KeepAlive string `json:"keep_alive"`
	}{Model: generator.model, KeepAlive: "0"})
	if err != nil {
		return fmt.Errorf("encode Ollama embedding release: %w", err)
	}
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, generator.endpoint+"/api/generate", bytes.NewReader(contents))
	if err != nil {
		return fmt.Errorf("create Ollama embedding release: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := generator.client.Do(request)
	if err != nil {
		return fmt.Errorf("release Ollama embedding model: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("release Ollama embedding model: status %s", response.Status)
	}
	return nil
}

func (generator *OllamaCatalogEmbeddingGenerator) generateCatalogEmbeddings(ctx context.Context, texts []string, keepAlive string) ([][]float32, error) {
	if len(texts) == 0 || len(texts) > ollamaEmbeddingBatchSize {
		return nil, fmt.Errorf("generate Ollama embedding: use 1 through %d texts", ollamaEmbeddingBatchSize)
	}
	for _, text := range texts {
		if strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("generate Ollama embedding: text is required")
		}
	}
	requestContext, cancel := context.WithTimeout(ctx, ollamaEmbeddingTimeout)
	defer cancel()
	contents, err := json.Marshal(struct {
		Model   string   `json:"model"`
		Input   []string `json:"input"`
		Options struct {
			NumThread int `json:"num_thread"`
		} `json:"options"`
		KeepAlive string `json:"keep_alive"`
	}{
		Model: generator.model,
		Input: texts,
		Options: struct {
			NumThread int `json:"num_thread"`
		}{NumThread: ollamaEmbeddingThreadLimit},
		KeepAlive: keepAlive,
	})
	if err != nil {
		return nil, fmt.Errorf("encode Ollama embedding request: %w", err)
	}
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, generator.endpoint+"/api/embed", bytes.NewReader(contents))
	if err != nil {
		return nil, fmt.Errorf("create Ollama embedding request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := generator.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request Ollama embedding: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("request Ollama embedding: status %s", response.Status)
	}
	responseContents, err := io.ReadAll(io.LimitReader(response.Body, ollamaEmbeddingMaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Ollama embedding response: %w", err)
	}
	if len(responseContents) > ollamaEmbeddingMaxResponseBytes {
		return nil, fmt.Errorf("read Ollama embedding response: exceeds %d bytes", ollamaEmbeddingMaxResponseBytes)
	}
	var payload struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(responseContents, &payload); err != nil {
		return nil, fmt.Errorf("decode Ollama embedding response: %w", err)
	}
	if len(payload.Embeddings) != len(texts) {
		return nil, fmt.Errorf("decode Ollama embedding response: expected %d embeddings", len(texts))
	}
	for _, embedding := range payload.Embeddings {
		if len(embedding) == 0 {
			return nil, fmt.Errorf("decode Ollama embedding response: embeddings must be nonempty")
		}
	}
	return payload.Embeddings, nil
}

type CatalogWriteOptions struct {
	SynopsisGenerator     CatalogSynopsisGenerator
	SynopsisProvider      CatalogSynopsisProvider
	SynopsisSourceLimit   int
	SynopsisProcessLimit  int
	EmbeddingGenerator    CatalogEmbeddingGenerator
	EmbeddingProcessLimit int
}

type CatalogWriteResult struct {
	CopilotUnavailableReason   string
	OllamaUnavailableReason    string
	ClaudeUnavailableReason    string
	EmbeddingUnavailableReason string
}

func WriteDeterministicCatalog(ctx context.Context, writer storage.CatalogWriter, snapshot storage.Snapshot, contributions []extractor.Contribution) error {
	_, err := WriteCatalog(ctx, writer, snapshot, contributions, CatalogWriteOptions{})
	return err
}

func WriteCatalog(ctx context.Context, writer storage.CatalogWriter, snapshot storage.Snapshot, contributions []extractor.Contribution, options CatalogWriteOptions) (CatalogWriteResult, error) {
	if writer == nil {
		return CatalogWriteResult{}, fmt.Errorf("write catalog: catalog writer is required")
	}
	units := make([]extractor.CatalogUnit, 0)
	for _, contribution := range contributions {
		units = append(units, contribution.CatalogUnits()...)
	}
	return writeCatalogUnits(ctx, writer, snapshot, units, options)
}

func writeCatalogUnits(ctx context.Context, writer storage.CatalogWriter, snapshot storage.Snapshot, units []extractor.CatalogUnit, options CatalogWriteOptions) (CatalogWriteResult, error) {
	entries := make([]storage.CatalogEntry, 0, len(units))
	for _, unit := range units {
		entries = append(entries, storage.CatalogEntry{
			NodeID:                unit.NodeID,
			Name:                  unit.Name,
			DeterministicSynopsis: unit.DeterministicSynopsis(),
		})
	}
	if len(entries) == 0 {
		return CatalogWriteResult{}, nil
	}
	result := addSelectedSynopsis(ctx, entries, units, options)
	if err := writer.WriteCatalog(ctx, snapshot, storage.CatalogWriteRequest{Entries: entries}); err != nil {
		return CatalogWriteResult{}, fmt.Errorf("write catalog: %w", err)
	}
	addCatalogEmbeddings(ctx, writer, snapshot, entries, options.EmbeddingGenerator, options.EmbeddingProcessLimit, &result)
	return result, nil
}

func addCatalogEmbeddings(ctx context.Context, writer storage.CatalogWriter, snapshot storage.Snapshot, entries []storage.CatalogEntry, generator CatalogEmbeddingGenerator, processLimit int, result *CatalogWriteResult) {
	if generator == nil {
		return
	}
	embeddingWriter, supported := writer.(storage.CatalogEmbeddingWriter)
	if !supported {
		result.EmbeddingUnavailableReason = "Catalog embedding storage is unavailable"
		return
	}
	embeddingRequests := make([]catalogEmbeddingRequest, 0, len(entries)*4)
	for _, entry := range entries {
		embeddingRequests = append(embeddingRequests, catalogEmbeddingRequest{nodeID: entry.NodeID, source: storage.CatalogEmbeddingDeterministic, text: entry.DeterministicSynopsis})
		if entry.CopilotSynopsis != "" {
			embeddingRequests = append(embeddingRequests, catalogEmbeddingRequest{nodeID: entry.NodeID, source: storage.CatalogEmbeddingCopilot, text: entry.CopilotSynopsis})
		}
		if entry.OllamaSynopsis != "" {
			embeddingRequests = append(embeddingRequests, catalogEmbeddingRequest{nodeID: entry.NodeID, source: storage.CatalogEmbeddingOllama, text: entry.OllamaSynopsis})
		}
		if entry.ClaudeSynopsis != "" {
			embeddingRequests = append(embeddingRequests, catalogEmbeddingRequest{nodeID: entry.NodeID, source: storage.CatalogEmbeddingClaude, text: entry.ClaudeSynopsis})
		}
	}
	embeddings := generateCatalogEmbeddings(ctx, generator, embeddingRequests, processLimit, result)
	if len(embeddings) == 0 {
		return
	}
	if err := embeddingWriter.WriteCatalogEmbeddings(ctx, snapshot, storage.CatalogEmbeddingWriteRequest{Embeddings: embeddings}); err != nil {
		result.EmbeddingUnavailableReason = "Catalog embedding storage is unavailable"
	}
}

type catalogEmbeddingRequest struct {
	nodeID string
	source storage.CatalogEmbeddingSource
	text   string
}

func generateCatalogEmbeddings(ctx context.Context, generator CatalogEmbeddingGenerator, requests []catalogEmbeddingRequest, processLimit int, result *CatalogWriteResult) []storage.CatalogEmbedding {
	batchGenerator, supported := generator.(CatalogEmbeddingBatchGenerator)
	if !supported {
		embeddings := make([]storage.CatalogEmbedding, 0, len(requests))
		for _, request := range requests {
			addCatalogEmbedding(ctx, generator, request.nodeID, request.source, request.text, &embeddings, result)
		}
		return embeddings
	}
	defer func() {
		releaseContext, cancel := context.WithTimeout(context.Background(), ollamaEmbeddingTimeout)
		defer cancel()
		if err := batchGenerator.ReleaseCatalogEmbeddingModel(releaseContext); err != nil {
			result.EmbeddingUnavailableReason = "Catalog embedding model release is unavailable"
		}
	}()
	batchCount := (len(requests) + ollamaEmbeddingBatchSize - 1) / ollamaEmbeddingBatchSize
	if processLimit <= 0 {
		processLimit = 1
	}
	processLimit = min(processLimit, MaximumEmbeddingProcessLimit, batchCount)
	type batchResult struct {
		index   int
		vectors [][]float32
		err     error
	}
	jobs := make(chan int)
	results := make(chan batchResult, batchCount)
	var workers sync.WaitGroup
	workers.Add(processLimit)
	for range processLimit {
		go func() {
			defer workers.Done()
			for batchIndex := range jobs {
				start := batchIndex * ollamaEmbeddingBatchSize
				end := min(start+ollamaEmbeddingBatchSize, len(requests))
				texts := make([]string, end-start)
				for index, request := range requests[start:end] {
					texts[index] = request.text
				}
				vectors, err := batchGenerator.GenerateCatalogEmbeddings(ctx, texts)
				results <- batchResult{index: batchIndex, vectors: vectors, err: err}
			}
		}()
	}
	go func() {
		for batchIndex := range batchCount {
			jobs <- batchIndex
		}
		close(jobs)
		workers.Wait()
		close(results)
	}()
	completed := make([]batchResult, batchCount)
	for completedBatch := range results {
		completed[completedBatch.index] = completedBatch
	}
	embeddings := make([]storage.CatalogEmbedding, 0, len(requests))
	for batchIndex, completedBatch := range completed {
		start := batchIndex * ollamaEmbeddingBatchSize
		end := min(start+ollamaEmbeddingBatchSize, len(requests))
		batch := requests[start:end]
		if completedBatch.err != nil || len(completedBatch.vectors) != len(batch) {
			result.EmbeddingUnavailableReason = "Catalog embedding generation is unavailable"
			continue
		}
		for index, vector := range completedBatch.vectors {
			if len(vector) == 0 {
				result.EmbeddingUnavailableReason = "Catalog embedding generation is unavailable"
				continue
			}
			request := batch[index]
			embeddings = append(embeddings, storage.CatalogEmbedding{NodeID: request.nodeID, Source: request.source, Text: request.text, Vector: vector})
		}
	}
	return embeddings
}

func addCatalogEmbedding(ctx context.Context, generator CatalogEmbeddingGenerator, nodeID string, source storage.CatalogEmbeddingSource, text string, embeddings *[]storage.CatalogEmbedding, result *CatalogWriteResult) {
	vector, err := generator.GenerateCatalogEmbedding(ctx, text)
	if err != nil || len(vector) == 0 {
		result.EmbeddingUnavailableReason = "Catalog embedding generation is unavailable"
		return
	}
	*embeddings = append(*embeddings, storage.CatalogEmbedding{NodeID: nodeID, Source: source, Text: text, Vector: vector})
}

func addSelectedSynopsis(ctx context.Context, entries []storage.CatalogEntry, units []extractor.CatalogUnit, options CatalogWriteOptions) CatalogWriteResult {
	if options.SynopsisGenerator == nil {
		return CatalogWriteResult{}
	}
	provider, store, unavailable := selectedSynopsisDestination(options.SynopsisProvider)
	message := addCatalogSynopses(ctx, entries, boundedSynopsisInputs(units, options.SynopsisSourceLimit), options.SynopsisGenerator, options.SynopsisProcessLimit, provider, store)
	return unavailable(message)
}

func boundedSynopsisInputs(units []extractor.CatalogUnit, sourceLimit int) []CatalogSynopsisInput {
	inputs := make([]CatalogSynopsisInput, len(units))
	for index, unit := range units {
		inputs[index] = CatalogSynopsisInput{
			Name:              unit.Name,
			Kind:              string(unit.Kind),
			Owner:             unit.Owner,
			Signature:         unit.Signature,
			DeclarationSource: unit.DeclarationSource,
			Comments:          append([]string(nil), unit.Comments...),
			IdentifierTokens:  append([]string(nil), unit.IdentifierTokens...),
		}
		if sourceLimit <= 0 {
			inputs[index].DeclarationSource = ""
			continue
		}
		if len(inputs[index].DeclarationSource) > sourceLimit {
			inputs[index].DeclarationSource = inputs[index].DeclarationSource[:sourceLimit]
		}
	}
	return inputs
}

func selectedSynopsisDestination(provider CatalogSynopsisProvider) (string, func(*storage.CatalogEntry, string), func(string) CatalogWriteResult) {
	switch provider {
	case CatalogSynopsisProviderCopilot:
		return "Copilot", func(entry *storage.CatalogEntry, synopsis string) {
				entry.CopilotSynopsis = synopsis
			}, func(message string) CatalogWriteResult {
				return CatalogWriteResult{CopilotUnavailableReason: message}
			}
	case CatalogSynopsisProviderOllama:
		return "Ollama", func(entry *storage.CatalogEntry, synopsis string) {
				entry.OllamaSynopsis = synopsis
			}, func(message string) CatalogWriteResult {
				return CatalogWriteResult{OllamaUnavailableReason: message}
			}
	case CatalogSynopsisProviderClaude:
		return "Claude", func(entry *storage.CatalogEntry, synopsis string) {
				entry.ClaudeSynopsis = synopsis
			}, func(message string) CatalogWriteResult {
				return CatalogWriteResult{ClaudeUnavailableReason: message}
			}
	default:
		return "Catalog", func(*storage.CatalogEntry, string) {}, func(message string) CatalogWriteResult {
			return CatalogWriteResult{CopilotUnavailableReason: message}
		}
	}
}

func addCatalogSynopses(ctx context.Context, entries []storage.CatalogEntry, inputs []CatalogSynopsisInput, generator CatalogSynopsisGenerator, processLimit int, provider string, store func(*storage.CatalogEntry, string)) string {
	if generator == nil {
		return ""
	}
	if processLimit <= 0 {
		processLimit = 1
	}
	processLimit = min(processLimit, len(inputs))
	type synopsisResult struct {
		index    int
		synopsis string
		err      error
	}
	jobs := make(chan int)
	results := make(chan synopsisResult, len(inputs))
	var workers sync.WaitGroup
	workers.Add(processLimit)
	for range processLimit {
		go func() {
			defer workers.Done()
			for index := range jobs {
				synopsis, err := generator.GenerateCatalogSynopsis(ctx, inputs[index])
				results <- synopsisResult{index: index, synopsis: synopsis, err: err}
			}
		}()
	}
	go func() {
		for index := range inputs {
			jobs <- index
		}
		close(jobs)
		workers.Wait()
		close(results)
	}()

	unavailable := ""
	for generated := range results {
		if generated.err != nil {
			unavailable = provider + " synopsis generation is unavailable"
			continue
		}
		store(&entries[generated.index], generated.synopsis)
	}
	return unavailable
}
