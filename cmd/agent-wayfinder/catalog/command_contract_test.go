package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-wayfinder/index"
	"agent-wayfinder/storage"
	"agent-wayfinder/storage/sqlite"
	"agent-wayfinder/testkit"
)

func indexWorkspace(t *testing.T, workspace, database string) storage.Snapshot {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(database), 0o755); err != nil {
		t.Fatalf("create catalog database directory: %v", err)
	}
	store, err := sqlite.Open(context.Background(), database)
	if err != nil {
		t.Fatalf("open catalog database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	result, err := index.Index(context.Background(), store, index.Request{Root: workspace})
	if err != nil {
		t.Fatalf("index workspace: %v", err)
	}
	return result.Snapshot
}

func runCatalogCommand(t *testing.T, commandName string, arguments []string, standardOutput, standardError *bytes.Buffer) int {
	t.Helper()
	exitCode := 0
	var commandRunner interface {
		SetArgs([]string)
		Execute() error
	}
	if commandName == "catalog" {
		commandRunner = New(standardOutput, standardError, &exitCode)
	} else {
		commandRunner = NewStatus(standardOutput, standardError, &exitCode)
	}
	commandRunner.SetArgs(arguments)
	if err := commandRunner.Execute(); err != nil {
		t.Fatalf("run %s command: %v", commandName, err)
	}
	return exitCode
}

func TestCatalogCommandRefreshesPublishedCatalogAndReportsRetrievalMethods(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"go.mod":             "module example.com/fixture\n",
		"validator/token.go": "package validator\n\nfunc ValidateToken(token string) error { return nil }\n",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	indexWorkspace(t, workspace.Root, database)

	output := &bytes.Buffer{}
	standardError := &bytes.Buffer{}
	if exitCode := runCatalogCommand(t, "catalog", []string{"--database", database, workspace.Root}, output, standardError); exitCode != 0 {
		t.Fatalf("catalog workspace exit code = %d, want 0; error %s", exitCode, standardError.String())
	}
	if !strings.Contains(standardError.String(), "Catalog progress: 1/1") {
		t.Errorf("catalog progress = %q, want unit progress", standardError.String())
	}
	if !strings.Contains(output.String(), "Retrieval methods: lexical") {
		t.Errorf("catalog output = %q, want lexical retrieval method", output.String())
	}
	store, err := sqlite.Open(context.Background(), database)
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	snapshot, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: workspace.Root})
	if err != nil {
		t.Fatalf("open indexed snapshot: %v", err)
	}
	matches, err := store.SearchCatalog(context.Background(), snapshot, storage.CatalogSearchRequest{Text: "validate token", Limit: 10})
	if err != nil {
		t.Fatalf("search refreshed catalog: %v", err)
	}
	if len(matches) != 1 || matches[0].Entry.Name != "ValidateToken" {
		t.Errorf("refreshed catalog matches = %+v, want ValidateToken", matches)
	}
}

func TestCatalogStatusReportsCompletedCatalogPass(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/token.ts": "export function validateAccessToken(token: string) { return token.length > 0; }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	standardOutput := &bytes.Buffer{}
	standardError := &bytes.Buffer{}
	indexWorkspace(t, workspace.Root, database)
	standardOutput.Reset()
	standardError.Reset()
	if exitCode := runCatalogCommand(t, "catalog", []string{"--database", database, workspace.Root}, standardOutput, standardError); exitCode != 0 {
		t.Fatalf("run catalog command: exit code %d, error %s", exitCode, standardError.String())
	}
	standardOutput.Reset()
	standardError.Reset()
	if exitCode := runCatalogCommand(t, "catalog-status", []string{"--format", "json", "--database", database, workspace.Root}, standardOutput, standardError); exitCode != 0 {
		t.Fatalf("run catalog-status command: exit code %d, error %s", exitCode, standardError.String())
	}
	var envelope struct {
		Result struct {
			State          string               `json:"state"`
			GraphVersion   storage.GraphVersion `json:"graphVersion"`
			StartedAt      time.Time            `json:"startedAt"`
			Elapsed        time.Duration        `json:"elapsed"`
			CompletedUnits int                  `json:"completedUnits"`
			TotalUnits     int                  `json:"totalUnits"`
		} `json:"result"`
	}
	if err := json.Unmarshal(standardOutput.Bytes(), &envelope); err != nil {
		t.Fatalf("decode catalog status: %v\n%s", err, standardOutput.String())
	}
	result := envelope.Result
	if result.State != "complete" || result.GraphVersion == 0 || result.StartedAt.IsZero() || result.Elapsed < 0 || result.TotalUnits == 0 || result.CompletedUnits != result.TotalUnits {
		t.Errorf("catalog status = %+v, want completed catalog task details", result)
	}
}

func TestCatalogStatusReportsOneShotCatalogSetupFailure(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":                 `{"name":"fixture"}`,
		"src/token.ts":                 "export function validateAccessToken(token: string) { return token.length > 0; }",
		".agent-wayfinder/config.json": "{",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	standardOutput := &bytes.Buffer{}
	standardError := &bytes.Buffer{}
	indexWorkspace(t, workspace.Root, database)
	standardOutput.Reset()
	standardError.Reset()
	if exitCode := runCatalogCommand(t, "catalog", []string{"--database", database, workspace.Root}, standardOutput, standardError); exitCode == 0 {
		t.Fatal("run catalog command: exit code 0, want configuration failure")
	}
	standardOutput.Reset()
	standardError.Reset()
	if exitCode := runCatalogCommand(t, "catalog-status", []string{"--format", "json", "--database", database, workspace.Root}, standardOutput, standardError); exitCode != 0 {
		t.Fatalf("run catalog-status command: exit code %d, error %s", exitCode, standardError.String())
	}
	var envelope struct {
		Result struct {
			State   string `json:"state"`
			Failure string `json:"failure"`
		} `json:"result"`
	}
	if err := json.Unmarshal(standardOutput.Bytes(), &envelope); err != nil {
		t.Fatalf("decode catalog status: %v\n%s", err, standardOutput.String())
	}
	if envelope.Result.State != string(storage.CatalogTaskFailed) || envelope.Result.Failure == "" {
		t.Errorf("catalog status = %+v, want failed one-shot catalog setup", envelope.Result)
	}
}

func TestCatalogStatusReportsRunningTaskProgress(t *testing.T) {
	workspace := t.TempDir()
	database := filepath.Join(t.TempDir(), "graph.db")
	store, err := sqlite.Open(context.Background(), database)
	if err != nil {
		t.Fatalf("open catalog database: %v", err)
	}
	if err := store.WriteCatalogTask(context.Background(), storage.CatalogTask{
		Workspace:      workspace,
		GraphVersion:   7,
		State:          storage.CatalogTaskRunning,
		StartedAt:      time.Now().UTC(),
		CompletedUnits: 2,
		TotalUnits:     4,
	}); err != nil {
		_ = store.Close()
		t.Fatalf("write running catalog task: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close catalog database: %v", err)
	}
	standardOutput := &bytes.Buffer{}
	standardError := &bytes.Buffer{}
	if exitCode := runCatalogCommand(t, "catalog-status", []string{"--format", "json", "--database", database, workspace}, standardOutput, standardError); exitCode != 0 {
		t.Fatalf("run catalog-status command: exit code %d, error %s", exitCode, standardError.String())
	}
	var envelope struct {
		Result struct {
			State          string `json:"state"`
			CompletedUnits int    `json:"completedUnits"`
			TotalUnits     int    `json:"totalUnits"`
		} `json:"result"`
	}
	if err := json.Unmarshal(standardOutput.Bytes(), &envelope); err != nil {
		t.Fatalf("decode catalog status: %v\n%s", err, standardOutput.String())
	}
	if envelope.Result.State != string(storage.CatalogTaskRunning) || envelope.Result.CompletedUnits != 2 || envelope.Result.TotalUnits != 4 {
		t.Errorf("catalog status = %+v, want running task progress", envelope.Result)
	}
}

func TestCatalogStatusShowsNotStartedState(t *testing.T) {
	workspace := t.TempDir()
	database := filepath.Join(t.TempDir(), "graph.db")
	standardOutput := &bytes.Buffer{}
	standardError := &bytes.Buffer{}
	if exitCode := runCatalogCommand(t, "catalog-status", []string{"--format", "json", "--database", database, workspace}, standardOutput, standardError); exitCode != 0 {
		t.Fatalf("run catalog-status command: exit code %d, error %s", exitCode, standardError.String())
	}
	var envelope struct {
		Result struct {
			State string `json:"state"`
		} `json:"result"`
	}
	if err := json.Unmarshal(standardOutput.Bytes(), &envelope); err != nil {
		t.Fatalf("decode catalog status: %v\n%s", err, standardOutput.String())
	}
	if envelope.Result.State != "not_started" {
		t.Errorf("catalog status = %+v, want not-started state", envelope.Result)
	}
}

func TestCatalogStatusFormatsFailedChangedFilePass(t *testing.T) {
	workspace := t.TempDir()
	database := filepath.Join(t.TempDir(), "graph.db")
	store, err := sqlite.Open(context.Background(), database)
	if err != nil {
		t.Fatalf("open catalog database: %v", err)
	}
	startedAt := time.Date(2026, time.March, 8, 12, 0, 0, 0, time.UTC)
	if err := store.WriteCatalogTask(context.Background(), storage.CatalogTask{
		Workspace:      workspace,
		GraphVersion:   7,
		State:          storage.CatalogTaskFailed,
		StartedAt:      startedAt,
		FinishedAt:     startedAt.Add(2 * time.Second),
		CompletedUnits: 1,
		TotalUnits:     2,
		ChangedPaths:   []string{"src/token.ts"},
		Failure:        "catalog configuration is invalid",
	}); err != nil {
		_ = store.Close()
		t.Fatalf("write failed catalog task: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close catalog database: %v", err)
	}
	standardOutput := &bytes.Buffer{}
	standardError := &bytes.Buffer{}
	if exitCode := runCatalogCommand(t, "catalog-status", []string{"--database", database, workspace}, standardOutput, standardError); exitCode != 0 {
		t.Fatalf("run catalog-status command: exit code %d, error %s", exitCode, standardError.String())
	}
	for _, want := range []string{
		"Capability Catalog Status",
		"FIELD",
		"VALUE",
		"State",
		"Failed",
		workspace,
		"Graph version",
		"7",
		"Started",
		"2026-03-08T12:00:00Z",
		"Elapsed",
		"2s",
		"Progress",
		"1/2 catalog units",
		"Scope",
		"1 changed files",
		"Failure",
		"catalog configuration is invalid",
	} {
		if !strings.Contains(standardOutput.String(), want) {
			t.Errorf("catalog status output = %q, want %q", standardOutput.String(), want)
		}
	}
}

func TestCatalogCommandRejectsWorkspaceWithoutPublishedGraph(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"go.mod":             "module example.com/fixture\n",
		"validator/token.go": "package validator\n\nfunc ValidateToken(token string) error { return nil }\n",
	})
	output := &bytes.Buffer{}
	standardError := &bytes.Buffer{}

	if exitCode := runCatalogCommand(t, "catalog", []string{"--database", filepath.Join(t.TempDir(), "graph.db"), workspace.Root}, output, standardError); exitCode != 1 {
		t.Errorf("catalog exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(standardError.String(), "no published graph is available") {
		t.Errorf("catalog error = %q, want missing published graph error", standardError.String())
	}
}
