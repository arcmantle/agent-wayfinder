package benchmark

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-wayfinder/cli"
	cmd "agent-wayfinder/cmd/agent-wayfinder/internal/command"
	"agent-wayfinder/testkit"
)

func run(arguments []string, standardOutput, standardError *strings.Builder) int {
	if len(arguments) == 0 || arguments[0] != "benchmark" {
		return cmd.WriteError(standardError, cli.NewInvalidArgumentError("expected benchmark command"))
	}
	exitCode := 0
	command := New(standardOutput, standardError, &exitCode)
	command.SetArgs(arguments[1:])
	if err := command.Execute(); err != nil {
		return cmd.WriteError(standardError, cli.NewInvalidArgumentError(err.Error()))
	}
	return exitCode
}

func TestBenchmarkCommandMeasuresCriticalUserPath(t *testing.T) {
	output := &strings.Builder{}
	standardError := &strings.Builder{}
	if exitCode := run([]string{"benchmark", "--format", "json", "--source-files", "3", "--functions-per-file", "4"}, output, standardError); exitCode != 0 {
		t.Fatalf("run benchmark command: exit code %d, error %s", exitCode, standardError.String())
	}

	var result struct {
		GraphVersion int    `json:"graphVersion"`
		PublishedAt  string `json:"publishedAt"`
		Result       struct {
			ContributionQueueHighWater int `json:"contributionQueueHighWater"`
			ContributionQueueCapacity  int `json:"contributionQueueCapacity"`
			Measurements               []struct {
				Name       string `json:"name"`
				DurationNS int64  `json:"durationNs"`
			} `json:"measurements"`
			PhaseMeasurements []struct {
				Name       string `json:"name"`
				DurationNS int64  `json:"durationNs"`
			} `json:"phaseMeasurements"`
			ResolverMeasurements []struct {
				Name       string `json:"name"`
				DurationNS int64  `json:"durationNs"`
			} `json:"resolverMeasurements"`
			SQLiteWriteMeasurements []struct {
				Name       string `json:"name"`
				DurationNS int64  `json:"durationNs"`
			} `json:"sqliteWriteMeasurements"`
			Runs []struct {
				ContributionQueueHighWater int `json:"contributionQueueHighWater"`
				ContributionQueueCapacity  int `json:"contributionQueueCapacity"`
				Measurements               []struct {
					Name       string `json:"name"`
					DurationNS int64  `json:"durationNs"`
				} `json:"measurements"`
				PhaseMeasurements []struct {
					Name       string `json:"name"`
					DurationNS int64  `json:"durationNs"`
				} `json:"phaseMeasurements"`
				ResolverMeasurements []struct {
					Name       string `json:"name"`
					DurationNS int64  `json:"durationNs"`
				} `json:"resolverMeasurements"`
				SQLiteWriteMeasurements []struct {
					Name       string `json:"name"`
					DurationNS int64  `json:"durationNs"`
				} `json:"sqliteWriteMeasurements"`
				PeakRSSBytes   uint64 `json:"peakRssBytes"`
				DatabaseBytes  int64  `json:"databaseBytes"`
				OutputChecksum string `json:"outputChecksum"`
			} `json:"runs"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output.String()), &result); err != nil {
		t.Fatalf("decode benchmark result: %v\n%s", err, output.String())
	}
	if result.GraphVersion != 2 || result.PublishedAt == "" {
		t.Errorf("benchmark metadata = {%d, %q}, want published graph metadata", result.GraphVersion, result.PublishedAt)
	}
	if len(result.Result.Measurements) != 5 {
		t.Fatalf("measurements = %+v, want five critical-path measurements", result.Result.Measurements)
	}
	for index, want := range []string{"initial_index", "incremental_update", "query", "path", "inspect"} {
		measurement := result.Result.Measurements[index]
		if measurement.Name != want {
			t.Errorf("measurement %d name = %q, want %q", index, measurement.Name, want)
		}
		if measurement.DurationNS < 0 {
			t.Errorf("measurement %q duration = %d, want non-negative", measurement.Name, measurement.DurationNS)
		}
	}
	for runIndex, run := range result.Result.Runs {
		if len(run.PhaseMeasurements) != 12 {
			t.Fatalf("run %d phase measurements = %+v, want twelve indexing phases", runIndex, run.PhaseMeasurements)
		}
		for phaseIndex, want := range []string{"discovery", "pipeline_wall", "extraction", "extractor_busy", "writer_busy", "producer_blocked", "extraction_write_overlap", "resolution", "publication_preparation", "sqlite_write", "commit", "staged_transaction"} {
			measurement := run.PhaseMeasurements[phaseIndex]
			if measurement.Name != want {
				t.Errorf("run %d phase measurement %d name = %q, want %q", runIndex, phaseIndex, measurement.Name, want)
			}
			if measurement.DurationNS < 0 {
				t.Errorf("run %d phase %q duration = %d, want non-negative", runIndex, measurement.Name, measurement.DurationNS)
			}
		}
		if run.ContributionQueueHighWater <= 0 || run.ContributionQueueHighWater > run.ContributionQueueCapacity {
			t.Errorf("run %d queue statistics = %d/%d, want positive high-water at most capacity", runIndex, run.ContributionQueueHighWater, run.ContributionQueueCapacity)
		}
		if len(run.ResolverMeasurements) != 6 {
			t.Fatalf("run %d resolver measurements = %+v, want six incremental phase measurements", runIndex, run.ResolverMeasurements)
		}
		for measurementIndex, want := range []string{"affected_source_selection", "contribution_restoration", "workspace_resolution", "publication_preparation", "sqlite_write", "commit"} {
			measurement := run.ResolverMeasurements[measurementIndex]
			if measurement.Name != want {
				t.Errorf("run %d resolver measurement %d name = %q, want %q", runIndex, measurementIndex, measurement.Name, want)
			}
			if measurement.DurationNS < 0 {
				t.Errorf("run %d resolver measurement %q duration = %d, want non-negative", runIndex, measurement.Name, measurement.DurationNS)
			}
		}
	}
	if len(result.Result.ResolverMeasurements) != 6 {
		t.Fatalf("incremental phase medians = %+v, want six measurements", result.Result.ResolverMeasurements)
	}
	if result.Result.ContributionQueueHighWater <= 0 || result.Result.ContributionQueueHighWater > result.Result.ContributionQueueCapacity {
		t.Errorf("queue statistics = %d/%d, want positive high-water at most capacity", result.Result.ContributionQueueHighWater, result.Result.ContributionQueueCapacity)
	}
	writeTables := []string{
		"workspace_nodes",
		"workspace_edges",
		"file_contributions",
		"contribution_nodes",
		"contribution_edges",
		"contribution_extensions",
		"contribution_dependencies",
		"contribution_exported_surfaces",
		"contribution_diagnostics",
		"contribution_unresolved_references",
		"contribution_module_bindings",
		"contribution_symbol_references",
	}
	if len(result.Result.SQLiteWriteMeasurements) != len(writeTables) {
		t.Fatalf("SQLite write medians = %+v, want %d table measurements", result.Result.SQLiteWriteMeasurements, len(writeTables))
	}
	for measurementIndex, want := range writeTables {
		measurement := result.Result.SQLiteWriteMeasurements[measurementIndex]
		if measurement.Name != want {
			t.Errorf("SQLite write median %d name = %q, want %q", measurementIndex, measurement.Name, want)
		}
		if measurement.DurationNS < 0 {
			t.Errorf("SQLite write median %q duration = %d, want non-negative", measurement.Name, measurement.DurationNS)
		}
	}
	if len(result.Result.Runs) != 1 {
		t.Fatalf("warm runs = %+v, want one", result.Result.Runs)
	}
	for runIndex, run := range result.Result.Runs {
		if len(run.Measurements) != 5 || run.PeakRSSBytes == 0 || run.DatabaseBytes <= 0 || run.OutputChecksum == "" {
			t.Errorf("warm run %d = %+v, want measurements and resource metadata", runIndex, run)
		}
		if len(run.SQLiteWriteMeasurements) != len(writeTables) {
			t.Errorf("warm run %d SQLite write measurements = %+v, want %d table measurements", runIndex, run.SQLiteWriteMeasurements, len(writeTables))
		}
	}
	if progress := standardError.String(); !strings.Contains(progress, "Benchmark setup: generated 3/3 source files") || !strings.Contains(progress, "Benchmark setup: validation extract 3/3 sources") || !strings.Contains(progress, "Benchmark setup: validation resolve 3/3 sources") || !strings.Contains(progress, "Benchmark setup: validation publish 3/3 sources") || !strings.Contains(progress, "Benchmark setup: validated 16/16 nodes and 30/30 edges") || !strings.Contains(progress, "Benchmark run 1/1: complete") {
		t.Errorf("benchmark progress = %q, want setup counts and run completion messages", progress)
	}
}

func TestBenchmarkCommandRunsExplicitExactScaleAcceptance(t *testing.T) {
	output := &strings.Builder{}
	standardError := &strings.Builder{}
	if exitCode := run([]string{"benchmark", "--format", "json", "--exact-scale", "--source-files", "4", "--runs", "3"}, output, standardError); exitCode != 0 {
		t.Fatalf("run exact-scale benchmark command: exit code %d, error %s", exitCode, standardError.String())
	}

	var result struct {
		Result struct {
			Configuration struct {
				ExtractionWorkers         int `json:"extractionWorkers"`
				SourceQueueCapacity       int `json:"sourceQueueCapacity"`
				ContributionQueueCapacity int `json:"contributionQueueCapacity"`
				ContributionBatchRows     int `json:"contributionBatchRows"`
				ContributionBatchBytes    int `json:"contributionBatchBytes"`
				ContributionBatchSources  int `json:"contributionBatchSources"`
				ResolverPageSize          int `json:"resolverPageSize"`
				WorkspaceFactBatchRows    int `json:"workspaceFactBatchRows"`
				WorkspaceFactBatchBytes   int `json:"workspaceFactBatchBytes"`
			} `json:"configuration"`
			Runs []struct {
				SourceFiles    int    `json:"sourceFiles"`
				NodeCount      int    `json:"nodeCount"`
				EdgeCount      int    `json:"edgeCount"`
				OutputChecksum string `json:"outputChecksum"`
			} `json:"runs"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output.String()), &result); err != nil {
		t.Fatalf("decode exact-scale benchmark result: %v\n%s", err, output.String())
	}
	if len(result.Result.Runs) != 3 {
		t.Fatalf("exact-scale runs = %d, want 3", len(result.Result.Runs))
	}
	configuration := result.Result.Configuration
	if configuration.ExtractionWorkers <= 0 || configuration.SourceQueueCapacity <= 0 || configuration.ContributionQueueCapacity <= 0 || configuration.ContributionBatchRows <= 0 || configuration.ContributionBatchBytes <= 0 || configuration.ContributionBatchSources <= 0 || configuration.ResolverPageSize <= 0 || configuration.WorkspaceFactBatchRows <= 0 || configuration.WorkspaceFactBatchBytes <= 0 {
		t.Errorf("exact-scale configuration = %+v, want all positive memory bounds", configuration)
	}
	checksum := result.Result.Runs[0].OutputChecksum
	for runIndex, benchmarkRun := range result.Result.Runs {
		if benchmarkRun.SourceFiles != 4 || benchmarkRun.NodeCount != 400 || benchmarkRun.EdgeCount != 800 {
			t.Errorf("run %d counts = %d sources, %d nodes and %d edges, want 4, 400 and 800", runIndex, benchmarkRun.SourceFiles, benchmarkRun.NodeCount, benchmarkRun.EdgeCount)
		}
		if checksum == "" || benchmarkRun.OutputChecksum != checksum {
			t.Errorf("run %d checksum = %q, want stable nonempty %q", runIndex, benchmarkRun.OutputChecksum, checksum)
		}
	}
}

func TestBenchmarkCommandWritesInitialIndexCPUProfile(t *testing.T) {
	profilePath := filepath.Join(t.TempDir(), "initial-index.pprof")
	output := &strings.Builder{}
	standardError := &strings.Builder{}

	if exitCode := run([]string{"benchmark", "--source-files", "3", "--functions-per-file", "4", "--cpu-profile", profilePath}, output, standardError); exitCode != 0 {
		t.Fatalf("run benchmark with CPU profile: exit code %d, error %s", exitCode, standardError.String())
	}

	profile, err := os.Stat(profilePath)
	if err != nil {
		t.Fatalf("stat CPU profile: %v", err)
	}
	if profile.Size() == 0 {
		t.Error("CPU profile is empty")
	}
}

func TestBenchmarkCommandWritesIncrementalUpdateCPUProfile(t *testing.T) {
	profilePath := filepath.Join(t.TempDir(), "incremental-update.pprof")
	output := &strings.Builder{}
	standardError := &strings.Builder{}

	if exitCode := run([]string{"benchmark", "--source-files", "3", "--functions-per-file", "4", "--incremental-cpu-profile", profilePath}, output, standardError); exitCode != 0 {
		t.Fatalf("run benchmark with incremental CPU profile: exit code %d, error %s", exitCode, standardError.String())
	}

	profile, err := os.Stat(profilePath)
	if err != nil {
		t.Fatalf("stat incremental CPU profile: %v", err)
	}
	if profile.Size() == 0 {
		t.Error("incremental CPU profile is empty")
	}
}

func TestBenchmarkCommandMeasuresGoWorkspace(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"sources":{"exclude":["reference/**"]}}`,
		"go.mod":                       "module example.com/fixture\n\ngo 1.24\n",
		"cmd/main.go":                  "package main\n\nimport \"example.com/fixture/service\"\n\nfunc main() { service.Run() }\n",
		"service/run.go":               "package service\n\nfunc Run() {}\n",
		"reference/api.ts":             "export function incompatible(name: string): string { return \"\"; }\n",
	})
	updatePath := filepath.Join(workspace.Root, "cmd", "main.go")
	baseline, err := os.ReadFile(updatePath)
	if err != nil {
		t.Fatalf("read workspace source baseline: %v", err)
	}
	output := &strings.Builder{}
	standardError := &strings.Builder{}

	if exitCode := run([]string{"benchmark", "--format", "json", workspace.Root}, output, standardError); exitCode != 0 {
		t.Fatalf("run Go workspace benchmark: exit code %d, error %s", exitCode, standardError.String())
	}

	var result struct {
		Result struct {
			Runs []struct {
				OutputChecksum string `json:"outputChecksum"`
			} `json:"runs"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output.String()), &result); err != nil {
		t.Fatalf("decode Go workspace benchmark result: %v\n%s", err, output.String())
	}
	if len(result.Result.Runs) != 1 {
		t.Errorf("benchmark runs = %+v, want one workspace measurement", result.Result.Runs)
	}
	for runIndex, run := range result.Result.Runs {
		if run.OutputChecksum == "" {
			t.Errorf("benchmark run %d has no output checksum", runIndex)
		}
	}
	contents, err := os.ReadFile(updatePath)
	if err != nil {
		t.Fatalf("read benchmarked workspace source: %v", err)
	}
	if string(contents) != string(baseline) {
		t.Errorf("benchmarked workspace source changed\ngot:  %q\nwant: %q", contents, baseline)
	}
	if progress := standardError.String(); !strings.Contains(progress, "Benchmark setup: measure 2 workspace source files") {
		t.Errorf("benchmark progress = %q, want ignored source exclusion", progress)
	}
}

func TestBenchmarkCommandRejectsWorkspaceWithoutSources(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"README.md": "No source files.\n",
	})
	output := &strings.Builder{}
	standardError := &strings.Builder{}

	if exitCode := run([]string{"benchmark", workspace.Root}, output, standardError); exitCode != 2 {
		t.Errorf("exit code = %d, want 2", exitCode)
	}
	if errorOutput := standardError.String(); !strings.Contains(errorOutput, "no supported source files") {
		t.Errorf("error = %q, want missing source files message", errorOutput)
	}
}

func TestBenchmarkCommandRejectsNonpositiveCorpusSize(t *testing.T) {
	output := &strings.Builder{}
	standardError := &strings.Builder{}

	if exitCode := run([]string{"benchmark", "--source-files", "0"}, output, standardError); exitCode != 2 {
		t.Errorf("benchmark invalid source file count exit code = %d, want 2", exitCode)
	}
	if !strings.Contains(standardError.String(), "must be positive") {
		t.Errorf("benchmark invalid source file count error = %q, want validation error", standardError.String())
	}
}
