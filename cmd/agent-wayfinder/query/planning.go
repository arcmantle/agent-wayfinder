package query

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"strings"

	"agent-wayfinder/cmd/agent-wayfinder/internal/claude"
	"agent-wayfinder/cmd/agent-wayfinder/internal/copilot"
	"agent-wayfinder/cmd/agent-wayfinder/internal/ollama"
	"agent-wayfinder/cmd/agent-wayfinder/internal/planning"
	"agent-wayfinder/cmd/agent-wayfinder/internal/spending"
	"agent-wayfinder/query"
	"agent-wayfinder/storage"

	"github.com/spf13/cobra"
)

type queryPlanningResult struct {
	Plan            *query.QueryPlan
	CopilotMetadata copilotQueryMetadata
	ClaudeMetadata  claudeQueryMetadata
	OllamaMetadata  ollamaQueryMetadata
	Metadata        queryPlanningMetadata
	CopilotWarning  *query.PlanWarning
	ClaudeWarning   *query.PlanWarning
	CopilotMetric   *storage.CopilotPlannerMetric
	ClaudeMetric    *storage.ClaudePlannerMetric
}

func planQuery(command *cobra.Command, workspaceRoot string, store storage.SpendReservationStore, plan *query.QueryPlan, maxDepth, maxNodes int, projectIDs []string) (queryPlanningResult, error) {
	copilotReport, err := copilot.ResolveConfiguration(command, workspaceRoot)
	if err != nil {
		return queryPlanningResult{}, err
	}
	claudeReport, err := claude.ResolveConfigurationReport(command, workspaceRoot)
	if err != nil {
		return queryPlanningResult{}, err
	}
	claudeConfiguration, err := claudeReport.PlannerConfiguration()
	if err != nil {
		return queryPlanningResult{}, err
	}
	ollamaConfiguration, err := ollama.ReadConfiguration(workspaceRoot)
	if err != nil {
		return queryPlanningResult{}, err
	}
	spendingConfiguration, err := spending.ReadConfiguration(workspaceRoot)
	if err != nil {
		return queryPlanningResult{}, err
	}
	provider, err := planning.ReadProvider(workspaceRoot)
	if err != nil {
		return queryPlanningResult{}, err
	}
	if provider != "" {
		ollamaConfiguration.Enabled = provider == planning.ProviderOllama
		claudeConfiguration.Enabled = provider == planning.ProviderClaude
		claudeReport.Enabled = claudeConfiguration.Enabled
		copilotReport.Enabled = provider == planning.ProviderCopilot
	}
	result := queryPlanningResult{
		Plan:            plan,
		CopilotMetadata: copilotQueryMetadata{ConfigurationReport: copilotReport},
		ClaudeMetadata:  claudeQueryMetadata{ConfigurationReport: claudeReport},
		OllamaMetadata:  newOllamaQueryMetadata(ollamaConfiguration),
		Metadata:        queryPlanningMetadata{Method: "terms"},
	}
	if plan == nil {
		return result, nil
	}
	result.Metadata.Method = "deterministic"
	if plan.Intent != query.IntentUnknown {
		return result, nil
	}

	result.Metadata.Method = "fallback"
	if ollamaConfiguration.Enabled {
		response, err := ollama.Run(context.Background(), ollamaConfiguration, plan.Question, ollamaConfiguration.Endpoint, http.DefaultClient)
		if err != nil {
			result.OllamaMetadata.UnavailableReason = ollamaFallbackReason(err)
		} else if planned, err := query.ParseLocalPlannerResponse(response); err != nil {
			result.OllamaMetadata.UnavailableReason = ollamaFallbackReason(err)
		} else {
			result.Plan = applyPlannedQuestion(plan, planned, maxDepth, maxNodes, projectIDs)
			result.Metadata.Method = "ollama"
			result.OllamaMetadata.Available = true
		}
	}
	if result.Metadata.Method != "ollama" && claudeConfiguration.Enabled {
		reserved, reserveErr := spendingConfiguration.Reserve(context.Background(), store, spending.ProviderClaude, claudeConfiguration.MaxBudgetUSD, 0)
		if reserveErr != nil {
			warning := spendingFallbackWarning("Claude")
			result.ClaudeWarning = &warning
			result.ClaudeMetadata.UnavailableReason = warning.Message
		} else {
			claudeConfiguration.MaxBudgetUSD = reserved.Amount
			claudeRun, err := claude.Run(context.Background(), claudeConfiguration, plan.Question, exec.CommandContext)
			if claudeRun.CostUSD.Availability == storage.MetricValueExact {
				if settleErr := spendingConfiguration.Settle(context.Background(), store, reserved, claudeRun.CostUSD.Value); settleErr != nil {
					return queryPlanningResult{}, settleErr
				}
			}
			result.ClaudeMetadata.ActualModel = claudeRun.ActualModel
			result.ClaudeMetadata.UsedFallbackModel = claudeRun.UsedFallbackModel
			result.ClaudeMetadata.DurationNS = claudeRun.Duration.Nanoseconds()
			result.ClaudeMetadata.PromptBytes = claudeRun.PromptBytes
			result.ClaudeMetadata.ResponseBytes = claudeRun.ResponseBytes
			result.ClaudeMetadata.InputTokens = claudeRun.InputTokens
			result.ClaudeMetadata.OutputTokens = claudeRun.OutputTokens
			result.ClaudeMetadata.APIDurationMilliseconds = claudeRun.APIDurationMilliseconds
			result.ClaudeMetadata.CostUSD = claudeRun.CostUSD
			if err != nil {
				outcome := storage.ClaudePlannerOutcomeUnavailable
				if errors.Is(err, claude.ErrTimeout) {
					outcome = storage.ClaudePlannerOutcomeTimeout
				} else if errors.Is(err, claude.ErrBudgetExceeded) {
					outcome = storage.ClaudePlannerOutcomeBudget
				} else if errors.Is(err, claude.ErrModelUnavailable) {
					outcome = storage.ClaudePlannerOutcomeModelUnavailable
				} else if strings.HasPrefix(err.Error(), "parse Claude planner response:") {
					outcome = storage.ClaudePlannerOutcomeFallback
				}
				metric := claude.NewPlannerMetric(claudeConfiguration, claudeRun, outcome)
				result.ClaudeMetric = &metric
				warning := claudeFallbackWarning(err)
				result.ClaudeWarning = &warning
				result.ClaudeMetadata.UnavailableReason = warning.Message
			} else if planned, err := query.ParseClaudePlannerResponse(claudeRun.Response); err != nil {
				metric := claude.NewPlannerMetric(claudeConfiguration, claudeRun, storage.ClaudePlannerOutcomeFallback)
				result.ClaudeMetric = &metric
				warning := claudeFallbackWarning(err)
				result.ClaudeWarning = &warning
				result.ClaudeMetadata.UnavailableReason = warning.Message
			} else {
				metric := claude.NewPlannerMetric(claudeConfiguration, claudeRun, storage.ClaudePlannerOutcomeSuccess)
				result.ClaudeMetric = &metric
				result.Plan = applyPlannedQuestion(plan, planned, maxDepth, maxNodes, projectIDs)
				result.Metadata.Method = "claude"
				result.ClaudeMetadata.Available = true
			}
		}
	}
	if result.Metadata.Method != "ollama" && copilotReport.Enabled {
		plannerConfiguration, err := copilotReport.PlannerConfiguration()
		if err != nil {
			return queryPlanningResult{}, err
		}
		reservation, reserveErr := spendingConfiguration.Reserve(context.Background(), store, spending.ProviderCopilot, float64(plannerConfiguration.MaxAICredits), copilot.MinimumAICredits)
		if reserveErr != nil {
			warning := spendingFallbackWarning("Copilot")
			result.CopilotWarning = &warning
			result.CopilotMetadata.UnavailableReason = warning.Message
			return result, nil
		}
		plannerConfiguration.MaxAICredits = int(reservation.Amount)
		if reservation.Amount != float64(copilotReport.MaxAICredits) {
			plannerConfiguration.UseAutomaticFallback = false
		}
		run, err := copilot.RunPlannerWithMetrics(context.Background(), plannerConfiguration, plan.Question, exec.CommandContext)
		if run.UsageAvailable {
			if settleErr := spendingConfiguration.Settle(context.Background(), store, reservation, float64(run.Usage.PremiumRequestCredits)); settleErr != nil {
				return queryPlanningResult{}, settleErr
			}
		}
		if err != nil {
			outcome := storage.CopilotPlannerOutcomeFallback
			if errors.Is(err, copilot.ErrPlannerTimeout) {
				outcome = storage.CopilotPlannerOutcomeTimeout
			}
			metric := copilot.NewPlannerMetric(plannerConfiguration, run, outcome)
			result.CopilotMetric = &metric
			warning := copilotFallbackWarning(err)
			result.CopilotWarning = &warning
			result.CopilotMetadata.UnavailableReason = warning.Message
		} else if planned, err := query.ParseCopilotPlannerResponse(run.Response); err != nil {
			metric := copilot.NewPlannerMetric(plannerConfiguration, run, storage.CopilotPlannerOutcomeFallback)
			result.CopilotMetric = &metric
			warning := copilotFallbackWarning(err)
			result.CopilotWarning = &warning
			result.CopilotMetadata.UnavailableReason = warning.Message
		} else {
			metric := copilot.NewPlannerMetric(plannerConfiguration, run, storage.CopilotPlannerOutcomeSuccess)
			result.CopilotMetric = &metric
			result.Plan = applyPlannedQuestion(plan, planned, maxDepth, maxNodes, projectIDs)
			result.Metadata.Method = "copilot"
			result.CopilotMetadata.Available = true
			result.CopilotMetadata.Response = append(json.RawMessage(nil), run.Response...)
		}
	}
	return result, nil
}

func applyPlannedQuestion(original *query.QueryPlan, planned query.QueryPlan, maxDepth, maxNodes int, projectIDs []string) *query.QueryPlan {
	planned.Question = original.Question
	planned.MaxDepth = maxDepth
	planned.MaxNodes = maxNodes
	for index := range planned.EntitySlots {
		planned.EntitySlots[index].Retrieval.ProjectIDs = append([]string(nil), projectIDs...)
	}
	return &planned
}

func ollamaFallbackReason(err error) string {
	if strings.HasPrefix(err.Error(), "parse local planner response:") {
		return "Ollama planner returned an invalid response."
	}
	return "Ollama planner is unavailable."
}

func copilotFallbackWarning(err error) query.PlanWarning {
	message := "Copilot planner is unavailable. Used deterministic query results."
	switch {
	case errors.Is(err, copilot.ErrPlannerTimeout):
		message = "Copilot planner timed out. Used deterministic query results."
	case errors.Is(err, copilot.ErrPromptBudgetExceeded), errors.Is(err, copilot.ErrResponseBudgetExceeded):
		message = "Copilot planner exceeded its token budget. Used deterministic query results."
	case strings.HasPrefix(err.Error(), "parse Copilot planner response:"):
		message = "Copilot planner returned an invalid response. Used deterministic query results."
	}
	return query.PlanWarning{Code: "copilot_unavailable", Message: message}
}

func claudeFallbackWarning(err error) query.PlanWarning {
	message := "Claude planner is unavailable. Used the next configured planner or deterministic query results."
	switch {
	case errors.Is(err, claude.ErrTimeout):
		message = "Claude planner timed out. Used the next configured planner or deterministic query results."
	case errors.Is(err, claude.ErrBudgetExceeded):
		message = "Claude planner exceeded its budget. Used the next configured planner or deterministic query results."
	case errors.Is(err, claude.ErrModelUnavailable):
		message = "Claude planner model is unavailable. Used the next configured planner or deterministic query results."
	case strings.HasPrefix(err.Error(), "parse Claude planner response:"):
		message = "Claude planner returned an invalid response. Used the next configured planner or deterministic query results."
	}
	return query.PlanWarning{Code: "claude_unavailable", Message: message}
}

func spendingFallbackWarning(provider string) query.PlanWarning {
	message := provider + " spending limit prevented this request. Used the next configured planner or deterministic query results."
	if provider == "Copilot" {
		message = "Copilot spending limit prevented this request. Used deterministic query results."
	}
	return query.PlanWarning{Code: strings.ToLower(provider) + "_spending_limit", Message: message}
}
