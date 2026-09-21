package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const defaultEndpoint = "http://127.0.0.1:11434"

const configurationPath = ".agent-wayfinder/config.json"

const (
	PlannerContextTokens    = 512
	PlannerPredictionTokens = 128
	PlannerThreadLimit      = 1
	PlannerMaxResponseBytes = 8 * 1024
)

type Configuration struct {
	Enabled bool
	Model   string
	Timeout time.Duration
}

type configurationFile struct {
	Enabled *bool           `json:"enabled"`
	Model   *string         `json:"model"`
	Timeout json.RawMessage `json:"timeout"`
}

var modelPattern = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`)

func ReadConfiguration(workspaceRoot string) (Configuration, error) {
	configuration := Configuration{Model: "qwen3:8b", Timeout: 30 * time.Second}
	contents, err := os.ReadFile(filepath.Join(workspaceRoot, configurationPath))
	if os.IsNotExist(err) {
		return configuration, nil
	}
	if err != nil {
		return Configuration{}, fmt.Errorf("read Ollama planner configuration: %w", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(contents, &root); err != nil {
		return Configuration{}, fmt.Errorf("parse Ollama planner configuration: %w", err)
	}
	planningContents, exists := root["planning"]
	if !exists {
		return configuration, nil
	}
	var planning map[string]json.RawMessage
	if err := json.Unmarshal(planningContents, &planning); err != nil {
		return Configuration{}, fmt.Errorf("parse Ollama planner configuration: %w", err)
	}
	plannerContents, exists := planning["ollama"]
	if !exists {
		return configuration, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(plannerContents))
	decoder.DisallowUnknownFields()
	var fileConfiguration configurationFile
	if err := decoder.Decode(&fileConfiguration); err != nil {
		return Configuration{}, fmt.Errorf("parse Ollama planner configuration: %w", err)
	}
	if fileConfiguration.Enabled != nil {
		configuration.Enabled = *fileConfiguration.Enabled
	}
	if fileConfiguration.Model != nil {
		configuration.Model = *fileConfiguration.Model
	}
	if len(fileConfiguration.Timeout) > 0 {
		configuration.Timeout, err = parseJSONTimeout(fileConfiguration.Timeout)
		if err != nil {
			return Configuration{}, fmt.Errorf("invalid planning.ollama.timeout: %w", err)
		}
	}
	if err := validateConfiguration(configuration); err != nil {
		return Configuration{}, err
	}
	return configuration, nil
}

func parseJSONTimeout(value json.RawMessage) (time.Duration, error) {
	var duration string
	if err := json.Unmarshal(value, &duration); err == nil {
		return parseTimeout(duration)
	}
	var seconds int
	if err := json.Unmarshal(value, &seconds); err != nil {
		return 0, fmt.Errorf("use a duration string or integer seconds")
	}
	return parseTimeout(strconv.Itoa(seconds))
}

func parseTimeout(value string) (time.Duration, error) {
	if strings.TrimSpace(value) != value || value == "" {
		return 0, fmt.Errorf("use a duration or integer seconds")
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("use a duration or integer seconds")
	}
	return duration, nil
}

func validateConfiguration(configuration Configuration) error {
	if strings.TrimSpace(configuration.Model) == "" || !modelPattern.MatchString(configuration.Model) {
		return fmt.Errorf("invalid Ollama planner model %q", configuration.Model)
	}
	if configuration.Timeout <= 0 || configuration.Timeout > 30*time.Second {
		return fmt.Errorf("invalid Ollama planner timeout: use a duration from 1ns through 30s")
	}
	return nil
}

type plannerRequest struct {
	Model     string           `json:"model"`
	Messages  []plannerMessage `json:"messages"`
	Format    plannerSchema    `json:"format"`
	Options   plannerOptions   `json:"options"`
	Think     bool             `json:"think"`
	KeepAlive string           `json:"keep_alive"`
	Stream    bool             `json:"stream"`
}

type plannerOptions struct {
	NumCtx     int `json:"num_ctx"`
	NumPredict int `json:"num_predict"`
	NumThread  int `json:"num_thread"`
}

type plannerMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type plannerSchema struct {
	Type                 string                    `json:"type"`
	AdditionalProperties bool                      `json:"additionalProperties"`
	Required             []string                  `json:"required"`
	Properties           map[string]schemaProperty `json:"properties"`
}

type schemaProperty struct {
	Type  string   `json:"type"`
	Const int      `json:"const,omitempty"`
	Enum  []string `json:"enum,omitempty"`
	Items *struct {
		Type string `json:"type"`
	} `json:"items,omitempty"`
}

type plannerResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
}

func Run(parent context.Context, configuration Configuration, question, endpoint string, client *http.Client) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, configuration.Timeout)
	defer cancel()
	requestBody, err := json.Marshal(plannerRequest{
		Model: configuration.Model,
		Messages: []plannerMessage{
			{Role: "system", Content: instructions},
			{Role: "user", Content: question},
		},
		Format:    questionPlanSchema(),
		Options:   plannerOptions{NumCtx: PlannerContextTokens, NumPredict: PlannerPredictionTokens, NumThread: PlannerThreadLimit},
		Think:     false,
		KeepAlive: "0",
		Stream:    false,
	})
	if err != nil {
		return nil, fmt.Errorf("encode Ollama planner request: %w", err)
	}
	if client == nil {
		client = http.DefaultClient
	}
	url := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if url == "" {
		url = defaultEndpoint
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/api/chat", bytes.NewReader(requestBody))
	if err != nil {
		return nil, fmt.Errorf("create Ollama planner request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("run Ollama planner: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("run Ollama planner: unexpected HTTP status %s", response.Status)
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, PlannerMaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Ollama planner response: %w", err)
	}
	if len(contents) > PlannerMaxResponseBytes {
		return nil, fmt.Errorf("read Ollama planner response: exceeds %d bytes", PlannerMaxResponseBytes)
	}
	var responseData plannerResponse
	if err := json.Unmarshal(contents, &responseData); err != nil {
		return nil, fmt.Errorf("parse Ollama planner response: %w", err)
	}
	if strings.TrimSpace(responseData.Message.Content) == "" {
		return nil, fmt.Errorf("parse Ollama planner response: missing message content")
	}
	return []byte(responseData.Message.Content), nil
}

func Endpoint() string {
	endpoint := strings.TrimSpace(os.Getenv("OLLAMA_HOST"))
	if endpoint != "" && !strings.Contains(endpoint, "://") {
		return "http://" + endpoint
	}
	return endpoint
}

const instructions = "Return only JSON that follows the response schema. Select the supported intent that best answers the question, then give the required entity names."

func questionPlanSchema() plannerSchema {
	stringItems := &struct {
		Type string `json:"type"`
	}{Type: "string"}
	return plannerSchema{
		Type:                 "object",
		AdditionalProperties: false,
		Required:             []string{"schemaVersion", "intent", "entities", "confidence"},
		Properties: map[string]schemaProperty{
			"schemaVersion": {Type: "integer", Const: 1},
			"intent":        {Type: "string", Enum: []string{"lookup", "explain", "calls", "called_by", "dependencies", "dependents", "path", "reachability", "shared_contract", "impact"}},
			"entities":      {Type: "array", Items: stringItems},
			"confidence":    {Type: "string", Enum: []string{"high"}},
		},
	}
}
