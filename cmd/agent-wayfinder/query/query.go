package query

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"agent-wayfinder/cli"
	"agent-wayfinder/cmd/agent-wayfinder/catalog"
	cmd "agent-wayfinder/cmd/agent-wayfinder/internal/command"
	"agent-wayfinder/graph"
	"agent-wayfinder/query"
	"agent-wayfinder/storage"
	"agent-wayfinder/storage/sqlite"

	"github.com/spf13/cobra"
)

func run(command *cobra.Command, arguments []string, standardOutput, standardError io.Writer) int {
	if len(arguments) < 2 {
		return cmd.WriteError(standardError, cli.NewInvalidArgumentError("query requires one workspace path and at least one term"))
	}
	format, err := cmd.Format(command)
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	maxDepth, err := command.Flags().GetInt("max-depth")
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	maxNodes, err := command.Flags().GetInt("max-nodes")
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	projectIDs, err := command.Flags().GetStringArray("project")
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	relations, err := command.Flags().GetStringArray("relation")
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	forceQuestion, err := command.Flags().GetBool("question")
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	forceTerms, err := command.Flags().GetBool("terms")
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	showPlan, err := command.Flags().GetBool("show-plan")
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	if forceQuestion && forceTerms {
		return cmd.WriteError(standardError, cli.NewInvalidArgumentError("--question and --terms cannot be used together"))
	}

	workspace, terms := arguments[0], arguments[1:]
	questionMode := forceQuestion || !forceTerms && isQuestionArgument(terms)
	var plan *query.QueryPlan
	if questionMode {
		if len(terms) != 1 {
			return cmd.WriteError(standardError, cli.NewInvalidArgumentError("question mode requires exactly one question argument"))
		}
		analyzed := query.AnalyzeQuestion(terms[0])
		analyzed.MaxDepth = maxDepth
		analyzed.MaxNodes = maxNodes
		for index := range analyzed.EntitySlots {
			analyzed.EntitySlots[index].Retrieval.ProjectIDs = append([]string(nil), projectIDs...)
		}
		plan = &analyzed
		terms = nil
	}
	workspaceRoot, err := filepath.Abs(workspace)
	if err != nil {
		return cmd.WriteError(standardError, fmt.Errorf("resolve query workspace path: %w", err))
	}
	planning, err := planQuery(command, workspaceRoot, plan, maxDepth, maxNodes, projectIDs)
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	plan = planning.Plan
	database, err := cmd.DatabasePathForCommand(command, workspaceRoot)
	if err != nil {
		return cmd.WriteError(standardError, fmt.Errorf("resolve query database path: %w", err))
	}
	store, err := sqlite.Open(context.Background(), database)
	if err != nil {
		return cmd.WriteError(standardError, fmt.Errorf("open query database: %w", err))
	}
	defer store.Close()
	if planning.CopilotMetric != nil {
		if err := store.RecordCopilotPlannerMetric(context.Background(), *planning.CopilotMetric); err != nil {
			return cmd.WriteError(standardError, err)
		}
	}
	if planning.ClaudeMetric != nil {
		if err := store.RecordClaudePlannerMetric(context.Background(), *planning.ClaudeMetric); err != nil {
			return cmd.WriteError(standardError, err)
		}
	}

	snapshot, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: workspaceRoot})
	if errors.Is(err, storage.ErrWorkspaceNotFound) {
		return cmd.WriteError(standardError, cli.NewIndexUnavailableError(workspaceRoot))
	}
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	embeddingGenerator, err := catalog.NewEmbeddingGenerator(workspaceRoot, nil)
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	var result query.Result
	var catalogResult *query.CatalogResult
	if plan != nil && plan.Intent == query.IntentCapability {
		rankedCatalog, err := query.RankCatalogSnapshot(context.Background(), store, snapshot, query.CatalogRequest{Text: plan.Question}, query.CatalogRankingOptions{EmbeddingGenerator: embeddingGenerator, EmbeddingReader: store, VectorSearcher: store})
		if err != nil {
			return cmd.WriteError(standardError, cli.NewInvalidArgumentError(err.Error()))
		}
		catalogResult = &rankedCatalog
	} else {
		result, err = query.QuerySnapshot(context.Background(), store, store, snapshot, query.Request{
			Plan:       plan,
			Terms:      terms,
			ProjectIDs: append([]string(nil), projectIDs...),
			Relations:  RelationKinds(relations),
			MaxDepth:   maxDepth,
			MaxNodes:   maxNodes,
		})
		if err != nil {
			return cmd.WriteError(standardError, cli.NewInvalidArgumentError(err.Error()))
		}
		if plan != nil && plan.Intent == query.IntentUnknown {
			rankedCatalog, err := query.RankCatalogSnapshot(context.Background(), store, snapshot, query.CatalogRequest{Text: plan.Question}, query.CatalogRankingOptions{EmbeddingGenerator: embeddingGenerator, EmbeddingReader: store, VectorSearcher: store})
			if err != nil {
				return cmd.WriteError(standardError, cli.NewInvalidArgumentError(err.Error()))
			}
			catalogResult = &rankedCatalog
		}
	}
	if planning.CopilotWarning != nil {
		result.Warnings = append(result.Warnings, *planning.CopilotWarning)
	}
	if planning.ClaudeWarning != nil {
		result.Warnings = append(result.Warnings, *planning.ClaudeWarning)
	}
	data := resultData(result, catalogResult, plan, maxDepth, maxNodes, planning.OllamaMetadata, planning.CopilotMetadata, planning.ClaudeMetadata, planning.Metadata)
	if err := cli.Render(standardOutput, cli.Result{
		Snapshot: snapshot,
		Text:     renderText(data, showPlan),
		Data:     data,
	}, format); err != nil {
		return cmd.WriteError(standardError, err)
	}
	return 0
}

func RelationKinds(relations []string) []graph.RelationKind {
	kinds := make([]graph.RelationKind, len(relations))
	for index, relation := range relations {
		kinds[index] = graph.RelationKind(relation)
	}
	return kinds
}

func isQuestionArgument(terms []string) bool {
	if len(terms) != 1 {
		return false
	}
	term := strings.TrimSpace(terms[0])
	if strings.ContainsAny(term, " ?!\t\n") {
		return true
	}
	word := strings.ToLower(strings.Trim(term, "."))
	switch word {
	case "describe", "explain", "find", "how", "show", "what", "where", "which", "who":
		return true
	default:
		return false
	}
}
