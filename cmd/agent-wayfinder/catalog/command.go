package catalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agent-wayfinder/cli"
	cmd "agent-wayfinder/cmd/agent-wayfinder/internal/command"
	"agent-wayfinder/cmd/agent-wayfinder/internal/copilot"
	"agent-wayfinder/cmd/agent-wayfinder/internal/ollama"
	"agent-wayfinder/cmd/agent-wayfinder/internal/spending"
	"agent-wayfinder/index"
	"agent-wayfinder/storage"
	"agent-wayfinder/storage/sqlite"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"
)

func New(standardOutput, standardError io.Writer, exitCode *int) *cobra.Command {
	return cmd.NewLeaf("catalog WORKSPACE", "Build the capability catalog for a published graph", configureCommand, runCatalog, standardOutput, standardError, exitCode)
}

func NewStatus(standardOutput, standardError io.Writer, exitCode *int) *cobra.Command {
	return cmd.NewLeaf("catalog-status WORKSPACE", "Show later catalog pass status", cmd.DatabaseAndFormatFlags, runCatalogStatus, standardOutput, standardError, exitCode)
}

func configureCommand(command *cobra.Command) {
	cmd.DatabaseAndFormatFlags(command)
	ConfigureFlags(command)
}

type ProcessRequest struct {
	Executable string
	Database   string
	Workspace  string
}

func StartProcess(request ProcessRequest) error {
	if strings.HasSuffix(filepath.Base(request.Executable), ".test") {
		return nil
	}
	command := exec.Command(request.Executable, "catalog", "--database", request.Database, request.Workspace)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return fmt.Errorf("start catalog process: %w", err)
	}
	return nil
}

type RefreshRequest struct {
	Database         string
	Workspace        string
	Snapshot         storage.Snapshot
	PreviousSnapshot storage.Snapshot
	ChangedPaths     []string
}

const (
	catalogRefreshTimeout     = 30 * time.Second
	catalogStatusWriteTimeout = 5 * time.Second
)

var runCatalogRefresh = refreshCatalog

var catalogRefreshes = refreshCoordinator{tails: make(map[string]*refreshEntry)}

type refreshCoordinator struct {
	mu    sync.Mutex
	tails map[string]*refreshEntry
}

type refreshEntry struct {
	done chan struct{}
	err  error
}

func StartRefresh(request RefreshRequest) {
	catalogRefreshes.mu.Lock()
	previous := catalogRefreshes.tails[request.Workspace]
	current := &refreshEntry{done: make(chan struct{})}
	catalogRefreshes.tails[request.Workspace] = current
	catalogRefreshes.mu.Unlock()

	go func() {
		if previous != nil {
			<-previous.done
			if previous.err != nil {
				request.PreviousSnapshot = storage.Snapshot{}
				request.ChangedPaths = nil
			}
		}
		defer func() {
			close(current.done)
			catalogRefreshes.mu.Lock()
			if catalogRefreshes.tails[request.Workspace] == current {
				delete(catalogRefreshes.tails, request.Workspace)
			}
			catalogRefreshes.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), catalogRefreshTimeout)
		defer cancel()
		current.err = runCatalogRefresh(ctx, request)
	}()
}

func refreshCatalog(ctx context.Context, request RefreshRequest) error {
	store, err := sqlite.Open(ctx, request.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	snapshot := request.Snapshot
	if snapshot.Version == 0 {
		snapshot, err = store.OpenSnapshot(ctx, storage.OpenSnapshotRequest{Workspace: request.Workspace})
		if err != nil {
			return err
		}
	}
	task := storage.CatalogTask{
		Workspace:    request.Workspace,
		GraphVersion: snapshot.Version,
		State:        storage.CatalogTaskRunning,
		StartedAt:    time.Now().UTC(),
		ChangedPaths: append([]string(nil), request.ChangedPaths...),
	}
	if err := store.WriteCatalogTask(ctx, task); err != nil {
		return err
	}
	fail := func(catalogError error) error {
		task.State = storage.CatalogTaskFailed
		task.FinishedAt = time.Now().UTC()
		task.Failure = catalogError.Error()
		statusContext, cancelStatusWrite := context.WithTimeout(context.Background(), catalogStatusWriteTimeout)
		defer cancelStatusWrite()
		if statusError := store.WriteCatalogTask(statusContext, task); statusError != nil {
			return statusError
		}
		return catalogError
	}
	configuration, err := readCatalogConfiguration(request.Workspace)
	if err != nil {
		return fail(err)
	}
	options, err := catalogWriteOptions(configuration, request.Workspace, store)
	if err != nil {
		return fail(err)
	}
	result, err := index.Catalog(ctx, store, index.CatalogRequest{
		Root:                request.Workspace,
		Snapshot:            snapshot,
		PreviousSnapshot:    request.PreviousSnapshot,
		ChangedPaths:        request.ChangedPaths,
		CatalogWriteOptions: options,
		Progress: func(progress index.CatalogProgress) {
			task.CompletedUnits = progress.CompletedUnits
			task.TotalUnits = progress.TotalUnits
			_ = store.WriteCatalogTask(ctx, task)
		},
	})
	if err != nil {
		return fail(err)
	}
	task.State = storage.CatalogTaskComplete
	task.FinishedAt = time.Now().UTC()
	task.CompletedUnits = result.Units
	task.TotalUnits = result.Units
	if err := store.WriteCatalogTask(ctx, task); err != nil {
		return fail(err)
	}
	return nil
}

func runCatalog(command *cobra.Command, arguments []string, standardOutput, standardError io.Writer) int {
	if len(arguments) != 1 {
		return cmd.WriteError(standardError, cli.NewInvalidArgumentError("catalog requires one workspace path"))
	}
	format, err := cmd.Format(command)
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	workspaceRoot, err := filepath.Abs(arguments[0])
	if err != nil {
		return cmd.WriteError(standardError, fmt.Errorf("resolve catalog workspace path: %w", err))
	}
	database, err := cmd.DatabasePathForCommand(command, workspaceRoot)
	if err != nil {
		return cmd.WriteError(standardError, fmt.Errorf("resolve catalog database path: %w", err))
	}
	store, err := sqlite.Open(context.Background(), database)
	if err != nil {
		return cmd.WriteError(standardError, fmt.Errorf("open catalog database: %w", err))
	}
	defer store.Close()
	snapshot, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: workspaceRoot})
	if errors.Is(err, storage.ErrWorkspaceNotFound) {
		return cmd.WriteError(standardError, cli.NewIndexUnavailableError(workspaceRoot))
	}
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	task := storage.CatalogTask{Workspace: workspaceRoot, GraphVersion: snapshot.Version, State: storage.CatalogTaskRunning, StartedAt: time.Now().UTC()}
	if err := store.WriteCatalogTask(context.Background(), task); err != nil {
		return cmd.WriteError(standardError, err)
	}
	fail := func(catalogError error) int {
		task.State = storage.CatalogTaskFailed
		task.FinishedAt = time.Now().UTC()
		task.Failure = catalogError.Error()
		_ = store.WriteCatalogTask(context.Background(), task)
		return cmd.WriteError(standardError, catalogError)
	}
	configuration, err := resolveCatalogConfiguration(command, workspaceRoot)
	if err != nil {
		return fail(err)
	}
	options, err := catalogWriteOptions(configuration, workspaceRoot, store)
	if err != nil {
		return fail(err)
	}
	result, err := index.Catalog(context.Background(), store, index.CatalogRequest{
		Root:                workspaceRoot,
		Snapshot:            snapshot,
		CatalogWriteOptions: options,
		Progress: func(progress index.CatalogProgress) {
			task.CompletedUnits = progress.CompletedUnits
			task.TotalUnits = progress.TotalUnits
			_ = store.WriteCatalogTask(context.Background(), task)
			_, _ = fmt.Fprintf(standardError, "Catalog progress: %d/%d\n", progress.CompletedUnits, progress.TotalUnits)
		},
	})
	if err != nil {
		return fail(err)
	}
	task.State = storage.CatalogTaskComplete
	task.FinishedAt = time.Now().UTC()
	task.CompletedUnits = result.Units
	task.TotalUnits = result.Units
	if err := store.WriteCatalogTask(context.Background(), task); err != nil {
		return cmd.WriteError(standardError, err)
	}
	retrievalMethods := []string{"lexical"}
	if result.Units > 0 && result.Write.EmbeddingUnavailableReason == "" {
		retrievalMethods = append(retrievalMethods, "optional embeddings")
	}
	data := struct {
		Workspace                  string   `json:"workspace"`
		Units                      int      `json:"units"`
		RetrievalMethods           []string `json:"retrievalMethods"`
		CopilotUnavailableReason   string   `json:"copilotUnavailableReason,omitempty"`
		OllamaUnavailableReason    string   `json:"ollamaUnavailableReason,omitempty"`
		ClaudeUnavailableReason    string   `json:"claudeUnavailableReason,omitempty"`
		EmbeddingUnavailableReason string   `json:"embeddingUnavailableReason,omitempty"`
	}{
		Workspace:                  result.Snapshot.Workspace,
		Units:                      result.Units,
		RetrievalMethods:           retrievalMethods,
		CopilotUnavailableReason:   result.Write.CopilotUnavailableReason,
		OllamaUnavailableReason:    result.Write.OllamaUnavailableReason,
		ClaudeUnavailableReason:    result.Write.ClaudeUnavailableReason,
		EmbeddingUnavailableReason: result.Write.EmbeddingUnavailableReason,
	}
	text := fmt.Sprintf("Catalog units: %d\nRetrieval methods: %s", data.Units, strings.Join(data.RetrievalMethods, ", "))
	if data.EmbeddingUnavailableReason != "" {
		text += fmt.Sprintf("\nOptional embeddings: skipped (%s)", data.EmbeddingUnavailableReason)
	}
	if err := cli.Render(standardOutput, cli.Result{Snapshot: result.Snapshot, Text: text, Data: data}, format); err != nil {
		return cmd.WriteError(standardError, err)
	}
	return 0
}

func catalogWriteOptions(configuration catalogConfiguration, workspaceRoot string, store storage.SpendReservationStore) (index.CatalogWriteOptions, error) {
	embeddingGenerator, err := newCatalogEmbeddingGenerator(configuration, nil)
	if err != nil {
		return index.CatalogWriteOptions{}, err
	}
	options := index.CatalogWriteOptions{
		EmbeddingGenerator:    embeddingGenerator,
		EmbeddingProcessLimit: configuration.Embedding.ProcessLimit,
		SynopsisProvider:      configuration.Synopsis.Provider,
		SynopsisSourceLimit:   configuration.Synopsis.SourceLimit,
	}
	switch configuration.Synopsis.Provider {
	case "":
		return options, nil
	case index.CatalogSynopsisProviderCopilot:
		spendingConfiguration, err := spending.ReadConfiguration(workspaceRoot)
		if err != nil {
			return index.CatalogWriteOptions{}, err
		}
		options.SynopsisGenerator = spendingCatalogSynopsisGenerator{
			provider:      index.CatalogSynopsisProviderCopilot,
			configuration: spendingConfiguration,
			store:         store,
			copilotConfiguration: copilot.CatalogConfiguration{
				Path:         configuration.Copilot.Path,
				Model:        configuration.Copilot.Model,
				MaxAICredits: configuration.Copilot.MaxAICredits,
			},
		}
		options.SynopsisProcessLimit = configuration.Copilot.ProcessLimit
	case index.CatalogSynopsisProviderOllama:
		options.SynopsisGenerator = ollama.NewCatalogSynopsisGenerator(ollama.CatalogSynopsisConfiguration{Model: configuration.Ollama.Model, Timeout: 30 * time.Second}, configuration.Ollama.Endpoint, nil)
	case index.CatalogSynopsisProviderClaude:
		spendingConfiguration, err := spending.ReadConfiguration(workspaceRoot)
		if err != nil {
			return index.CatalogWriteOptions{}, err
		}
		options.SynopsisGenerator = spendingCatalogSynopsisGenerator{
			provider:            index.CatalogSynopsisProviderClaude,
			configuration:       spendingConfiguration,
			store:               store,
			claudeConfiguration: configuration.Claude,
		}
	default:
		return index.CatalogWriteOptions{}, fmt.Errorf("invalid catalog synopsis provider %q", configuration.Synopsis.Provider)
	}
	return options, nil
}

type catalogStatusData struct {
	Workspace      string               `json:"workspace"`
	State          string               `json:"state"`
	GraphVersion   storage.GraphVersion `json:"graphVersion"`
	StartedAt      time.Time            `json:"startedAt"`
	Elapsed        time.Duration        `json:"elapsed"`
	CompletedUnits int                  `json:"completedUnits"`
	TotalUnits     int                  `json:"totalUnits"`
	ChangedPaths   []string             `json:"changedPaths,omitempty"`
	Failure        string               `json:"failure,omitempty"`
}

func runCatalogStatus(command *cobra.Command, arguments []string, standardOutput, standardError io.Writer) int {
	if len(arguments) != 1 {
		return cmd.WriteError(standardError, cli.NewInvalidArgumentError("catalog-status requires one workspace path"))
	}
	format, err := cmd.Format(command)
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	workspaceRoot, err := filepath.Abs(arguments[0])
	if err != nil {
		return cmd.WriteError(standardError, fmt.Errorf("resolve catalog status workspace path: %w", err))
	}
	database, err := cmd.DatabasePathForCommand(command, workspaceRoot)
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	store, err := sqlite.Open(context.Background(), database)
	if err != nil {
		return cmd.WriteError(standardError, fmt.Errorf("open catalog status database: %w", err))
	}
	defer store.Close()
	task, found, err := store.ReadCatalogTask(context.Background(), workspaceRoot)
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	if !found {
		data := catalogStatusData{Workspace: workspaceRoot, State: "not_started"}
		if err := cli.Render(standardOutput, cli.Result{OmitSnapshot: true, Text: formatCatalogStatusText(data), Data: data}, format); err != nil {
			return cmd.WriteError(standardError, err)
		}
		return 0
	}
	endedAt := time.Now().UTC()
	if !task.FinishedAt.IsZero() {
		endedAt = task.FinishedAt
	}
	data := catalogStatusData{Workspace: task.Workspace, State: string(task.State), GraphVersion: task.GraphVersion, StartedAt: task.StartedAt, Elapsed: endedAt.Sub(task.StartedAt), CompletedUnits: task.CompletedUnits, TotalUnits: task.TotalUnits, ChangedPaths: task.ChangedPaths, Failure: task.Failure}
	if err := cli.Render(standardOutput, cli.Result{OmitSnapshot: true, Text: formatCatalogStatusText(data), Data: data}, format); err != nil {
		return cmd.WriteError(standardError, err)
	}
	return 0
}

func formatCatalogStatusText(data catalogStatusData) string {
	status := table.NewWriter()
	status.SetStyle(table.StyleLight)
	status.AppendHeader(table.Row{"FIELD", "VALUE"})
	status.AppendRow(table.Row{"State", displayCatalogTaskState(data.State)})
	status.AppendRow(table.Row{"Workspace", data.Workspace})
	if data.State == "not_started" {
		return fmt.Sprintf("Capability Catalog Status\n\n%s\n\nNo catalog pass has been recorded for this workspace.", status.Render())
	}
	status.AppendRow(table.Row{"Graph version", data.GraphVersion})
	status.AppendRow(table.Row{"Started", data.StartedAt.UTC().Format(time.RFC3339)})
	status.AppendRow(table.Row{"Elapsed", data.Elapsed.Round(time.Millisecond)})
	status.AppendRow(table.Row{"Progress", fmt.Sprintf("%d/%d catalog units", data.CompletedUnits, data.TotalUnits)})
	if len(data.ChangedPaths) == 0 {
		status.AppendRow(table.Row{"Scope", "full workspace"})
	} else {
		status.AppendRow(table.Row{"Scope", fmt.Sprintf("%d changed files", len(data.ChangedPaths))})
	}
	if data.Failure != "" {
		status.AppendRow(table.Row{"Failure", data.Failure})
	}
	return fmt.Sprintf("Capability Catalog Status\n\n%s", status.Render())
}

func displayCatalogTaskState(state string) string {
	switch state {
	case "not_started":
		return "Not started"
	case string(storage.CatalogTaskQueued):
		return "Queued"
	case string(storage.CatalogTaskRunning):
		return "Running"
	case string(storage.CatalogTaskComplete):
		return "Complete"
	case string(storage.CatalogTaskFailed):
		return "Failed"
	default:
		return state
	}
}
