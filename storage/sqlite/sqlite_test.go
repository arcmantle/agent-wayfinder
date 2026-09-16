package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agent-wayfinder/extractor"
	goextractor "agent-wayfinder/extractors/go"
	"agent-wayfinder/extractors/typescript"
	"agent-wayfinder/graph"
	"agent-wayfinder/storage"
	"agent-wayfinder/storage/sqlite"

	_ "github.com/mattn/go-sqlite3"
)

func TestOpenMigratesNewDatabaseAndReopensIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")

	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open new database: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close new database: %v", err)
	}

	store, err = sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen migrated database: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close migrated database: %v", err)
	}
}

func TestOpenRecordsCurrentSchemaVersion(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	version, err := store.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != sqlite.CurrentSchemaVersion {
		t.Errorf("schema version = %d, want %d", version, sqlite.CurrentSchemaVersion)
	}
}

func TestOpenRecreatesMismatchedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mismatched-schema.db")
	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open mismatched database: %v", err)
	}
	_, err = database.Exec(fmt.Sprintf(`
		CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
		CREATE TABLE old_graph_data (value TEXT NOT NULL);
		INSERT INTO schema_migrations (version, applied_at) VALUES (%d, '2026-08-14T00:00:00Z');
		INSERT INTO old_graph_data (value) VALUES ('stale');
	`, sqlite.CurrentSchemaVersion-1))
	if err != nil {
		t.Fatalf("seed mismatched schema version: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close mismatched database: %v", err)
	}

	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open mismatched schema database: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("close mismatched schema database store: %v", closeErr)
		}
	})

	version, err := store.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("read recreated schema version: %v", err)
	}
	if version != sqlite.CurrentSchemaVersion {
		t.Errorf("recreated schema version = %d, want %d", version, sqlite.CurrentSchemaVersion)
	}
	if _, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: "workspace"}); !errors.Is(err, storage.ErrWorkspaceNotFound) {
		t.Errorf("open snapshot after schema reset error = %v, want workspace not found", err)
	}
}

func TestOpenMigratesVersionTenAndRebuildsLexicalRowsInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "version-ten.db")
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdate(t, "src/LegacyHandler.go", "function:LegacyHandler"),
	})
	if err != nil {
		t.Fatalf("publish pre-migration snapshot: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close pre-migration store: %v", err)
	}

	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open version-ten database: %v", err)
	}
	if _, err := database.Exec(`DROP TABLE node_search; DELETE FROM schema_migrations WHERE version = 11; INSERT INTO schema_migrations (version, applied_at) VALUES (10, '2026-08-31T00:00:00Z')`); err != nil {
		t.Fatalf("downgrade schema marker: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close version-ten database: %v", err)
	}

	store, err = sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("migrate version-ten database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	matches, err := store.SearchNodes(context.Background(), snapshot, storage.LexicalSearchRequest{Text: "legacy handler", Limit: 10})
	if err != nil {
		t.Fatalf("search rebuilt lexical index: %v", err)
	}
	if len(matches) != 1 || matches[0].Node.ID != "function:LegacyHandler" {
		t.Errorf("rebuilt lexical matches = %+v, want legacy handler", matches)
	}
}

func TestPublishCreatesFirstWorkspaceSnapshot(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdate(t, "src/main.ts", "function:main"),
	})
	if err != nil {
		t.Fatalf("publish first workspace snapshot: %v", err)
	}
	if snapshot.Workspace != "workspace" {
		t.Errorf("snapshot workspace = %q, want %q", snapshot.Workspace, "workspace")
	}
	if snapshot.Version != 1 {
		t.Errorf("snapshot version = %d, want 1", snapshot.Version)
	}
	if snapshot.PublishedAt.IsZero() {
		t.Fatal("snapshot publication time is zero")
	}
}

func TestPublishReportsStablePhaseMeasurements(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	measurements := make([]storage.PublishMeasurement, 0, 3)
	_, err = store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdate(t, "src/main.ts", "function:main"),
		Measurement: func(measurement storage.PublishMeasurement) {
			measurements = append(measurements, measurement)
		},
	})
	if err != nil {
		t.Fatalf("publish graph update: %v", err)
	}

	wantNames := []string{"publication_preparation", "sqlite_write", "commit"}
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
}

func TestPublishIncludesWorkspaceVersionFacts(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateForSources(t,
			sourceFact{path: "src/main.ts", nodeID: "function:main"},
			sourceFact{path: "src/helper.ts", nodeID: "function:helper"},
		),
		WorkspaceFacts: graph.Facts{Edges: []graph.Edge{{
			SourceID: "function:main",
			TargetID: "function:helper",
			Relation: "calls",
			Evidence: evidence("src/main.ts"),
		}}},
	})
	if err != nil {
		t.Fatalf("publish workspace facts: %v", err)
	}

	collector := &factCollector{}
	if err := store.Export(context.Background(), snapshot, storage.ExportRequest{}, collector); err != nil {
		t.Fatalf("export workspace facts: %v", err)
	}
	if len(collector.edges) != 1 {
		t.Fatalf("exported edge count = %d, want 1", len(collector.edges))
	}
	if got := collector.edges[0]; got.SourceID != "function:main" || got.TargetID != "function:helper" || got.Relation != "calls" {
		t.Errorf("exported edge = %+v, want function:main calls function:helper", got)
	}
}

func TestExportDeduplicatesNodeIdentityWithWorkspaceFactPrecedence(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	contributionNode := graphNode("src/main.ts", "function:main")
	workspaceNode := contributionNode
	workspaceNode.Label = "resolved main"
	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace:      "workspace",
		Update:         graphUpdateWithFacts(t, "src/main.ts", graph.Facts{Nodes: []graph.Node{contributionNode}}),
		WorkspaceFacts: graph.Facts{Nodes: []graph.Node{workspaceNode}},
	})
	if err != nil {
		t.Fatalf("publish duplicate node identity: %v", err)
	}

	collector := &factCollector{}
	if err := store.Export(context.Background(), snapshot, storage.ExportRequest{}, collector); err != nil {
		t.Fatalf("export duplicate node identity: %v", err)
	}
	matching := make([]graph.Node, 0, 1)
	for _, node := range collector.nodes {
		if node.ID == contributionNode.ID {
			matching = append(matching, node)
		}
	}
	if len(matching) != 1 || matching[0].Label != workspaceNode.Label {
		t.Errorf("exported duplicate identity = %+v, want one workspace node", matching)
	}
}

func TestFactCountsReturnsVisibleUniqueFacts(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdateForSources(t, sourceFact{path: "src/main.ts", nodeID: "function:main"}),
		WorkspaceFacts: graph.Facts{
			Nodes: []graph.Node{graphNode("src/main.ts", "function:main")},
			Edges: []graph.Edge{{SourceID: "function:main", TargetID: "function:main", Relation: "calls", Evidence: evidence("src/main.ts")}},
		},
	})
	if err != nil {
		t.Fatalf("publish graph facts: %v", err)
	}

	counts, err := store.FactCounts(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("count graph facts: %v", err)
	}
	if counts != (storage.FactCounts{Nodes: 1, Edges: 1}) {
		t.Errorf("fact counts = %+v, want 1 node and 1 edge", counts)
	}
}

func TestPublishStoresSourceAndWorkspaceFactsAsRelationalRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	if _, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateWithFacts(t, "src/main.ts", graph.Facts{
			Nodes: []graph.Node{
				graphNode("src/main.ts", "function:main"),
				graphNode("src/helper.ts", "function:helper"),
			},
			Edges: []graph.Edge{{
				SourceID: "function:main",
				TargetID: "function:helper",
				Relation: "calls",
				Evidence: evidence("src/main.ts"),
			}},
		}),
		WorkspaceFacts: graph.Facts{Edges: []graph.Edge{{
			SourceID: "function:main",
			TargetID: "function:helper",
			Relation: "resolved_calls",
			Evidence: evidence("src/main.ts"),
		}}},
	}); err != nil {
		t.Fatalf("publish normalized facts: %v", err)
	}

	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open database for verification: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close verification database: %v", err)
		}
	})

	var sourceNodes, sourceEdges, workspaceEdges int
	if err := database.QueryRow("SELECT COUNT(*) FROM contribution_nodes").Scan(&sourceNodes); err != nil {
		t.Fatalf("count normalized source nodes: %v", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM contribution_edges").Scan(&sourceEdges); err != nil {
		t.Fatalf("count normalized source edges: %v", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM workspace_edges WHERE resolved_fact_owner = ?", "src/main.ts").Scan(&workspaceEdges); err != nil {
		t.Fatalf("count normalized workspace edges: %v", err)
	}
	if sourceNodes != 2 || sourceEdges != 1 || workspaceEdges != 1 {
		t.Errorf("normalized fact counts = nodes:%d edges:%d workspaceEdges:%d, want 2, 1, 1", sourceNodes, sourceEdges, workspaceEdges)
	}
	var integrity string
	if err := database.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("check database integrity: %v", err)
	}
	if integrity != "ok" {
		t.Errorf("database integrity = %q, want ok", integrity)
	}
}

func TestSourceContributionsReadRelationalResolverMetadata(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	vocabulary, err := graph.NewVocabulary(graph.VocabularyDefinition{NodeKinds: []graph.NodeKind{"function"}, Relations: []graph.RelationDefinition{{Kind: "references", Endpoints: []graph.EndpointRule{{Source: "function", Target: "function"}}}}})
	if err != nil {
		t.Fatalf("create vocabulary: %v", err)
	}
	contribution, err := extractor.NewContribution(vocabulary, extractor.ContributionInput{
		SourcePath: "src/main.ts",
		Metadata:   extractor.Metadata{Name: "typescript", Version: "1", Extensions: []string{".ts"}},
		Facts:      graph.Facts{Nodes: []graph.Node{graphNode("src/main.ts", "function:main")}},
		UnresolvedReferences: []extractor.UnresolvedReference{{
			SourceID: "function:main",
			Target:   "./helper",
			Kind:     extractor.ModuleReferenceImport,
			Bindings: []extractor.ModuleBinding{{ImportedName: "helper", LocalName: "localHelper"}},
		}},
		SymbolReferences: []extractor.SymbolReference{{
			SourceID: "function:main",
			Target:   "helper",
			Relation: "references",
			Evidence: evidence("src/main.ts"),
		}},
		Diagnostics: []extractor.Diagnostic{{Severity: extractor.DiagnosticWarning, Message: "fixture warning"}},
	})
	if err != nil {
		t.Fatalf("create contribution: %v", err)
	}
	update, err := extractor.NewGraphUpdate([]extractor.Contribution{contribution})
	if err != nil {
		t.Fatalf("create graph update: %v", err)
	}

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{Workspace: "workspace", Update: update})
	if err != nil {
		t.Fatalf("publish contribution metadata: %v", err)
	}
	contributions, err := store.SourceContributions(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("read source contributions: %v", err)
	}
	if len(contributions) != 1 {
		t.Fatalf("source contribution count = %d, want 1", len(contributions))
	}
	got := contributions[0]
	if len(got.UnresolvedReferences) != 1 || got.UnresolvedReferences[0].Target != "./helper" || len(got.UnresolvedReferences[0].Bindings) != 1 || got.UnresolvedReferences[0].Bindings[0].LocalName != "localHelper" {
		t.Errorf("unresolved references = %+v, want ./helper", got.UnresolvedReferences)
	}
	if len(got.SymbolReferences) != 1 || got.SymbolReferences[0].Relation != "references" {
		t.Errorf("symbol references = %+v, want references", got.SymbolReferences)
	}
	if len(got.Diagnostics) != 1 || got.Diagnostics[0].Message != "fixture warning" {
		t.Errorf("diagnostics = %+v, want fixture warning", got.Diagnostics)
	}
}

func TestResolverProjectionsReadResolverDataWithoutGraphFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	vocabulary, err := graph.NewVocabulary(graph.VocabularyDefinition{NodeKinds: []graph.NodeKind{"function"}, Relations: []graph.RelationDefinition{{Kind: "references", Endpoints: []graph.EndpointRule{{Source: "function", Target: "function"}}}}})
	if err != nil {
		t.Fatalf("create vocabulary: %v", err)
	}
	contribution, err := extractor.NewContribution(vocabulary, extractor.ContributionInput{
		ProjectID:  "project:fixture",
		SourcePath: "src/main.ts",
		Metadata:   extractor.Metadata{Name: "typescript", Version: "1", Extensions: []string{".ts"}},
		Facts:      graph.Facts{Nodes: []graph.Node{graphNode("src/main.ts", "function:main")}},
		UnresolvedReferences: []extractor.UnresolvedReference{{
			SourceID: "function:main",
			Target:   "./helper",
			Kind:     extractor.ModuleReferenceImport,
			Bindings: []extractor.ModuleBinding{{ImportedName: "helper", LocalName: "localHelper"}},
		}},
		SymbolReferences: []extractor.SymbolReference{{
			SourceID: "function:main",
			Target:   "helper",
			Relation: "references",
			Evidence: evidence("src/main.ts"),
		}},
		ExportedSurfaces: []extractor.ExportedSurface{{NodeID: "function:main", Name: "main"}},
		Dependencies:     []extractor.Dependency{{SourcePath: "src/main.ts", TargetPath: "src/helper.ts"}},
	})
	if err != nil {
		t.Fatalf("create contribution: %v", err)
	}
	update, err := extractor.NewGraphUpdate([]extractor.Contribution{contribution})
	if err != nil {
		t.Fatalf("create graph update: %v", err)
	}
	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{Workspace: "workspace", Update: update})
	if err != nil {
		t.Fatalf("publish resolver projection: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	reopened, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	projections, err := reopened.ResolverProjections(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("read resolver projections: %v", err)
	}
	if len(projections) != 1 {
		t.Fatalf("resolver projection count = %d, want 1", len(projections))
	}
	got := projections[0]
	if got.ProjectID != "project:fixture" || got.SourcePath != "src/main.ts" || got.Metadata.Name != "typescript" {
		t.Errorf("resolver projection identity = %+v, want project:fixture src/main.ts typescript", got)
	}
	if len(got.UnresolvedReferences) != 1 || got.UnresolvedReferences[0].Target != "./helper" || len(got.UnresolvedReferences[0].Bindings) != 1 {
		t.Errorf("resolver projection unresolved references = %+v, want ./helper with binding", got.UnresolvedReferences)
	}
	if len(got.SymbolReferences) != 1 || got.SymbolReferences[0].Evidence.Span.Path != "src/main.ts" {
		t.Errorf("resolver projection symbol references = %+v, want evidence", got.SymbolReferences)
	}
	if len(got.ExportedSurfaces) != 1 || len(got.Dependencies) != 1 {
		t.Errorf("resolver projection surfaces and dependencies = %+v, %+v, want one each", got.ExportedSurfaces, got.Dependencies)
	}
}

func TestResolverProjectionPageFiltersAndOrdersSnapshotProjections(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	workspace := "workspace"
	for _, sourcePath := range []string{"src/c.ts", "src/a.ts", "src/b.ts"} {
		contribution, err := typescript.Extract(extractor.Source{
			ProjectID:  "project:fixture",
			SourcePath: sourcePath,
			Contents:   []byte("export const value = 1;"),
		})
		if err != nil {
			t.Fatalf("extract %q: %v", sourcePath, err)
		}
		update, err := extractor.NewGraphUpdate([]extractor.Contribution{contribution})
		if err != nil {
			t.Fatalf("create update for %q: %v", sourcePath, err)
		}
		if _, err := store.Publish(context.Background(), storage.PublishRequest{Workspace: workspace, Update: update}); err != nil {
			t.Fatalf("publish %q: %v", sourcePath, err)
		}
	}
	snapshot, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: workspace})
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}

	page, err := store.ResolverProjectionPage(context.Background(), snapshot, storage.ResolverProjectionPageRequest{
		ProjectID: "project:fixture",
		Language:  "typescript",
		Limit:     2,
	})
	if err != nil {
		t.Fatalf("read first projection page: %v", err)
	}
	if got := projectionPaths(page); !reflect.DeepEqual(got, []string{"src/a.ts", "src/b.ts"}) {
		t.Errorf("first projection page paths = %q, want src/a.ts then src/b.ts", got)
	}
	for projectionIndex, projection := range page {
		if len(projection.Nodes) == 0 {
			t.Errorf("projection %d for %q has no source nodes", projectionIndex, projection.SourcePath)
		}
	}

	page, err = store.ResolverProjectionPage(context.Background(), snapshot, storage.ResolverProjectionPageRequest{
		ProjectID:       "project:fixture",
		Language:        "typescript",
		AfterSourcePath: "src/b.ts",
		Limit:           2,
	})
	if err != nil {
		t.Fatalf("read next projection page: %v", err)
	}
	if got := projectionPaths(page); !reflect.DeepEqual(got, []string{"src/c.ts"}) {
		t.Errorf("next projection page paths = %q, want src/c.ts", got)
	}
}

func TestPublishStagedReadsOrderedResolverProjections(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	workspace := "workspace"
	for _, sourcePath := range []string{"src/a.ts", "src/b.ts", "src/c.ts"} {
		contribution, err := typescript.Extract(extractor.Source{
			ProjectID:  "project:fixture",
			SourcePath: sourcePath,
			Contents:   []byte("export const value = 1;"),
		})
		if err != nil {
			t.Fatalf("extract %q: %v", sourcePath, err)
		}
		update, err := extractor.NewGraphUpdate([]extractor.Contribution{contribution})
		if err != nil {
			t.Fatalf("create update for %q: %v", sourcePath, err)
		}
		if _, err := store.Publish(context.Background(), storage.PublishRequest{Workspace: workspace, Update: update}); err != nil {
			t.Fatalf("publish %q: %v", sourcePath, err)
		}
	}

	var got []string
	_, err = store.PublishStaged(context.Background(), storage.PublishRequest{
		Workspace: workspace,
		Update:    graphUpdate(t, "src/main.ts", "function:main"),
	}, func(ctx context.Context, stager storage.ResolverStager) (storage.PublishRequest, error) {
		if err := stager.StageResolverSources(ctx, []storage.ResolverStageSource{
			{ProjectID: "project:fixture", Language: "typescript", SourcePath: "src/c.ts"},
			{ProjectID: "project:fixture", Language: "typescript", SourcePath: "src/a.ts"},
		}); err != nil {
			return storage.PublishRequest{}, err
		}
		page, err := stager.ResolverProjectionPage(ctx, stager.Snapshot(), storage.ResolverProjectionPageRequest{ProjectID: "project:fixture", Language: "typescript", Limit: 2})
		if err != nil {
			return storage.PublishRequest{}, err
		}
		got = projectionPaths(page)
		return storage.PublishRequest{}, errors.New("roll back staged read")
	})
	if err == nil {
		t.Fatal("publish staged read succeeded")
	}
	if !reflect.DeepEqual(got, []string{"src/a.ts", "src/c.ts"}) {
		t.Errorf("staged projection paths = %q, want src/a.ts then src/c.ts", got)
	}
}

func TestPublishStagedDeduplicatesRepeatedSourcesAndReadsPackageTargets(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	workspace := "workspace"
	for _, sourcePath := range []string{"src/a.ts", "src/b.ts"} {
		contribution, err := typescript.Extract(extractor.Source{
			ProjectID:  "project:fixture",
			SourcePath: sourcePath,
			Contents:   []byte("export const value = 1;"),
		})
		if err != nil {
			t.Fatalf("extract %q: %v", sourcePath, err)
		}
		update, err := extractor.NewGraphUpdate([]extractor.Contribution{contribution})
		if err != nil {
			t.Fatalf("create update for %q: %v", sourcePath, err)
		}
		if _, err := store.Publish(context.Background(), storage.PublishRequest{Workspace: workspace, Update: update}); err != nil {
			t.Fatalf("publish %q: %v", sourcePath, err)
		}
	}

	var got []string
	_, err = store.PublishStaged(context.Background(), storage.PublishRequest{
		Workspace: workspace,
		Update:    graphUpdate(t, "src/main.ts", "function:main"),
	}, func(ctx context.Context, stager storage.ResolverStager) (storage.PublishRequest, error) {
		for _, sources := range [][]storage.ResolverStageSource{
			{{ProjectID: "project:fixture", Language: "typescript", SourcePath: "src/a.ts"}},
			{{ProjectID: "project:fixture", Language: "typescript", SourcePath: "src/a.ts"}},
		} {
			if err := stager.StageResolverSources(ctx, sources); err != nil {
				return storage.PublishRequest{}, err
			}
		}
		targets, err := stager.ResolverPackagePage(ctx, stager.Snapshot(), extractor.ResolverPackagePageRequest{
			ProjectID:   "project:fixture",
			Language:    "typescript",
			PackagePath: "src",
			Limit:       3,
		})
		if err != nil {
			return storage.PublishRequest{}, err
		}
		for _, target := range targets {
			got = append(got, target.SourcePath)
		}
		return storage.PublishRequest{}, errors.New("roll back staged package page")
	})
	if err == nil {
		t.Fatal("publish staged package page succeeded")
	}
	if !reflect.DeepEqual(got, []string{"src/a.ts", "src/b.ts"}) {
		t.Errorf("staged package target paths = %q, want src/a.ts then src/b.ts", got)
	}
}

func TestResolverProjectionPageStopsOnCancellation(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	contribution, err := typescript.Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "src/main.ts",
		Contents:   []byte("export const value = 1;"),
	})
	if err != nil {
		t.Fatalf("extract source: %v", err)
	}
	update, err := extractor.NewGraphUpdate([]extractor.Contribution{contribution})
	if err != nil {
		t.Fatalf("create graph update: %v", err)
	}
	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{Workspace: "workspace", Update: update})
	if err != nil {
		t.Fatalf("publish source: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = store.ResolverProjectionPage(ctx, snapshot, storage.ResolverProjectionPageRequest{
		ProjectID: "project:fixture",
		Language:  "typescript",
		Limit:     1,
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("read cancelled projection page error = %v, want context cancellation", err)
	}
}

func TestResolverTargetReadsSnapshotScopedFileDeclarationsAndSurfaces(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	workspace := "workspace"
	contribution, err := typescript.Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "src/main.ts",
		Contents:   []byte("export function main() {}"),
	})
	if err != nil {
		t.Fatalf("extract source: %v", err)
	}
	update, err := extractor.NewGraphUpdate([]extractor.Contribution{contribution})
	if err != nil {
		t.Fatalf("create graph update: %v", err)
	}
	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{Workspace: workspace, Update: update})
	if err != nil {
		t.Fatalf("publish source: %v", err)
	}

	target, found, err := store.ResolverTarget(context.Background(), snapshot, extractor.ResolverTargetRequest{
		ProjectID:  "project:fixture",
		Language:   "typescript",
		SourcePath: "src/main.ts",
	})
	if err != nil {
		t.Fatalf("read resolver target: %v", err)
	}
	if !found {
		t.Fatal("resolver target was not found")
	}
	if target.ProjectID != "project:fixture" || target.SourcePath != "src/main.ts" || target.Metadata.Name != "typescript" {
		t.Errorf("resolver target identity = %+v, want project:fixture src/main.ts typescript", target)
	}
	if !hasNodeKind(target.Nodes, "file") || !hasNodeKind(target.Nodes, typescript.FunctionNodeKind) {
		t.Errorf("resolver target nodes = %+v, want file and function declarations", target.Nodes)
	}
	if len(target.ExportedSurfaces) != 1 || target.ExportedSurfaces[0].Name != "main" {
		t.Errorf("resolver target surfaces = %+v, want main", target.ExportedSurfaces)
	}
	if len(target.UnresolvedReferences) != 0 || len(target.SymbolReferences) != 0 || len(target.Diagnostics) != 0 {
		t.Errorf("resolver target metadata = %+v, %+v, %+v, want no resolver references or diagnostics", target.UnresolvedReferences, target.SymbolReferences, target.Diagnostics)
	}
}

func TestResolverPackagePageReadsOrderedGoTargets(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := "workspace"
	for _, sourcePath := range []string{"service/c.go", "service/a.go", "other/ignored.go", "service/b.go"} {
		contribution, err := goextractor.Extract(extractor.Source{
			ProjectID:  "project:fixture",
			SourcePath: sourcePath,
			Contents:   []byte("package service\n\nfunc Run() {}\n"),
		})
		if err != nil {
			t.Fatalf("extract %q: %v", sourcePath, err)
		}
		update, err := extractor.NewGraphUpdate([]extractor.Contribution{contribution})
		if err != nil {
			t.Fatalf("create update for %q: %v", sourcePath, err)
		}
		if _, err := store.Publish(context.Background(), storage.PublishRequest{Workspace: workspace, Update: update}); err != nil {
			t.Fatalf("publish %q: %v", sourcePath, err)
		}
	}
	snapshot, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: workspace})
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	page, err := store.ResolverPackagePage(context.Background(), snapshot, extractor.ResolverPackagePageRequest{
		ProjectID:   "project:fixture",
		Language:    "go",
		PackagePath: "service",
		Limit:       2,
	})
	if err != nil {
		t.Fatalf("read Go package page: %v", err)
	}
	if got := resolverTargetPaths(page); !reflect.DeepEqual(got, []string{"service/a.go", "service/b.go"}) {
		t.Errorf("Go package page paths = %q, want service/a.go then service/b.go", got)
	}
}

func resolverTargetPaths(targets []extractor.ResolverTarget) []string {
	paths := make([]string, 0, len(targets))
	for _, target := range targets {
		paths = append(paths, target.SourcePath)
	}
	return paths
}

func hasNodeKind(nodes []graph.Node, kind graph.NodeKind) bool {
	for _, node := range nodes {
		if node.Kind == kind {
			return true
		}
	}
	return false
}

func projectionPaths(projections []storage.ResolverProjection) []string {
	paths := make([]string, 0, len(projections))
	for _, projection := range projections {
		paths = append(paths, projection.SourcePath)
	}
	return paths
}

func TestResolverProjectionCacheReturnsDefensiveCopies(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    resolverProjectionUpdate(t, "project:fixture", "src/main.ts", "function:main", "main"),
	})
	if err != nil {
		t.Fatalf("publish resolver projection: %v", err)
	}

	first, err := store.ResolverProjections(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("read resolver projections: %v", err)
	}
	first[0].Metadata.Extensions[0] = ".mutated"
	first[0].ExportedSurfaces[0].Name = "mutated"

	second, err := store.ResolverProjections(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("read cached resolver projections: %v", err)
	}
	if second[0].Metadata.Extensions[0] != ".ts" || second[0].ExportedSurfaces[0].Name != "main" {
		t.Errorf("cached resolver projection changed through caller mutation: %+v", second[0])
	}
}

func TestResolverProjectionCacheServesPublishedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	store, err := sqlite.OpenWithOptions(context.Background(), path, sqlite.Options{MaxResolverProjectionCacheBytes: 1 << 20})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    resolverProjectionUpdate(t, "project:fixture", "src/main.ts", "function:main", "main"),
	})
	if err != nil {
		t.Fatalf("publish resolver projection: %v", err)
	}
	if _, err := store.ResolverProjections(context.Background(), snapshot); err != nil {
		t.Fatalf("read resolver projections: %v", err)
	}

	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open database for cache check: %v", err)
	}
	if _, err := database.Exec("DELETE FROM file_contributions WHERE workspace = ?", "workspace"); err != nil {
		_ = database.Close()
		t.Fatalf("remove backing projection row: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close database for cache check: %v", err)
	}

	projections, err := store.ResolverProjections(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("read cached resolver projections: %v", err)
	}
	if len(projections) != 1 || projections[0].ExportedSurfaces[0].Name != "main" {
		t.Errorf("cached resolver projections = %+v, want main projection", projections)
	}
}

func TestResolverProjectionCacheDropsRolledBackAndPrunedVersions(t *testing.T) {
	store, err := sqlite.OpenWithOptions(context.Background(), filepath.Join(t.TempDir(), "graph.db"), sqlite.Options{MaxResolverProjectionCacheBytes: 1 << 20})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	first, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    resolverProjectionUpdate(t, "project:fixture", "src/main.ts", "function:main", "main"),
	})
	if err != nil {
		t.Fatalf("publish first resolver projection: %v", err)
	}
	second, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    resolverProjectionUpdate(t, "project:fixture", "src/main.ts", "function:replacement", "replacement"),
	})
	if err != nil {
		t.Fatalf("publish replacement resolver projection: %v", err)
	}
	if _, err := store.ResolverProjections(context.Background(), second); err != nil {
		t.Fatalf("cache replacement projection: %v", err)
	}

	if _, err := store.Rollback(context.Background(), storage.RollbackRequest{Workspace: "workspace", Version: first.Version}); err != nil {
		t.Fatalf("roll back resolver projection: %v", err)
	}
	projections, err := store.ResolverProjections(context.Background(), second)
	if err != nil {
		t.Fatalf("read removed rollback version: %v", err)
	}
	if len(projections) != 1 || projections[0].ExportedSurfaces[0].Name != "main" {
		t.Errorf("rolled back projection cache = %+v, want restored main projection", projections)
	}

	second, err = store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    resolverProjectionUpdate(t, "project:fixture", "src/main.ts", "function:replacement", "replacement"),
	})
	if err != nil {
		t.Fatalf("republish replacement resolver projection: %v", err)
	}
	if _, err := store.ResolverProjections(context.Background(), first); err != nil {
		t.Fatalf("cache pruned projection: %v", err)
	}
	if _, err := store.Prune(context.Background(), storage.PruneRequest{Workspace: "workspace", BeforeVersion: second.Version}); err != nil {
		t.Fatalf("prune resolver projection: %v", err)
	}
	projections, err = store.ResolverProjections(context.Background(), first)
	if err != nil {
		t.Fatalf("read removed pruned version: %v", err)
	}
	if len(projections) != 0 {
		t.Errorf("pruned projection cache = %+v, want no projections", projections)
	}
}

func TestResolverProjectionCacheEvictsOldestSnapshotAtBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	store, err := sqlite.OpenWithOptions(context.Background(), path, sqlite.Options{MaxResolverProjectionCacheBytes: 256})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	first, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    resolverProjectionUpdate(t, "project:fixture", "src/main.ts", "function:main", "main"),
	})
	if err != nil {
		t.Fatalf("publish first resolver projection: %v", err)
	}
	second, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    resolverProjectionUpdate(t, "project:fixture", "src/main.ts", "function:replacement", "replacement"),
	})
	if err != nil {
		t.Fatalf("publish second resolver projection: %v", err)
	}
	if _, err := store.ResolverProjections(context.Background(), first); err != nil {
		t.Fatalf("cache first resolver projection: %v", err)
	}
	if _, err := store.ResolverProjections(context.Background(), second); err != nil {
		t.Fatalf("cache second resolver projection: %v", err)
	}

	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open database for eviction check: %v", err)
	}
	if _, err := database.Exec("DELETE FROM file_contributions WHERE workspace = ?", "workspace"); err != nil {
		_ = database.Close()
		t.Fatalf("remove backing projection rows: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close database for eviction check: %v", err)
	}

	projections, err := store.ResolverProjections(context.Background(), first)
	if err != nil {
		t.Fatalf("read evicted first projection: %v", err)
	}
	if len(projections) != 0 {
		t.Errorf("evicted first projection = %+v, want none", projections)
	}
	projections, err = store.ResolverProjections(context.Background(), second)
	if err != nil {
		t.Fatalf("read retained second projection: %v", err)
	}
	if len(projections) != 1 || projections[0].ExportedSurfaces[0].Name != "replacement" {
		t.Errorf("retained second projection = %+v, want replacement projection", projections)
	}
}

func TestResolverProjectionsFollowReplacementRollbackAndPruning(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	first, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    resolverProjectionUpdate(t, "project:fixture", "src/main.ts", "function:main", "main"),
	})
	if err != nil {
		t.Fatalf("publish first projection: %v", err)
	}
	second, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    resolverProjectionUpdate(t, "project:fixture", "src/main.ts", "function:replacement", "replacement"),
	})
	if err != nil {
		t.Fatalf("publish replacement projection: %v", err)
	}
	projections, err := store.ResolverProjections(context.Background(), second)
	if err != nil {
		t.Fatalf("read replacement projections: %v", err)
	}
	if len(projections) != 1 || projections[0].ExportedSurfaces[0].Name != "replacement" {
		t.Errorf("replacement projections = %+v, want replacement surface", projections)
	}

	rolledBack, err := store.Rollback(context.Background(), storage.RollbackRequest{Workspace: "workspace", Version: first.Version})
	if err != nil {
		t.Fatalf("roll back projection: %v", err)
	}
	projections, err = store.ResolverProjections(context.Background(), rolledBack)
	if err != nil {
		t.Fatalf("read rolled back projections: %v", err)
	}
	if len(projections) != 1 || projections[0].ExportedSurfaces[0].Name != "main" {
		t.Errorf("rolled back projections = %+v, want main surface", projections)
	}
	projections, err = store.ResolverProjections(context.Background(), second)
	if err != nil {
		t.Fatalf("read rolled back cached projection: %v", err)
	}
	if len(projections) != 1 || projections[0].ExportedSurfaces[0].Name != "main" {
		t.Errorf("rolled back cached projections = %+v, want main surface", projections)
	}

	for versionIndex := 2; versionIndex <= 3; versionIndex++ {
		if _, err := store.Publish(context.Background(), storage.PublishRequest{
			Workspace: "workspace",
			Update:    resolverProjectionUpdate(t, "project:fixture", "src/main.ts", fmt.Sprintf("function:main-%d", versionIndex), "main"),
		}); err != nil {
			t.Fatalf("publish projection version %d: %v", versionIndex, err)
		}
	}
	if _, err := store.Prune(context.Background(), storage.PruneRequest{Workspace: "workspace", BeforeVersion: 2}); err != nil {
		t.Fatalf("prune projections: %v", err)
	}
	projections, err = store.ResolverProjections(context.Background(), first)
	if err != nil {
		t.Fatalf("read pruned cached projection: %v", err)
	}
	if len(projections) != 0 {
		t.Errorf("pruned cached projections = %+v, want none", projections)
	}
	retainedVersion := storage.GraphVersion(2)
	retained, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: "workspace", Version: &retainedVersion})
	if err != nil {
		t.Fatalf("open retained projection snapshot: %v", err)
	}
	projections, err = store.ResolverProjections(context.Background(), retained)
	if err != nil {
		t.Fatalf("read retained projections: %v", err)
	}
	if len(projections) != 1 || projections[0].ProjectID != "project:fixture" || projections[0].ExportedSurfaces[0].Name != "main" {
		t.Errorf("retained projections = %+v, want active projection", projections)
	}
}

func TestSourceContributionsReturnDefensiveCopiesAndReopenFromSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdateWithDependency(t, "src/main.ts", "function:main", "src/helper.ts", []string{"main"}),
	})
	if err != nil {
		t.Fatalf("publish cached contribution: %v", err)
	}

	first, err := store.SourceContributions(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("read cached source contributions: %v", err)
	}
	first[0].Facts.Nodes[0].ID = "mutated"
	first[0].Metadata.Extensions[0] = ".mutated"
	first[0].ExportedSurfaces[0].Name = "mutated"
	first[0].Dependencies[0].TargetPath = "mutated"
	second, err := store.SourceContributions(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("read cached source contributions again: %v", err)
	}
	if second[0].Facts.Nodes[0].ID != "function:main" || second[0].Metadata.Extensions[0] != ".ts" || second[0].ExportedSurfaces[0].Name != "main" || second[0].Dependencies[0].TargetPath != "src/helper.ts" {
		t.Errorf("cached contribution changed through caller mutation: %+v", second[0])
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close cached database: %v", err)
	}
	reopened, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restored, err := reopened.SourceContributions(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("restore source contributions from SQLite: %v", err)
	}
	if len(restored) != 1 || restored[0].Facts.Nodes[0].ID != "function:main" || restored[0].ExportedSurfaces[0].Name != "main" {
		t.Errorf("restored contribution = %+v, want published contribution", restored)
	}
}

func TestOpenSnapshotReturnsLatestPublishedVersion(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	published, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdate(t, "src/main.ts", "function:main"),
	})
	if err != nil {
		t.Fatalf("publish workspace snapshot: %v", err)
	}

	snapshot, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: "workspace"})
	if err != nil {
		t.Fatalf("open latest snapshot: %v", err)
	}
	if snapshot != published {
		t.Errorf("latest snapshot = %+v, want %+v", snapshot, published)
	}
}

func TestLookupNodesUsesFactsActiveInSpecifiedSnapshot(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	first, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateForSources(t,
			sourceFact{path: "src/main.ts", nodeID: "function:main"},
			sourceFact{path: "src/helper.ts", nodeID: "function:helper"},
		),
	})
	if err != nil {
		t.Fatalf("publish first snapshot: %v", err)
	}
	if _, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdate(t, "src/main.ts", "function:replacement"),
	}); err != nil {
		t.Fatalf("publish replacement snapshot: %v", err)
	}

	matches, err := store.LookupNodes(context.Background(), first, storage.NodeLookupRequest{
		Text:  "function:",
		Limit: 1,
	})
	if err != nil {
		t.Fatalf("look up nodes: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("node match count = %d, want 1", len(matches))
	}
	if matches[0].Node.ID != "function:helper" {
		t.Errorf("matched node ID = %q, want function:helper", matches[0].Node.ID)
	}
}

func TestLookupNodesRanksMatchesWithinProjectScope(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateWithFacts(t, "apps/app/src/main.ts", graph.Facts{
			Nodes: []graph.Node{
				{ID: "project:app", Kind: "project", Label: "app", QualifiedName: "apps/app", Evidence: evidence("apps/app/package.json")},
				{ID: "file:app", Kind: "file", Label: "main.ts", QualifiedName: "apps/app/src/main.ts", Evidence: evidence("apps/app/src/main.ts")},
				{ID: "function:exact", Kind: "function", Label: "target", QualifiedName: "apps/app/src/main.ts::target", Evidence: evidence("apps/app/src/main.ts")},
				{ID: "project:library", Kind: "project", Label: "library", QualifiedName: "packages/library", Evidence: evidence("packages/library/package.json")},
				{ID: "file:library", Kind: "file", Label: "helper.ts", QualifiedName: "packages/library/src/helper.ts", Evidence: evidence("packages/library/src/helper.ts")},
				{ID: "function:external", Kind: "function", Label: "target", QualifiedName: "packages/library/src/helper.ts::target", Evidence: evidence("packages/library/src/helper.ts")},
			},
			Edges: []graph.Edge{
				{SourceID: "project:app", TargetID: "file:app", Relation: "contains", Evidence: evidence("apps/app/package.json")},
				{SourceID: "file:app", TargetID: "function:exact", Relation: "contains", Evidence: evidence("apps/app/src/main.ts")},
				{SourceID: "project:library", TargetID: "file:library", Relation: "contains", Evidence: evidence("packages/library/package.json")},
				{SourceID: "file:library", TargetID: "function:external", Relation: "contains", Evidence: evidence("packages/library/src/helper.ts")},
			},
		}),
	})
	if err != nil {
		t.Fatalf("publish lookup fixture: %v", err)
	}

	matches, err := store.LookupNodes(context.Background(), snapshot, storage.NodeLookupRequest{
		Text:       "target",
		ProjectIDs: []string{"project:app"},
		Limit:      3,
	})
	if err != nil {
		t.Fatalf("look up scoped nodes: %v", err)
	}
	if len(matches) != 1 || matches[0].Node.ID != "function:exact" {
		t.Errorf("scoped matches = %+v, want function:exact", matches)
	}
}

func TestLookupExactNodesMatchesIDQualifiedNameAndFilePath(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	indexFile := graph.Node{ID: "file:index", Kind: "file", Label: "core-auth/src/index.ts", QualifiedName: "core-auth/src/index.ts", Evidence: evidence("core-auth/src/index.ts")}
	authInfo := graph.Node{ID: "function:auth-info", Kind: "function", Label: "AuthInfo", QualifiedName: "core-auth/src/auth-info.ts::AuthInfo", Evidence: evidence("core-auth/src/auth-info.ts")}
	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdateWithFacts(t, "core-auth/src/index.ts", graph.Facts{Nodes: []graph.Node{indexFile, authInfo}}),
	})
	if err != nil {
		t.Fatalf("publish exact lookup fixture: %v", err)
	}

	for _, test := range []struct {
		identifier string
		wantID     string
	}{
		{identifier: indexFile.ID, wantID: indexFile.ID},
		{identifier: authInfo.QualifiedName, wantID: authInfo.ID},
		{identifier: indexFile.Evidence.Span.Path, wantID: indexFile.ID},
	} {
		matches, err := store.LookupExactNodes(context.Background(), snapshot, test.identifier)
		if err != nil {
			t.Fatalf("look up exact node %q: %v", test.identifier, err)
		}
		if len(matches) != 1 || matches[0].Node.ID != test.wantID {
			t.Errorf("exact matches for %q = %+v, want %q", test.identifier, matches, test.wantID)
		}
	}
}

func TestSearchNodesUsesFTSAndReportsMatchedFields(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateWithFacts(t, "storage/sqlite/lookup.go", graph.Facts{Nodes: []graph.Node{
			{ID: "go:method:lookup", Kind: "function", Label: "LookupExactNodes", QualifiedName: "storage/sqlite.Store.LookupExactNodes", Evidence: evidence("storage/sqlite/lookup.go")},
			{ID: "go:type:other", Kind: "class", Label: "Lookup", QualifiedName: "query.Lookup", Evidence: evidence("query/lookup.go")},
		}}),
	})
	if err != nil {
		t.Fatalf("publish lexical fixture: %v", err)
	}

	matches, err := store.SearchNodes(context.Background(), snapshot, storage.LexicalSearchRequest{
		TokenGroups: [][]string{{"lookup", "exact", "nodes"}},
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("search nodes: %v", err)
	}
	if len(matches) == 0 || matches[0].Node.ID != "go:method:lookup" {
		t.Fatalf("lexical matches = %+v, want lookup method first", matches)
	}
	if !reflect.DeepEqual(matches[0].MatchedFields, []string{"label", "qualifiedName", "path", "identifierTokens"}) {
		t.Errorf("matched fields = %v, want label, qualifiedName, path, and identifierTokens", matches[0].MatchedFields)
	}
	if matches[0].Score <= 0 {
		t.Errorf("match score = %f, want positive relevance", matches[0].Score)
	}
}

func TestSearchNodesPrioritizesExactSymbolLabel(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateWithFacts(t, "query/query.go", graph.Facts{Nodes: []graph.Node{
			{ID: "go:function:query-snapshot", Kind: "function", Label: "QuerySnapshot", QualifiedName: "query.QuerySnapshot", Evidence: evidence("query/query.go")},
			{ID: "go:variable:snapshot", Kind: "function", Label: "snapshot", QualifiedName: "query.QuerySnapshot::snapshot", Evidence: evidence("query/query_test.go")},
		}}),
	})
	if err != nil {
		t.Fatalf("publish exact-label fixture: %v", err)
	}

	matches, err := store.SearchNodes(context.Background(), snapshot, storage.LexicalSearchRequest{
		Text:        "QuerySnapshot",
		TokenGroups: [][]string{{"query", "snapshot"}},
		Limit:       2,
	})
	if err != nil {
		t.Fatalf("search exact-label fixture: %v", err)
	}
	if len(matches) != 2 || matches[0].Node.ID != "go:function:query-snapshot" {
		t.Fatalf("exact-label matches = %+v, want QuerySnapshot first", matches)
	}

	aliasMatches, err := store.SearchNodes(context.Background(), snapshot, storage.LexicalSearchRequest{
		Text:        "query_snapshot",
		TokenGroups: [][]string{{"query", "snapshot"}},
		Limit:       2,
	})
	if err != nil {
		t.Fatalf("search normalized-label fixture: %v", err)
	}
	if len(aliasMatches) != 2 || aliasMatches[0].Node.ID != "go:function:query-snapshot" {
		t.Fatalf("normalized-label matches = %+v, want QuerySnapshot first", aliasMatches)
	}
}

func TestSearchNodesAppliesWeightedBM25Ranking(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateWithFacts(t, "src/search.ts", graph.Facts{Nodes: []graph.Node{
			{ID: "function:label", Kind: "function", Label: "Needle", QualifiedName: "search.LabelMatch", Evidence: evidence("src/search.ts")},
			{ID: "function:path", Kind: "function", Label: "Other", QualifiedName: "search.PathMatch", Evidence: evidence("src/needle.ts")},
		}}),
	})
	if err != nil {
		t.Fatalf("publish ranking fixture: %v", err)
	}
	matches, err := store.SearchNodes(context.Background(), snapshot, storage.LexicalSearchRequest{Phrases: []string{"needle"}, Limit: 10})
	if err != nil {
		t.Fatalf("search weighted fields: %v", err)
	}
	if len(matches) != 2 || matches[0].Node.ID != "function:label" || matches[0].Score <= matches[1].Score {
		t.Errorf("weighted matches = %+v, want label match above path match", matches)
	}
}

func TestSearchNodesTreatsRawTextAsDataAndTracksSnapshots(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	first, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateWithFacts(t, "src/main.go", graph.Facts{Nodes: []graph.Node{
			{ID: "go:function:old", Kind: "function", Label: "OldHandler", QualifiedName: "src.OldHandler", Evidence: evidence("src/main.go")},
		}}),
	})
	if err != nil {
		t.Fatalf("publish first lexical snapshot: %v", err)
	}
	second, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateWithFacts(t, "src/main.go", graph.Facts{Nodes: []graph.Node{
			{ID: "go:function:new", Kind: "function", Label: "NewHandler", QualifiedName: "src.NewHandler", Evidence: evidence("src/main.go")},
		}}),
	})
	if err != nil {
		t.Fatalf("publish replacement lexical snapshot: %v", err)
	}

	oldMatches, err := store.SearchNodes(context.Background(), first, storage.LexicalSearchRequest{Text: `old OR "unterminated`, Limit: 10})
	if err != nil {
		t.Fatalf("search first snapshot with syntax characters: %v", err)
	}
	if len(oldMatches) != 1 || oldMatches[0].Node.ID != "go:function:old" {
		t.Errorf("first snapshot matches = %+v, want old handler", oldMatches)
	}
	newMatches, err := store.SearchNodes(context.Background(), second, storage.LexicalSearchRequest{Text: "old", Limit: 10})
	if err != nil {
		t.Fatalf("search replacement snapshot: %v", err)
	}
	if len(newMatches) != 0 {
		t.Errorf("replacement snapshot matches = %+v, want no stale old handler", newMatches)
	}
}

func TestSearchNodesHonorsProjectAndKindFilters(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateWithFacts(t, "apps/app/src/main.ts", graph.Facts{
			Nodes: []graph.Node{
				{ID: "project:app", Kind: "project", Label: "app", QualifiedName: "apps/app", Evidence: evidence("apps/app/package.json")},
				{ID: "file:app", Kind: "file", Label: "main.ts", QualifiedName: "apps/app/src/main.ts", Evidence: evidence("apps/app/src/main.ts")},
				{ID: "function:app-target", Kind: "function", Label: "SharedTarget", QualifiedName: "apps/app.SharedTarget", Evidence: evidence("apps/app/src/main.ts")},
				{ID: "project:library", Kind: "project", Label: "library", QualifiedName: "packages/library", Evidence: evidence("packages/library/package.json")},
				{ID: "file:library", Kind: "file", Label: "main.ts", QualifiedName: "packages/library/src/main.ts", Evidence: evidence("packages/library/src/main.ts")},
				{ID: "function:library-target", Kind: "function", Label: "SharedTarget", QualifiedName: "packages/library.SharedTarget", Evidence: evidence("packages/library/src/main.ts")},
			},
			Edges: []graph.Edge{
				{SourceID: "project:app", TargetID: "file:app", Relation: "contains", Evidence: evidence("apps/app/package.json")},
				{SourceID: "file:app", TargetID: "function:app-target", Relation: "contains", Evidence: evidence("apps/app/src/main.ts")},
				{SourceID: "project:library", TargetID: "file:library", Relation: "contains", Evidence: evidence("packages/library/package.json")},
				{SourceID: "file:library", TargetID: "function:library-target", Relation: "contains", Evidence: evidence("packages/library/src/main.ts")},
			},
		}),
	})
	if err != nil {
		t.Fatalf("publish filtered lexical fixture: %v", err)
	}

	matches, err := store.SearchNodes(context.Background(), snapshot, storage.LexicalSearchRequest{
		Text:       "shared target",
		Kinds:      []graph.NodeKind{"function"},
		ProjectIDs: []string{"project:app"},
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("search filtered nodes: %v", err)
	}
	if len(matches) != 1 || matches[0].Node.ID != "function:app-target" {
		t.Errorf("filtered lexical matches = %+v, want app target", matches)
	}
}

func TestLexicalRowsFollowRebuildRollbackAndPrune(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	first, err := store.Publish(context.Background(), storage.PublishRequest{Workspace: "workspace", Update: graphUpdate(t, "src/first.ts", "function:first")})
	if err != nil {
		t.Fatalf("publish first snapshot: %v", err)
	}
	second, err := store.Publish(context.Background(), storage.PublishRequest{Workspace: "workspace", Update: graphUpdate(t, "src/second.ts", "function:second")})
	if err != nil {
		t.Fatalf("publish second snapshot: %v", err)
	}
	if err := store.RebuildLexicalIndex(context.Background(), "workspace"); err != nil {
		t.Fatalf("rebuild lexical index: %v", err)
	}
	if matches, err := store.SearchNodes(context.Background(), second, storage.LexicalSearchRequest{Text: "second", Limit: 10}); err != nil || len(matches) != 1 {
		t.Fatalf("rebuilt lexical matches = %+v, error %v, want second", matches, err)
	}

	if _, err := store.Rollback(context.Background(), storage.RollbackRequest{Workspace: "workspace", Version: first.Version}); err != nil {
		t.Fatalf("roll back lexical snapshot: %v", err)
	}
	if matches, err := store.SearchNodes(context.Background(), second, storage.LexicalSearchRequest{Text: "second", Limit: 10}); err != nil || len(matches) != 0 {
		t.Errorf("rolled back lexical matches = %+v, error %v, want none", matches, err)
	}

	retained, err := store.Publish(context.Background(), storage.PublishRequest{Workspace: "workspace", Update: graphUpdate(t, "src/retained.ts", "function:retained")})
	if err != nil {
		t.Fatalf("publish retained snapshot: %v", err)
	}
	if _, err := store.Prune(context.Background(), storage.PruneRequest{Workspace: "workspace", BeforeVersion: retained.Version}); err != nil {
		t.Fatalf("prune lexical snapshots: %v", err)
	}
	if matches, err := store.SearchNodes(context.Background(), first, storage.LexicalSearchRequest{Text: "first", Limit: 10}); err != nil || len(matches) != 0 {
		t.Errorf("pruned lexical matches = %+v, error %v, want none", matches, err)
	}
}

func TestTraverseFollowsOutgoingEdgesAtSpecifiedSnapshot(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateWithFacts(t, "src/main.ts", graph.Facts{
			Nodes: []graph.Node{
				graphNode("src/main.ts", "function:main"),
				graphNode("src/main.ts", "function:helper"),
			},
			Edges: []graph.Edge{{
				SourceID: "function:main",
				TargetID: "function:helper",
				Relation: "calls",
				Evidence: evidence("src/main.ts"),
			}},
		}),
	})
	if err != nil {
		t.Fatalf("publish snapshot: %v", err)
	}

	result, err := store.Traverse(context.Background(), snapshot, storage.TraversalRequest{
		StartNodeIDs: []string{"function:main"},
		Direction:    storage.TraverseOutgoing,
		MaxDepth:     1,
		MaxNodes:     2,
	})
	if err != nil {
		t.Fatalf("traverse graph: %v", err)
	}
	if len(result.Facts.Nodes) != 2 {
		t.Fatalf("traversed node count = %d, want 2", len(result.Facts.Nodes))
	}
	if result.Facts.Nodes[0].ID != "function:helper" || result.Facts.Nodes[1].ID != "function:main" {
		t.Errorf("traversed node IDs = %q, %q, want function:helper, function:main", result.Facts.Nodes[0].ID, result.Facts.Nodes[1].ID)
	}
	if len(result.Facts.Edges) != 1 || result.Facts.Edges[0].Relation != "calls" {
		t.Errorf("traversed edges = %+v, want one calls edge", result.Facts.Edges)
	}
}

func TestTraverseStopsAtExternalProjectBoundary(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateWithFacts(t, "src/main.ts", graph.Facts{
			Nodes: []graph.Node{
				{ID: "project:app", Kind: "project", Evidence: evidence("apps/app/package.json")},
				{ID: "file:app", Kind: "file", Evidence: evidence("apps/app/src/main.ts")},
				graphNode("apps/app/src/main.ts", "function:main"),
				{ID: "project:library", Kind: "project", Evidence: evidence("packages/library/package.json")},
				{ID: "file:library", Kind: "file", Evidence: evidence("packages/library/src/helper.ts")},
				graphNode("packages/library/src/helper.ts", "function:helper"),
				graphNode("packages/library/src/helper.ts", "function:internal"),
			},
			Edges: []graph.Edge{
				{SourceID: "project:app", TargetID: "file:app", Relation: "contains", Evidence: evidence("apps/app/package.json")},
				{SourceID: "file:app", TargetID: "function:main", Relation: "contains", Evidence: evidence("apps/app/src/main.ts")},
				{SourceID: "project:library", TargetID: "file:library", Relation: "contains", Evidence: evidence("packages/library/package.json")},
				{SourceID: "file:library", TargetID: "function:helper", Relation: "contains", Evidence: evidence("packages/library/src/helper.ts")},
				{SourceID: "file:library", TargetID: "function:internal", Relation: "contains", Evidence: evidence("packages/library/src/helper.ts")},
				{SourceID: "function:main", TargetID: "function:helper", Relation: "calls", Evidence: evidence("apps/app/src/main.ts")},
				{SourceID: "function:helper", TargetID: "function:internal", Relation: "calls", Evidence: evidence("packages/library/src/helper.ts")},
			},
		}),
	})
	if err != nil {
		t.Fatalf("publish snapshot: %v", err)
	}

	result, err := store.Traverse(context.Background(), snapshot, storage.TraversalRequest{
		StartNodeIDs: []string{"function:main"},
		ProjectIDs:   []string{"project:app"},
		Direction:    storage.TraverseOutgoing,
		MaxDepth:     3,
		MaxNodes:     10,
	})
	if err != nil {
		t.Fatalf("traverse scoped graph: %v", err)
	}
	if len(result.Facts.Nodes) != 2 || result.Facts.Nodes[0].ID != "function:helper" || result.Facts.Nodes[1].ID != "function:main" {
		t.Errorf("traversed nodes = %+v, want helper boundary and main", result.Facts.Nodes)
	}
	if len(result.Facts.Edges) != 1 || result.Facts.Edges[0].SourceID != "function:main" || result.Facts.Edges[0].TargetID != "function:helper" {
		t.Errorf("traversed edges = %+v, want main-to-helper boundary", result.Facts.Edges)
	}
	if result.ScopeBoundary == nil || result.ScopeBoundary.ID != "function:helper" {
		t.Errorf("scope boundary = %+v, want function:helper", result.ScopeBoundary)
	}
}

func TestTraverseRejectsStartNodeOutsideProjectScope(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateWithFacts(t, "src/main.ts", graph.Facts{
			Nodes: []graph.Node{
				{ID: "project:app", Kind: "project", Evidence: evidence("apps/app/package.json")},
				{ID: "project:library", Kind: "project", Evidence: evidence("packages/library/package.json")},
				{ID: "file:library", Kind: "file", Evidence: evidence("packages/library/src/helper.ts")},
				graphNode("packages/library/src/helper.ts", "function:helper"),
			},
			Edges: []graph.Edge{
				{SourceID: "project:library", TargetID: "file:library", Relation: "contains", Evidence: evidence("packages/library/package.json")},
				{SourceID: "file:library", TargetID: "function:helper", Relation: "contains", Evidence: evidence("packages/library/src/helper.ts")},
			},
		}),
	})
	if err != nil {
		t.Fatalf("publish snapshot: %v", err)
	}

	_, err = store.Traverse(context.Background(), snapshot, storage.TraversalRequest{
		StartNodeIDs: []string{"function:helper"},
		ProjectIDs:   []string{"project:app"},
		Direction:    storage.TraverseOutgoing,
		MaxDepth:     1,
		MaxNodes:     2,
	})
	if !errors.Is(err, storage.ErrInvalidRequest) {
		t.Errorf("traverse error = %v, want invalid request", err)
	}
}

func TestTraverseSelectsNeighborsDeterministicallyAtNodeLimit(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateWithFacts(t, "src/main.ts", graph.Facts{
			Nodes: []graph.Node{
				graphNode("src/main.ts", "function:main"),
				graphNode("src/alpha.ts", "function:alpha"),
				graphNode("src/zeta.ts", "function:zeta"),
			},
			Edges: []graph.Edge{
				{SourceID: "function:main", TargetID: "function:zeta", Relation: "calls", Evidence: evidence("src/main.ts")},
				{SourceID: "function:main", TargetID: "function:alpha", Relation: "calls", Evidence: evidence("src/main.ts")},
			},
		}),
	})
	if err != nil {
		t.Fatalf("publish snapshot: %v", err)
	}

	result, err := store.Traverse(context.Background(), snapshot, storage.TraversalRequest{
		StartNodeIDs: []string{"function:main"},
		Direction:    storage.TraverseOutgoing,
		MaxDepth:     1,
		MaxNodes:     2,
	})
	if err != nil {
		t.Fatalf("traverse graph: %v", err)
	}
	if len(result.Facts.Nodes) != 2 || result.Facts.Nodes[0].ID != "function:alpha" || result.Facts.Nodes[1].ID != "function:main" {
		t.Errorf("traversed nodes = %+v, want alpha and main", result.Facts.Nodes)
	}
	if len(result.Facts.Edges) != 1 || result.Facts.Edges[0].TargetID != "function:alpha" {
		t.Errorf("traversed edges = %+v, want main-to-alpha", result.Facts.Edges)
	}
	if len(result.TruncationReasons) != 1 || result.TruncationReasons[0] != storage.TruncatedByNodeLimit {
		t.Errorf("truncation reasons = %q, want node limit", result.TruncationReasons)
	}
}

func TestTraverseRestrictsFactsToExactRelationFilter(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateWithFacts(t, "src/main.ts", graph.Facts{
			Nodes: []graph.Node{
				graphNode("src/main.ts", "function:main"),
				graphNode("src/helper.ts", "function:helper"),
				graphNode("src/other.ts", "function:other"),
			},
			Edges: []graph.Edge{
				{SourceID: "function:main", TargetID: "function:helper", Relation: "calls", Evidence: evidence("src/main.ts")},
				{SourceID: "function:main", TargetID: "function:other", Relation: "references", Evidence: evidence("src/main.ts")},
			},
		}),
	})
	if err != nil {
		t.Fatalf("publish snapshot: %v", err)
	}

	result, err := store.Traverse(context.Background(), snapshot, storage.TraversalRequest{
		StartNodeIDs: []string{"function:main"},
		Direction:    storage.TraverseOutgoing,
		Relations:    []graph.RelationKind{"calls"},
		MaxDepth:     1,
		MaxNodes:     3,
	})
	if err != nil {
		t.Fatalf("traverse graph: %v", err)
	}
	if len(result.Facts.Nodes) != 2 || result.Facts.Nodes[0].ID != "function:helper" || result.Facts.Nodes[1].ID != "function:main" {
		t.Errorf("traversed nodes = %+v, want helper and main", result.Facts.Nodes)
	}
	if len(result.Facts.Edges) != 1 || result.Facts.Edges[0].Relation != "calls" {
		t.Errorf("traversed edges = %+v, want calls edge only", result.Facts.Edges)
	}
}

func TestTraverseFollowsIncomingEdges(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateWithFacts(t, "src/main.ts", graph.Facts{
			Nodes: []graph.Node{
				graphNode("src/main.ts", "function:main"),
				graphNode("src/helper.ts", "function:helper"),
			},
			Edges: []graph.Edge{{
				SourceID: "function:main",
				TargetID: "function:helper",
				Relation: "calls",
				Evidence: evidence("src/main.ts"),
			}},
		}),
	})
	if err != nil {
		t.Fatalf("publish snapshot: %v", err)
	}

	result, err := store.Traverse(context.Background(), snapshot, storage.TraversalRequest{
		StartNodeIDs: []string{"function:helper"},
		Direction:    storage.TraverseIncoming,
		MaxDepth:     1,
		MaxNodes:     2,
	})
	if err != nil {
		t.Fatalf("traverse graph: %v", err)
	}
	if len(result.Facts.Nodes) != 2 || result.Facts.Nodes[0].ID != "function:helper" || result.Facts.Nodes[1].ID != "function:main" {
		t.Errorf("traversed nodes = %+v, want helper and main", result.Facts.Nodes)
	}
	if len(result.Facts.Edges) != 1 || result.Facts.Edges[0].SourceID != "function:main" || result.Facts.Edges[0].TargetID != "function:helper" {
		t.Errorf("traversed edges = %+v, want main-to-helper", result.Facts.Edges)
	}
}

func TestTraverseReportsDepthLimitDeterministically(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateWithFacts(t, "src/main.ts", graph.Facts{
			Nodes: []graph.Node{
				graphNode("src/main.ts", "function:main"),
				graphNode("src/helper.ts", "function:helper"),
				graphNode("src/leaf.ts", "function:leaf"),
			},
			Edges: []graph.Edge{
				{SourceID: "function:main", TargetID: "function:helper", Relation: "calls", Evidence: evidence("src/main.ts")},
				{SourceID: "function:helper", TargetID: "function:leaf", Relation: "calls", Evidence: evidence("src/helper.ts")},
			},
		}),
	})
	if err != nil {
		t.Fatalf("publish snapshot: %v", err)
	}

	result, err := store.Traverse(context.Background(), snapshot, storage.TraversalRequest{
		StartNodeIDs: []string{"function:main"},
		Direction:    storage.TraverseOutgoing,
		MaxDepth:     1,
		MaxNodes:     3,
	})
	if err != nil {
		t.Fatalf("traverse graph: %v", err)
	}
	if len(result.Facts.Nodes) != 2 || result.Facts.Nodes[0].ID != "function:helper" || result.Facts.Nodes[1].ID != "function:main" {
		t.Errorf("traversed nodes = %+v, want helper and main", result.Facts.Nodes)
	}
	if len(result.TruncationReasons) != 1 || result.TruncationReasons[0] != storage.TruncatedByDepthLimit {
		t.Errorf("truncation reasons = %q, want depth limit", result.TruncationReasons)
	}
}

func TestExplainReturnsNodeAndIncidentFactsAtSpecifiedSnapshot(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateWithFacts(t, "src/main.ts", graph.Facts{
			Nodes: []graph.Node{
				graphNode("src/main.ts", "function:main"),
				graphNode("src/main.ts", "function:helper"),
			},
			Edges: []graph.Edge{{
				SourceID: "function:main",
				TargetID: "function:helper",
				Relation: "calls",
				Evidence: evidence("src/main.ts"),
			}},
		}),
	})
	if err != nil {
		t.Fatalf("publish snapshot: %v", err)
	}

	explanation, err := store.Explain(context.Background(), snapshot, storage.ExplainRequest{NodeID: "function:helper"})
	if err != nil {
		t.Fatalf("explain graph node: %v", err)
	}
	if explanation.Node.ID != "function:helper" || explanation.Node.Evidence.Extractor != "typescript@1" {
		t.Errorf("explained node = %+v, want function:helper with extractor evidence", explanation.Node)
	}
	if len(explanation.SupportingFacts.Nodes) != 2 || len(explanation.SupportingFacts.Edges) != 1 {
		t.Errorf("supporting facts = %+v, want two nodes and one edge", explanation.SupportingFacts)
	}
}

func TestExportStreamsFilteredFactsAtSpecifiedSnapshot(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateWithFacts(t, "src/main.ts", graph.Facts{
			Nodes: []graph.Node{
				graphNode("src/main.ts", "function:main"),
				graphNode("src/main.ts", "function:helper"),
				{ID: "class:ignored", Kind: "class", Evidence: evidence("src/main.ts")},
			},
			Edges: []graph.Edge{{
				SourceID: "function:main",
				TargetID: "function:helper",
				Relation: "calls",
				Evidence: evidence("src/main.ts"),
			}},
		}),
	})
	if err != nil {
		t.Fatalf("publish snapshot: %v", err)
	}

	var sink factCollector
	err = store.Export(context.Background(), snapshot, storage.ExportRequest{
		NodeKinds: []graph.NodeKind{"function"},
		Relations: []graph.RelationKind{"calls"},
	}, &sink)
	if err != nil {
		t.Fatalf("export graph: %v", err)
	}
	if len(sink.nodes) != 2 || sink.nodes[0].ID != "function:helper" || sink.nodes[1].ID != "function:main" {
		t.Errorf("exported nodes = %+v, want function nodes in ID order", sink.nodes)
	}
	if len(sink.edges) != 1 || sink.edges[0].Relation != "calls" {
		t.Errorf("exported edges = %+v, want one calls edge", sink.edges)
	}
}

func TestRollbackMakesEarlierSnapshotCurrent(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	first, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdate(t, "src/main.ts", "function:main"),
	})
	if err != nil {
		t.Fatalf("publish first snapshot: %v", err)
	}
	if _, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdate(t, "src/main.ts", "function:main-v2"),
	}); err != nil {
		t.Fatalf("publish replacement snapshot: %v", err)
	}

	rolledBack, err := store.Rollback(context.Background(), storage.RollbackRequest{
		Workspace: "workspace",
		Version:   first.Version,
	})
	if err != nil {
		t.Fatalf("roll back graph version: %v", err)
	}
	if rolledBack != first {
		t.Errorf("rollback snapshot = %+v, want %+v", rolledBack, first)
	}

	current, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: "workspace"})
	if err != nil {
		t.Fatalf("open current snapshot after rollback: %v", err)
	}
	if current != first {
		t.Errorf("current snapshot after rollback = %+v, want %+v", current, first)
	}
}

func TestPruneRemovesVersionsBeforeRetentionBoundary(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	var latest storage.Snapshot
	for versionIndex := 1; versionIndex <= 22; versionIndex++ {
		latest, err = store.Publish(context.Background(), storage.PublishRequest{
			Workspace: "workspace",
			Update:    graphUpdate(t, "src/main.ts", "function:main"),
		})
		if err != nil {
			t.Fatalf("publish snapshot %d: %v", versionIndex, err)
		}
	}

	result, err := store.Prune(context.Background(), storage.PruneRequest{
		Workspace:     "workspace",
		BeforeVersion: 2,
	})
	if err != nil {
		t.Fatalf("prune graph versions: %v", err)
	}
	if result.PrunedVersions != 1 {
		t.Errorf("pruned versions = %d, want 1", result.PrunedVersions)
	}

	prunedVersion := storage.GraphVersion(1)
	_, err = store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{
		Workspace: "workspace",
		Version:   &prunedVersion,
	})
	if !errors.Is(err, storage.ErrGraphVersionPruned) {
		t.Errorf("open pruned snapshot error = %v, want %v", err, storage.ErrGraphVersionPruned)
	}

	current, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: "workspace"})
	if err != nil {
		t.Fatalf("open current snapshot after prune: %v", err)
	}
	if current != latest {
		t.Errorf("current snapshot after prune = %+v, want %+v", current, latest)
	}
}

func TestPublishRetainsTheNewestTwentyFiveGraphVersions(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	var latest storage.Snapshot
	for versionIndex := 1; versionIndex <= 26; versionIndex++ {
		latest, err = store.Publish(context.Background(), storage.PublishRequest{
			Workspace: "workspace",
			Update:    graphUpdate(t, "src/main.ts", fmt.Sprintf("function:main-%d", versionIndex)),
		})
		if err != nil {
			t.Fatalf("publish snapshot %d: %v", versionIndex, err)
		}
	}

	firstVersion := storage.GraphVersion(1)
	_, err = store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{
		Workspace: "workspace",
		Version:   &firstVersion,
	})
	if !errors.Is(err, storage.ErrGraphVersionPruned) {
		t.Errorf("open first retained snapshot error = %v, want %v", err, storage.ErrGraphVersionPruned)
	}

	current, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: "workspace"})
	if err != nil {
		t.Fatalf("open current snapshot after retention: %v", err)
	}
	if current != latest {
		t.Errorf("current snapshot after retention = %+v, want %+v", current, latest)
	}
}

func TestPublishPreservesPriorSnapshotWhenTheNewVersionExceedsTheDatabaseBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	first, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdate(t, "src/main.ts", "function:main"),
	})
	if err != nil {
		t.Fatalf("publish first snapshot: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close database after first snapshot: %v", err)
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	store, err = sqlite.OpenWithOptions(context.Background(), path, sqlite.Options{MaxDatabaseBytes: fileInfo.Size()})
	if err != nil {
		t.Fatalf("reopen database with budget: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	_, err = store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateWithFacts(t, "src/main.ts", graph.Facts{Nodes: []graph.Node{{
			ID:            "function:replacement",
			Kind:          "function",
			Label:         strings.Repeat("replacement", 10_000),
			QualifiedName: "src/main.ts::replacement",
			Evidence:      evidence("src/main.ts"),
		}}}),
	})
	if err == nil {
		t.Fatal("publish version over the database budget succeeded")
	}

	current, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: "workspace"})
	if err != nil {
		t.Fatalf("open current snapshot after rejected publication: %v", err)
	}
	if current != first {
		t.Errorf("current snapshot after rejected publication = %+v, want %+v", current, first)
	}
}

func TestPruneRebasesUnchangedContributionAtRetentionBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	if _, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateForSources(t,
			sourceFact{path: "src/main.ts", nodeID: "function:main"},
			sourceFact{path: "src/util.ts", nodeID: "function:util"},
		),
	}); err != nil {
		t.Fatalf("publish first snapshot: %v", err)
	}
	if _, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdate(t, "src/main.ts", "function:main-v2"),
	}); err != nil {
		t.Fatalf("publish second snapshot: %v", err)
	}

	if _, err := store.Prune(context.Background(), storage.PruneRequest{
		Workspace:     "workspace",
		BeforeVersion: 2,
	}); err != nil {
		t.Fatalf("prune graph versions: %v", err)
	}

	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open database for verification: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close verification database: %v", err)
		}
	})
	var validFrom int
	var validTo sql.NullInt64
	if err := database.QueryRow(`
		SELECT valid_from_version, valid_to_version
		FROM file_contributions
		WHERE workspace = ? AND source_path = ?`, "workspace", "src/util.ts").Scan(&validFrom, &validTo); err != nil {
		t.Fatalf("read rebased utility contribution: %v", err)
	}
	if validFrom != 2 || validTo.Valid {
		t.Errorf("rebased utility contribution = {validFrom: %d, validTo: %+v}, want active from version 2", validFrom, validTo)
	}
}

func TestPublishReplacesChangedSourceAndSharesUnchangedSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	first, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateForSources(t,
			sourceFact{path: "src/main.ts", nodeID: "function:main"},
			sourceFact{path: "src/util.ts", nodeID: "function:util"},
		),
	})
	if err != nil {
		t.Fatalf("publish first snapshot: %v", err)
	}
	second, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdate(t, "src/main.ts", "function:main-v2"),
	})
	if err != nil {
		t.Fatalf("publish replacement snapshot: %v", err)
	}
	if second.Version != first.Version+1 {
		t.Errorf("second snapshot version = %d, want %d", second.Version, first.Version+1)
	}

	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open database for verification: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close verification database: %v", err)
		}
	})
	rows, err := database.Query(`
		SELECT source_path, valid_from_version, valid_to_version
		FROM file_contributions
		WHERE workspace = ?
		ORDER BY source_path, valid_from_version`, "workspace")
	if err != nil {
		t.Fatalf("read stored contributions: %v", err)
	}
	defer rows.Close()

	type contributionVersion struct {
		sourcePath string
		validFrom  int
		validTo    sql.NullInt64
	}
	var contributions []contributionVersion
	for rows.Next() {
		var contribution contributionVersion
		if err := rows.Scan(&contribution.sourcePath, &contribution.validFrom, &contribution.validTo); err != nil {
			t.Fatalf("scan stored contribution: %v", err)
		}
		contributions = append(contributions, contribution)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate stored contributions: %v", err)
	}
	if len(contributions) != 3 {
		t.Fatalf("stored contribution count = %d, want 3", len(contributions))
	}
	if got := contributions[0]; got.sourcePath != "src/main.ts" || got.validFrom != 1 || !got.validTo.Valid || got.validTo.Int64 != 1 {
		t.Errorf("closed main contribution = %+v, want source src/main.ts at version 1", got)
	}
	if got := contributions[1]; got.sourcePath != "src/main.ts" || got.validFrom != 2 || got.validTo.Valid {
		t.Errorf("replacement main contribution = %+v, want active source src/main.ts at version 2", got)
	}
	if got := contributions[2]; got.sourcePath != "src/util.ts" || got.validFrom != 1 || got.validTo.Valid {
		t.Errorf("shared utility contribution = %+v, want active source src/util.ts from version 1", got)
	}
}

func TestAffectedSourcesIncludesDependentsWhenExportedSurfaceChanges(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	if _, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdateWithDependency(t, "src/main.ts", "function:main", "src/support.ts", nil),
	}); err != nil {
		t.Fatalf("publish dependent source: %v", err)
	}
	second, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdateWithDependency(t, "src/support.ts", "function:support", "", []string{"support"}),
	})
	if err != nil {
		t.Fatalf("publish exported source: %v", err)
	}

	affected, err := store.AffectedSources(context.Background(), second, storage.AffectedSourcesRequest{
		Update: graphUpdateWithDependency(t, "src/support.ts", "function:support-v2", "", []string{"replacement"}),
	})
	if err != nil {
		t.Fatalf("find affected sources: %v", err)
	}
	if len(affected) != 1 || affected[0] != "src/main.ts" {
		t.Errorf("affected sources = %q, want src/main.ts", affected)
	}
}

func TestAffectedSourcesIgnoresImplementationOnlyChanges(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	if _, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdateWithDependency(t, "src/main.ts", "function:main", "src/support.ts", nil),
	}); err != nil {
		t.Fatalf("publish dependent source: %v", err)
	}
	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdateWithDependency(t, "src/support.ts", "function:support", "", []string{"support"}),
	})
	if err != nil {
		t.Fatalf("publish exported source: %v", err)
	}

	affected, err := store.AffectedSources(context.Background(), snapshot, storage.AffectedSourcesRequest{
		Update: graphUpdateWithDependency(t, "src/support.ts", "function:support", "", []string{"support"}),
	})
	if err != nil {
		t.Fatalf("find affected sources: %v", err)
	}
	if len(affected) != 0 {
		t.Errorf("affected sources = %q, want none", affected)
	}
}

func TestAffectedSourcesPreservesSurfaceStateAfterPrune(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	if _, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdateWithDependency(t, "src/main.ts", "function:main", "src/support.ts", nil),
	}); err != nil {
		t.Fatalf("publish dependent source: %v", err)
	}
	if _, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdateWithDependency(t, "src/support.ts", "function:support", "", []string{"support"}),
	}); err != nil {
		t.Fatalf("publish original export: %v", err)
	}
	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdateWithDependency(t, "src/support.ts", "function:replacement", "", []string{"replacement"}),
	})
	if err != nil {
		t.Fatalf("publish replacement export: %v", err)
	}
	if _, err := store.Prune(context.Background(), storage.PruneRequest{Workspace: "workspace", BeforeVersion: snapshot.Version}); err != nil {
		t.Fatalf("prune earlier snapshots: %v", err)
	}

	affected, err := store.AffectedSources(context.Background(), snapshot, storage.AffectedSourcesRequest{
		Update: graphUpdateWithDependency(t, "src/support.ts", "function:replacement", "", []string{"replacement"}),
	})
	if err != nil {
		t.Fatalf("find affected sources after prune: %v", err)
	}
	if len(affected) != 0 {
		t.Errorf("affected sources after prune = %q, want none", affected)
	}
}

func TestAffectedSourcesUsesRestoredSurfaceStateAfterRollback(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	if _, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdateWithDependency(t, "src/main.ts", "function:main", "src/support.ts", nil),
	}); err != nil {
		t.Fatalf("publish dependent source: %v", err)
	}
	original, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdateWithDependency(t, "src/support.ts", "function:support", "", []string{"support"}),
	})
	if err != nil {
		t.Fatalf("publish original export: %v", err)
	}
	if _, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdateWithDependency(t, "src/support.ts", "function:replacement", "", []string{"replacement"}),
	}); err != nil {
		t.Fatalf("publish replacement export: %v", err)
	}
	if _, err := store.Rollback(context.Background(), storage.RollbackRequest{Workspace: "workspace", Version: original.Version}); err != nil {
		t.Fatalf("roll back replacement export: %v", err)
	}

	affected, err := store.AffectedSources(context.Background(), original, storage.AffectedSourcesRequest{
		Update: graphUpdateWithDependency(t, "src/support.ts", "function:support", "", []string{"support"}),
	})
	if err != nil {
		t.Fatalf("find affected sources after rollback: %v", err)
	}
	if len(affected) != 0 {
		t.Errorf("affected sources after rollback = %q, want none", affected)
	}
}

func TestPublishCancellationKeepsPriorSnapshotCurrent(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	published, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdate(t, "src/main.ts", "function:main"),
	})
	if err != nil {
		t.Fatalf("publish initial snapshot: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Publish(canceled, storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdate(t, "src/main.ts", "function:main-v2"),
	}); err == nil {
		t.Fatal("publish canceled update succeeded")
	}

	current, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: "workspace"})
	if err != nil {
		t.Fatalf("open current snapshot: %v", err)
	}
	if current != published {
		t.Errorf("current snapshot after cancellation = %+v, want %+v", current, published)
	}
}

func TestPublishStagedFailureKeepsPriorSnapshotCurrent(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	published, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdate(t, "src/main.ts", "function:main"),
	})
	if err != nil {
		t.Fatalf("publish initial snapshot: %v", err)
	}

	stageFailure := errors.New("stage resolver paths")
	_, err = store.PublishStaged(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdate(t, "src/main.ts", "function:replacement"),
	}, func(context.Context, storage.ResolverStager) (storage.PublishRequest, error) {
		return storage.PublishRequest{}, stageFailure
	})
	if !errors.Is(err, stageFailure) {
		t.Errorf("publish staged failure = %v, want stage failure", err)
	}

	current, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: "workspace"})
	if err != nil {
		t.Fatalf("open current snapshot: %v", err)
	}
	if current != published {
		t.Errorf("current snapshot after staged failure = %+v, want %+v", current, published)
	}
}

func TestPublishStagedRemovesTemporaryRecordsAfterCommitRollbackAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	publishStaged := func(callback func(context.Context, storage.ResolverStager) (storage.PublishRequest, error)) error {
		_, err := store.PublishStaged(context.Background(), storage.PublishRequest{
			Workspace: "workspace",
			Update:    graphUpdate(t, "src/main.ts", "function:main"),
		}, callback)
		return err
	}
	stage := func(ctx context.Context, stager storage.ResolverStager) (storage.PublishRequest, error) {
		if err := stager.StageResolverSources(ctx, []storage.ResolverStageSource{{
			ProjectID: "project:fixture", Language: "typescript", SourcePath: "src/main.ts",
		}}); err != nil {
			return storage.PublishRequest{}, err
		}
		return storage.PublishRequest{
			Workspace: "workspace",
			Update:    graphUpdate(t, "src/main.ts", "function:main"),
		}, nil
	}
	if err := publishStaged(stage); err != nil {
		t.Fatalf("publish staged commit: %v", err)
	}
	if err := publishStaged(func(ctx context.Context, stager storage.ResolverStager) (storage.PublishRequest, error) {
		if err := stager.StageResolverSources(ctx, []storage.ResolverStageSource{{
			ProjectID: "project:fixture", Language: "typescript", SourcePath: "src/main.ts",
		}}); err != nil {
			return storage.PublishRequest{}, err
		}
		return storage.PublishRequest{}, errors.New("roll back temporary staging")
	}); err == nil {
		t.Fatal("publish staged rollback succeeded")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	store, err = sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := publishStaged(stage); err != nil {
		t.Fatalf("publish staged after reopen: %v", err)
	}
}

func TestPublishStagedCancellationKeepsPriorSnapshotCurrent(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	published, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdate(t, "src/main.ts", "function:main"),
	})
	if err != nil {
		t.Fatalf("publish initial snapshot: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = store.PublishStaged(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdate(t, "src/main.ts", "function:replacement"),
	}, func(_ context.Context, stager storage.ResolverStager) (storage.PublishRequest, error) {
		return storage.PublishRequest{}, stager.StageResolverSources(canceled, []storage.ResolverStageSource{{
			ProjectID: "project:fixture", Language: "typescript", SourcePath: "src/main.ts",
		}})
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("publish staged cancellation = %v, want context cancellation", err)
	}

	current, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: "workspace"})
	if err != nil {
		t.Fatalf("open current snapshot: %v", err)
	}
	if current != published {
		t.Errorf("current snapshot after staged cancellation = %+v, want %+v", current, published)
	}
}

func TestPublishBatchFailureKeepsPriorSnapshotCurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	published, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdate(t, "src/main.ts", "function:main"),
	})
	if err != nil {
		t.Fatalf("publish initial snapshot: %v", err)
	}
	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open verification database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.Exec(`
		CREATE TRIGGER fail_source_node_batch
		BEFORE INSERT ON contribution_nodes
		WHEN NEW.node_id = 'function:fail'
		BEGIN
			SELECT RAISE(ABORT, 'injected batch failure');
		END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	_, err = store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update: graphUpdateForSources(t,
			sourceFact{path: "src/one.ts", nodeID: "function:one"},
			sourceFact{path: "src/fail.ts", nodeID: "function:fail"},
		),
	})
	if err == nil {
		t.Fatal("publish with injected batch failure succeeded")
	}

	current, err := store.OpenSnapshot(context.Background(), storage.OpenSnapshotRequest{Workspace: "workspace"})
	if err != nil {
		t.Fatalf("open current snapshot: %v", err)
	}
	if current != published {
		t.Errorf("current snapshot after batch failure = %+v, want %+v", current, published)
	}
}

func TestPublishManyContributionsKeepsAllFactsAvailable(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	sources := make([]sourceFact, 0, 128)
	for sourceIndex := 0; sourceIndex < cap(sources); sourceIndex++ {
		sources = append(sources, sourceFact{
			path:   fmt.Sprintf("src/module-%03d.ts", sourceIndex),
			nodeID: fmt.Sprintf("function:module-%03d", sourceIndex),
		})
	}
	snapshot, err := store.Publish(context.Background(), storage.PublishRequest{
		Workspace: "workspace",
		Update:    graphUpdateForSources(t, sources...),
	})
	if err != nil {
		t.Fatalf("publish many source contributions: %v", err)
	}

	collector := &factCollector{}
	if err := store.Export(context.Background(), snapshot, storage.ExportRequest{}, collector); err != nil {
		t.Fatalf("export many source contributions: %v", err)
	}
	if len(collector.nodes) != len(sources) {
		t.Errorf("exported node count = %d, want %d", len(collector.nodes), len(sources))
	}
	for sourceIndex, node := range collector.nodes {
		want := fmt.Sprintf("function:module-%03d", sourceIndex)
		if node.ID != want {
			t.Errorf("exported node %d = %q, want %q", sourceIndex, node.ID, want)
		}
	}
}

func graphUpdate(t *testing.T, path, nodeID string) extractor.GraphUpdate {
	return graphUpdateForSources(t, sourceFact{path: path, nodeID: nodeID})
}

type sourceFact struct {
	path   string
	nodeID string
}

func graphUpdateForSources(t *testing.T, sourceFacts ...sourceFact) extractor.GraphUpdate {
	t.Helper()
	vocabulary, err := graph.NewVocabulary(graph.VocabularyDefinition{NodeKinds: []graph.NodeKind{"function"}})
	if err != nil {
		t.Fatalf("create vocabulary: %v", err)
	}
	contributions := make([]extractor.Contribution, 0, len(sourceFacts))
	for _, source := range sourceFacts {
		contribution, err := extractor.NewContribution(vocabulary, extractor.ContributionInput{
			SourcePath: source.path,
			Metadata:   extractor.Metadata{Name: "typescript", Version: "1", Extensions: []string{".ts"}},
			Facts: graph.Facts{Nodes: []graph.Node{{
				ID:   source.nodeID,
				Kind: "function",
				Evidence: graph.FactEvidence{
					Span:       graph.SourceSpan{Path: source.path, StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 5},
					FileHash:   "content-hash",
					Extractor:  "typescript@1",
					Provenance: "syntax",
					Confidence: graph.ConfidenceExtracted,
				},
			}}},
		})
		if err != nil {
			t.Fatalf("create contribution: %v", err)
		}
		contributions = append(contributions, contribution)
	}
	update, err := extractor.NewGraphUpdate(contributions)
	if err != nil {
		t.Fatalf("create graph update: %v", err)
	}
	return update
}

func graphUpdateWithDependency(t *testing.T, sourcePath, nodeID, dependencyPath string, exportedNames []string) extractor.GraphUpdate {
	t.Helper()
	vocabulary, err := graph.NewVocabulary(graph.VocabularyDefinition{NodeKinds: []graph.NodeKind{"function"}})
	if err != nil {
		t.Fatalf("create vocabulary: %v", err)
	}
	surfaces := make([]extractor.ExportedSurface, 0, len(exportedNames))
	for _, name := range exportedNames {
		surfaces = append(surfaces, extractor.ExportedSurface{NodeID: nodeID, Name: name})
	}
	dependencies := make([]extractor.Dependency, 0, 1)
	if dependencyPath != "" {
		dependencies = append(dependencies, extractor.Dependency{SourcePath: sourcePath, TargetPath: dependencyPath})
	}
	contribution, err := extractor.NewContribution(vocabulary, extractor.ContributionInput{
		SourcePath:       sourcePath,
		Metadata:         extractor.Metadata{Name: "typescript", Version: "1", Extensions: []string{".ts"}},
		Facts:            graph.Facts{Nodes: []graph.Node{graphNode(sourcePath, nodeID)}},
		ExportedSurfaces: surfaces,
		Dependencies:     dependencies,
	})
	if err != nil {
		t.Fatalf("create contribution: %v", err)
	}
	update, err := extractor.NewGraphUpdate([]extractor.Contribution{contribution})
	if err != nil {
		t.Fatalf("create graph update: %v", err)
	}
	return update
}

func resolverProjectionUpdate(t *testing.T, projectID, sourcePath, nodeID, exportedName string) extractor.GraphUpdate {
	t.Helper()
	vocabulary, err := graph.NewVocabulary(graph.VocabularyDefinition{NodeKinds: []graph.NodeKind{"function"}})
	if err != nil {
		t.Fatalf("create vocabulary: %v", err)
	}
	contribution, err := extractor.NewContribution(vocabulary, extractor.ContributionInput{
		ProjectID:        projectID,
		SourcePath:       sourcePath,
		Metadata:         extractor.Metadata{Name: "typescript", Version: "1", Extensions: []string{".ts"}},
		Facts:            graph.Facts{Nodes: []graph.Node{graphNode(sourcePath, nodeID)}},
		ExportedSurfaces: []extractor.ExportedSurface{{NodeID: nodeID, Name: exportedName}},
	})
	if err != nil {
		t.Fatalf("create resolver projection contribution: %v", err)
	}
	update, err := extractor.NewGraphUpdate([]extractor.Contribution{contribution})
	if err != nil {
		t.Fatalf("create resolver projection update: %v", err)
	}
	return update
}

func graphUpdateWithFacts(t *testing.T, path string, facts graph.Facts) extractor.GraphUpdate {
	t.Helper()
	vocabulary, err := graph.NewVocabulary(graph.VocabularyDefinition{
		NodeKinds: []graph.NodeKind{"project", "file", "function", "class"},
		Relations: []graph.RelationDefinition{
			{
				Kind: "contains",
				Endpoints: []graph.EndpointRule{
					{Source: "project", Target: "file"},
					{Source: "file", Target: "function"},
				},
			},
			{
				Kind: "calls",
				Endpoints: []graph.EndpointRule{{
					Source: "function",
					Target: "function",
				}},
			},
			{
				Kind: "references",
				Endpoints: []graph.EndpointRule{{
					Source: "function",
					Target: "function",
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("create vocabulary: %v", err)
	}
	contribution, err := extractor.NewContribution(vocabulary, extractor.ContributionInput{
		SourcePath: path,
		Metadata:   extractor.Metadata{Name: "typescript", Version: "1", Extensions: []string{".ts"}},
		Facts:      facts,
	})
	if err != nil {
		t.Fatalf("create contribution: %v", err)
	}
	update, err := extractor.NewGraphUpdate([]extractor.Contribution{contribution})
	if err != nil {
		t.Fatalf("create graph update: %v", err)
	}
	return update
}

func graphNode(path, nodeID string) graph.Node {
	return graph.Node{ID: nodeID, Kind: "function", Evidence: evidence(path)}
}

func evidence(path string) graph.FactEvidence {
	return graph.FactEvidence{
		Span:       graph.SourceSpan{Path: path, StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 5},
		FileHash:   "content-hash",
		Extractor:  "typescript@1",
		Provenance: "syntax",
		Confidence: graph.ConfidenceExtracted,
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
