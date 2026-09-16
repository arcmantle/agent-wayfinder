package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agent-wayfinder/graph"
	"agent-wayfinder/query"
	"agent-wayfinder/testkit"
)

func TestMain(testMain *testing.M) {
	previous := os.Getenv("GOFLAGS")
	flags := strings.TrimSpace(previous + " -tags=sqlite_fts5")
	if err := os.Setenv("GOFLAGS", flags); err != nil {
		panic(err)
	}
	os.Exit(testMain.Run())
}

func TestCommandRuns(t *testing.T) {
	command := exec.Command("go", "run", ".")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run command: %v\n%s", err, output)
	}
}

func TestCommandHelpListsPublicCommands(t *testing.T) {
	standardOutput := &strings.Builder{}
	standardError := &strings.Builder{}

	if exitCode := run([]string{"--help"}, standardOutput, standardError); exitCode != 0 {
		t.Fatalf("run help command: exit code %d, error %s", exitCode, standardError.String())
	}

	output := standardOutput.String()
	for _, command := range []string{"install", "index", "query", "path", "explain", "export", "indexer", "benchmark", "mcp"} {
		if !strings.Contains(output, command) {
			t.Errorf("help output = %q, want public command %q", output, command)
		}
	}
}

func TestInstallCommandWritesUserSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	standardOutput := &strings.Builder{}
	standardError := &strings.Builder{}

	if exitCode := run([]string{"install"}, standardOutput, standardError); exitCode != 0 {
		t.Fatalf("run install command: exit code %d, error %s", exitCode, standardError.String())
	}

	skillDirectory := filepath.Join(home, ".agents", "skills", "agent-wayfinder")
	assertFileContains(t, filepath.Join(skillDirectory, "SKILL.md"), "name: agent-wayfinder")
	assertFileContains(t, filepath.Join(skillDirectory, "references", "commands.md"), "# Agent Wayfinder Command Reference")
	if output := standardOutput.String(); !strings.Contains(output, skillDirectory) {
		t.Errorf("install output = %q, want destination %q", output, skillDirectory)
	}

	skillPath := filepath.Join(skillDirectory, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("stale"), 0o644); err != nil {
		t.Fatalf("write stale skill: %v", err)
	}
	if exitCode := run([]string{"install"}, &strings.Builder{}, standardError); exitCode != 0 {
		t.Fatalf("update installed skill: exit code %d, error %s", exitCode, standardError.String())
	}
	assertFileContains(t, skillPath, "name: agent-wayfinder")
}

func TestInstallCommandWritesProjectSkill(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Chdir(project)
	standardOutput := &strings.Builder{}
	standardError := &strings.Builder{}

	if exitCode := run([]string{"install", "--project"}, standardOutput, standardError); exitCode != 0 {
		t.Fatalf("run project install command: exit code %d, error %s", exitCode, standardError.String())
	}

	skillDirectory := filepath.Join(project, ".agents", "skills", "agent-wayfinder")
	assertFileContains(t, filepath.Join(skillDirectory, "SKILL.md"), "name: agent-wayfinder")
	assertFileContains(t, filepath.Join(skillDirectory, "references", "commands.md"), "# Agent Wayfinder Command Reference")
	if _, err := os.Stat(filepath.Join(home, ".agents", "skills", "agent-wayfinder", "SKILL.md")); !os.IsNotExist(err) {
		t.Errorf("global skill exists after project install: %v", err)
	}
}

func TestInstalledSkillStartsArchitectureQuestionsWithDeterministicQuestionMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if exitCode := run([]string{"install"}, &strings.Builder{}, &strings.Builder{}); exitCode != 0 {
		t.Fatalf("run install command: exit code %d", exitCode)
	}

	skillPath := filepath.Join(home, ".agents", "skills", "agent-wayfinder", "SKILL.md")
	contents, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read installed skill: %v", err)
	}
	skill := string(contents)
	for _, required := range []string{
		"one question-mode JSON query",
		"query plan, confidence, warnings, graph version, truncation, and ranked evidence",
		"Do not invent a query plan",
	} {
		if !strings.Contains(skill, required) {
			t.Errorf("installed skill does not contain %q", required)
		}
	}
}

func TestInstalledCommandReferenceDescribesQuestionMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if exitCode := run([]string{"install"}, &strings.Builder{}, &strings.Builder{}); exitCode != 0 {
		t.Fatalf("run install command: exit code %d", exitCode)
	}

	referencePath := filepath.Join(home, ".agents", "skills", "agent-wayfinder", "references", "commands.md")
	contents, err := os.ReadFile(referencePath)
	if err != nil {
		t.Fatalf("read installed command reference: %v", err)
	}
	reference := string(contents)
	for _, required := range []string{
		"agent-wayfinder query WORKSPACE QUESTION",
		"--question",
		"--terms",
		"--show-plan",
		"schemaVersion`, `interpretation`, `evidence`, `limits`, `warnings`, and `suggestions",
	} {
		if !strings.Contains(reference, required) {
			t.Errorf("installed command reference does not contain %q", required)
		}
	}
}

func TestInstalledSkillAssetsMatchBundledAssets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if exitCode := run([]string{"install"}, &strings.Builder{}, &strings.Builder{}); exitCode != 0 {
		t.Fatalf("run install command: exit code %d", exitCode)
	}

	for _, asset := range []struct {
		bundled   string
		installed string
	}{
		{bundled: "skill_assets/SKILL.md", installed: filepath.Join("SKILL.md")},
		{bundled: "skill_assets/references/commands.md", installed: filepath.Join("references", "commands.md")},
	} {
		want, err := bundledSkill.ReadFile(asset.bundled)
		if err != nil {
			t.Fatalf("read bundled asset %s: %v", asset.bundled, err)
		}
		path := filepath.Join(home, ".agents", "skills", "agent-wayfinder", asset.installed)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read installed asset %s: %v", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("installed asset %s does not match bundled asset %s", path, asset.bundled)
		}
	}
}

func TestBundledSkillIncludesCommandReference(t *testing.T) {
	skill, err := bundledSkill.ReadFile("skill_assets/SKILL.md")
	if err != nil {
		t.Fatalf("read bundled skill: %v", err)
	}
	if !strings.Contains(string(skill), "./references/commands.md") {
		t.Errorf("bundled skill does not link to its command reference")
	}
	if !strings.Contains(string(skill), "MCP tools") {
		t.Errorf("bundled skill does not describe MCP tool use")
	}
	if _, err := bundledSkill.ReadFile("skill_assets/references/commands.md"); err != nil {
		t.Fatalf("read bundled command reference: %v", err)
	}
}

func assertFileContains(t *testing.T, path, expected string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(contents), expected) {
		t.Errorf("%s = %q, want text %q", path, contents, expected)
	}
}

func TestIndexCommandHelpListsItsFlags(t *testing.T) {
	standardOutput := &strings.Builder{}
	standardError := &strings.Builder{}

	if exitCode := run([]string{"index", "--help"}, standardOutput, standardError); exitCode != 0 {
		t.Fatalf("run index help command: exit code %d, error %s", exitCode, standardError.String())
	}

	if output := standardOutput.String(); !strings.Contains(output, "--database") {
		t.Errorf("index help output = %q, want database flag", output)
	}
}

func TestQueryCommandHelpDescribesQuestionAndTermModes(t *testing.T) {
	standardOutput := &strings.Builder{}
	standardError := &strings.Builder{}

	if exitCode := run([]string{"query", "--help"}, standardOutput, standardError); exitCode != 0 {
		t.Fatalf("run query help command: exit code %d, error %s", exitCode, standardError.String())
	}

	output := standardOutput.String()
	for _, expected := range []string{"QUESTION", "TERM...", "--question", "--terms", "--show-plan"} {
		if !strings.Contains(output, expected) {
			t.Errorf("query help output = %q, want %q", output, expected)
		}
	}
}

func TestIndexerCommandHelpListsItsActions(t *testing.T) {
	standardOutput := &strings.Builder{}
	standardError := &strings.Builder{}

	if exitCode := run([]string{"indexer", "--help"}, standardOutput, standardError); exitCode != 0 {
		t.Fatalf("run indexer help command: exit code %d, error %s", exitCode, standardError.String())
	}

	output := standardOutput.String()
	for _, action := range []string{"serve", "start", "status", "stop"} {
		if !strings.Contains(output, action) {
			t.Errorf("indexer help output = %q, want action %q", output, action)
		}
	}
}

func TestCommandRejectsUnsupportedOutputFormat(t *testing.T) {
	standardOutput := &strings.Builder{}
	standardError := &strings.Builder{}

	if exitCode := run([]string{"--format", "yaml"}, standardOutput, standardError); exitCode != 2 {
		t.Errorf("exit code = %d, want 2", exitCode)
	}
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
	for index, want := range []string{"initial_index", "incremental_update", "query", "path", "explain"} {
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
		".wayfinderignore": "reference/\n",
		"go.mod":           "module example.com/fixture\n\ngo 1.24\n",
		"cmd/main.go":      "package main\n\nimport \"example.com/fixture/service\"\n\nfunc main() { service.Run() }\n",
		"service/run.go":   "package service\n\nfunc Run() {}\n",
		"reference/api.ts": "export function incompatible(name: string): string { return \"\"; }\n",
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

func TestIndexCommandPublishesWorkspaceGraph(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/main.ts":  "export function main() { return 1; }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")

	command := exec.Command("go", "run", ".", "index", "--database", database, "--format", "json", workspace.Root)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}

	var result struct {
		GraphVersion int    `json:"graphVersion"`
		PublishedAt  string `json:"publishedAt"`
		Result       struct {
			Workspace string `json:"workspace"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode index result: %v\n%s", err, output)
	}
	if result.GraphVersion != 1 {
		t.Errorf("graph version = %d, want 1", result.GraphVersion)
	}
	if result.PublishedAt == "" {
		t.Error("published time is empty")
	}
	if result.Result.Workspace != workspace.Root {
		t.Errorf("workspace = %q, want %q", result.Result.Workspace, workspace.Root)
	}
	if _, err := os.Stat(database); err != nil {
		t.Errorf("database %q does not exist: %v", database, err)
	}
}

func TestIndexCommandUsesWorkspaceLocalDatabaseByDefault(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/main.ts":  "export function main() { return 1; }",
	})

	command := exec.Command("go", "run", ".", "index", workspace.Root)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}

	database := filepath.Join(workspace.Root, ".agent-wayfinder", "graph.db")
	if _, err := os.Stat(database); err != nil {
		t.Errorf("default database %q does not exist: %v", database, err)
	}
}

func TestExportCommandWritesPublishedGraphAsJSON(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")

	indexCommand := exec.Command("go", "run", ".", "index", "--database", database, workspace.Root)
	if output, err := indexCommand.CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("read working directory: %v", err)
	}
	relativeWorkspace, err := filepath.Rel(workingDirectory, workspace.Root)
	if err != nil {
		t.Fatalf("resolve relative workspace: %v", err)
	}

	exportCommand := exec.Command("go", "run", ".", "export", "--database", database, "--format", "json", relativeWorkspace)
	output, err := exportCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("run export command: %v\n%s", err, output)
	}

	var result struct {
		GraphVersion int    `json:"graphVersion"`
		PublishedAt  string `json:"publishedAt"`
		Result       struct {
			Nodes []struct {
				ID       string `json:"id"`
				Evidence any    `json:"evidence"`
			} `json:"nodes"`
			Edges []struct {
				Relation string `json:"relation"`
				Evidence any    `json:"evidence"`
			} `json:"edges"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode export result: %v\n%s", err, output)
	}
	var schema map[string]any
	if err := json.Unmarshal(output, &schema); err != nil {
		t.Fatalf("decode export schema: %v\n%s", err, output)
	}
	resultData := schema["result"].(map[string]any)
	nodes := resultData["nodes"].([]any)
	firstNode := nodes[0].(map[string]any)
	if _, exists := firstNode["id"]; !exists {
		t.Errorf("exported node fields = %v, want lower-camel-case id", firstNode)
	}
	if _, exists := firstNode["ID"]; exists {
		t.Errorf("exported node fields = %v, do not want upper-case ID", firstNode)
	}
	if result.GraphVersion != 1 {
		t.Errorf("graph version = %d, want 1", result.GraphVersion)
	}
	if result.PublishedAt == "" {
		t.Error("published time is empty")
	}
	if len(result.Result.Nodes) == 0 || result.Result.Nodes[0].Evidence == nil {
		t.Errorf("exported nodes = %+v, want nodes with evidence", result.Result.Nodes)
	}
	for _, edge := range result.Result.Edges {
		if edge.Relation == "typescript:imports_from" && edge.Evidence != nil {
			return
		}
	}
	t.Errorf("exported edges = %+v, want resolved import edge with evidence", result.Result.Edges)
}

func TestExportCommandRejectsWorkspaceWithoutPublishedGraph(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	if err := os.MkdirAll(filepath.Dir(database), 0o755); err != nil {
		t.Fatalf("create database directory: %v", err)
	}

	command := exec.Command("go", "run", ".", "export", "--database", database, workspace.Root)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("run export command succeeded: %s", output)
	}
	if got := string(output); !strings.Contains(got, "no published graph is available for workspace") {
		t.Errorf("export error = %q, want index unavailable message", got)
	}
}

func TestQueryCommandReturnsRankedSeedsAndBoundedEvidence(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	if output, err := exec.Command("go", "run", ".", "index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}

	command := exec.Command("go", "run", ".", "query", "--database", database, "--format", "json", "--max-depth", "1", workspace.Root, "main")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run query command: %v\n%s", err, output)
	}

	var result struct {
		GraphVersion int    `json:"graphVersion"`
		PublishedAt  string `json:"publishedAt"`
		Result       struct {
			Seeds []struct {
				Term  string `json:"term"`
				Nodes []struct {
					QualifiedName string `json:"qualifiedName"`
				} `json:"nodes"`
			} `json:"seeds"`
			Nodes []struct {
				QualifiedName string `json:"qualifiedName"`
			} `json:"nodes"`
			Edges []struct {
				Relation string `json:"relation"`
			} `json:"edges"`
			MaxDepth int `json:"maxDepth"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode query result: %v\n%s", err, output)
	}
	if result.GraphVersion != 1 || result.PublishedAt == "" {
		t.Errorf("query envelope = %+v, want published graph metadata", result)
	}
	if len(result.Result.Seeds) != 1 || result.Result.Seeds[0].Term != "main" || len(result.Result.Seeds[0].Nodes) == 0 || result.Result.Seeds[0].Nodes[0].QualifiedName != "src/main.ts::main" {
		t.Errorf("query seeds = %+v, want ranked main seed", result.Result.Seeds)
	}
	if result.Result.MaxDepth != 1 {
		t.Errorf("maximum depth = %d, want 1", result.Result.MaxDepth)
	}
	hasCallEdge := false
	for _, edge := range result.Result.Edges {
		if edge.Relation == "typescript:calls" {
			hasCallEdge = true
			break
		}
	}
	if len(result.Result.Nodes) == 0 || !hasCallEdge {
		t.Errorf("query facts = nodes %+v, edges %+v, want bounded call evidence", result.Result.Nodes, result.Result.Edges)
	}
}

func TestQueryCommandSelectsQuestionModeAndReturnsPlan(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/main.ts":  "export function main() { return 1; }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	if output, err := exec.Command("go", "run", ".", "index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}

	output, err := exec.Command("go", "run", ".", "query", "--database", database, "--format", "json", "--max-depth", "1", "--max-nodes", "25", workspace.Root, "Where is main?").CombinedOutput()
	if err != nil {
		t.Fatalf("run question query: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			SchemaVersion  int `json:"schemaVersion"`
			Interpretation struct {
				SchemaVersion int     `json:"schemaVersion"`
				Question      string  `json:"question"`
				Intent        string  `json:"intent"`
				Confidence    float64 `json:"confidence"`
				Operator      string  `json:"operator"`
				MaxDepth      int     `json:"maxDepth"`
				MaxNodes      int     `json:"maxNodes"`
				EntitySlots   []struct {
					Role string `json:"role"`
					Text string `json:"text"`
				} `json:"entitySlots"`
			} `json:"interpretation"`
			Seeds []struct {
				Role     string `json:"role"`
				Term     string `json:"term"`
				Rankings []struct {
					NodeID     string  `json:"nodeId"`
					Score      float64 `json:"score"`
					Components struct {
						Exact          float64 `json:"exact"`
						Lexical        float64 `json:"lexical"`
						ReciprocalRank float64 `json:"reciprocalRank"`
					} `json:"components"`
				} `json:"rankings"`
			} `json:"seeds"`
			Evidence    []query.EvidenceGroup `json:"evidence"`
			Limits      []query.StageLimit    `json:"limits"`
			Warnings    []query.PlanWarning   `json:"warnings"`
			Suggestions []string              `json:"suggestions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode question query: %v\n%s", err, output)
	}
	if result.Result.SchemaVersion != 1 {
		t.Errorf("question result schema version = %d, want 1", result.Result.SchemaVersion)
	}
	plan := result.Result.Interpretation
	if plan.SchemaVersion != 1 || plan.Question != "Where is main?" || plan.Intent != "lookup" || plan.Confidence <= 0 || plan.Operator != "lookup" {
		t.Errorf("question plan = %+v, want stable lookup plan", plan)
	}
	if plan.MaxDepth != 1 || plan.MaxNodes != 25 {
		t.Errorf("question plan limits = {%d, %d}, want {1, 25}", plan.MaxDepth, plan.MaxNodes)
	}
	if len(plan.EntitySlots) != 1 || plan.EntitySlots[0].Role != "entity" || plan.EntitySlots[0].Text != "main" {
		t.Errorf("question slots = %+v, want main entity slot", plan.EntitySlots)
	}
	if len(result.Result.Seeds) != 1 || result.Result.Seeds[0].Term != "main" {
		t.Errorf("question seeds = %+v, want current lookup for main", result.Result.Seeds)
	}
	seed := result.Result.Seeds[0]
	if seed.Role != "entity" || len(seed.Rankings) == 0 || seed.Rankings[0].NodeID == "" || seed.Rankings[0].Score <= 0 {
		t.Fatalf("question seed rankings = %+v, want role-bound scored candidates", seed)
	}
	components := seed.Rankings[0].Components
	if components.Exact <= 0 && (components.Lexical <= 0 || components.ReciprocalRank <= 0) {
		t.Errorf("question seed components = %+v, want exact or fused lexical score", components)
	}
	if len(result.Result.Evidence) == 0 || result.Result.Evidence[0].Rank != 1 || result.Result.Evidence[0].SlotRole != "entity" || len(result.Result.Evidence[0].Nodes) != 1 || result.Result.Evidence[0].Nodes[0].Evidence.Span.Path == "" {
		t.Errorf("question evidence = %+v, want ranked entity evidence", result.Result.Evidence)
	}
	if len(result.Result.Limits) != 1 || result.Result.Limits[0].Stage != "retrieval" || result.Result.Limits[0].SlotRole != "entity" {
		t.Errorf("question limits = %+v, want entity retrieval limit", result.Result.Limits)
	}

	repeatedOutput, err := exec.Command("go", "run", ".", "query", "--database", database, "--format", "json", "--max-depth", "1", "--max-nodes", "25", workspace.Root, "Where is main?").CombinedOutput()
	if err != nil {
		t.Fatalf("repeat question query: %v\n%s", err, repeatedOutput)
	}
	if !bytes.Equal(output, repeatedOutput) {
		t.Errorf("repeated question JSON differs\nfirst: %s\nsecond: %s", output, repeatedOutput)
	}

	textOutput, err := exec.Command("go", "run", ".", "query", "--show-plan", "--database", database, workspace.Root, "Where is main?").CombinedOutput()
	if err != nil {
		t.Fatalf("run text question query: %v\n%s", err, textOutput)
	}
	if !strings.Contains(string(textOutput), "Interpreted as: lookup (lookup, confidence 1.00)") {
		t.Errorf("question text = %q, want interpreted plan", textOutput)
	}
}

func TestQueryCommandReportsLowConfidenceEmptyQuestionWithoutAnAnswerClaim(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/main.ts":  "export function main() { return 1; }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	if output, err := exec.Command("go", "run", ".", "index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}

	question := "Why do lunar widgets shimmer?"
	output, err := exec.Command("go", "run", ".", "query", "--database", database, "--format", "json", workspace.Root, question).CombinedOutput()
	if err != nil {
		t.Fatalf("run low-confidence question: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			SchemaVersion  int                   `json:"schemaVersion"`
			Interpretation query.QueryPlan       `json:"interpretation"`
			Evidence       []query.EvidenceGroup `json:"evidence"`
			Warnings       []query.PlanWarning   `json:"warnings"`
			Suggestions    []string              `json:"suggestions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode low-confidence question: %v\n%s", err, output)
	}
	if result.Result.SchemaVersion != 1 || result.Result.Interpretation.Confidence >= 0.5 {
		t.Errorf("low-confidence result = %+v, want versioned low-confidence interpretation", result.Result)
	}
	if len(result.Result.Evidence) != 0 || len(result.Result.Warnings) == 0 || len(result.Result.Suggestions) == 0 {
		t.Errorf("low-confidence result = %+v, want no evidence plus warnings and suggestions", result.Result)
	}

	textOutput, err := exec.Command("go", "run", ".", "query", "--database", database, workspace.Root, question).CombinedOutput()
	if err != nil {
		t.Fatalf("run low-confidence text question: %v\n%s", err, textOutput)
	}
	if !strings.Contains(string(textOutput), "No answer-ready evidence was found.") || !strings.Contains(string(textOutput), "Next:") {
		t.Errorf("low-confidence text = %q, want empty-result text and next command", textOutput)
	}
}

func TestQueryResultDataIncludesImpactEvidence(t *testing.T) {
	node := graph.Node{ID: "function:dependent", Kind: "function", Label: "dependent"}
	result := queryResultData(query.Result{Impact: []query.ImpactEvidence{{
		Node:     node,
		Relation: "calls",
		Distance: 1,
		Score:    1,
	}}}, nil, 2, 100)

	if len(result.Impact) != 1 || result.Impact[0].Node.ID != node.ID || result.Impact[0].Relation != "calls" || result.Impact[0].Distance != 1 || result.Impact[0].Score != 1 {
		t.Errorf("impact result = %+v, want visible node, relation, distance, and score", result.Impact)
	}
}

func TestQueryCommandReportsAmbiguousExplainWithoutNeighborhoodEvidence(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/first.ts":  "export function helper() { return 1; }",
		"src/second.ts": "export function helper() { return 2; }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	if output, err := exec.Command("go", "run", ".", "index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}

	output, err := exec.Command("go", "run", ".", "query", "--database", database, "--format", "json", workspace.Root, "Explain helper").CombinedOutput()
	if err != nil {
		t.Fatalf("run explain question: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			Warnings []struct {
				Code        string   `json:"code"`
				Suggestions []string `json:"suggestions"`
			} `json:"warnings"`
			Seeds []struct {
				Nodes []graph.Node `json:"nodes"`
			} `json:"seeds"`
			Nodes []graph.Node `json:"nodes"`
			Edges []graph.Edge `json:"edges"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode explain question: %v\n%s", err, output)
	}
	if len(result.Result.Warnings) != 1 || result.Result.Warnings[0].Code != "ambiguous_entity" || len(result.Result.Warnings[0].Suggestions) < 2 {
		t.Fatalf("warnings = %+v, want ambiguous_entity with exact follow-up commands", result.Result.Warnings)
	}
	if len(result.Result.Seeds) != 1 || len(result.Result.Seeds[0].Nodes) < 2 {
		t.Errorf("candidates = %+v, want ranked ambiguous candidates", result.Result.Seeds)
	}
	if len(result.Result.Nodes) != 0 || len(result.Result.Edges) != 0 {
		t.Errorf("neighborhood = {%+v, %+v}, want no answer evidence", result.Result.Nodes, result.Result.Edges)
	}
}

func TestQueryCommandReturnsDirectedPathEvidenceForAQuestion(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	if output, err := exec.Command("go", "run", ".", "index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}

	output, err := exec.Command("go", "run", ".", "query", "--database", database, "--format", "json", workspace.Root, "How does src/main.ts::main reach src/helper.ts::helper?").CombinedOutput()
	if err != nil {
		t.Fatalf("run path question: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			Plan struct {
				Intent   string `json:"intent"`
				Operator string `json:"operator"`
			} `json:"plan"`
			Nodes []graph.Node `json:"nodes"`
			Edges []graph.Edge `json:"edges"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode path question: %v\n%s", err, output)
	}
	if result.Result.Plan.Intent != "path" || result.Result.Plan.Operator != "path" {
		t.Errorf("plan = %+v, want path intent and operator", result.Result.Plan)
	}
	pathNodeNames := make([]string, len(result.Result.Nodes))
	for index, node := range result.Result.Nodes {
		pathNodeNames[index] = node.QualifiedName
	}
	if got, want := pathNodeNames, []string{"src/main.ts::main", "src/helper.ts::helper"}; !reflect.DeepEqual(got, want) {
		t.Errorf("path node names = %v, want %v", got, want)
	}
	if len(result.Result.Edges) != 1 || result.Result.Edges[0].Relation != "typescript:calls" {
		t.Errorf("path edges = %+v, want one TypeScript call edge", result.Result.Edges)
	}
}

func TestQueryCommandModeFlagsPreserveTermsAndRejectConflict(t *testing.T) {
	standardOutput := &strings.Builder{}
	standardError := &strings.Builder{}
	if exitCode := run([]string{"query", "--question", "--terms", ".", "main"}, standardOutput, standardError); exitCode != 2 {
		t.Fatalf("conflicting mode exit code = %d, want 2; error %s", exitCode, standardError.String())
	}
	if !strings.Contains(standardError.String(), "--question and --terms cannot be used together") {
		t.Errorf("conflicting mode error = %q, want mode conflict", standardError.String())
	}
	standardOutput.Reset()
	standardError.Reset()
	if exitCode := run([]string{"query", "--question", ".", "Where is main?", "Where is helper?"}, standardOutput, standardError); exitCode != 2 {
		t.Fatalf("multiple questions exit code = %d, want 2; error %s", exitCode, standardError.String())
	}
	if !strings.Contains(standardError.String(), "question mode requires exactly one question argument") {
		t.Errorf("multiple questions error = %q, want argument count error", standardError.String())
	}

	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/main.ts":  "export function main() { return 1; }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	if output, err := exec.Command("go", "run", ".", "index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}
	output, err := exec.Command("go", "run", ".", "query", "--terms", "--database", database, "--format", "json", workspace.Root, "Where is main?").CombinedOutput()
	if err != nil {
		t.Fatalf("run explicit terms query: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			Plan  json.RawMessage `json:"plan"`
			Seeds []struct {
				Term string `json:"term"`
			} `json:"seeds"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode explicit terms query: %v\n%s", err, output)
	}
	if result.Result.Plan != nil {
		t.Errorf("legacy terms plan = %s, want no question plan", result.Result.Plan)
	}
	if len(result.Result.Seeds) != 1 || result.Result.Seeds[0].Term != "Where is main?" {
		t.Errorf("legacy terms seeds = %+v, want unchanged literal term", result.Result.Seeds)
	}
}

func TestQueryCommandReportsLimitsAndFilteredEvidence(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	if output, err := exec.Command("go", "run", ".", "index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}

	command := exec.Command("go", "run", ".", "query", "--database", database, "--format", "json", "--max-depth", "0", "--relation", "typescript:calls", workspace.Root, "src/main.ts::main")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run query command: %v\n%s", err, output)
	}

	var result struct {
		Result struct {
			Nodes []struct {
				QualifiedName string `json:"qualifiedName"`
				Evidence      struct {
					Confidence string `json:"confidence"`
					Span       struct {
						Path string `json:"path"`
					} `json:"span"`
				} `json:"evidence"`
			} `json:"nodes"`
			Edges []struct {
				Relation string `json:"relation"`
			} `json:"edges"`
			TruncationReasons []string `json:"truncationReasons"`
			MaxDepth          int      `json:"maxDepth"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode query result: %v\n%s", err, output)
	}
	if result.Result.MaxDepth != 0 {
		t.Errorf("maximum depth = %d, want 0", result.Result.MaxDepth)
	}
	if len(result.Result.Nodes) != 1 || result.Result.Nodes[0].QualifiedName != "src/main.ts::main" || result.Result.Nodes[0].Evidence.Confidence == "" || result.Result.Nodes[0].Evidence.Span.Path != "src/main.ts" {
		t.Errorf("query nodes = %+v, want evidenced main seed only", result.Result.Nodes)
	}
	if len(result.Result.Edges) != 0 {
		t.Errorf("query edges = %+v, want no edges at depth zero", result.Result.Edges)
	}
	if len(result.Result.TruncationReasons) != 1 || result.Result.TruncationReasons[0] != "depth_limit" {
		t.Errorf("truncation reasons = %v, want depth limit", result.Result.TruncationReasons)
	}
}

func TestExplainCommandReportsNodeEvidenceAndGroupedDirectEdges(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")

	indexCommand := exec.Command("go", "run", ".", "index", "--database", database, workspace.Root)
	if output, err := indexCommand.CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}

	textCommand := exec.Command("go", "run", ".", "explain", "--database", database, workspace.Root, "src/helper.ts::helper")
	textOutput, err := textCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("run explain command: %v\n%s", err, textOutput)
	}
	for _, want := range []string{
		"Node:",
		"Source: src/helper.ts:",
		"Extractor: typescript",
		"Direct edges:",
		"typescript:imports_from (1):",
	} {
		if !strings.Contains(string(textOutput), want) {
			t.Errorf("explain text = %q, want %q", textOutput, want)
		}
	}

	jsonCommand := exec.Command("go", "run", ".", "explain", "--database", database, "--format", "json", workspace.Root, "src/helper.ts::helper")
	jsonOutput, err := jsonCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("run explain JSON command: %v\n%s", err, jsonOutput)
	}
	var result struct {
		Result struct {
			Explanation struct {
				Node struct {
					Evidence struct {
						Extractor  string `json:"extractor"`
						Confidence string `json:"confidence"`
					} `json:"evidence"`
				} `json:"node"`
				SupportingFacts struct {
					Edges []struct {
						Relation string `json:"relation"`
						Evidence any    `json:"evidence"`
					} `json:"edges"`
				} `json:"supportingFacts"`
			} `json:"explanation"`
		} `json:"result"`
	}
	if err := json.Unmarshal(jsonOutput, &result); err != nil {
		t.Fatalf("decode explain JSON result: %v\n%s", err, jsonOutput)
	}
	if !strings.HasPrefix(result.Result.Explanation.Node.Evidence.Extractor, "typescript") || result.Result.Explanation.Node.Evidence.Confidence == "" {
		t.Errorf("node evidence = %+v, want TypeScript evidence with confidence", result.Result.Explanation.Node.Evidence)
	}
	for _, edge := range result.Result.Explanation.SupportingFacts.Edges {
		if edge.Relation == "typescript:imports_from" && edge.Evidence != nil {
			return
		}
	}
	t.Errorf("supporting edges = %+v, want an evidenced import edge", result.Result.Explanation.SupportingFacts.Edges)
}

func TestExplainCommandReportsAmbiguousCandidatesAndRemainderCount(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/a.ts":     "export function helper() { return 1; }",
		"src/b.ts":     "export function helper() { return 2; }",
		"src/c.ts":     "export function helper() { return 3; }",
		"src/d.ts":     "export function helper() { return 4; }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	if output, err := exec.Command("go", "run", ".", "index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}

	command := exec.Command("go", "run", ".", "explain", "--database", database, "--format", "json", workspace.Root, "helper")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run ambiguous explain command: %v\n%s", err, output)
	}
	var result struct {
		Result struct {
			Candidates []struct {
				QualifiedName string `json:"qualifiedName"`
			} `json:"candidates"`
			RemainderCount int `json:"remainderCount"`
			Explanation    any `json:"explanation"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode ambiguous explain JSON result: %v\n%s", err, output)
	}
	if len(result.Result.Candidates) != 3 {
		t.Errorf("candidates = %+v, want three displayed candidates", result.Result.Candidates)
	}
	var candidates []string
	for _, candidate := range result.Result.Candidates {
		candidates = append(candidates, candidate.QualifiedName)
	}
	if want := []string{"src/a.ts::helper", "src/b.ts::helper", "src/c.ts::helper"}; !reflect.DeepEqual(candidates, want) {
		t.Errorf("candidate qualified names = %v, want %v", candidates, want)
	}
	if result.Result.RemainderCount != 1 {
		t.Errorf("remainder count = %d, want 1", result.Result.RemainderCount)
	}
	if result.Result.Explanation != nil {
		t.Errorf("explanation = %+v, want none for ambiguous candidates", result.Result.Explanation)
	}
}

func TestPathCommandReportsDirectedPathAndDeterministicNoResult(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	if output, err := exec.Command("go", "run", ".", "index", "--database", database, workspace.Root).CombinedOutput(); err != nil {
		t.Fatalf("run index command: %v\n%s", err, output)
	}

	pathCommand := exec.Command("go", "run", ".", "path", "--database", database, workspace.Root, "src/main.ts::main", "src/helper.ts::helper")
	pathOutput, err := pathCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("run path command: %v\n%s", err, pathOutput)
	}
	if got := string(pathOutput); !strings.Contains(got, "src/main.ts::main") || !strings.Contains(got, "src/helper.ts::helper") || !strings.Contains(got, "typescript:calls") {
		t.Errorf("path output = %q, want directed call path", got)
	}

	noResultCommand := exec.Command("go", "run", ".", "path", "--database", database, workspace.Root, "src/helper.ts::helper", "src/main.ts::main")
	noResultOutput, err := noResultCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("run no-result path command: %v\n%s", err, noResultOutput)
	}
	if got := string(noResultOutput); !strings.Contains(got, "No directed path found.") {
		t.Errorf("no-result path output = %q, want deterministic directed no-result", got)
	}

	fallbackCommand := exec.Command("go", "run", ".", "path", "--database", database, "--undirected", workspace.Root, "src/helper.ts::helper", "src/main.ts::main")
	fallbackOutput, err := fallbackCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("run undirected fallback path command: %v\n%s", err, fallbackOutput)
	}
	if got := string(fallbackOutput); !strings.Contains(got, "Used undirected fallback.") {
		t.Errorf("fallback path output = %q, want undirected fallback report", got)
	}

	missingFallbackCommand := exec.Command("go", "run", ".", "path", "--database", database, "--undirected", "--relation", "references", "--format", "json", workspace.Root, "src/main.ts::main", "src/helper.ts::helper")
	missingFallbackOutput, err := missingFallbackCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("run missing undirected path command: %v\n%s", err, missingFallbackOutput)
	}
	var missingFallbackResult struct {
		Result struct {
			UndirectedFallbackAttempted bool `json:"undirectedFallbackAttempted"`
		} `json:"result"`
	}
	if err := json.Unmarshal(missingFallbackOutput, &missingFallbackResult); err != nil {
		t.Fatalf("decode missing fallback path result: %v\n%s", err, missingFallbackOutput)
	}
	if !missingFallbackResult.Result.UndirectedFallbackAttempted {
		t.Errorf("missing fallback path result = %s, want fallback attempt", missingFallbackOutput)
	}
}

func TestIndexerLifecycleCommandsControlBackgroundService(t *testing.T) {
	workspace, err := os.MkdirTemp("", "ag-")
	if err != nil {
		t.Fatalf("create short workspace path: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(workspace); err != nil {
			t.Errorf("remove workspace: %v", err)
		}
	})
	binary := filepath.Join(t.TempDir(), "agent-wayfinder")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build command binary: %v\n%s", err, output)
	}
	request := func(arguments ...string) ([]byte, error) {
		return exec.Command(binary, arguments...).CombinedOutput()
	}

	output, err := request("indexer", "--format", "json", "start", workspace)
	if err != nil {
		t.Fatalf("start workspace indexer: %v\n%s", err, output)
	}
	var started struct {
		Workspace   string   `json:"workspace"`
		Running     bool     `json:"running"`
		QueuedPaths []string `json:"queuedPaths"`
	}
	if err := json.Unmarshal(output, &started); err != nil {
		t.Fatalf("decode started indexer status: %v\n%s", err, output)
	}
	if started.Workspace != workspace || !started.Running || started.QueuedPaths == nil {
		t.Errorf("started status = %+v, want running workspace with queued paths", started)
	}

	output, err = request("indexer", "--format", "json", "status", workspace)
	if err != nil {
		t.Fatalf("get workspace indexer status: %v\n%s", err, output)
	}
	var status map[string]any
	if err := json.Unmarshal(output, &status); err != nil {
		t.Fatalf("decode indexer status: %v\n%s", err, output)
	}
	for _, field := range []string{"workspace", "activity", "progress", "queuedPaths", "version", "error", "idleDeadline"} {
		if _, found := status[field]; !found {
			t.Errorf("status fields = %v, missing %q", status, field)
		}
	}

	output, err = request("indexer", "stop", workspace)
	if err != nil {
		t.Fatalf("stop workspace indexer: %v\n%s", err, output)
	}
	if _, err := request("indexer", "status", workspace); err == nil {
		t.Error("status after stop succeeded, want connection failure")
	}
}
