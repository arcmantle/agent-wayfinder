package query

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-wayfinder/cmd/agent-wayfinder/internal/claude"
	"agent-wayfinder/cmd/agent-wayfinder/internal/copilot"
	"agent-wayfinder/cmd/agent-wayfinder/internal/ollama"
	"agent-wayfinder/graph"
	"agent-wayfinder/query"
	"agent-wayfinder/storage"
)

type queryResult struct {
	SchemaVersion     int                        `json:"schemaVersion,omitempty"`
	Interpretation    *query.QueryPlan           `json:"interpretation,omitempty"`
	Plan              *query.QueryPlan           `json:"plan,omitempty"`
	Seeds             []query.SeedSet            `json:"seeds"`
	Nodes             []graph.Node               `json:"nodes"`
	Edges             []graph.Edge               `json:"edges"`
	Impact            []query.ImpactEvidence     `json:"impact,omitempty"`
	Evidence          []query.EvidenceGroup      `json:"evidence,omitempty"`
	Limits            []query.StageLimit         `json:"limits,omitempty"`
	Warnings          []query.PlanWarning        `json:"warnings,omitempty"`
	Suggestions       []string                   `json:"suggestions,omitempty"`
	TruncationReasons []storage.TruncationReason `json:"truncationReasons,omitempty"`
	ScopeBoundary     *query.ScopeBoundary       `json:"scopeBoundary,omitempty"`
	Catalog           *catalogResult             `json:"catalog,omitempty"`
	EvidenceGroups    []queryEvidenceGroup       `json:"evidenceGroups,omitempty"`
	Ollama            ollamaQueryMetadata        `json:"ollama"`
	Copilot           copilotQueryMetadata       `json:"copilot"`
	Claude            claudeQueryMetadata        `json:"claude"`
	Planning          queryPlanningMetadata      `json:"planning"`
	MaxDepth          int                        `json:"maxDepth"`
	MaxNodes          int                        `json:"maxNodes"`
}

type queryEvidenceGroup struct {
	Label   string         `json:"label"`
	Graph   *query.Result  `json:"graph,omitempty"`
	Catalog *catalogResult `json:"catalog,omitempty"`
}

type CatalogSynopsisEvidence struct {
	Generator string `json:"generator"`
	Synopsis  string `json:"synopsis"`
}

type catalogMatchResult struct {
	Node     graph.Node                `json:"node"`
	Score    float64                   `json:"score"`
	Evidence []CatalogSynopsisEvidence `json:"evidence"`
}

type catalogResult struct {
	Matches   []catalogMatchResult   `json:"matches"`
	Retrieval query.CatalogRetrieval `json:"retrieval"`
	Warnings  []string               `json:"warnings,omitempty"`
}

type copilotQueryMetadata struct {
	copilot.ConfigurationReport
	Available         bool            `json:"available"`
	UnavailableReason string          `json:"unavailableReason,omitempty"`
	Response          json.RawMessage `json:"response,omitempty"`
}

type ollamaQueryMetadata struct {
	Enabled           bool   `json:"enabled"`
	Model             string `json:"model"`
	Timeout           string `json:"timeout"`
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailableReason,omitempty"`
}

func newOllamaQueryMetadata(configuration ollama.Configuration) ollamaQueryMetadata {
	return ollamaQueryMetadata{
		Enabled: configuration.Enabled,
		Model:   configuration.Model,
		Timeout: configuration.Timeout.String(),
	}
}

type claudeQueryMetadata struct {
	claude.ConfigurationReport
	Available               bool                `json:"available"`
	ActualModel             string              `json:"actualModel,omitempty"`
	UsedFallbackModel       bool                `json:"usedFallbackModel,omitempty"`
	UnavailableReason       string              `json:"unavailableReason,omitempty"`
	DurationNS              int64               `json:"durationNs,omitempty"`
	PromptBytes             int64               `json:"promptBytes,omitempty"`
	ResponseBytes           int64               `json:"responseBytes,omitempty"`
	InputTokens             storage.MetricValue `json:"inputTokens"`
	OutputTokens            storage.MetricValue `json:"outputTokens"`
	APIDurationMilliseconds storage.MetricValue `json:"apiDurationMilliseconds"`
	CostUSD                 storage.DollarValue `json:"costUsd"`
}

type queryPlanningMetadata struct {
	Method string `json:"method"`
}

func resultData(result query.Result, catalog *query.CatalogResult, plan *query.QueryPlan, maxDepth, maxNodes int, ollama ollamaQueryMetadata, copilot copilotQueryMetadata, claude claudeQueryMetadata, planning ...queryPlanningMetadata) queryResult {
	planningMetadata := queryPlanningMetadata{Method: "terms"}
	if len(planning) > 0 {
		planningMetadata = planning[0]
	}
	catalogData := catalogResultData(catalog)
	data := queryResult{
		Plan:              plan,
		Seeds:             result.Seeds,
		Nodes:             result.Facts.Nodes,
		Edges:             result.Facts.Edges,
		Impact:            result.Impact,
		Evidence:          result.Evidence,
		Limits:            result.Limits,
		Warnings:          result.Warnings,
		TruncationReasons: result.TruncationReasons,
		ScopeBoundary:     result.ScopeBoundary,
		Catalog:           catalogData,
		Ollama:            ollama,
		Copilot:           copilot,
		Claude:            claude,
		Planning:          planningMetadata,
		MaxDepth:          maxDepth,
		MaxNodes:          maxNodes,
	}
	if plan != nil {
		data.SchemaVersion = 1
		data.Interpretation = plan
		if plan.Intent == query.IntentUnknown && catalogData != nil {
			data.EvidenceGroups = []queryEvidenceGroup{
				{Label: "graph", Graph: &result},
				{Label: "catalog", Catalog: catalogData},
			}
		}
		for _, warning := range append(append([]query.PlanWarning(nil), plan.Warnings...), result.Warnings...) {
			data.Suggestions = append(data.Suggestions, warning.Suggestions...)
		}
	}
	return data
}

func catalogResultData(result *query.CatalogResult) *catalogResult {
	if result == nil {
		return nil
	}
	data := &catalogResult{
		Matches:   make([]catalogMatchResult, 0, len(result.Matches)),
		Retrieval: result.Retrieval,
		Warnings:  append([]string(nil), result.Warnings...),
	}
	for _, match := range result.Matches {
		data.Matches = append(data.Matches, catalogMatchResult{
			Node:     match.Node,
			Score:    match.Score,
			Evidence: CatalogEvidenceData([]storage.CatalogEntry{match.Entry}),
		})
	}
	return data
}

func CatalogEvidenceData(entries []storage.CatalogEntry) []CatalogSynopsisEvidence {
	evidence := make([]CatalogSynopsisEvidence, 0, len(entries)*2)
	for _, entry := range entries {
		if entry.DeterministicSynopsis != "" {
			evidence = append(evidence, CatalogSynopsisEvidence{Generator: "deterministic", Synopsis: entry.DeterministicSynopsis})
		}
		if entry.CopilotSynopsis != "" {
			evidence = append(evidence, CatalogSynopsisEvidence{Generator: "copilot", Synopsis: entry.CopilotSynopsis})
		}
		if entry.OllamaSynopsis != "" {
			evidence = append(evidence, CatalogSynopsisEvidence{Generator: "ollama", Synopsis: entry.OllamaSynopsis})
		}
		if entry.ClaudeSynopsis != "" {
			evidence = append(evidence, CatalogSynopsisEvidence{Generator: "claude", Synopsis: entry.ClaudeSynopsis})
		}
	}
	return evidence
}

func renderText(result queryResult, showPlan bool) string {
	lines := make([]string, 0)
	if result.Plan != nil && (showPlan || len(result.Plan.Warnings) > 0) {
		lines = append(lines, fmt.Sprintf("Interpreted as: %s (%s, confidence %.2f)", result.Plan.Intent, result.Plan.Operator, result.Plan.Confidence))
		for _, warning := range result.Plan.Warnings {
			if resultWarningPresent(result.Warnings, warning) {
				continue
			}
			lines = append(lines, "Warning: "+warning.Message)
		}
	}
	if result.Copilot.Enabled {
		availability := "unavailable"
		if result.Copilot.Available {
			availability = "available"
		}
		lines = append(lines, "Copilot planner: "+availability+".")
	}
	for _, warning := range result.Warnings {
		lines = append(lines, "Warning: "+warning.Message)
		for _, suggestion := range warning.Suggestions {
			lines = append(lines, "Next: "+suggestion)
		}
	}
	if result.Plan != nil && len(result.Evidence) == 0 && (result.Catalog == nil || len(result.Catalog.Matches) == 0) {
		lines = append(lines, "No answer-ready evidence was found.")
	}
	if len(result.EvidenceGroups) > 0 {
		labels := make([]string, len(result.EvidenceGroups))
		for index, group := range result.EvidenceGroups {
			labels[index] = group.Label
		}
		lines = append(lines, "Evidence groups: "+strings.Join(labels, ", "))
	}
	lines = append(lines, "Seeds:")
	for _, seedSet := range result.Seeds {
		lines = append(lines, seedSet.Term+":")
		for _, node := range seedSet.Nodes {
			lines = append(lines, "- "+node.QualifiedName)
		}
	}
	lines = append(lines, fmt.Sprintf("Nodes: %d", len(result.Nodes)), fmt.Sprintf("Edges: %d", len(result.Edges)))
	if len(result.Impact) > 0 {
		lines = append(lines, "Impact:")
		for _, evidence := range result.Impact {
			lines = append(lines, fmt.Sprintf("- %s via %s (distance %d, score %.4f)", evidence.Node.QualifiedName, evidence.Relation, evidence.Distance, evidence.Score))
		}
	}
	if result.Catalog != nil {
		lines = append(lines, "Catalog:")
		for _, match := range result.Catalog.Matches {
			lines = append(lines, "- "+match.Node.QualifiedName)
		}
	}
	if len(result.TruncationReasons) > 0 {
		reasons := make([]string, len(result.TruncationReasons))
		for index, reason := range result.TruncationReasons {
			reasons[index] = string(reason)
		}
		lines = append(lines, "Truncated: "+strings.Join(reasons, ", "))
	}
	if result.ScopeBoundary != nil {
		lines = append(lines, "Scope boundary: "+result.ScopeBoundary.Node.QualifiedName)
	}
	return strings.Join(lines, "\n")
}

func resultWarningPresent(warnings []query.PlanWarning, target query.PlanWarning) bool {
	for _, warning := range warnings {
		if warning.Code == target.Code && warning.Message == target.Message {
			return true
		}
	}
	return false
}
