package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-wayfinder/benchmark"
	"agent-wayfinder/cli"
	"agent-wayfinder/extractor"
	"agent-wayfinder/graph"
	"agent-wayfinder/index"
	"agent-wayfinder/indexer"
	"agent-wayfinder/query"
	"agent-wayfinder/storage"
	"agent-wayfinder/storage/sqlite"
	"agent-wayfinder/workspace"

	"github.com/spf13/cobra"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, standardOutput, standardError io.Writer) int {
	command, exitCode := newRootCommand(standardOutput, standardError)
	command.SetArgs(arguments)
	if err := command.Execute(); err != nil {
		return writeCommandError(standardError, cli.NewInvalidArgumentError(err.Error()))
	}
	return *exitCode
}

func newRootCommand(standardOutput, standardError io.Writer) (*cobra.Command, *int) {
	exitCode := 0
	root := &cobra.Command{
		Use:           "agent-wayfinder",
		Short:         "Index and query a local code graph",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.SetOut(standardOutput)
	root.SetErr(standardError)

	addCommand := func(use, short string, configure func(*cobra.Command), runCommand func(*cobra.Command, []string, io.Writer, io.Writer) int) {
		command := &cobra.Command{
			Use:   use,
			Short: short,
			Run: func(command *cobra.Command, arguments []string) {
				exitCode = runCommand(command, arguments, standardOutput, standardError)
			},
		}
		configure(command)
		root.AddCommand(command)
	}

	addCommand("benchmark [WORKSPACE]", "Measure critical graph operations", benchmarkFlags, runBenchmark)
	addCommand("install", "Install the Agent Wayfinder skill", installFlags, runInstall)
	addCommand("index WORKSPACE", "Index a workspace", databaseAndFormatFlags, runIndex)
	addCommand("mcp", "Run the Model Context Protocol server", func(*cobra.Command) {}, runMCP)
	addCommand("query WORKSPACE (QUESTION | TERM...)", "Query a published graph", queryFlags, runQuery)
	root.AddCommand(newIndexerCommand(standardOutput, standardError, &exitCode))
	addCommand("export WORKSPACE", "Export a published graph", databaseAndFormatFlags, runExport)
	addCommand("explain WORKSPACE NODE", "Explain a graph node", databaseAndFormatFlags, runExplain)
	addCommand("path WORKSPACE SOURCE TARGET", "Find a graph path", pathFlags, runPath)

	return root, &exitCode
}

func newIndexerCommand(standardOutput, standardError io.Writer, exitCode *int) *cobra.Command {
	indexerCommand := &cobra.Command{
		Use:           "indexer",
		Short:         "Control the workspace indexer",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	indexerCommand.PersistentFlags().String("format", "", "output format: text or json")

	addAction := func(use, short string, action func(string, cli.Format, io.Writer, io.Writer) int) {
		indexerCommand.AddCommand(&cobra.Command{
			Use:   use + " WORKSPACE",
			Short: short,
			Args:  cobra.ExactArgs(1),
			Run: func(command *cobra.Command, arguments []string) {
				format, err := commandFormat(command)
				if err != nil {
					*exitCode = writeCommandError(standardError, err)
					return
				}
				*exitCode = action(arguments[0], format, standardOutput, standardError)
			},
		})
	}
	addAction("serve", "Run the workspace indexer", func(workspace string, _ cli.Format, _ io.Writer, standardError io.Writer) int {
		return runIndexerServer(workspace, standardError)
	})
	addAction("start", "Start the workspace indexer", runIndexerStart)
	addAction("status", "Show workspace indexer status", func(workspace string, format cli.Format, standardOutput, standardError io.Writer) int {
		return runIndexerRequest(workspace, indexer.StatusCommand, format, standardOutput, standardError)
	})
	addAction("stop", "Stop the workspace indexer", func(workspace string, format cli.Format, standardOutput, standardError io.Writer) int {
		return runIndexerRequest(workspace, indexer.StopCommand, format, standardOutput, standardError)
	})

	return indexerCommand
}

func formatFlag(command *cobra.Command) {
	command.Flags().String("format", "", "output format: text or json")
}

func benchmarkFlags(command *cobra.Command) {
	formatFlag(command)
	command.Flags().Int("source-files", benchmark.DefaultCorpusSpec.SourceFiles, "generated source file count")
	command.Flags().Int("functions-per-file", benchmark.DefaultCorpusSpec.FunctionsPerFile, "generated minimum function count per source file")
	command.Flags().Bool("exact-scale", false, "generate the exact-scale acceptance corpus")
	command.Flags().Int("runs", 1, "number of benchmark runs")
	command.Flags().Bool("realistic", false, "generate a realistic-density corpus (bounded imports, mixed declaration kinds) instead of the dense linear-chain corpus; ignores --functions-per-file")
	command.Flags().String("cpu-profile", "", "write an initial-index CPU profile to this file")
	command.Flags().String("incremental-cpu-profile", "", "write an incremental-update CPU profile to this file")
	command.Flags().Bool("internal-sample", false, "run one unvalidated benchmark sample")
	_ = command.Flags().MarkHidden("internal-sample")
}

func databaseAndFormatFlags(command *cobra.Command) {
	command.Flags().String("database", "", "SQLite database path")
	formatFlag(command)
}

func queryFlags(command *cobra.Command) {
	databaseAndFormatFlags(command)
	command.Flags().Bool("question", false, "interpret one argument as an architecture question")
	command.Flags().Bool("terms", false, "treat all arguments as literal lookup terms")
	command.Flags().Bool("show-plan", false, "show the interpreted query plan in text output")
	command.Flags().Int("max-depth", 2, "maximum traversal depth")
	command.Flags().Int("max-nodes", 100, "maximum traversed nodes")
	command.Flags().StringArray("project", nil, "project scope ID")
	command.Flags().StringArray("relation", nil, "allowed relation")
}

func pathFlags(command *cobra.Command) {
	databaseAndFormatFlags(command)
	command.Flags().Bool("undirected", false, "allow undirected fallback")
	command.Flags().Int("max-depth", 8, "maximum path depth")
	command.Flags().Int("max-nodes", 100, "maximum traversed nodes")
	command.Flags().StringArray("project", nil, "project scope ID")
	command.Flags().StringArray("relation", nil, "allowed relation")
}

func commandFormat(command *cobra.Command) (cli.Format, error) {
	value, err := command.Flags().GetString("format")
	if err != nil {
		return "", err
	}
	return cli.ParseFormat(value)
}

func commandDatabasePath(command *cobra.Command, workspace string) (string, error) {
	value, err := command.Flags().GetString("database")
	if err != nil {
		return "", err
	}
	return databasePath(workspace, value)
}

type benchmarkMeasurement struct {
	Name          string `json:"name"`
	DurationNS    int64  `json:"durationNs"`
	NotApplicable bool   `json:"notApplicable,omitempty"`
}

type benchmarkRunResult struct {
	Measurements               []benchmarkMeasurement `json:"measurements"`
	PhaseMeasurements          []benchmarkMeasurement `json:"phaseMeasurements"`
	ResolverMeasurements       []benchmarkMeasurement `json:"resolverMeasurements"`
	SQLiteWriteMeasurements    []benchmarkMeasurement `json:"sqliteWriteMeasurements"`
	SourceFiles                int                    `json:"sourceFiles"`
	NodeCount                  int                    `json:"nodeCount"`
	EdgeCount                  int                    `json:"edgeCount"`
	ContributionQueueHighWater int                    `json:"contributionQueueHighWater"`
	ContributionQueueCapacity  int                    `json:"contributionQueueCapacity"`
	PeakRSSBytes               uint64                 `json:"peakRssBytes"`
	RetainedHeapBytes          uint64                 `json:"retainedHeapBytes"`
	DatabaseBytes              int64                  `json:"databaseBytes"`
	OutputChecksum             string                 `json:"outputChecksum"`
}

type benchmarkResult struct {
	Configuration              benchmarkConfiguration    `json:"configuration"`
	RSSCalibration             *benchmark.RSSCalibration `json:"rssCalibration,omitempty"`
	ScaleShape                 *benchmark.ScaleShape     `json:"scaleShape,omitempty"`
	Measurements               []benchmarkMeasurement    `json:"measurements"`
	PhaseMeasurements          []benchmarkMeasurement    `json:"phaseMeasurements"`
	ResolverMeasurements       []benchmarkMeasurement    `json:"resolverMeasurements"`
	SQLiteWriteMeasurements    []benchmarkMeasurement    `json:"sqliteWriteMeasurements"`
	ContributionQueueHighWater int                       `json:"contributionQueueHighWater"`
	ContributionQueueCapacity  int                       `json:"contributionQueueCapacity"`
	Runs                       []benchmarkRunResult      `json:"runs"`
	PeakRSSBytes               uint64                    `json:"peakRssBytes"`
	RetainedHeapBytes          uint64                    `json:"retainedHeapBytes"`
	DatabaseBytes              int64                     `json:"databaseBytes"`
	OutputChecksum             string                    `json:"outputChecksum"`
}

type benchmarkConfiguration struct {
	ExtractionWorkers         int `json:"extractionWorkers"`
	SourceQueueCapacity       int `json:"sourceQueueCapacity"`
	ContributionQueueCapacity int `json:"contributionQueueCapacity"`
	ContributionBatchRows     int `json:"contributionBatchRows"`
	ContributionBatchBytes    int `json:"contributionBatchBytes"`
	ContributionBatchSources  int `json:"contributionBatchSources"`
	ResolverPageSize          int `json:"resolverPageSize"`
	WorkspaceFactBatchRows    int `json:"workspaceFactBatchRows"`
	WorkspaceFactBatchBytes   int `json:"workspaceFactBatchBytes"`
}

const benchmarkProgressInterval = 5 * time.Second

const benchmarkValidationProgressInterval = 100000

type benchmarkProgressReporter struct {
	writer     io.Writer
	runNumber  int
	totalRuns  int
	startedAt  time.Time
	completed  int
	phase      string
	stopTicker chan struct{}
	done       chan struct{}
}

type benchmarkSetupReporter struct {
	writer           io.Writer
	startedAt        time.Time
	phase            string
	completedSources int
	totalSources     int
	lastReported     int
	lastWrittenFacts int
	mutex            sync.Mutex
	stopTicker       chan struct{}
	done             chan struct{}
}

func newBenchmarkSetupReporter(writer io.Writer) *benchmarkSetupReporter {
	return &benchmarkSetupReporter{writer: writer, startedAt: time.Now(), stopTicker: make(chan struct{}), done: make(chan struct{})}
}

func (reporter *benchmarkSetupReporter) start(phase string, totalSources int) {
	reporter.mutex.Lock()
	reporter.phase = phase
	reporter.totalSources = totalSources
	reporter.mutex.Unlock()
	fmt.Fprintf(reporter.writer, "Benchmark setup: %s 0/%d sources\n", phase, totalSources)
	go func() {
		ticker := time.NewTicker(benchmarkProgressInterval)
		defer ticker.Stop()
		defer close(reporter.done)
		for {
			select {
			case <-ticker.C:
				reporter.reportHeartbeat()
			case <-reporter.stopTicker:
				return
			}
		}
	}()
}

func (reporter *benchmarkSetupReporter) update(progress index.Progress) {
	reporter.mutex.Lock()
	reporter.phase = "validation " + string(progress.Phase)
	reporter.completedSources = progress.CompletedSources
	reporter.totalSources = progress.TotalSources
	writtenFacts := progress.WrittenNodes + progress.WrittenEdges
	shouldReport := progress.Phase != index.ExtractPhase || progress.CompletedSources == progress.TotalSources || progress.CompletedSources-reporter.lastReported >= 1000
	if shouldReport && progress.Phase == index.ExtractPhase {
		reporter.lastReported = progress.CompletedSources
	}
	if progress.Phase == index.PublishPhase {
		shouldReport = writtenFacts == progress.TotalNodes+progress.TotalEdges || writtenFacts-reporter.lastWrittenFacts >= 100000
		if shouldReport {
			reporter.lastWrittenFacts = writtenFacts
		}
	}
	phase := reporter.phase
	completed := reporter.completedSources
	total := reporter.totalSources
	writtenNodes := progress.WrittenNodes
	totalNodes := progress.TotalNodes
	writtenEdges := progress.WrittenEdges
	totalEdges := progress.TotalEdges
	reporter.mutex.Unlock()
	if shouldReport {
		if progress.Phase == index.PublishPhase {
			fmt.Fprintf(reporter.writer, "Benchmark setup: %s %d/%d sources, %d/%d node records, %d/%d edge records\n", phase, completed, total, writtenNodes, totalNodes, writtenEdges, totalEdges)
		} else {
			fmt.Fprintf(reporter.writer, "Benchmark setup: %s %d/%d sources\n", phase, completed, total)
		}
	}
}

func (reporter *benchmarkSetupReporter) reportHeartbeat() {
	reporter.mutex.Lock()
	phase := reporter.phase
	completed := reporter.completedSources
	total := reporter.totalSources
	reporter.mutex.Unlock()
	fmt.Fprintf(reporter.writer, "Benchmark setup: %s %d/%d sources (%s elapsed)\n", phase, completed, total, time.Since(reporter.startedAt).Round(time.Second))
}

func (reporter *benchmarkSetupReporter) finish() {
	close(reporter.stopTicker)
	<-reporter.done
}

func newBenchmarkProgressReporter(writer io.Writer, runNumber, totalRuns, completed int) *benchmarkProgressReporter {
	return &benchmarkProgressReporter{
		writer:    writer,
		runNumber: runNumber,
		totalRuns: totalRuns,
		startedAt: time.Now(),
		completed: completed,
	}
}

func (reporter *benchmarkProgressReporter) start(phase string) {
	reporter.phase = phase
	fmt.Fprintf(reporter.writer, "Benchmark run %d/%d: %s\n", reporter.runNumber, reporter.totalRuns, phase)
	reporter.stopTicker = make(chan struct{})
	reporter.done = make(chan struct{})
	go func() {
		ticker := time.NewTicker(benchmarkProgressInterval)
		defer ticker.Stop()
		defer close(reporter.done)
		for {
			select {
			case <-ticker.C:
				reporter.reportHeartbeat()
			case <-reporter.stopTicker:
				return
			}
		}
	}()
}

func (reporter *benchmarkProgressReporter) finishPhase() {
	if reporter.stopTicker == nil {
		return
	}
	close(reporter.stopTicker)
	<-reporter.done
	reporter.stopTicker = nil
}

func (reporter *benchmarkProgressReporter) complete() {
	reporter.finishPhase()
	fmt.Fprintf(reporter.writer, "Benchmark run %d/%d: complete (%s elapsed)\n", reporter.runNumber, reporter.totalRuns, time.Since(reporter.startedAt).Round(time.Second))
}

func (reporter *benchmarkProgressReporter) reportHeartbeat() {
	elapsed := time.Since(reporter.startedAt).Round(time.Second)
	message := fmt.Sprintf("Benchmark run %d/%d: %s (%s elapsed", reporter.runNumber, reporter.totalRuns, reporter.phase, elapsed)
	if reporter.completed > 0 {
		estimatedTotal := time.Duration(reporter.completed+1) * elapsed / time.Duration(reporter.completed)
		remaining := estimatedTotal - elapsed
		message += fmt.Sprintf(", about %s remaining", max(remaining, 0).Round(time.Second))
	}
	fmt.Fprintln(reporter.writer, message+")")
}

func runBenchmark(command *cobra.Command, arguments []string, standardOutput, standardError io.Writer) (exitCode int) {
	if len(arguments) > 1 {
		return writeCommandError(standardError, cli.NewInvalidArgumentError("benchmark accepts at most one workspace argument"))
	}
	format, err := commandFormat(command)
	if err != nil {
		return writeCommandError(standardError, err)
	}
	cpuProfilePath, err := command.Flags().GetString("cpu-profile")
	if err != nil {
		return writeCommandError(standardError, err)
	}
	incrementalCPUProfilePath, err := command.Flags().GetString("incremental-cpu-profile")
	if err != nil {
		return writeCommandError(standardError, err)
	}
	realistic, err := command.Flags().GetBool("realistic")
	if err != nil {
		return writeCommandError(standardError, err)
	}
	exactScale, err := command.Flags().GetBool("exact-scale")
	if err != nil {
		return writeCommandError(standardError, err)
	}
	runCount, err := command.Flags().GetInt("runs")
	if err != nil {
		return writeCommandError(standardError, err)
	}
	if runCount <= 0 {
		return writeCommandError(standardError, cli.NewInvalidArgumentError("benchmark run count must be positive"))
	}
	internalSample, err := command.Flags().GetBool("internal-sample")
	if err != nil {
		return writeCommandError(standardError, err)
	}
	if exactScale && realistic {
		return writeCommandError(standardError, cli.NewInvalidArgumentError("benchmark exact-scale and realistic modes cannot be combined"))
	}
	if exactScale && len(arguments) != 0 {
		return writeCommandError(standardError, cli.NewInvalidArgumentError("benchmark exact-scale mode does not accept a workspace"))
	}
	var benchmarkWorkspace string
	var corpus benchmark.Corpus
	if len(arguments) == 0 && realistic {
		sourceFiles, err := command.Flags().GetInt("source-files")
		if err != nil {
			return writeCommandError(standardError, err)
		}
		benchmarkWorkspace, corpus, err = prepareRealisticBenchmarkCorpus(sourceFiles, standardError)
		if err != nil {
			return writeCommandError(standardError, err)
		}
		defer os.RemoveAll(benchmarkWorkspace)
	} else if len(arguments) == 0 {
		specification, err := benchmarkCorpusSpec(command)
		if err != nil {
			return writeCommandError(standardError, err)
		}
		benchmarkWorkspace, corpus, err = prepareBenchmarkCorpus(specification, standardError)
		if err != nil {
			return writeCommandError(standardError, err)
		}
		defer os.RemoveAll(benchmarkWorkspace)
	} else {
		benchmarkWorkspace, corpus, err = prepareWorkspaceBenchmark(arguments[0])
		if err != nil {
			return writeCommandError(standardError, err)
		}
		fmt.Fprintf(standardError, "Benchmark setup: measure %d workspace source files\n", corpus.SourceFiles)
	}
	stateDirectory, err := os.MkdirTemp("", "agent-wayfinder-benchmark-state-")
	if err != nil {
		return writeCommandError(standardError, fmt.Errorf("create benchmark state directory: %w", err))
	}
	defer os.RemoveAll(stateDirectory)
	updateSource := filepath.Join(benchmarkWorkspace, corpus.UpdatePath)
	baselineContents, err := os.ReadFile(updateSource)
	if err != nil {
		return writeCommandError(standardError, fmt.Errorf("read benchmark source baseline: %w", err))
	}
	defer func() {
		if err := os.WriteFile(updateSource, baselineContents, 0o644); err != nil {
			exitCode = writeCommandError(standardError, fmt.Errorf("restore benchmark source baseline: %w", err))
		}
	}()

	runs := make([]benchmark.Run, 0, runCount)
	var snapshot storage.Snapshot
	for runNumber := 0; runNumber < runCount; runNumber++ {
		if exactScale {
			run, runSnapshot, err := measureIsolatedBenchmarkRun(corpus.SourceFiles, standardError)
			if err != nil {
				return writeCommandError(standardError, err)
			}
			runs = append(runs, run)
			snapshot = runSnapshot
			continue
		}
		if err := os.WriteFile(updateSource, baselineContents, 0o644); err != nil {
			return writeCommandError(standardError, fmt.Errorf("restore benchmark source baseline: %w", err))
		}
		reporter := newBenchmarkProgressReporter(standardError, runNumber+1, runCount, len(runs))
		profilePath := ""
		incrementalProfilePath := ""
		if runNumber == 0 {
			profilePath = cpuProfilePath
			incrementalProfilePath = incrementalCPUProfilePath
		}
		run, runSnapshot, err := measureBenchmarkRun(benchmarkWorkspace, stateDirectory, corpus, reporter, profilePath, incrementalProfilePath)
		if err != nil {
			reporter.finishPhase()
			return writeCommandError(standardError, err)
		}
		reporter.complete()
		runs = append(runs, run)
		snapshot = runSnapshot
	}
	medianMeasurements := benchmark.Medians(runs)
	measurements := make([]benchmarkMeasurement, 0, len(medianMeasurements))
	for _, measurement := range medianMeasurements {
		measurements = append(measurements, benchmarkMeasurement{Name: measurement.Name, DurationNS: measurement.Duration.Nanoseconds()})
	}
	data := benchmarkResult{
		Configuration:              benchmarkConfigurationFromRuns(runs),
		Measurements:               measurements,
		PhaseMeasurements:          benchmarkMeasurements(benchmark.PhaseMedians(runs)),
		ResolverMeasurements:       benchmarkMeasurements(benchmark.ResolverMedians(runs)),
		SQLiteWriteMeasurements:    benchmarkMeasurements(benchmark.SQLiteWriteMedians(runs)),
		ContributionQueueHighWater: maxContributionQueueHighWater(runs),
		ContributionQueueCapacity:  maxContributionQueueCapacity(runs),
		Runs:                       benchmarkRunResults(runs),
		PeakRSSBytes:               maxPeakRSS(runs),
		RetainedHeapBytes:          maxRetainedHeap(runs),
		DatabaseBytes:              maxDatabaseBytes(runs),
		OutputChecksum:             runs[len(runs)-1].OutputChecksum,
	}
	if exactScale && corpus.SourceFiles == benchmark.DefaultCorpusSpec.SourceFiles {
		calibration := benchmark.ExactScaleRSSCalibration()
		data.RSSCalibration = &calibration
	}
	if exactScale && corpus.SourceFiles == benchmark.DefaultCorpusSpec.SourceFiles {
		smallerSpecification, err := benchmark.ExactScaleCorpusSpec(1000)
		if err != nil {
			return writeCommandError(standardError, err)
		}
		smallerWorkspace, smallerCorpus, err := prepareBenchmarkCorpus(smallerSpecification, standardError)
		if err != nil {
			return writeCommandError(standardError, err)
		}
		defer os.RemoveAll(smallerWorkspace)
		smallerBaseline, err := os.ReadFile(filepath.Join(smallerWorkspace, smallerCorpus.UpdatePath))
		if err != nil {
			return writeCommandError(standardError, fmt.Errorf("read smaller benchmark source baseline: %w", err))
		}
		smallerRuns := make([]benchmark.Run, 0, runCount)
		for runNumber := 0; runNumber < runCount; runNumber++ {
			if err := os.WriteFile(filepath.Join(smallerWorkspace, smallerCorpus.UpdatePath), smallerBaseline, 0o644); err != nil {
				return writeCommandError(standardError, fmt.Errorf("restore smaller benchmark source baseline: %w", err))
			}
			reporter := newBenchmarkProgressReporter(standardError, runNumber+1, runCount, len(smallerRuns))
			run, _, err := measureBenchmarkRun(smallerWorkspace, stateDirectory, smallerCorpus, reporter, "", "")
			if err != nil {
				reporter.finishPhase()
				return writeCommandError(standardError, fmt.Errorf("measure smaller exact-scale run: %w", err))
			}
			reporter.complete()
			smallerRuns = append(smallerRuns, run)
		}
		if err := benchmark.ValidateReport(smallerRuns, smallerCorpus.SourceFiles); err != nil {
			return writeCommandError(standardError, fmt.Errorf("validate smaller exact-scale report: %w", err))
		}
		shape, err := benchmark.MeasureScaleShape(smallerRuns, runs)
		if err != nil {
			return writeCommandError(standardError, err)
		}
		data.ScaleShape = &shape
	}
	if err := cli.Render(standardOutput, cli.Result{Snapshot: snapshot, Text: renderBenchmarkText(data), Data: data}, format); err != nil {
		return writeCommandError(standardError, err)
	}
	if internalSample {
		return 0
	}
	if exactScale {
		err = benchmark.ValidateExactScaleReport(runs, corpus.SourceFiles)
	} else {
		err = benchmark.ValidateReport(runs, corpus.SourceFiles)
	}
	if err != nil {
		return writeCommandError(standardError, err)
	}
	return 0
}

func measureIsolatedBenchmarkRun(sourceFiles int, standardError io.Writer) (benchmark.Run, storage.Snapshot, error) {
	executable, err := os.Executable()
	if err != nil {
		return benchmark.Run{}, storage.Snapshot{}, fmt.Errorf("find benchmark executable: %w", err)
	}
	arguments := []string{"benchmark", "--format", "json", "--source-files", strconv.Itoa(sourceFiles), "--runs", "1", "--internal-sample"}
	var command *exec.Cmd
	if strings.HasSuffix(executable, ".test") {
		command = exec.Command("go", append([]string{"run", "."}, arguments...)...)
	} else {
		command = exec.Command(executable, arguments...)
	}
	var output strings.Builder
	command.Stdout = &output
	command.Stderr = standardError
	if err := command.Run(); err != nil {
		return benchmark.Run{}, storage.Snapshot{}, fmt.Errorf("run isolated benchmark: %w", err)
	}
	var envelope struct {
		GraphVersion storage.GraphVersion `json:"graphVersion"`
		PublishedAt  string               `json:"publishedAt"`
		Result       benchmarkResult      `json:"result"`
	}
	if err := json.Unmarshal([]byte(output.String()), &envelope); err != nil {
		return benchmark.Run{}, storage.Snapshot{}, fmt.Errorf("decode isolated benchmark: %w", err)
	}
	if len(envelope.Result.Runs) != 1 {
		return benchmark.Run{}, storage.Snapshot{}, fmt.Errorf("isolated benchmark returned %d runs, want 1", len(envelope.Result.Runs))
	}
	publishedAt, err := time.Parse("2006-01-02T15:04:05Z", envelope.PublishedAt)
	if err != nil {
		return benchmark.Run{}, storage.Snapshot{}, fmt.Errorf("decode isolated benchmark publication time: %w", err)
	}
	return benchmarkRunFromResult(envelope.Result.Runs[0], envelope.Result.Configuration), storage.Snapshot{Version: envelope.GraphVersion, PublishedAt: publishedAt}, nil
}

func benchmarkRunFromResult(result benchmarkRunResult, configuration benchmarkConfiguration) benchmark.Run {
	return benchmark.Run{
		Measurements:               measurementsFromResults(result.Measurements),
		PhaseMeasurements:          measurementsFromResults(result.PhaseMeasurements),
		ResolverMeasurements:       measurementsFromResults(result.ResolverMeasurements),
		SQLiteWriteMeasurements:    measurementsFromResults(result.SQLiteWriteMeasurements),
		SourceFiles:                result.SourceFiles,
		NodeCount:                  result.NodeCount,
		EdgeCount:                  result.EdgeCount,
		ExtractionWorkers:          configuration.ExtractionWorkers,
		SourceQueueCapacity:        configuration.SourceQueueCapacity,
		ContributionQueueHighWater: result.ContributionQueueHighWater,
		ContributionQueueCapacity:  result.ContributionQueueCapacity,
		ContributionBatchRows:      configuration.ContributionBatchRows,
		ContributionBatchBytes:     configuration.ContributionBatchBytes,
		ContributionBatchSources:   configuration.ContributionBatchSources,
		ResolverPageSize:           configuration.ResolverPageSize,
		WorkspaceFactBatchRows:     configuration.WorkspaceFactBatchRows,
		WorkspaceFactBatchBytes:    configuration.WorkspaceFactBatchBytes,
		PeakRSSBytes:               result.PeakRSSBytes,
		RetainedHeapBytes:          result.RetainedHeapBytes,
		DatabaseBytes:              result.DatabaseBytes,
		OutputChecksum:             result.OutputChecksum,
	}
}

func measurementsFromResults(results []benchmarkMeasurement) []benchmark.Measurement {
	measurements := make([]benchmark.Measurement, 0, len(results))
	for _, result := range results {
		measurements = append(measurements, benchmark.Measurement{Name: result.Name, Duration: time.Duration(result.DurationNS), NotApplicable: result.NotApplicable})
	}
	return measurements
}

func benchmarkConfigurationFromRuns(runs []benchmark.Run) benchmarkConfiguration {
	if len(runs) == 0 {
		return benchmarkConfiguration{}
	}
	run := runs[0]
	return benchmarkConfiguration{
		ExtractionWorkers:         run.ExtractionWorkers,
		SourceQueueCapacity:       run.SourceQueueCapacity,
		ContributionQueueCapacity: run.ContributionQueueCapacity,
		ContributionBatchRows:     run.ContributionBatchRows,
		ContributionBatchBytes:    run.ContributionBatchBytes,
		ContributionBatchSources:  run.ContributionBatchSources,
		ResolverPageSize:          run.ResolverPageSize,
		WorkspaceFactBatchRows:    run.WorkspaceFactBatchRows,
		WorkspaceFactBatchBytes:   run.WorkspaceFactBatchBytes,
	}
}

func prepareWorkspaceBenchmark(root string) (string, benchmark.Corpus, error) {
	workspaceRoot, err := filepath.Abs(root)
	if err != nil {
		return "", benchmark.Corpus{}, fmt.Errorf("resolve benchmark workspace: %w", err)
	}
	discovery, err := workspace.Discover(workspaceRoot, workspace.DiscoverOptions{})
	if err != nil {
		return "", benchmark.Corpus{}, fmt.Errorf("discover benchmark workspace: %w", err)
	}
	if len(discovery.Sources) == 0 {
		return "", benchmark.Corpus{}, cli.NewInvalidArgumentError("benchmark workspace has no supported source files")
	}
	source := discovery.Sources[0]
	return workspaceRoot, benchmark.Corpus{
		SourceFiles: len(discovery.Sources),
		UpdatePath:  source.Path,
		QueryTerm:   source.Path,
		PathSource:  source.ProjectID,
		PathTarget:  source.Path,
		ExplainTerm: source.Path,
	}, nil
}

func prepareBenchmarkCorpus(specification benchmark.CorpusSpec, standardError io.Writer) (string, benchmark.Corpus, error) {
	workspace, err := os.MkdirTemp("", "agent-wayfinder-benchmark-")
	if err != nil {
		return "", benchmark.Corpus{}, fmt.Errorf("create benchmark workspace: %w", err)
	}
	lastReported := 0
	fmt.Fprintf(standardError, "Benchmark setup: generate %d source files\n", specification.SourceFiles)
	corpus, err := benchmark.GenerateCorpusWithProgress(workspace, specification, func(created, total int) {
		if created == total || created-lastReported >= 1000 {
			fmt.Fprintf(standardError, "Benchmark setup: generated %d/%d source files\n", created, total)
			lastReported = created
		}
	})
	if err != nil {
		os.RemoveAll(workspace)
		return "", benchmark.Corpus{}, err
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".agent-wayfinder"), 0o755); err != nil {
		os.RemoveAll(workspace)
		return "", benchmark.Corpus{}, fmt.Errorf("create benchmark database directory: %w", err)
	}

	fmt.Fprintf(standardError, "Benchmark setup: corpus expects %d nodes and %d edges; run 1 will validate the indexed result\n", corpus.ExpectedNodes, corpus.ExpectedEdges)
	return workspace, corpus, nil
}

func prepareRealisticBenchmarkCorpus(sourceFiles int, standardError io.Writer) (string, benchmark.Corpus, error) {
	specification, err := benchmark.NewRealisticCorpusSpec(sourceFiles)
	if err != nil {
		return "", benchmark.Corpus{}, cli.NewInvalidArgumentError(err.Error())
	}
	workspace, err := os.MkdirTemp("", "agent-wayfinder-benchmark-")
	if err != nil {
		return "", benchmark.Corpus{}, fmt.Errorf("create benchmark workspace: %w", err)
	}
	lastReported := 0
	fmt.Fprintf(standardError, "Benchmark setup: generate %d realistic source files\n", specification.SourceFiles)
	corpus, err := benchmark.GenerateRealisticCorpusWithProgress(workspace, specification, func(created, total int) {
		if created == total || created-lastReported >= 1000 {
			fmt.Fprintf(standardError, "Benchmark setup: generated %d/%d source files\n", created, total)
			lastReported = created
		}
	})
	if err != nil {
		os.RemoveAll(workspace)
		return "", benchmark.Corpus{}, err
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".agent-wayfinder"), 0o755); err != nil {
		os.RemoveAll(workspace)
		return "", benchmark.Corpus{}, fmt.Errorf("create benchmark database directory: %w", err)
	}

	fmt.Fprintln(standardError, "Benchmark setup: realistic corpus node and edge counts are data-derived; skipping exact-count validation")
	return workspace, corpus, nil
}

func benchmarkRunResults(runs []benchmark.Run) []benchmarkRunResult {
	results := make([]benchmarkRunResult, 0, len(runs))
	for _, run := range runs {
		measurements := make([]benchmarkMeasurement, 0, len(run.Measurements))
		for _, measurement := range run.Measurements {
			measurements = append(measurements, benchmarkMeasurement{Name: measurement.Name, DurationNS: measurement.Duration.Nanoseconds()})
		}
		results = append(results, benchmarkRunResult{
			Measurements:               measurements,
			PhaseMeasurements:          benchmarkMeasurements(run.PhaseMeasurements),
			ResolverMeasurements:       benchmarkMeasurements(run.ResolverMeasurements),
			SQLiteWriteMeasurements:    benchmarkMeasurements(run.SQLiteWriteMeasurements),
			SourceFiles:                run.SourceFiles,
			NodeCount:                  run.NodeCount,
			EdgeCount:                  run.EdgeCount,
			ContributionQueueHighWater: run.ContributionQueueHighWater,
			ContributionQueueCapacity:  run.ContributionQueueCapacity,
			PeakRSSBytes:               run.PeakRSSBytes,
			RetainedHeapBytes:          run.RetainedHeapBytes,
			DatabaseBytes:              run.DatabaseBytes,
			OutputChecksum:             run.OutputChecksum,
		})
	}
	return results
}

func benchmarkMeasurements(measurements []benchmark.Measurement) []benchmarkMeasurement {
	result := make([]benchmarkMeasurement, 0, len(measurements))
	for _, measurement := range measurements {
		result = append(result, benchmarkMeasurement{Name: measurement.Name, DurationNS: measurement.Duration.Nanoseconds(), NotApplicable: measurement.NotApplicable})
	}
	return result
}

func benchmarkCorpusSpec(command *cobra.Command) (benchmark.CorpusSpec, error) {
	sourceFiles, err := command.Flags().GetInt("source-files")
	if err != nil {
		return benchmark.CorpusSpec{}, err
	}
	functionsPerFile, err := command.Flags().GetInt("functions-per-file")
	if err != nil {
		return benchmark.CorpusSpec{}, err
	}
	if sourceFiles <= 0 || functionsPerFile <= 0 {
		return benchmark.CorpusSpec{}, cli.NewInvalidArgumentError("benchmark source file count and function count must be positive")
	}
	if !command.Flags().Changed("functions-per-file") {
		specification, err := benchmark.ExactScaleCorpusSpec(sourceFiles)
		if err != nil {
			return benchmark.CorpusSpec{}, cli.NewInvalidArgumentError(err.Error())
		}
		return specification, nil
	}
	additionalFunctions := 0
	uncalledFunctions := 0
	extraSideEffectImport := false
	if sourceFiles == benchmark.DefaultCorpusSpec.SourceFiles && functionsPerFile == benchmark.DefaultCorpusSpec.FunctionsPerFile {
		additionalFunctions = benchmark.DefaultCorpusSpec.AdditionalFunctions
		uncalledFunctions = benchmark.DefaultCorpusSpec.UncalledFunctions
		extraSideEffectImport = benchmark.DefaultCorpusSpec.ExtraSideEffectImport
	}
	return benchmark.CorpusSpec{
		SourceFiles:           sourceFiles,
		FunctionsPerFile:      functionsPerFile,
		AdditionalFunctions:   additionalFunctions,
		UncalledFunctions:     uncalledFunctions,
		ExtraSideEffectImport: extraSideEffectImport,
	}, nil
}

func measureBenchmarkRun(workspace, stateDirectory string, corpus benchmark.Corpus, reporter *benchmarkProgressReporter, cpuProfilePath, incrementalCPUProfilePath string) (benchmark.Run, storage.Snapshot, error) {
	database := filepath.Join(stateDirectory, fmt.Sprintf("benchmark-%d.db", reporter.runNumber))
	if err := os.Remove(database); err != nil && !os.IsNotExist(err) {
		return benchmark.Run{}, storage.Snapshot{}, fmt.Errorf("reset benchmark database: %w", err)
	}
	store, err := sqlite.Open(context.Background(), database)
	if err != nil {
		return benchmark.Run{}, storage.Snapshot{}, fmt.Errorf("open benchmark database: %w", err)
	}
	defer store.Close()

	measurements := make([]benchmark.Measurement, 0, 5)
	phaseMeasurements := make([]benchmark.Measurement, 0, 5)
	resolverMeasurements := make([]benchmark.Measurement, 0, 3)
	sqliteWriteMeasurements := make([]benchmark.Measurement, 0, 12)
	measure := func(name string, operation func() error) error {
		reporter.start(name)
		started := time.Now()
		err := operation()
		reporter.finishPhase()
		if err != nil {
			return fmt.Errorf("benchmark %s: %w", name, err)
		}
		measurements = append(measurements, benchmark.Measurement{Name: name, Duration: time.Since(started)})
		return nil
	}

	var snapshot storage.Snapshot
	var pipelineStatistics index.PipelineStatistics
	var initialIndexPeakRSS uint64
	var initialIndexRetainedHeap uint64
	measureInitialIndex := func() error {
		request := index.Request{
			Root: workspace,
			Measurement: func(measurement index.Measurement) {
				phaseMeasurements = append(phaseMeasurements, benchmark.Measurement{Name: measurement.Name, Duration: measurement.Duration})
			},
			PipelineStatistics: func(statistics index.PipelineStatistics) {
				pipelineStatistics = statistics
			},
			SQLiteWriteMeasurement: func(measurement storage.PublishMeasurement) {
				sqliteWriteMeasurements = append(sqliteWriteMeasurements, benchmark.Measurement{Name: measurement.Name, Duration: measurement.Duration, NotApplicable: measurement.NotApplicable})
			},
		}
		var setupReporter *benchmarkSetupReporter
		if reporter.runNumber == 1 {
			setupReporter = newBenchmarkSetupReporter(reporter.writer)
			setupReporter.start("validation extract", corpus.SourceFiles)
			request.Progress = setupReporter.update
		}
		result, err := index.Index(context.Background(), store, request)
		if setupReporter != nil {
			setupReporter.finish()
		}
		if err == nil {
			snapshot = result.Snapshot
			initialIndexPeakRSS = benchmark.PeakRSSBytes()
		}
		return err
	}
	var initialIndexError error
	if reporter.runNumber == 1 {
		profile, err := startCPUProfile(cpuProfilePath)
		if err != nil {
			return benchmark.Run{}, storage.Snapshot{}, err
		}
		started := time.Now()
		initialIndexError = measureInitialIndex()
		if profile != nil {
			pprof.StopCPUProfile()
			if err := profile.Close(); err != nil {
				initialIndexError = errors.Join(initialIndexError, fmt.Errorf("close initial-index CPU profile: %w", err))
			}
		}
		if initialIndexError == nil {
			measurements = append(measurements, benchmark.Measurement{Name: "initial_index", Duration: time.Since(started)})
		}
	} else {
		initialIndexError = measure("initial_index", measureInitialIndex)
	}
	if initialIndexError != nil {
		return benchmark.Run{}, storage.Snapshot{}, fmt.Errorf("benchmark initial_index: %w", initialIndexError)
	}
	initialIndexRetainedHeap = retainedHeapBytes()
	if reporter.runNumber == 1 {
		counts, err := validateBenchmarkSnapshot(store, snapshot, corpus)
		if err != nil {
			return benchmark.Run{}, storage.Snapshot{}, err
		}
		if corpus.ExpectedNodes > 0 || corpus.ExpectedEdges > 0 {
			fmt.Fprintf(reporter.writer, "Benchmark setup: validated %d/%d nodes and %d/%d edges\n", counts.Nodes, corpus.ExpectedNodes, counts.Edges, corpus.ExpectedEdges)
		} else {
			fmt.Fprintln(reporter.writer, "Benchmark setup: validated published workspace graph")
		}
	}
	updatedSource := filepath.Join(workspace, corpus.UpdatePath)
	contents, err := os.ReadFile(updatedSource)
	if err != nil {
		return benchmark.Run{}, storage.Snapshot{}, fmt.Errorf("read benchmark source update: %w", err)
	}
	if err := os.WriteFile(updatedSource, append(contents, '\n'), 0o644); err != nil {
		return benchmark.Run{}, storage.Snapshot{}, fmt.Errorf("update benchmark source: %w", err)
	}
	if err := measure("incremental_update", func() error {
		profile, err := startCPUProfile(incrementalCPUProfilePath)
		if err != nil {
			return err
		}
		if profile != nil {
			defer func() {
				pprof.StopCPUProfile()
				_ = profile.Close()
			}()
		}
		snapshot, err = index.PublishBatch(context.Background(), store, index.BatchRequest{
			Root:         workspace,
			ChangedPaths: []string{corpus.UpdatePath},
			Measurement: func(measurement index.Measurement) {
				resolverMeasurements = append(resolverMeasurements, benchmark.Measurement{Name: measurement.Name, Duration: measurement.Duration})
			},
		})
		return err
	}); err != nil {
		return benchmark.Run{}, storage.Snapshot{}, err
	}
	if err := measure("query", func() error {
		_, err := query.QuerySnapshot(context.Background(), store, store, snapshot, query.Request{Terms: []string{corpus.QueryTerm}, MaxDepth: 1, MaxNodes: 100})
		return err
	}); err != nil {
		return benchmark.Run{}, storage.Snapshot{}, err
	}
	if err := measure("path", func() error {
		_, err := query.FindPathSnapshot(context.Background(), store, store, snapshot, query.PathRequest{Source: corpus.PathSource, Target: corpus.PathTarget, MaxDepth: 1, MaxNodes: 10})
		return err
	}); err != nil {
		return benchmark.Run{}, storage.Snapshot{}, err
	}
	if err := measure("explain", func() error {
		_, err := query.ExplainSnapshot(context.Background(), store, store, snapshot, corpus.ExplainTerm)
		return err
	}); err != nil {
		return benchmark.Run{}, storage.Snapshot{}, err
	}
	reporter.start("checksum and resource summary")
	checksum, err := graphChecksum(context.Background(), store, snapshot)
	if err != nil {
		return benchmark.Run{}, storage.Snapshot{}, err
	}
	databaseInfo, err := os.Stat(database)
	if err != nil {
		return benchmark.Run{}, storage.Snapshot{}, fmt.Errorf("read benchmark database size: %w", err)
	}
	reporter.finishPhase()
	orderedSQLiteWriteMeasurements, err := benchmark.OrderSQLiteWriteMeasurements(sqliteWriteMeasurements)
	if err != nil {
		return benchmark.Run{}, storage.Snapshot{}, fmt.Errorf("measure benchmark run: %w", err)
	}
	counts, err := store.FactCounts(context.Background(), snapshot)
	if err != nil {
		return benchmark.Run{}, storage.Snapshot{}, fmt.Errorf("measure benchmark run: count graph facts: %w", err)
	}
	memoryLimits := store.MemoryLimits()
	return benchmark.Run{Measurements: measurements, PhaseMeasurements: phaseMeasurements, ResolverMeasurements: resolverMeasurements, SQLiteWriteMeasurements: orderedSQLiteWriteMeasurements, SourceFiles: corpus.SourceFiles, NodeCount: counts.Nodes, EdgeCount: counts.Edges, ExtractionWorkers: pipelineStatistics.ExtractionWorkers, SourceQueueCapacity: pipelineStatistics.SourceQueueCapacity, ContributionQueueHighWater: pipelineStatistics.ContributionQueueHighWater, ContributionQueueCapacity: pipelineStatistics.ContributionQueueCapacity, ContributionBatchRows: memoryLimits.ContributionBatchRows, ContributionBatchBytes: memoryLimits.ContributionBatchBytes, ContributionBatchSources: memoryLimits.ContributionBatchSources, ResolverPageSize: pipelineStatistics.ResolverPageSize, WorkspaceFactBatchRows: memoryLimits.WorkspaceFactBatchRows, WorkspaceFactBatchBytes: memoryLimits.WorkspaceFactBatchBytes, PeakRSSBytes: initialIndexPeakRSS, RetainedHeapBytes: initialIndexRetainedHeap, DatabaseBytes: databaseInfo.Size(), OutputChecksum: checksum}, snapshot, nil
}

func startCPUProfile(path string) (*os.File, error) {
	if path == "" {
		return nil, nil
	}
	profile, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create initial-index CPU profile: %w", err)
	}
	if err := pprof.StartCPUProfile(profile); err != nil {
		_ = profile.Close()
		return nil, fmt.Errorf("start initial-index CPU profile: %w", err)
	}
	return profile, nil
}

func validateBenchmarkSnapshot(store *sqlite.Store, snapshot storage.Snapshot, corpus benchmark.Corpus) (storage.FactCounts, error) {
	counts, err := store.FactCounts(context.Background(), snapshot)
	if err != nil {
		return storage.FactCounts{}, fmt.Errorf("validate benchmark corpus: count graph facts: %w", err)
	}
	if (corpus.ExpectedNodes > 0 && counts.Nodes != corpus.ExpectedNodes) || (corpus.ExpectedEdges > 0 && counts.Edges != corpus.ExpectedEdges) {
		return storage.FactCounts{}, fmt.Errorf("validate benchmark corpus: graph contains %d nodes and %d edges, want %d nodes and %d edges", counts.Nodes, counts.Edges, corpus.ExpectedNodes, corpus.ExpectedEdges)
	}
	for _, term := range []string{corpus.QueryTerm, corpus.PathSource, corpus.PathTarget, corpus.ExplainTerm} {
		matches, err := store.LookupNodes(context.Background(), snapshot, storage.NodeLookupRequest{Text: term, Limit: 1})
		if err != nil {
			return storage.FactCounts{}, fmt.Errorf("validate benchmark corpus: look up %q: %w", term, err)
		}
		if len(matches) == 0 {
			return storage.FactCounts{}, fmt.Errorf("validate benchmark corpus: expected graph node %q is unavailable", term)
		}
	}
	return counts, nil
}

type benchmarkChecksumSink struct {
	hash hash.Hash
}

func (sink benchmarkChecksumSink) WriteNode(node graph.Node) error {
	_, _ = fmt.Fprintf(sink.hash, "node\x00%s\x00%s\x00%s\n", node.ID, node.Kind, node.QualifiedName)
	return nil
}

func (sink benchmarkChecksumSink) WriteEdge(edge graph.Edge) error {
	_, _ = fmt.Fprintf(sink.hash, "edge\x00%s\x00%s\x00%s\n", edge.SourceID, edge.TargetID, edge.Relation)
	return nil
}

func graphChecksum(ctx context.Context, store storage.Exporter, snapshot storage.Snapshot) (string, error) {
	sum := sha256.New()
	if err := store.Export(ctx, snapshot, storage.ExportRequest{}, benchmarkChecksumSink{hash: sum}); err != nil {
		return "", fmt.Errorf("checksum benchmark graph: %w", err)
	}
	return fmt.Sprintf("sha256:%x", sum.Sum(nil)), nil
}

func retainedHeapBytes() uint64 {
	runtime.GC()
	var statistics runtime.MemStats
	runtime.ReadMemStats(&statistics)
	return statistics.HeapAlloc
}

func maxPeakRSS(runs []benchmark.Run) uint64 {
	var maximum uint64
	for _, run := range runs {
		maximum = max(maximum, run.PeakRSSBytes)
	}
	return maximum
}

func maxRetainedHeap(runs []benchmark.Run) uint64 {
	var maximum uint64
	for _, run := range runs {
		maximum = max(maximum, run.RetainedHeapBytes)
	}
	return maximum
}

func maxDatabaseBytes(runs []benchmark.Run) int64 {
	var maximum int64
	for _, run := range runs {
		maximum = max(maximum, run.DatabaseBytes)
	}
	return maximum
}

func maxContributionQueueHighWater(runs []benchmark.Run) int {
	maximum := 0
	for _, run := range runs {
		maximum = max(maximum, run.ContributionQueueHighWater)
	}
	return maximum
}

func maxContributionQueueCapacity(runs []benchmark.Run) int {
	maximum := 0
	for _, run := range runs {
		maximum = max(maximum, run.ContributionQueueCapacity)
	}
	return maximum
}

func renderBenchmarkText(result benchmarkResult) string {
	lines := make([]string, 0, len(result.Measurements)+len(result.PhaseMeasurements)+len(result.ResolverMeasurements))
	for _, measurement := range result.Measurements {
		lines = append(lines, fmt.Sprintf("%s: %d ns", measurement.Name, measurement.DurationNS))
	}
	for _, measurement := range result.PhaseMeasurements {
		lines = append(lines, fmt.Sprintf("%s: %d ns", measurement.Name, measurement.DurationNS))
	}
	for _, measurement := range result.ResolverMeasurements {
		lines = append(lines, fmt.Sprintf("%s: %d ns", measurement.Name, measurement.DurationNS))
	}
	for _, measurement := range result.SQLiteWriteMeasurements {
		if measurement.NotApplicable {
			lines = append(lines, measurement.Name+": not applicable")
		} else {
			lines = append(lines, fmt.Sprintf("%s: %d ns", measurement.Name, measurement.DurationNS))
		}
	}
	lines = append(lines, fmt.Sprintf("contribution queue high-water: %d/%d", result.ContributionQueueHighWater, result.ContributionQueueCapacity))
	lines = append(lines, fmt.Sprintf("peak RSS: %d bytes", result.PeakRSSBytes))
	lines = append(lines, fmt.Sprintf("retained heap: %d bytes", result.RetainedHeapBytes))
	lines = append(lines, fmt.Sprintf("database: %d bytes", result.DatabaseBytes))
	lines = append(lines, "output checksum: "+result.OutputChecksum)
	return strings.Join(lines, "\n")
}

func runIndexerServer(workspace string, standardError io.Writer) int {
	if err := indexer.NewServer(indexer.NewManager()).Serve(workspace); err != nil {
		return writeCommandError(standardError, err)
	}
	return 0
}

func runIndexerStart(workspace string, format cli.Format, standardOutput, standardError io.Writer) int {
	workspaceRoot, err := filepath.Abs(workspace)
	if err != nil {
		return writeCommandError(standardError, fmt.Errorf("resolve indexer workspace path: %w", err))
	}
	status, err := indexer.Request(context.Background(), workspaceRoot, indexer.StatusCommand)
	if err == nil {
		return renderIndexerStatus(standardOutput, format, status)
	}

	executable, err := os.Executable()
	if err != nil {
		return writeCommandError(standardError, fmt.Errorf("start workspace indexer: find executable: %w", err))
	}
	command := exec.Command(executable, "indexer", "serve", workspaceRoot)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return writeCommandError(standardError, fmt.Errorf("start workspace indexer: launch background process: %w", err))
	}

	deadline := time.Now().Add(time.Second)
	for {
		status, err = indexer.Request(context.Background(), workspaceRoot, indexer.StatusCommand)
		if err == nil {
			return renderIndexerStatus(standardOutput, format, status)
		}
		if time.Now().After(deadline) {
			return writeCommandError(standardError, fmt.Errorf("start workspace indexer: wait for control endpoint: %w", err))
		}
		time.Sleep(time.Millisecond)
	}
}

func runIndexerRequest(workspace string, command indexer.Command, format cli.Format, standardOutput, standardError io.Writer) int {
	status, err := indexer.Request(context.Background(), workspace, command)
	if err != nil {
		return writeCommandError(standardError, err)
	}
	return renderIndexerStatus(standardOutput, format, status)
}

func renderIndexerStatus(writer io.Writer, format cli.Format, status indexer.Status) int {
	switch format {
	case cli.FormatText:
		_, err := fmt.Fprintf(
			writer,
			"Workspace: %s\nRunning: %t\nActivity: %s\nProgress: %d/%d\nQueued paths: %d\nVersion: %d\nError: %s\nIdle deadline: %s\n",
			status.Workspace,
			status.Running,
			status.Activity.UTC().Format(time.RFC3339),
			status.Progress.Completed,
			status.Progress.Total,
			len(status.QueuedPaths),
			status.Version,
			status.Error,
			status.IdleDeadline.UTC().Format(time.RFC3339),
		)
		if err != nil {
			return writeCommandError(writer, fmt.Errorf("render indexer status: %w", err))
		}
		return 0
	case cli.FormatJSON:
		if err := json.NewEncoder(writer).Encode(status); err != nil {
			return writeCommandError(writer, fmt.Errorf("render indexer status: %w", err))
		}
		return 0
	default:
		return writeCommandError(writer, fmt.Errorf("render indexer status: unsupported format %q", format))
	}
}

func runIndex(command *cobra.Command, arguments []string, standardOutput, standardError io.Writer) int {
	if len(arguments) != 1 {
		return writeCommandError(standardError, cli.NewInvalidArgumentError("index requires one workspace path"))
	}
	format, err := commandFormat(command)
	if err != nil {
		return writeCommandError(standardError, err)
	}

	workspace := arguments[0]
	workspaceRoot, err := filepath.Abs(workspace)
	if err != nil {
		return writeCommandError(standardError, fmt.Errorf("resolve index workspace path: %w", err))
	}
	database, err := commandDatabasePath(command, workspaceRoot)
	if err != nil {
		return writeCommandError(standardError, fmt.Errorf("resolve index database path: %w", err))
	}
	if err := os.MkdirAll(filepath.Dir(database), 0o755); err != nil {
		return writeCommandError(standardError, fmt.Errorf("create index database directory: %w", err))
	}
	store, err := sqlite.Open(context.Background(), database)
	if err != nil {
		return writeCommandError(standardError, fmt.Errorf("open index database: %w", err))
	}
	defer store.Close()

	result, err := index.Index(context.Background(), store, index.Request{Root: workspace})
	if err != nil {
		return writeCommandError(standardError, err)
	}
	data := struct {
		Workspace   string                 `json:"workspace"`
		Diagnostics []extractor.Diagnostic `json:"diagnostics"`
	}{
		Workspace:   result.Snapshot.Workspace,
		Diagnostics: result.Diagnostics,
	}
	if err := cli.Render(standardOutput, cli.Result{
		Snapshot: result.Snapshot,
		Text:     fmt.Sprintf("Workspace: %s", result.Snapshot.Workspace),
		Data:     data,
	}, format); err != nil {
		return writeCommandError(standardError, err)
	}
	return 0
}

func runExport(command *cobra.Command, arguments []string, standardOutput, standardError io.Writer) int {
	if len(arguments) != 1 {
		return writeCommandError(standardError, cli.NewInvalidArgumentError("export requires one workspace path"))
	}
	format, err := commandFormat(command)
	if err != nil {
		return writeCommandError(standardError, err)
	}

	workspace := arguments[0]
	workspaceRoot, err := filepath.Abs(workspace)
	if err != nil {
		return writeCommandError(standardError, fmt.Errorf("resolve export workspace path: %w", err))
	}
	database, err := commandDatabasePath(command, workspaceRoot)
	if err != nil {
		return writeCommandError(standardError, fmt.Errorf("resolve export database path: %w", err))
	}
	store, err := sqlite.Open(context.Background(), database)
	if err != nil {
		return writeCommandError(standardError, fmt.Errorf("open export database: %w", err))
	}
	defer store.Close()

	snapshot, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: workspaceRoot})
	if errors.Is(err, storage.ErrWorkspaceNotFound) {
		return writeCommandError(standardError, cli.NewIndexUnavailableError(workspaceRoot))
	}
	if err != nil {
		return writeCommandError(standardError, err)
	}
	collector := &factCollector{}
	if err := store.Export(context.Background(), snapshot, storage.ExportRequest{}, collector); err != nil {
		return writeCommandError(standardError, err)
	}
	data := struct {
		Nodes []graph.Node `json:"nodes"`
		Edges []graph.Edge `json:"edges"`
	}{
		Nodes: collector.nodes,
		Edges: collector.edges,
	}
	if err := cli.Render(standardOutput, cli.Result{
		Snapshot: snapshot,
		Text:     fmt.Sprintf("Nodes: %d\nEdges: %d", len(data.Nodes), len(data.Edges)),
		Data:     data,
	}, format); err != nil {
		return writeCommandError(standardError, err)
	}
	return 0
}

func runQuery(command *cobra.Command, arguments []string, standardOutput, standardError io.Writer) int {
	if len(arguments) < 2 {
		return writeCommandError(standardError, cli.NewInvalidArgumentError("query requires one workspace path and at least one term"))
	}
	format, err := commandFormat(command)
	if err != nil {
		return writeCommandError(standardError, err)
	}
	maxDepth, err := command.Flags().GetInt("max-depth")
	if err != nil {
		return writeCommandError(standardError, err)
	}
	maxNodes, err := command.Flags().GetInt("max-nodes")
	if err != nil {
		return writeCommandError(standardError, err)
	}
	projectIDs, err := command.Flags().GetStringArray("project")
	if err != nil {
		return writeCommandError(standardError, err)
	}
	relations, err := command.Flags().GetStringArray("relation")
	if err != nil {
		return writeCommandError(standardError, err)
	}
	forceQuestion, err := command.Flags().GetBool("question")
	if err != nil {
		return writeCommandError(standardError, err)
	}
	forceTerms, err := command.Flags().GetBool("terms")
	if err != nil {
		return writeCommandError(standardError, err)
	}
	showPlan, err := command.Flags().GetBool("show-plan")
	if err != nil {
		return writeCommandError(standardError, err)
	}
	if forceQuestion && forceTerms {
		return writeCommandError(standardError, cli.NewInvalidArgumentError("--question and --terms cannot be used together"))
	}

	workspace, terms := arguments[0], arguments[1:]
	questionMode := forceQuestion || !forceTerms && isQuestionArgument(terms)
	var plan *query.QueryPlan
	if questionMode {
		if len(terms) != 1 {
			return writeCommandError(standardError, cli.NewInvalidArgumentError("question mode requires exactly one question argument"))
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
		return writeCommandError(standardError, fmt.Errorf("resolve query workspace path: %w", err))
	}
	database, err := commandDatabasePath(command, workspaceRoot)
	if err != nil {
		return writeCommandError(standardError, fmt.Errorf("resolve query database path: %w", err))
	}
	store, err := sqlite.Open(context.Background(), database)
	if err != nil {
		return writeCommandError(standardError, fmt.Errorf("open query database: %w", err))
	}
	defer store.Close()

	snapshot, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: workspaceRoot})
	if errors.Is(err, storage.ErrWorkspaceNotFound) {
		return writeCommandError(standardError, cli.NewIndexUnavailableError(workspaceRoot))
	}
	if err != nil {
		return writeCommandError(standardError, err)
	}
	result, err := query.QuerySnapshot(context.Background(), store, store, snapshot, query.Request{
		Plan:       plan,
		Terms:      terms,
		ProjectIDs: append([]string(nil), projectIDs...),
		Relations:  relationKinds(relations),
		MaxDepth:   maxDepth,
		MaxNodes:   maxNodes,
	})
	if err != nil {
		return writeCommandError(standardError, cli.NewInvalidArgumentError(err.Error()))
	}
	data := queryResultData(result, plan, maxDepth, maxNodes)
	if err := cli.Render(standardOutput, cli.Result{
		Snapshot: snapshot,
		Text:     renderQueryText(data, showPlan),
		Data:     data,
	}, format); err != nil {
		return writeCommandError(standardError, err)
	}
	return 0
}
func runExplain(command *cobra.Command, arguments []string, standardOutput, standardError io.Writer) int {
	if len(arguments) != 2 {
		return writeCommandError(standardError, cli.NewInvalidArgumentError("explain requires one workspace path and one node query"))
	}
	format, err := commandFormat(command)
	if err != nil {
		return writeCommandError(standardError, err)
	}

	workspace, term := arguments[0], arguments[1]
	workspaceRoot, err := filepath.Abs(workspace)
	if err != nil {
		return writeCommandError(standardError, fmt.Errorf("resolve explain workspace path: %w", err))
	}
	database, err := commandDatabasePath(command, workspaceRoot)
	if err != nil {
		return writeCommandError(standardError, fmt.Errorf("resolve explain database path: %w", err))
	}
	store, err := sqlite.Open(context.Background(), database)
	if err != nil {
		return writeCommandError(standardError, fmt.Errorf("open explain database: %w", err))
	}
	defer store.Close()

	snapshot, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: workspaceRoot})
	if errors.Is(err, storage.ErrWorkspaceNotFound) {
		return writeCommandError(standardError, cli.NewIndexUnavailableError(workspaceRoot))
	}
	if err != nil {
		return writeCommandError(standardError, err)
	}
	result, err := query.ExplainSnapshot(context.Background(), store, store, snapshot, term)
	if err != nil {
		return writeCommandError(standardError, err)
	}
	data := explainResultData(result)
	if err := cli.Render(standardOutput, cli.Result{
		Snapshot: snapshot,
		Text:     renderExplanationText(data),
		Data:     data,
	}, format); err != nil {
		return writeCommandError(standardError, err)
	}
	return 0
}

func runPath(command *cobra.Command, arguments []string, standardOutput, standardError io.Writer) int {
	if len(arguments) != 3 {
		return writeCommandError(standardError, cli.NewInvalidArgumentError("path requires one workspace path, one source query, and one target query"))
	}
	format, err := commandFormat(command)
	if err != nil {
		return writeCommandError(standardError, err)
	}
	undirected, err := command.Flags().GetBool("undirected")
	if err != nil {
		return writeCommandError(standardError, err)
	}
	maxDepth, err := command.Flags().GetInt("max-depth")
	if err != nil {
		return writeCommandError(standardError, err)
	}
	maxNodes, err := command.Flags().GetInt("max-nodes")
	if err != nil {
		return writeCommandError(standardError, err)
	}
	projectIDs, err := command.Flags().GetStringArray("project")
	if err != nil {
		return writeCommandError(standardError, err)
	}
	relations, err := command.Flags().GetStringArray("relation")
	if err != nil {
		return writeCommandError(standardError, err)
	}

	workspace, source, target := arguments[0], arguments[1], arguments[2]
	workspaceRoot, err := filepath.Abs(workspace)
	if err != nil {
		return writeCommandError(standardError, fmt.Errorf("resolve path workspace path: %w", err))
	}
	database, err := commandDatabasePath(command, workspaceRoot)
	if err != nil {
		return writeCommandError(standardError, fmt.Errorf("resolve path database path: %w", err))
	}
	store, err := sqlite.Open(context.Background(), database)
	if err != nil {
		return writeCommandError(standardError, fmt.Errorf("open path database: %w", err))
	}
	defer store.Close()

	snapshot, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: workspaceRoot})
	if errors.Is(err, storage.ErrWorkspaceNotFound) {
		return writeCommandError(standardError, cli.NewIndexUnavailableError(workspaceRoot))
	}
	if err != nil {
		return writeCommandError(standardError, err)
	}
	result, err := query.FindPathSnapshot(context.Background(), store, store, snapshot, query.PathRequest{
		Source:                  source,
		Target:                  target,
		AllowUndirectedFallback: undirected,
		ProjectIDs:              append([]string(nil), projectIDs...),
		Relations:               relationKinds(relations),
		MaxDepth:                maxDepth,
		MaxNodes:                maxNodes,
	})
	if err != nil {
		return writeCommandError(standardError, err)
	}
	data := pathResultData(result, maxDepth, maxNodes)
	if err := cli.Render(standardOutput, cli.Result{
		Snapshot: snapshot,
		Text:     renderPathText(data),
		Data:     data,
	}, format); err != nil {
		return writeCommandError(standardError, err)
	}
	return 0
}

func relationKinds(relations []string) []graph.RelationKind {
	kinds := make([]graph.RelationKind, len(relations))
	for index, relation := range relations {
		kinds[index] = graph.RelationKind(relation)
	}
	return kinds
}

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
	MaxDepth          int                        `json:"maxDepth"`
	MaxNodes          int                        `json:"maxNodes"`
}

func queryResultData(result query.Result, plan *query.QueryPlan, maxDepth, maxNodes int) queryResult {
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
		MaxDepth:          maxDepth,
		MaxNodes:          maxNodes,
	}
	if plan != nil {
		data.SchemaVersion = 1
		data.Interpretation = plan
		for _, warning := range append(append([]query.PlanWarning(nil), plan.Warnings...), result.Warnings...) {
			data.Suggestions = append(data.Suggestions, warning.Suggestions...)
		}
	}
	return data
}

func renderQueryText(result queryResult, showPlan bool) string {
	lines := make([]string, 0)
	if result.Plan != nil && (showPlan || len(result.Plan.Warnings) > 0) {
		lines = append(lines, fmt.Sprintf("Interpreted as: %s (%s, confidence %.2f)", result.Plan.Intent, result.Plan.Operator, result.Plan.Confidence))
		for _, warning := range result.Plan.Warnings {
			lines = append(lines, "Warning: "+warning.Message)
		}
	}
	for _, warning := range result.Warnings {
		lines = append(lines, "Warning: "+warning.Message)
		for _, suggestion := range warning.Suggestions {
			lines = append(lines, "Next: "+suggestion)
		}
	}
	if result.Plan != nil && len(result.Evidence) == 0 {
		lines = append(lines, "No answer-ready evidence was found.")
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

type pathResult struct {
	SourceCandidates            []graph.Node         `json:"sourceCandidates,omitempty"`
	SourceRemainderCount        int                  `json:"sourceRemainderCount,omitempty"`
	TargetCandidates            []graph.Node         `json:"targetCandidates,omitempty"`
	TargetRemainderCount        int                  `json:"targetRemainderCount,omitempty"`
	Nodes                       []graph.Node         `json:"nodes,omitempty"`
	Edges                       []graph.Edge         `json:"edges,omitempty"`
	ScopeBoundary               *query.ScopeBoundary `json:"scopeBoundary,omitempty"`
	UsedUndirectedFallback      bool                 `json:"usedUndirectedFallback"`
	UndirectedFallbackAttempted bool                 `json:"undirectedFallbackAttempted"`
	MaxDepth                    int                  `json:"maxDepth"`
	MaxNodes                    int                  `json:"maxNodes"`
}

func pathResultData(result query.PathResult, maxDepth, maxNodes int) pathResult {
	return pathResult{
		SourceCandidates:            result.SourceCandidates,
		SourceRemainderCount:        result.SourceRemainderCount,
		TargetCandidates:            result.TargetCandidates,
		TargetRemainderCount:        result.TargetRemainderCount,
		Nodes:                       result.Nodes,
		Edges:                       result.Edges,
		ScopeBoundary:               result.ScopeBoundary,
		UsedUndirectedFallback:      result.UsedUndirectedFallback,
		UndirectedFallbackAttempted: result.UndirectedFallbackAttempted,
		MaxDepth:                    maxDepth,
		MaxNodes:                    maxNodes,
	}
}

func renderPathText(result pathResult) string {
	if len(result.SourceCandidates) > 0 || len(result.TargetCandidates) > 0 {
		return renderPathCandidates(result)
	}
	if len(result.Nodes) == 0 {
		if result.UndirectedFallbackAttempted {
			return "No path found."
		}
		return "No directed path found."
	}
	lines := []string{fmt.Sprintf("Path (%d hops):", len(result.Edges))}
	for index, node := range result.Nodes {
		lines = append(lines, node.QualifiedName)
		if index < len(result.Edges) {
			edge := result.Edges[index]
			if edge.SourceID == node.ID {
				lines = append(lines, fmt.Sprintf("  --%s--> ", edge.Relation))
			} else {
				lines = append(lines, fmt.Sprintf("  <--%s-- ", edge.Relation))
			}
		}
	}
	if result.UsedUndirectedFallback {
		lines = append(lines, "Used undirected fallback.")
	}
	if result.ScopeBoundary != nil {
		lines = append(lines, fmt.Sprintf("Scope boundary: %s", result.ScopeBoundary.Node.QualifiedName))
	}
	return strings.Join(lines, "\n")
}

func renderPathCandidates(result pathResult) string {
	lines := []string{"Ambiguous path endpoint."}
	appendCandidates := func(label string, candidates []graph.Node, remainder int) {
		if len(candidates) == 0 {
			return
		}
		lines = append(lines, label+":")
		for _, candidate := range candidates {
			lines = append(lines, fmt.Sprintf("- %s: %s", candidate.ID, candidate.QualifiedName))
		}
		if remainder > 0 {
			lines = append(lines, fmt.Sprintf("- %d additional candidate(s)", remainder))
		}
	}
	appendCandidates("Source candidates", result.SourceCandidates, result.SourceRemainderCount)
	appendCandidates("Target candidates", result.TargetCandidates, result.TargetRemainderCount)
	return strings.Join(lines, "\n")
}

type explainResult struct {
	Candidates     []graph.Node         `json:"candidates,omitempty"`
	RemainderCount int                  `json:"remainderCount,omitempty"`
	Explanation    *storage.Explanation `json:"explanation,omitempty"`
}

func explainResultData(result query.ExplainResult) explainResult {
	return explainResult{
		Candidates:     result.Candidates,
		RemainderCount: result.RemainderCount,
		Explanation:    result.Explanation,
	}
}

func renderExplanationText(result explainResult) string {
	if result.Explanation == nil {
		if len(result.Candidates) == 0 {
			return "No matching node."
		}
		lines := []string{"Ambiguous node query. Candidates:"}
		for _, candidate := range result.Candidates {
			lines = append(lines, fmt.Sprintf("- %s: %s", candidate.ID, candidate.QualifiedName))
		}
		if result.RemainderCount > 0 {
			lines = append(lines, fmt.Sprintf("- %d additional candidate(s)", result.RemainderCount))
		}
		return strings.Join(lines, "\n")
	}

	node := result.Explanation.Node
	lines := []string{
		fmt.Sprintf("Node: %s", node.ID),
		fmt.Sprintf("Name: %s", node.QualifiedName),
		fmt.Sprintf("Kind: %s", node.Kind),
		fmt.Sprintf("Source: %s:%d:%d-%d:%d", node.Evidence.Span.Path, node.Evidence.Span.StartLine, node.Evidence.Span.StartColumn, node.Evidence.Span.EndLine, node.Evidence.Span.EndColumn),
		fmt.Sprintf("Extractor: %s", node.Evidence.Extractor),
		fmt.Sprintf("Provenance: %s", node.Evidence.Provenance),
		fmt.Sprintf("Confidence: %s", node.Evidence.Confidence),
		"",
		"Direct edges:",
	}
	groups := make(map[graph.RelationKind][]graph.Edge)
	for _, edge := range result.Explanation.SupportingFacts.Edges {
		groups[edge.Relation] = append(groups[edge.Relation], edge)
	}
	relations := make([]string, 0, len(groups))
	for relation := range groups {
		relations = append(relations, string(relation))
	}
	sort.Strings(relations)
	for _, relation := range relations {
		edges := groups[graph.RelationKind(relation)]
		sort.Slice(edges, func(left, right int) bool {
			if edges[left].SourceID != edges[right].SourceID {
				return edges[left].SourceID < edges[right].SourceID
			}
			return edges[left].TargetID < edges[right].TargetID
		})
		lines = append(lines, fmt.Sprintf("%s (%d):", relation, len(edges)))
		for _, edge := range edges {
			lines = append(lines, fmt.Sprintf("- %s -> %s [%s, %s]", edge.SourceID, edge.TargetID, edge.Evidence.Confidence, edge.Evidence.Span.Path))
		}
	}
	if len(relations) == 0 {
		lines = append(lines, "None.")
	}
	return strings.Join(lines, "\n")
}

type factCollector struct {
	nodes []graph.Node
	edges []graph.Edge
}

func (collector *factCollector) WriteNode(node graph.Node) error {
	collector.nodes = append(collector.nodes, node)
	return nil
}

func (collector *factCollector) WriteEdge(edge graph.Edge) error {
	collector.edges = append(collector.edges, edge)
	return nil
}

func databasePath(workspace, database string) (string, error) {
	workspaceRoot, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	if database == "" {
		return filepath.Join(workspaceRoot, ".agent-wayfinder", "graph.db"), nil
	}
	return filepath.Abs(database)
}

func writeCommandError(standardError io.Writer, err error) int {
	if renderErr := cli.RenderError(standardError, err); renderErr != nil {
		return 1
	}
	return cli.ExitCode(err)
}
