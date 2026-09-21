package benchmark

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"agent-wayfinder/benchmark"
	"agent-wayfinder/cli"
	cmd "agent-wayfinder/cmd/agent-wayfinder/internal/command"
	"agent-wayfinder/storage"

	"github.com/spf13/cobra"
)

func New(standardOutput, standardError io.Writer, exitCode *int) *cobra.Command {
	return cmd.NewLeaf("benchmark [WORKSPACE]", "Measure critical graph operations", benchmarkFlags, runBenchmark, standardOutput, standardError, exitCode)
}

func benchmarkFlags(command *cobra.Command) {
	cmd.FormatFlag(command)
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

func runBenchmark(command *cobra.Command, arguments []string, standardOutput, standardError io.Writer) (exitCode int) {
	if len(arguments) > 1 {
		return cmd.WriteError(standardError, cli.NewInvalidArgumentError("benchmark accepts at most one workspace argument"))
	}
	format, err := cmd.Format(command)
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	cpuProfilePath, err := command.Flags().GetString("cpu-profile")
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	incrementalCPUProfilePath, err := command.Flags().GetString("incremental-cpu-profile")
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	realistic, err := command.Flags().GetBool("realistic")
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	exactScale, err := command.Flags().GetBool("exact-scale")
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	runCount, err := command.Flags().GetInt("runs")
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	if runCount <= 0 {
		return cmd.WriteError(standardError, cli.NewInvalidArgumentError("benchmark run count must be positive"))
	}
	internalSample, err := command.Flags().GetBool("internal-sample")
	if err != nil {
		return cmd.WriteError(standardError, err)
	}
	if exactScale && realistic {
		return cmd.WriteError(standardError, cli.NewInvalidArgumentError("benchmark exact-scale and realistic modes cannot be combined"))
	}
	if exactScale && len(arguments) != 0 {
		return cmd.WriteError(standardError, cli.NewInvalidArgumentError("benchmark exact-scale mode does not accept a workspace"))
	}
	var benchmarkWorkspace string
	var corpus benchmark.Corpus
	if len(arguments) == 0 && realistic {
		sourceFiles, err := command.Flags().GetInt("source-files")
		if err != nil {
			return cmd.WriteError(standardError, err)
		}
		benchmarkWorkspace, corpus, err = prepareRealisticBenchmarkCorpus(sourceFiles, standardError)
		if err != nil {
			return cmd.WriteError(standardError, err)
		}
		defer os.RemoveAll(benchmarkWorkspace)
	} else if len(arguments) == 0 {
		specification, err := benchmarkCorpusSpec(command)
		if err != nil {
			return cmd.WriteError(standardError, err)
		}
		benchmarkWorkspace, corpus, err = prepareBenchmarkCorpus(specification, standardError)
		if err != nil {
			return cmd.WriteError(standardError, err)
		}
		defer os.RemoveAll(benchmarkWorkspace)
	} else {
		benchmarkWorkspace, corpus, err = prepareWorkspaceBenchmark(arguments[0])
		if err != nil {
			return cmd.WriteError(standardError, err)
		}
		fmt.Fprintf(standardError, "Benchmark setup: measure %d workspace source files\n", corpus.SourceFiles)
	}
	stateDirectory, err := os.MkdirTemp("", "agent-wayfinder-benchmark-state-")
	if err != nil {
		return cmd.WriteError(standardError, fmt.Errorf("create benchmark state directory: %w", err))
	}
	defer os.RemoveAll(stateDirectory)
	updateSource := filepath.Join(benchmarkWorkspace, corpus.UpdatePath)
	baselineContents, err := os.ReadFile(updateSource)
	if err != nil {
		return cmd.WriteError(standardError, fmt.Errorf("read benchmark source baseline: %w", err))
	}
	defer func() {
		if err := os.WriteFile(updateSource, baselineContents, 0o644); err != nil {
			exitCode = cmd.WriteError(standardError, fmt.Errorf("restore benchmark source baseline: %w", err))
		}
	}()

	runs := make([]benchmark.Run, 0, runCount)
	var snapshot storage.Snapshot
	for runNumber := 0; runNumber < runCount; runNumber++ {
		if exactScale {
			run, runSnapshot, err := measureIsolatedBenchmarkRun(corpus.SourceFiles, standardError)
			if err != nil {
				return cmd.WriteError(standardError, err)
			}
			runs = append(runs, run)
			snapshot = runSnapshot
			continue
		}
		if err := os.WriteFile(updateSource, baselineContents, 0o644); err != nil {
			return cmd.WriteError(standardError, fmt.Errorf("restore benchmark source baseline: %w", err))
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
			return cmd.WriteError(standardError, err)
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
			return cmd.WriteError(standardError, err)
		}
		smallerWorkspace, smallerCorpus, err := prepareBenchmarkCorpus(smallerSpecification, standardError)
		if err != nil {
			return cmd.WriteError(standardError, err)
		}
		defer os.RemoveAll(smallerWorkspace)
		smallerBaseline, err := os.ReadFile(filepath.Join(smallerWorkspace, smallerCorpus.UpdatePath))
		if err != nil {
			return cmd.WriteError(standardError, fmt.Errorf("read smaller benchmark source baseline: %w", err))
		}
		smallerRuns := make([]benchmark.Run, 0, runCount)
		for runNumber := 0; runNumber < runCount; runNumber++ {
			if err := os.WriteFile(filepath.Join(smallerWorkspace, smallerCorpus.UpdatePath), smallerBaseline, 0o644); err != nil {
				return cmd.WriteError(standardError, fmt.Errorf("restore smaller benchmark source baseline: %w", err))
			}
			reporter := newBenchmarkProgressReporter(standardError, runNumber+1, runCount, len(smallerRuns))
			run, _, err := measureBenchmarkRun(smallerWorkspace, stateDirectory, smallerCorpus, reporter, "", "")
			if err != nil {
				reporter.finishPhase()
				return cmd.WriteError(standardError, fmt.Errorf("measure smaller exact-scale run: %w", err))
			}
			reporter.complete()
			smallerRuns = append(smallerRuns, run)
		}
		if err := benchmark.ValidateReport(smallerRuns, smallerCorpus.SourceFiles); err != nil {
			return cmd.WriteError(standardError, fmt.Errorf("validate smaller exact-scale report: %w", err))
		}
		shape, err := benchmark.MeasureScaleShape(smallerRuns, runs)
		if err != nil {
			return cmd.WriteError(standardError, err)
		}
		data.ScaleShape = &shape
	}
	if err := cli.Render(standardOutput, cli.Result{Snapshot: snapshot, Text: renderBenchmarkText(data), Data: data}, format); err != nil {
		return cmd.WriteError(standardError, err)
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
		return cmd.WriteError(standardError, err)
	}
	return 0
}
