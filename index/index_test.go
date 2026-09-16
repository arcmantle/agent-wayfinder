package index_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-wayfinder/extractor"
	goextractor "agent-wayfinder/extractors/go"
	"agent-wayfinder/extractors/typescript"
	"agent-wayfinder/graph"
	"agent-wayfinder/index"
	"agent-wayfinder/indexer"
	"agent-wayfinder/storage"
	"agent-wayfinder/storage/sqlite"
	"agent-wayfinder/testkit"
)

type contributionSessionStore struct {
	*sqlite.Store
	observationMu           sync.Mutex
	sessions                int
	contributions           int
	stagedSources           int
	stagedAtFirstWrite      int
	resolverProjectionPages int
	resolverProjectionError error
	resolverTargetError     error
	resolverPackageError    error
	dependencyWriteError    error
	workspaceFactPages      int
	workspaceFactNodes      int
	workspaceFactEdges      int
	workspaceFactFailAfter  int
	workspaceFactWriteError error
	writeStarted            chan struct{}
	releaseWrites           chan struct{}
	writeError              error
	writeOnce               sync.Once
	sealed                  bool
}

func (store *contributionSessionStore) Publish(context.Context, storage.PublishRequest) (storage.Snapshot, error) {
	return storage.Snapshot{}, errors.New("initial index used legacy publisher")
}

func (store *contributionSessionStore) BeginContributionSession(ctx context.Context, workspace string) (storage.ContributionSession, error) {
	session, err := store.Store.BeginContributionSession(ctx, workspace)
	if err != nil {
		return nil, err
	}
	store.sessions++
	return contributionSessionObserver{ContributionSession: session, store: store}, nil
}

type contributionSessionObserver struct {
	storage.ContributionSession
	store *contributionSessionStore
}

func (observer contributionSessionObserver) StageSource(ctx context.Context, sourcePath string) error {
	if err := observer.ContributionSession.StageSource(ctx, sourcePath); err != nil {
		return err
	}
	observer.store.observationMu.Lock()
	observer.store.stagedSources++
	observer.store.observationMu.Unlock()
	return nil
}

func (observer contributionSessionObserver) WriteContribution(ctx context.Context, contribution extractor.Contribution) error {
	observer.store.observationMu.Lock()
	if observer.store.contributions == 0 {
		observer.store.stagedAtFirstWrite = observer.store.stagedSources
	}
	observer.store.observationMu.Unlock()
	if observer.store.writeError != nil {
		return observer.store.writeError
	}
	if observer.store.writeStarted != nil {
		observer.store.writeOnce.Do(func() {
			close(observer.store.writeStarted)
			<-observer.store.releaseWrites
		})
	}
	if err := observer.ContributionSession.WriteContribution(ctx, contribution); err != nil {
		return err
	}
	observer.store.contributions++
	return nil
}

func (observer contributionSessionObserver) SealContributions(ctx context.Context) error {
	if err := observer.ContributionSession.SealContributions(ctx); err != nil {
		return err
	}
	observer.store.sealed = true
	return nil
}

func (observer contributionSessionObserver) ReplaceContributionDependencies(ctx context.Context, contributions []extractor.Contribution) error {
	if !observer.store.sealed {
		return errors.New("replace contribution dependencies before seal")
	}
	if observer.store.dependencyWriteError != nil {
		return observer.store.dependencyWriteError
	}
	return observer.ContributionSession.ReplaceContributionDependencies(ctx, contributions)
}

func (observer contributionSessionObserver) WriteWorkspaceFacts(ctx context.Context, facts graph.Facts) error {
	if observer.store.workspaceFactWriteError != nil && observer.store.workspaceFactPages >= observer.store.workspaceFactFailAfter {
		return observer.store.workspaceFactWriteError
	}
	if err := observer.ContributionSession.WriteWorkspaceFacts(ctx, facts); err != nil {
		return err
	}
	observer.store.workspaceFactPages++
	observer.store.workspaceFactNodes += len(facts.Nodes)
	observer.store.workspaceFactEdges += len(facts.Edges)
	return nil
}

func (observer contributionSessionObserver) ResolverProjectionPage(ctx context.Context, snapshot storage.Snapshot, request storage.ResolverProjectionPageRequest) ([]storage.ResolverProjection, error) {
	observer.store.resolverProjectionPages++
	if observer.store.resolverProjectionError != nil {
		return nil, observer.store.resolverProjectionError
	}
	return observer.ContributionSession.ResolverProjectionPage(ctx, snapshot, request)
}

func (observer contributionSessionObserver) ResolverTarget(ctx context.Context, snapshot storage.Snapshot, request extractor.ResolverTargetRequest) (extractor.ResolverTarget, bool, error) {
	if observer.store.resolverTargetError != nil {
		return extractor.ResolverTarget{}, false, observer.store.resolverTargetError
	}
	return observer.ContributionSession.ResolverTarget(ctx, snapshot, request)
}

func (observer contributionSessionObserver) ResolverPackagePage(ctx context.Context, snapshot storage.Snapshot, request extractor.ResolverPackagePageRequest) ([]extractor.ResolverTarget, error) {
	if observer.store.resolverPackageError != nil {
		return nil, observer.store.resolverPackageError
	}
	return observer.ContributionSession.ResolverPackagePage(ctx, snapshot, request)
}

func TestPublishCreatesInitialWorkspaceGraph(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/main.ts":  "export function main() { return 1; }",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})

	snapshot, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish initial workspace graph: %v", err)
	}
	if snapshot.Workspace != workspace.Root {
		t.Errorf("snapshot workspace = %q, want %q", snapshot.Workspace, workspace.Root)
	}
	if snapshot.Version != 1 {
		t.Errorf("snapshot version = %d, want 1", snapshot.Version)
	}
	if snapshot.PublishedAt.IsZero() {
		t.Error("snapshot publication time is zero")
	}
}

func TestIndexPublishesInitialWorkspaceThroughContributionSession(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	baseStore, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := baseStore.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})
	store := &contributionSessionStore{Store: baseStore}

	result, err := index.Index(context.Background(), store, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("index workspace: %v", err)
	}
	if store.sessions != 1 || store.contributions != 2 {
		t.Errorf("contribution session activity = %d sessions, %d contributions, want 1 session and 2 contributions", store.sessions, store.contributions)
	}
	if store.resolverProjectionPages == 0 {
		t.Error("initial index did not read staged resolver projection pages")
	}
	if store.workspaceFactPages == 0 {
		t.Error("initial index did not stream resolved workspace fact pages")
	}
	if store.workspaceFactNodes != 0 || store.workspaceFactEdges == 0 {
		t.Errorf("streamed workspace facts = %d nodes and %d edges, want no duplicate contribution nodes and positive resolved edges", store.workspaceFactNodes, store.workspaceFactEdges)
	}
	collector := &factCollector{}
	if err := store.Export(context.Background(), result.Snapshot, storage.ExportRequest{}, collector); err != nil {
		t.Fatalf("export indexed graph: %v", err)
	}
	if !hasImportTargetFrom(collector, "src/main.ts", "src/helper.ts::helper", "typescript:imports_from") {
		t.Errorf("indexed graph does not contain the resolved TypeScript import: %+v", collector.edges)
	}
}

func TestIndexPublishesLocalGoCallFactsFromProjectionPages(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"go.mod":      "module example.com/fixture\n",
		"cmd/main.go": "package main\n\nfunc run() {}\n\nfunc main() { run() }\n",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	result, err := index.Index(context.Background(), store, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish Go workspace: %v", err)
	}
	facts := &factCollector{}
	if err := store.Export(context.Background(), result.Snapshot, storage.ExportRequest{}, facts); err != nil {
		t.Fatalf("export Go workspace: %v", err)
	}
	if !hasRelationBetweenQualifiedNames(facts, "cmd/main.go::main.main", "cmd/main.go::main.run", "go:calls") {
		t.Errorf("published facts omit local Go call edge: %+v", facts.edges)
	}
}

func TestIndexTypeScriptProjectionPagesResolveAcrossBoundary(t *testing.T) {
	files := map[string]string{
		"package.json":    `{"name":"fixture"}`,
		"src/a-target.ts": "export function target() { return 1; }",
		"src/z-main.ts":   "import { target } from './a-target'; export function main() { return target(); }",
	}
	for sourceIndex := 0; sourceIndex < resolverPageTestSourceCount-2; sourceIndex++ {
		files[fmt.Sprintf("src/m-filler-%03d.ts", sourceIndex)] = fmt.Sprintf("export const value%d = %d;", sourceIndex, sourceIndex)
	}
	workspace := testkit.NewWorkspace(t, files)
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	snapshot, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish paged initial workspace: %v", err)
	}
	collector := &factCollector{}
	if err := store.Export(context.Background(), snapshot, storage.ExportRequest{}, collector); err != nil {
		t.Fatalf("export paged initial workspace: %v", err)
	}
	if !hasImportTargetFrom(collector, "src/z-main.ts", "src/a-target.ts::target", "typescript:imports_from") {
		t.Error("paged initial graph does not contain the cross-page TypeScript import")
	}
	contributions, err := store.SourceContributions(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("read paged initial contributions: %v", err)
	}
	for _, contribution := range contributions {
		if contribution.SourcePath != "src/z-main.ts" {
			continue
		}
		if !reflect.DeepEqual(contribution.Dependencies, []extractor.Dependency{{SourcePath: "src/z-main.ts", TargetPath: "src/a-target.ts"}}) {
			t.Errorf("src/z-main.ts dependencies = %+v, want src/a-target.ts", contribution.Dependencies)
		}
		return
	}
	t.Error("src/z-main.ts contribution was not stored")
}

func TestIndexJavaScriptResolvesFromStagedProjectionPages(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.js": "export function helper() { return 1; }",
		"src/main.js":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	snapshot, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish JavaScript workspace: %v", err)
	}
	collector := &factCollector{}
	if err := store.Export(context.Background(), snapshot, storage.ExportRequest{}, collector); err != nil {
		t.Fatalf("export JavaScript workspace: %v", err)
	}
	if !hasImportTargetFrom(collector, "src/main.js", "src/helper.js::helper", "javascript:imports_from") {
		t.Error("initial JavaScript graph does not contain the resolved import")
	}
}

func TestIndexProjectionPageFailureKeepsPriorSnapshotCurrent(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	baseStore, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() { _ = baseStore.Close() })
	prior, err := index.Publish(context.Background(), baseStore, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish prior workspace: %v", err)
	}

	projectionError := errors.New("injected projection failure")
	_, err = index.Publish(context.Background(), &contributionSessionStore{
		Store:                   baseStore,
		resolverProjectionError: projectionError,
	}, index.Request{Root: workspace.Root})
	if !errors.Is(err, projectionError) {
		t.Fatalf("publish with projection failure = %v, want injected failure", err)
	}
	current, err := baseStore.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: workspace.Root})
	if err != nil {
		t.Fatalf("open snapshot after projection failure: %v", err)
	}
	if current != prior {
		t.Errorf("snapshot after projection failure = %+v, want prior %+v", current, prior)
	}
}

func TestIndexWorkspaceFactPageFailureKeepsPriorSnapshotCurrent(t *testing.T) {
	files := map[string]string{
		"package.json":    `{"name":"fixture"}`,
		"src/a-target.ts": "export function target() { return 1; }",
		"src/z-main.ts":   "import { target } from './a-target'; export function main() { return target(); }",
	}
	for sourceIndex := 0; sourceIndex < resolverPageTestSourceCount-2; sourceIndex++ {
		files[fmt.Sprintf("src/m-filler-%03d.ts", sourceIndex)] = fmt.Sprintf("export const value%d = %d;", sourceIndex, sourceIndex)
	}
	workspace := testkit.NewWorkspace(t, files)
	baseStore, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() { _ = baseStore.Close() })
	prior, err := index.Publish(context.Background(), baseStore, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish prior workspace: %v", err)
	}

	writeError := errors.New("injected workspace fact page failure")
	store := &contributionSessionStore{
		Store:                   baseStore,
		workspaceFactFailAfter:  1,
		workspaceFactWriteError: writeError,
	}
	_, err = index.Publish(context.Background(), store, index.Request{Root: workspace.Root})
	if !errors.Is(err, writeError) {
		t.Fatalf("publish with workspace fact failure = %v, want injected failure", err)
	}
	if store.workspaceFactPages != 1 {
		t.Fatalf("workspace fact pages before failure = %d, want 1", store.workspaceFactPages)
	}
	current, err := baseStore.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: workspace.Root})
	if err != nil {
		t.Fatalf("open snapshot after workspace fact failure: %v", err)
	}
	if current != prior {
		t.Errorf("snapshot after workspace fact failure = %+v, want prior %+v", current, prior)
	}
}

func TestIndexResolutionSessionFailureKeepsPriorSnapshotCurrent(t *testing.T) {
	testCases := []struct {
		name            string
		files           map[string]string
		configuredRoots []string
		configure       func(*contributionSessionStore, error)
	}{
		{
			name: "resolver target",
			files: map[string]string{
				"package.json":  `{"name":"fixture"}`,
				"src/helper.ts": "export function helper() { return 1; }",
				"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
			},
			configure: func(store *contributionSessionStore, failure error) { store.resolverTargetError = failure },
		},
		{
			name: "resolver package",
			files: map[string]string{
				"go.mod":                    "module example.com/fixture\n",
				"internal/helper/helper.go": "package helper\n\nfunc Help() {}\n",
				"cmd/main.go":               "package main\n\nimport \"example.com/fixture/internal/helper\"\n\nfunc Main() { helper.Help() }\n",
			},
			configuredRoots: []string{"."},
			configure:       func(store *contributionSessionStore, failure error) { store.resolverPackageError = failure },
		},
		{
			name: "dependency write",
			files: map[string]string{
				"package.json": `{"name":"fixture"}`,
				"src/main.ts":  "export function main() { return 1; }",
			},
			configure: func(store *contributionSessionStore, failure error) { store.dependencyWriteError = failure },
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			workspace := testkit.NewWorkspace(t, testCase.files)
			baseStore, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
			if err != nil {
				t.Fatalf("open graph store: %v", err)
			}
			t.Cleanup(func() { _ = baseStore.Close() })
			request := index.Request{Root: workspace.Root, ConfiguredRoots: testCase.configuredRoots}
			prior, err := index.Publish(context.Background(), baseStore, request)
			if err != nil {
				t.Fatalf("publish prior workspace: %v", err)
			}

			failure := errors.New("injected " + testCase.name + " failure")
			store := &contributionSessionStore{Store: baseStore}
			testCase.configure(store, failure)
			_, err = index.Publish(context.Background(), store, request)
			if !errors.Is(err, failure) {
				t.Fatalf("publish with %s failure = %v, want injected failure", testCase.name, err)
			}
			current, err := baseStore.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: workspace.Root})
			if err != nil {
				t.Fatalf("open snapshot after %s failure: %v", testCase.name, err)
			}
			if current != prior {
				t.Errorf("snapshot after %s failure = %+v, want prior %+v", testCase.name, current, prior)
			}
		})
	}
}

func TestIndexFullReplacementRemovesDeletedSourceAndKeepsPriorSnapshot(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "import { main } from './main'; export function helper() { return main(); }",
		"src/main.ts":   "export function main() { return 2; }",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	prior, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish initial workspace: %v", err)
	}
	workspace.RemoveFile(t, "src/helper.ts")
	current, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish replacement workspace: %v", err)
	}

	priorContributions, err := store.SourceContributions(context.Background(), prior)
	if err != nil {
		t.Fatalf("read prior contributions: %v", err)
	}
	if len(priorContributions) != 2 {
		t.Errorf("prior contributions = %d, want 2", len(priorContributions))
	}
	currentContributions, err := store.SourceContributions(context.Background(), current)
	if err != nil {
		t.Fatalf("read replacement contributions: %v", err)
	}
	if len(currentContributions) != 1 || currentContributions[0].SourcePath != "src/main.ts" {
		t.Errorf("replacement contributions = %+v, want only src/main.ts", currentContributions)
	}
	collector := &factCollector{}
	if err := store.Export(context.Background(), current, storage.ExportRequest{}, collector); err != nil {
		t.Fatalf("export replacement graph: %v", err)
	}
	if hasImport(collector.edges, "src/helper.ts") {
		t.Errorf("replacement graph retains an import owned by deleted src/helper.ts: %+v", collector.edges)
	}
}

func TestIndexFullReplacementPublishesDeletionOnlySnapshot(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/main.ts":  "export function main() { return 1; }",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	prior, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish initial workspace: %v", err)
	}
	workspace.RemoveFile(t, "src/main.ts")
	current, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish deletion-only workspace: %v", err)
	}
	if current.Version != prior.Version+1 {
		t.Errorf("deletion-only version = %d, want %d", current.Version, prior.Version+1)
	}
	contributions, err := store.SourceContributions(context.Background(), current)
	if err != nil {
		t.Fatalf("read deletion-only contributions: %v", err)
	}
	if len(contributions) != 0 {
		t.Errorf("deletion-only contributions = %+v, want none", contributions)
	}
}

func TestIndexWritesContributionsBeforeExtractionCompletes(t *testing.T) {
	const sourceCount = 129
	files := map[string]string{"package.json": `{"name":"fixture"}`}
	for sourceIndex := range sourceCount {
		files[fmt.Sprintf("src/source-%03d.ts", sourceIndex)] = fmt.Sprintf("export const value%d = %d;", sourceIndex, sourceIndex)
	}
	workspace := testkit.NewWorkspace(t, files)
	baseStore, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := baseStore.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})
	store := &contributionSessionStore{
		Store:         baseStore,
		writeStarted:  make(chan struct{}),
		releaseWrites: make(chan struct{}),
	}
	progress := make(chan int, sourceCount+1)
	indexed := make(chan error, 1)
	go func() {
		_, err := index.Index(context.Background(), store, index.Request{
			Root: workspace.Root,
			Progress: func(update index.Progress) {
				if update.Phase == index.ExtractPhase {
					progress <- update.CompletedSources
				}
			},
		})
		indexed <- err
	}()

	select {
	case <-store.writeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("initial index did not start a contribution write")
	}
	completedSources := 0
readProgress:
	for {
		select {
		case completedSources = <-progress:
		default:
			break readProgress
		}
	}
	close(store.releaseWrites)
	if completedSources == sourceCount {
		t.Error("initial index completed extraction before starting the first contribution write")
	}
	if err := <-indexed; err != nil {
		t.Fatalf("index workspace: %v", err)
	}
}

func TestIndexStreamsDiscoveredSourcesBeforeDiscoveryCompletes(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previousProcs) })

	const sourceCount = 64
	files := map[string]string{"package.json": `{"name":"fixture"}`}
	for sourceIndex := range sourceCount {
		files[fmt.Sprintf("src/source-%03d.ts", sourceIndex)] = fmt.Sprintf("export const value%d = %d;", sourceIndex, sourceIndex)
	}
	workspace := testkit.NewWorkspace(t, files)
	baseStore, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() { _ = baseStore.Close() })
	store := &contributionSessionStore{Store: baseStore}

	if _, err := index.Index(context.Background(), store, index.Request{Root: workspace.Root}); err != nil {
		t.Fatalf("index workspace: %v", err)
	}
	store.observationMu.Lock()
	stagedAtFirstWrite := store.stagedAtFirstWrite
	stagedSources := store.stagedSources
	store.observationMu.Unlock()
	if stagedSources != sourceCount {
		t.Errorf("staged sources = %d, want %d", stagedSources, sourceCount)
	}
	if stagedAtFirstWrite >= sourceCount {
		t.Errorf("sources staged at first write = %d, want fewer than %d", stagedAtFirstWrite, sourceCount)
	}
}

func TestIndexContributionWriteFailureKeepsPriorSnapshotCurrent(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/main.ts":  "export function main() { return 1; }",
	})
	baseStore, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := baseStore.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})
	published, err := index.Publish(context.Background(), baseStore, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish prior snapshot: %v", err)
	}
	workspace.WriteFile(t, "src/main.ts", "export function replacement() { return 2; }")
	store := &contributionSessionStore{Store: baseStore, writeError: errors.New("injected contribution write failure")}

	_, err = index.Index(context.Background(), store, index.Request{Root: workspace.Root})
	if err == nil || !strings.Contains(err.Error(), "injected contribution write failure") {
		t.Errorf("index error = %v, want contribution write failure", err)
	}
	current, err := baseStore.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: workspace.Root})
	if err != nil {
		t.Fatalf("open current snapshot: %v", err)
	}
	if current != published {
		t.Errorf("current snapshot after contribution write failure = %+v, want %+v", current, published)
	}
}

func TestIndexCancellationKeepsPriorSnapshotCurrent(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/main.ts":  "export function main() { return 1; }",
	})
	baseStore, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := baseStore.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})
	published, err := index.Publish(context.Background(), baseStore, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish prior snapshot: %v", err)
	}
	workspace.WriteFile(t, "src/main.ts", "export function replacement() { return 2; }")
	store := &contributionSessionStore{
		Store:         baseStore,
		writeStarted:  make(chan struct{}),
		releaseWrites: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	indexed := make(chan error, 1)
	go func() {
		_, err := index.Index(ctx, store, index.Request{Root: workspace.Root})
		indexed <- err
	}()
	select {
	case <-store.writeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("initial index did not start a contribution write")
	}
	cancel()
	close(store.releaseWrites)
	if err := <-indexed; !errors.Is(err, context.Canceled) {
		t.Errorf("index error = %v, want context cancellation", err)
	}
	current, err := baseStore.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: workspace.Root})
	if err != nil {
		t.Fatalf("open current snapshot: %v", err)
	}
	if current != published {
		t.Errorf("current snapshot after cancellation = %+v, want %+v", current, published)
	}
}

func TestIndexReportsExtractionResolutionAndPublicationProgress(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})

	progress := make([]index.Progress, 0)
	result, err := index.Index(context.Background(), store, index.Request{
		Root: workspace.Root,
		Progress: func(update index.Progress) {
			progress = append(progress, update)
		},
	})
	if err != nil {
		t.Fatalf("index workspace: %v", err)
	}

	if len(progress) < 6 {
		t.Fatalf("progress = %+v, want extraction, resolution, and publication updates", progress)
	}
	if progress[0] != (index.Progress{Phase: index.ExtractPhase}) {
		t.Errorf("initial progress = %+v, want extraction with total pending", progress[0])
	}
	if progress[3] != (index.Progress{Phase: index.ExtractPhase, CompletedSources: 2, TotalSources: 2}) {
		t.Errorf("final extraction progress = %+v, want two completed sources", progress[3])
	}
	if progress[4] != (index.Progress{Phase: index.ResolvePhase, CompletedSources: 2, TotalSources: 2}) {
		t.Errorf("resolution progress = %+v, want resolved workspace", progress[4])
	}
	publication := progress[len(progress)-1]
	if publication.Phase != index.PublishPhase || publication.CompletedSources != 2 || publication.TotalSources != 2 {
		t.Errorf("publication progress = %+v, want two published sources", publication)
	}
	if publication.TotalNodes == 0 || publication.TotalEdges == 0 {
		t.Errorf("publication progress = %+v, want positive fact totals", publication)
	}
	if publication.WrittenNodes != publication.TotalNodes || publication.WrittenEdges != publication.TotalEdges {
		t.Errorf("publication progress = %+v, want all facts written", publication)
	}
	counts, err := store.FactCounts(context.Background(), result.Snapshot)
	if err != nil {
		t.Fatalf("count published facts: %v", err)
	}
	if publication.TotalNodes != counts.Nodes || publication.TotalEdges != counts.Edges {
		t.Errorf("publication totals = %d nodes and %d edges, want committed counts %+v", publication.TotalNodes, publication.TotalEdges, counts)
	}
}

func TestIndexReportsStablePhaseMeasurements(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/main.ts":  "export function main() { return 1; }",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})

	measurements := make([]index.Measurement, 0, 12)
	var statistics index.PipelineStatistics
	if _, err := index.Index(context.Background(), store, index.Request{
		Root: workspace.Root,
		Measurement: func(measurement index.Measurement) {
			measurements = append(measurements, measurement)
		},
		PipelineStatistics: func(update index.PipelineStatistics) {
			statistics = update
		},
	}); err != nil {
		t.Fatalf("index workspace: %v", err)
	}

	wantNames := []string{"discovery", "pipeline_wall", "extraction", "extractor_busy", "writer_busy", "producer_blocked", "extraction_write_overlap", "resolution", "publication_preparation", "sqlite_write", "commit", "staged_transaction"}
	if len(measurements) != len(wantNames) {
		t.Fatalf("measurements = %+v, want %d phases", measurements, len(wantNames))
	}
	for measurementIndex, want := range wantNames {
		if measurements[measurementIndex].Name != want {
			t.Errorf("measurement %d name = %q, want %q", measurementIndex, measurements[measurementIndex].Name, want)
		}
		if measurements[measurementIndex].Duration < 0 {
			t.Errorf("measurement %q duration = %s, want non-negative", want, measurements[measurementIndex].Duration)
		}
	}
	if statistics.ContributionQueueHighWater <= 0 {
		t.Errorf("contribution queue high-water = %d, want positive", statistics.ContributionQueueHighWater)
	}
	if statistics.ContributionQueueHighWater > statistics.ContributionQueueCapacity {
		t.Errorf("contribution queue high-water = %d, want at most capacity %d", statistics.ContributionQueueHighWater, statistics.ContributionQueueCapacity)
	}
}

func TestPublishBatchReportsStableResolverMeasurements(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})

	if _, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root}); err != nil {
		t.Fatalf("publish initial workspace graph: %v", err)
	}
	workspace.WriteFile(t, "src/helper.ts", "export function replacement() { return 2; }")
	measurements := make([]index.Measurement, 0, 3)
	if _, err := index.PublishBatch(context.Background(), store, index.BatchRequest{
		Root:         workspace.Root,
		ChangedPaths: []string{"src/helper.ts"},
		Measurement: func(measurement index.Measurement) {
			measurements = append(measurements, measurement)
		},
	}); err != nil {
		t.Fatalf("publish changed source batch: %v", err)
	}

	wantNames := []string{"affected_source_selection", "contribution_restoration", "workspace_resolution", "publication_preparation", "sqlite_write", "commit"}
	if len(measurements) != len(wantNames) {
		t.Fatalf("measurements = %+v, want %d resolver measurements", measurements, len(wantNames))
	}
	for measurementIndex, want := range wantNames {
		if measurements[measurementIndex].Name != want {
			t.Errorf("measurement %d name = %q, want %q", measurementIndex, measurements[measurementIndex].Name, want)
		}
		if measurements[measurementIndex].Duration < 0 {
			t.Errorf("measurement %q duration = %s, want non-negative", want, measurements[measurementIndex].Duration)
		}
	}
}

func TestPublishIncludesLocalGoFacts(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"src/main.go": "package fixture\n\nfunc Main() {}\n",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})

	snapshot, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root, ConfiguredRoots: []string{"."}})
	if err != nil {
		t.Fatalf("publish Go workspace graph: %v", err)
	}
	collector := &factCollector{}
	if err := store.Export(context.Background(), snapshot, storage.ExportRequest{}, collector); err != nil {
		t.Fatalf("export published graph: %v", err)
	}
	for _, node := range collector.nodes {
		if node.Kind == goextractor.FunctionNodeKind && node.QualifiedName == "src/main.go::fixture.Main" {
			return
		}
	}
	t.Errorf("published graph nodes = %+v, want Go function", collector.nodes)
}

func TestPublishIncludesResolvedGoImportFacts(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"go.mod":                    "module example.com/fixture\n",
		"internal/helper/helper.go": "package helper\n\nfunc Help() {}\n",
		"cmd/main.go":               "package main\n\nimport \"example.com/fixture/internal/helper\"\n\nfunc Main() { helper.Help() }\n",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})

	snapshot, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root, ConfiguredRoots: []string{"."}})
	if err != nil {
		t.Fatalf("publish Go workspace graph: %v", err)
	}
	collector := &factCollector{}
	if err := store.Export(context.Background(), snapshot, storage.ExportRequest{}, collector); err != nil {
		t.Fatalf("export published graph: %v", err)
	}
	for _, edge := range collector.edges {
		if edge.Relation == goextractor.ImportsFromRelation {
			return
		}
	}
	t.Errorf("published graph edges = %+v, want resolved Go import fact", collector.edges)
}

func TestPublishDiscoversGoModuleWithoutConfiguredRoot(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"go.mod":      "module example.com/fixture\n",
		"src/main.go": "package fixture\n\nfunc Main() {}\n",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})

	if _, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root}); err != nil {
		t.Fatalf("publish Go module workspace: %v", err)
	}
}

func TestPublishIncludesResolvedTypeScriptImportFacts(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})

	snapshot, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish initial workspace graph: %v", err)
	}
	collector := &factCollector{}
	if err := store.Export(context.Background(), snapshot, storage.ExportRequest{}, collector); err != nil {
		t.Fatalf("export published graph: %v", err)
	}
	for _, edge := range collector.edges {
		if edge.Relation == "typescript:imports_from" {
			return
		}
	}
	t.Error("published graph has no resolved TypeScript import fact")
}

func TestPublishBatchReresolvesOnlyDependenciesInTheChangedProject(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"packages/first/package.json":   `{"name":"first"}`,
		"packages/first/src/helper.ts":  "export const helper = 1;\n",
		"packages/first/src/main.ts":    "import { helper } from './helper'; export const first = helper;\n",
		"packages/second/package.json":  `{"name":"second"}`,
		"packages/second/src/helper.ts": "export const helper = 2;\n",
		"packages/second/src/main.ts":   "import { helper } from './helper'; export const second = helper;\n",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})

	first, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish multi-project workspace: %v", err)
	}
	initialFacts := &factCollector{}
	if err := store.Export(context.Background(), first, storage.ExportRequest{}, initialFacts); err != nil {
		t.Fatalf("export initial multi-project graph: %v", err)
	}
	for projectID, sourcePath := range map[string]string{
		"project:packages/first":  "packages/first/src/main.ts",
		"project:packages/second": "packages/second/src/main.ts",
	} {
		if !hasProjectSource(initialFacts, projectID, sourcePath) {
			t.Errorf("initial graph does not assign %q to %q", sourcePath, projectID)
		}
	}
	for sourcePath, qualifiedName := range map[string]string{
		"packages/first/src/main.ts":  "packages/first/src/helper.ts::helper",
		"packages/second/src/main.ts": "packages/second/src/helper.ts::helper",
	} {
		if !hasImportTargetFrom(initialFacts, sourcePath, qualifiedName, "typescript:imports_from") {
			t.Errorf("initial graph has no named import from %q to %q", sourcePath, qualifiedName)
		}
	}

	workspace.WriteFile(t, "packages/first/src/helper.ts", "export const replacement = 1;\n")
	second, err := index.PublishBatch(context.Background(), store, index.BatchRequest{
		Root:         workspace.Root,
		ChangedPaths: []string{"packages/first/src/helper.ts"},
	})
	if err != nil {
		t.Fatalf("publish changed first project source: %v", err)
	}
	if second.Version != first.Version+1 {
		t.Errorf("changed snapshot version = %d, want %d", second.Version, first.Version+1)
	}

	updatedFacts := &factCollector{}
	if err := store.Export(context.Background(), second, storage.ExportRequest{}, updatedFacts); err != nil {
		t.Fatalf("export changed multi-project graph: %v", err)
	}
	if hasImportTargetFrom(updatedFacts, "packages/first/src/main.ts", "packages/first/src/helper.ts::helper", "typescript:imports_from") {
		t.Error("changed project retains a named import for its removed exported surface")
	}
	if !hasImportTargetFrom(updatedFacts, "packages/second/src/main.ts", "packages/second/src/helper.ts::helper", "typescript:imports_from") {
		t.Error("unchanged project loses its named import target")
	}
}

func TestPublishBatchTypeScriptUsesProjectionPagesWithoutContributionRestoration(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	database, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := index.Publish(context.Background(), database, index.Request{Root: workspace.Root}); err != nil {
		t.Fatalf("publish initial workspace graph: %v", err)
	}

	workspace.WriteFile(t, "src/helper.ts", "export function replacement() { return 2; }")
	store := rejectingContributionReader{Store: database}
	if _, err := index.PublishBatch(context.Background(), store, index.BatchRequest{
		Root:         workspace.Root,
		ChangedPaths: []string{"src/helper.ts"},
	}); err != nil {
		t.Fatalf("publish TypeScript batch without contribution restoration: %v", err)
	}
}

func TestPublishBatchRetainsResolvedFactsForUnchangedOwners(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	database, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := index.Publish(context.Background(), database, index.Request{Root: workspace.Root}); err != nil {
		t.Fatalf("publish initial workspace graph: %v", err)
	}

	workspace.WriteFile(t, "src/helper.ts", "export function helper() { return 2; }")
	snapshot, err := index.PublishBatch(context.Background(), excludingResolverProjectionStore{
		Store:    database,
		excluded: "src/main.ts",
	}, index.BatchRequest{
		Root:         workspace.Root,
		ChangedPaths: []string{"src/helper.ts"},
	})
	if err != nil {
		t.Fatalf("publish implementation-only batch: %v", err)
	}

	facts := &factCollector{}
	if err := database.Export(context.Background(), snapshot, storage.ExportRequest{}, facts); err != nil {
		t.Fatalf("export implementation-only batch: %v", err)
	}
	if !hasImport(facts.edges, "src/main.ts") {
		t.Error("implementation-only batch does not retain the unchanged owner's resolved import")
	}
}

func TestPublishBatchJavaScriptUsesProjectionPagesWithoutContributionRestoration(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.js": "export function helper() { return 1; }",
		"src/main.js":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	database, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := index.Publish(context.Background(), database, index.Request{Root: workspace.Root}); err != nil {
		t.Fatalf("publish initial workspace graph: %v", err)
	}

	workspace.WriteFile(t, "src/helper.js", "export function replacement() { return 2; }")
	store := rejectingContributionReader{Store: database}
	if _, err := index.PublishBatch(context.Background(), store, index.BatchRequest{
		Root:         workspace.Root,
		ChangedPaths: []string{"src/helper.js"},
	}); err != nil {
		t.Fatalf("publish JavaScript batch without contribution restoration: %v", err)
	}
}

func TestPublishBatchGoUsesProjectionPagesWithoutContributionRestoration(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"go.mod":                    "module example.com/fixture\n",
		"internal/helper/helper.go": "package helper\n\nfunc Help() {}\n",
		"cmd/main.go":               "package main\n\nimport \"example.com/fixture/internal/helper\"\n\nfunc Main() { helper.Help() }\n",
	})
	database, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := index.Publish(context.Background(), database, index.Request{Root: workspace.Root, ConfiguredRoots: []string{"."}}); err != nil {
		t.Fatalf("publish initial workspace graph: %v", err)
	}

	workspace.WriteFile(t, "internal/helper/helper.go", "package helper\n\nfunc Replacement() {}\n")
	store := rejectingContributionReader{Store: database}
	if _, err := index.PublishBatch(context.Background(), store, index.BatchRequest{
		Root:            workspace.Root,
		ConfiguredRoots: []string{"."},
		ChangedPaths:    []string{"internal/helper/helper.go"},
	}); err != nil {
		t.Fatalf("publish Go batch without contribution restoration: %v", err)
	}
}

func TestPublishBatchGoPagesRecomputeInterfaceImplementation(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"go.mod":              "module example.com/fixture\n",
		"service/contract.go": "package service\n\ntype Runner interface {\n\tRun()\n}\n",
		"service/service.go":  "package service\n\ntype Worker struct{}\n\nfunc (Worker) Run() {}\n",
	})
	database, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := index.Publish(context.Background(), database, index.Request{Root: workspace.Root, ConfiguredRoots: []string{"."}}); err != nil {
		t.Fatalf("publish initial Go workspace graph: %v", err)
	}

	workspace.WriteFile(t, "service/service.go", "package service\n\ntype Worker struct{}\n")
	snapshot, err := index.PublishBatch(context.Background(), rejectingContributionReader{Store: database}, index.BatchRequest{
		Root:            workspace.Root,
		ConfiguredRoots: []string{"."},
		ChangedPaths:    []string{"service/service.go"},
	})
	if err != nil {
		t.Fatalf("publish Go implementation update: %v", err)
	}
	facts := &factCollector{}
	if err := database.Export(context.Background(), snapshot, storage.ExportRequest{}, facts); err != nil {
		t.Fatalf("export Go implementation update: %v", err)
	}
	if hasRelationBetweenQualifiedNames(facts, "service/service.go::service.Worker", "service/contract.go::service.Runner", "go:implements") {
		t.Error("Go implementation edge remains after the method was removed")
	}
}

func TestPublishBatchTypeScriptPagesMatchFreshUnpagedIndex(t *testing.T) {
	files := map[string]string{"package.json": `{"name":"fixture"}`}
	for sourceIndex := 0; sourceIndex <= resolverPageTestSourceCount; sourceIndex++ {
		files[fmt.Sprintf("src/file-%03d.ts", sourceIndex)] = fmt.Sprintf("export const value%d = %d;", sourceIndex, sourceIndex)
	}
	files["src/main.ts"] = fmt.Sprintf("import { value%d } from './file-%03d'; export const result = value%d;", resolverPageTestSourceCount, resolverPageTestSourceCount, resolverPageTestSourceCount)
	workspace := testkit.NewWorkspace(t, files)
	boundedStore, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "bounded.db"))
	if err != nil {
		t.Fatalf("open bounded graph store: %v", err)
	}
	t.Cleanup(func() { _ = boundedStore.Close() })
	if _, err := index.Publish(context.Background(), boundedStore, index.Request{Root: workspace.Root}); err != nil {
		t.Fatalf("publish initial workspace graph: %v", err)
	}
	changedPaths := make([]string, 0, resolverPageTestSourceCount+1)
	for sourceIndex := 0; sourceIndex <= resolverPageTestSourceCount; sourceIndex++ {
		changedPath := fmt.Sprintf("src/file-%03d.ts", sourceIndex)
		workspace.WriteFile(t, changedPath, fmt.Sprintf("export const replacement%d = %d;", sourceIndex, sourceIndex))
		changedPaths = append(changedPaths, changedPath)
	}
	measurements := make([]index.Measurement, 0, 6)
	boundedSnapshot, err := index.PublishBatch(context.Background(), rejectingContributionReader{Store: boundedStore}, index.BatchRequest{
		Root:         workspace.Root,
		ChangedPaths: changedPaths,
		Measurement: func(measurement index.Measurement) {
			measurements = append(measurements, measurement)
		},
	})
	if err != nil {
		t.Fatalf("publish bounded TypeScript batch: %v", err)
	}

	freshStore, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("open fresh graph store: %v", err)
	}
	t.Cleanup(func() { _ = freshStore.Close() })
	freshSnapshot, err := index.Publish(context.Background(), freshStore, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish fresh workspace graph: %v", err)
	}

	bounded := &factCollector{}
	if err := boundedStore.Export(context.Background(), boundedSnapshot, storage.ExportRequest{}, bounded); err != nil {
		t.Fatalf("export bounded graph: %v", err)
	}
	fresh := &factCollector{}
	if err := freshStore.Export(context.Background(), freshSnapshot, storage.ExportRequest{}, fresh); err != nil {
		t.Fatalf("export fresh graph: %v", err)
	}
	if !reflect.DeepEqual(bounded.nodes, fresh.nodes) || !reflect.DeepEqual(bounded.edges, fresh.edges) {
		t.Errorf("bounded facts differ from fresh facts\nbounded: %+v\nfresh: %+v", bounded, fresh)
	}
	wantMeasurements := []string{"affected_source_selection", "contribution_restoration", "workspace_resolution", "publication_preparation", "sqlite_write", "commit"}
	if len(measurements) != len(wantMeasurements) {
		t.Fatalf("staged measurements = %+v, want %q", measurements, wantMeasurements)
	}
	for measurementIndex, want := range wantMeasurements {
		if measurements[measurementIndex].Name != want {
			t.Errorf("staged measurement %d = %q, want %q", measurementIndex, measurements[measurementIndex].Name, want)
		}
	}
}

func TestPublishBatchGoPagesMatchFreshUnpagedIndex(t *testing.T) {
	files := map[string]string{
		"go.mod":      "module example.com/fixture\n",
		"cmd/main.go": "package main\n\nimport \"example.com/fixture/internal/helper\"\n\nfunc Main() { helper.Help000() }\n",
	}
	for sourceIndex := 0; sourceIndex <= resolverPageTestSourceCount; sourceIndex++ {
		files[fmt.Sprintf("internal/helper/helper-%03d.go", sourceIndex)] = fmt.Sprintf("package helper\n\nfunc Help%03d() {}\n", sourceIndex)
	}
	workspace := testkit.NewWorkspace(t, files)
	boundedStore, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "bounded.db"))
	if err != nil {
		t.Fatalf("open bounded graph store: %v", err)
	}
	t.Cleanup(func() { _ = boundedStore.Close() })
	if _, err := index.Publish(context.Background(), boundedStore, index.Request{Root: workspace.Root, ConfiguredRoots: []string{"."}}); err != nil {
		t.Fatalf("publish initial Go workspace graph: %v", err)
	}
	changedPath := fmt.Sprintf("internal/helper/helper-%03d.go", resolverPageTestSourceCount)
	workspace.WriteFile(t, changedPath, fmt.Sprintf("package helper\n\nfunc Replacement%03d() {}\n", resolverPageTestSourceCount))
	boundedSnapshot, err := index.PublishBatch(context.Background(), rejectingContributionReader{Store: boundedStore}, index.BatchRequest{
		Root:            workspace.Root,
		ConfiguredRoots: []string{"."},
		ChangedPaths:    []string{changedPath},
	})
	if err != nil {
		t.Fatalf("publish bounded Go batch: %v", err)
	}

	freshStore, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("open fresh graph store: %v", err)
	}
	t.Cleanup(func() { _ = freshStore.Close() })
	freshSnapshot, err := index.Publish(context.Background(), freshStore, index.Request{Root: workspace.Root, ConfiguredRoots: []string{"."}})
	if err != nil {
		t.Fatalf("publish fresh Go workspace graph: %v", err)
	}

	bounded := &factCollector{}
	if err := boundedStore.Export(context.Background(), boundedSnapshot, storage.ExportRequest{}, bounded); err != nil {
		t.Fatalf("export bounded Go graph: %v", err)
	}
	fresh := &factCollector{}
	if err := freshStore.Export(context.Background(), freshSnapshot, storage.ExportRequest{}, fresh); err != nil {
		t.Fatalf("export fresh Go graph: %v", err)
	}
	if !reflect.DeepEqual(bounded.edges, fresh.edges) {
		t.Errorf("bounded Go resolved edges differ from fresh edges: %s", firstEdgeDifference(bounded.edges, fresh.edges))
	}
}

func firstEdgeDifference(bounded, fresh []graph.Edge) string {
	for index := range bounded {
		if bounded[index] != fresh[index] {
			return fmt.Sprintf("edge %d = %+v, want %+v", index, bounded[index], fresh[index])
		}
	}
	return fmt.Sprintf("counts differ: %d edges, want %d edges", len(bounded), len(fresh))
}

const resolverPageTestSourceCount = 129

type rejectingContributionReader struct {
	*sqlite.Store
}

func (store rejectingContributionReader) SourceContributions(context.Context, storage.Snapshot) ([]storage.SourceContribution, error) {
	return nil, errors.New("full source contribution restoration is not allowed")
}

type excludingResolverProjectionStore struct {
	*sqlite.Store
	excluded string
}

func (store excludingResolverProjectionStore) ResolverProjectionPage(ctx context.Context, snapshot storage.Snapshot, request storage.ResolverProjectionPageRequest) ([]storage.ResolverProjection, error) {
	projections, err := store.Store.ResolverProjectionPage(ctx, snapshot, request)
	if err != nil {
		return nil, err
	}
	filtered := projections[:0]
	for _, projection := range projections {
		if projection.SourcePath != store.excluded {
			filtered = append(filtered, projection)
		}
	}
	return filtered, nil
}

func TestIndexPersistsResolvedDependenciesForAffectedSourceLookup(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})

	snapshot, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish initial workspace graph: %v", err)
	}
	replacement, err := typescript.Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "src/helper.ts",
		Contents:   []byte("export function replacement() { return 2; }"),
	})
	if err != nil {
		t.Fatalf("extract replacement helper: %v", err)
	}
	update, err := extractor.NewGraphUpdate([]extractor.Contribution{replacement})
	if err != nil {
		t.Fatalf("create replacement update: %v", err)
	}

	affected, err := store.AffectedSources(context.Background(), snapshot, storage.AffectedSourcesRequest{Update: update})
	if err != nil {
		t.Fatalf("find affected sources: %v", err)
	}
	if len(affected) != 1 || affected[0] != "src/main.ts" {
		t.Errorf("affected sources = %q, want src/main.ts", affected)
	}
}

func TestPublishBatchReplacesChangedSourcesAndRemovesDeletedSourcesAtomically(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})

	first, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish initial workspace graph: %v", err)
	}
	workspace.WriteFile(t, "src/main.ts", "export function main() { return 2; }")
	workspace.RemoveFile(t, "src/helper.ts")

	second, err := index.PublishBatch(context.Background(), store, index.BatchRequest{
		Root:         workspace.Root,
		ChangedPaths: []string{"src/main.ts", "src/helper.ts"},
	})
	if err != nil {
		t.Fatalf("publish changed source batch: %v", err)
	}
	if second.Version != first.Version+1 {
		t.Errorf("replacement snapshot version = %d, want %d", second.Version, first.Version+1)
	}

	firstFacts := &factCollector{}
	if err := store.Export(context.Background(), first, storage.ExportRequest{}, firstFacts); err != nil {
		t.Fatalf("export first snapshot: %v", err)
	}
	if !hasQualifiedName(firstFacts.nodes, "src/helper.ts::helper") {
		t.Error("first snapshot no longer contains deleted helper source")
	}

	secondFacts := &factCollector{}
	if err := store.Export(context.Background(), second, storage.ExportRequest{}, secondFacts); err != nil {
		t.Fatalf("export replacement snapshot: %v", err)
	}
	if hasQualifiedName(secondFacts.nodes, "src/helper.ts::helper") {
		t.Error("replacement snapshot contains deleted helper source")
	}
	if hasImport(secondFacts.edges, "src/main.ts") {
		t.Error("replacement snapshot retains import facts from unchanged dependency source")
	}
}

func TestPublishBatchPublishesDeletionOnly(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/main.ts":  "export function main() { return 1; }",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})

	if _, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root}); err != nil {
		t.Fatalf("publish initial workspace graph: %v", err)
	}
	if err := os.Remove(filepath.Join(workspace.Root, "src", "main.ts")); err != nil {
		t.Fatalf("remove source: %v", err)
	}

	snapshot, err := index.PublishBatch(context.Background(), store, index.BatchRequest{
		Root:         workspace.Root,
		ChangedPaths: []string{"src/main.ts"},
	})
	if err != nil {
		t.Fatalf("publish deletion batch: %v", err)
	}
	facts := &factCollector{}
	if err := store.Export(context.Background(), snapshot, storage.ExportRequest{}, facts); err != nil {
		t.Fatalf("export deletion snapshot: %v", err)
	}
	if len(facts.nodes) != 0 {
		t.Errorf("deletion snapshot node count = %d, want 0", len(facts.nodes))
	}
}

func TestQueuedBatchPublishesChangedAndDeletedSources(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})
	first, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish initial workspace graph: %v", err)
	}

	manager := indexer.NewManager(
		indexer.WithPublisher(func(batch indexer.Batch) error {
			_, err := index.PublishBatch(context.Background(), store, index.BatchRequest{
				Root:         batch.Workspace,
				ChangedPaths: batch.Paths,
			})
			return err
		}),
		indexer.WithPublishThrottle(time.Millisecond),
	)
	t.Cleanup(func() {
		if err := manager.Stop(workspace.Root); err != nil {
			t.Errorf("stop indexer manager: %v", err)
		}
	})
	if _, err := manager.Start(workspace.Root); err != nil {
		t.Fatalf("start indexer manager: %v", err)
	}

	workspace.WriteFile(t, "src/main.ts", "export function main() { return 2; }")
	workspace.RemoveFile(t, "src/helper.ts")
	if err := manager.Enqueue(workspace.Root,
		indexer.Event{Path: "src/main.ts"},
		indexer.Event{Path: "src/helper.ts"},
	); err != nil {
		t.Fatalf("enqueue changed sources: %v", err)
	}

	deadline := time.After(time.Second)
	for {
		snapshot, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: workspace.Root})
		if err == nil && snapshot.Version == first.Version+1 {
			facts := &factCollector{}
			if err := store.Export(context.Background(), snapshot, storage.ExportRequest{}, facts); err != nil {
				t.Fatalf("export queued snapshot: %v", err)
			}
			if hasQualifiedName(facts.nodes, "src/helper.ts::helper") {
				t.Error("queued batch snapshot contains deleted helper source")
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for queued publication, latest snapshot error = %v", err)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestPublishBatchPreservesPublishedGraphWhenExtractionFails(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/main.ts":  "export function main() { return 1; }",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})
	first, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish initial workspace graph: %v", err)
	}
	workspace.WriteFile(t, "src/main.ts", "export function main( {")

	_, err = index.PublishBatch(context.Background(), store, index.BatchRequest{
		Root:         workspace.Root,
		ChangedPaths: []string{"src/main.ts"},
	})
	if err == nil {
		t.Fatal("publish malformed source batch succeeded")
	}
	current, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: workspace.Root})
	if err != nil {
		t.Fatalf("open published snapshot after failed batch: %v", err)
	}
	if current.Version != first.Version {
		t.Errorf("published version after failed batch = %d, want %d", current.Version, first.Version)
	}
	facts := &factCollector{}
	if err := store.Export(context.Background(), current, storage.ExportRequest{}, facts); err != nil {
		t.Fatalf("export published snapshot after failed batch: %v", err)
	}
	if !hasQualifiedName(facts.nodes, "src/main.ts::main") {
		t.Error("published snapshot after failed batch does not retain the prior source facts")
	}
}

func TestPublishBatchDoesNotExtractUnchangedSources(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/main.ts":  "export function main() { return 1; }",
		"src/other.ts": "export function other() { return 1; }",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})
	first, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish initial workspace graph: %v", err)
	}

	workspace.WriteFile(t, "src/main.ts", "export function mainReplacement() { return 2; }")
	workspace.WriteFile(t, "src/other.ts", "export function other( {")

	second, err := index.PublishBatch(context.Background(), store, index.BatchRequest{
		Root:         workspace.Root,
		ChangedPaths: []string{"src/main.ts"},
	})
	if err != nil {
		t.Fatalf("publish changed source batch: %v", err)
	}
	if second.Version != first.Version+1 {
		t.Errorf("changed snapshot version = %d, want %d", second.Version, first.Version+1)
	}

	facts := &factCollector{}
	if err := store.Export(context.Background(), second, storage.ExportRequest{}, facts); err != nil {
		t.Fatalf("export changed snapshot: %v", err)
	}
	if !hasQualifiedName(facts.nodes, "src/main.ts::mainReplacement") {
		t.Error("changed snapshot does not contain replacement source facts")
	}
	if !hasQualifiedName(facts.nodes, "src/other.ts::other") {
		t.Error("changed snapshot does not retain prior facts for unchanged source")
	}
}

func TestPublishBatchIgnoresUnsupportedFileChanges(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/main.ts":  "export function main() { return 1; }",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})
	first, err := index.Publish(context.Background(), store, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("publish initial workspace graph: %v", err)
	}

	workspace.WriteFile(t, "README.md", "changed documentation")
	snapshot, err := index.PublishBatch(context.Background(), store, index.BatchRequest{
		Root:         workspace.Root,
		ChangedPaths: []string{"README.md"},
	})
	if err != nil {
		t.Fatalf("publish unsupported source batch: %v", err)
	}
	if snapshot != first {
		t.Errorf("unsupported source snapshot = %+v, want existing snapshot %+v", snapshot, first)
	}
}

func TestIndexReturnsResolverDiagnostics(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }",
		"src/main.ts":   "import { missing } from './helper'; export function main() { return missing(); }",
	})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})

	result, err := index.Index(context.Background(), store, index.Request{Root: workspace.Root})
	if err != nil {
		t.Fatalf("index workspace: %v", err)
	}
	if result.Snapshot.Version != 1 {
		t.Errorf("snapshot version = %d, want 1", result.Snapshot.Version)
	}
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Message == `TypeScript export "missing" from "./helper" is not indexed` {
			return
		}
	}
	t.Errorf("diagnostics = %+v, want missing export diagnostic", result.Diagnostics)
}

func TestPublishRejectsWorkspaceWithoutSupportedSources(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{"README.md": "fixture"})
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close graph store: %v", err)
		}
	})

	_, err = index.Publish(context.Background(), store, index.Request{Root: workspace.Root})
	if err == nil || !strings.Contains(err.Error(), "no supported source files") {
		t.Errorf("publish error = %v, want no supported source error", err)
	}
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

func hasQualifiedName(nodes []graph.Node, qualifiedName string) bool {
	for _, node := range nodes {
		if node.QualifiedName == qualifiedName {
			return true
		}
	}
	return false
}

func hasImport(edges []graph.Edge, sourcePath string) bool {
	for _, edge := range edges {
		if edge.Relation == "typescript:imports_from" && edge.Evidence.Span.Path == sourcePath {
			return true
		}
	}
	return false
}

func hasProjectSource(facts *factCollector, projectID, sourcePath string) bool {
	var sourceID string
	for _, node := range facts.nodes {
		if node.QualifiedName == sourcePath {
			sourceID = node.ID
			break
		}
	}
	for _, edge := range facts.edges {
		if edge.SourceID == projectID && edge.TargetID == sourceID && edge.Relation == "contains" {
			return true
		}
	}
	return false
}

func hasImportTargetFrom(facts *factCollector, sourcePath, qualifiedName string, relation graph.RelationKind) bool {
	var sourceID string
	for _, node := range facts.nodes {
		if node.QualifiedName == sourcePath {
			sourceID = node.ID
			break
		}
	}
	for _, node := range facts.nodes {
		if node.QualifiedName != qualifiedName {
			continue
		}
		for _, edge := range facts.edges {
			if edge.SourceID == sourceID && edge.TargetID == node.ID && edge.Relation == relation {
				return true
			}
		}
	}
	return false
}

func hasRelationBetweenQualifiedNames(facts *factCollector, sourceQualifiedName, targetQualifiedName string, relation graph.RelationKind) bool {
	sourceID := ""
	targetID := ""
	for _, node := range facts.nodes {
		if node.QualifiedName == sourceQualifiedName {
			sourceID = node.ID
		}
		if node.QualifiedName == targetQualifiedName {
			targetID = node.ID
		}
	}
	for _, edge := range facts.edges {
		if edge.SourceID == sourceID && edge.TargetID == targetID && edge.Relation == relation {
			return true
		}
	}
	return false
}
