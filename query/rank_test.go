package query_test

import (
	"context"
	"reflect"
	"testing"

	"agent-wayfinder/graph"
	"agent-wayfinder/query"
	"agent-wayfinder/storage"
)

func TestRankPrioritizesMatchKindsAndPreservesTerms(t *testing.T) {
	nodes := []graph.Node{
		{ID: "function:target", Label: "other", QualifiedName: "src/other.ts::other", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "src/other.ts"}}},
		{ID: "function:qualified", Label: "other", QualifiedName: "target", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "src/qualified.ts"}}},
		{ID: "function:label", Label: "target", QualifiedName: "src/label.ts::target", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "src/label.ts"}}},
		{ID: "function:prefix", Label: "targetValue", QualifiedName: "src/prefix.ts::targetValue", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "src/prefix.ts"}}},
		{ID: "function:token", Label: "other", QualifiedName: "src/token.ts::run-target-task", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "src/token.ts"}}},
		{ID: "function:path", Label: "other", QualifiedName: "src/path.ts::other", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "src/target-file.ts"}}},
		{ID: "function:substring", Label: "otherTarget", QualifiedName: "src/substring.ts::otherTarget", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "src/substring.ts"}}},
	}

	seeds := query.Rank(nodes, []string{"target", "function:target", "target-file"})
	if len(seeds) != 3 {
		t.Fatalf("seed sets = %d, want 3", len(seeds))
	}
	if seeds[0].Term != "target" {
		t.Errorf("first term = %q, want target", seeds[0].Term)
	}
	if got, want := nodeIDs(seeds[0].Nodes), []string{"function:qualified", "function:label", "function:prefix"}; !reflect.DeepEqual(got, want) {
		t.Errorf("target seed IDs = %v, want %v", got, want)
	}
	if seeds[1].Term != "function:target" {
		t.Errorf("second term = %q, want function:target", seeds[1].Term)
	}
	if got, want := nodeIDs(seeds[1].Nodes), []string{"function:target"}; !reflect.DeepEqual(got, want) {
		t.Errorf("exact ID seed IDs = %v, want %v", got, want)
	}
	if seeds[2].Term != "target-file" {
		t.Errorf("third term = %q, want target-file", seeds[2].Term)
	}
	if got, want := nodeIDs(seeds[2].Nodes), []string{"function:path"}; !reflect.DeepEqual(got, want) {
		t.Errorf("target-file seed IDs = %v, want %v", got, want)
	}
}

func TestRankUsesStableTieBreaksAndDistinctSeeds(t *testing.T) {
	nodes := []graph.Node{
		{ID: "function:z", Label: "target", QualifiedName: "src/z.ts::target", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "src/z.ts"}}},
		{ID: "function:a", Label: "target", QualifiedName: "src/a.ts::target", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "src/a.ts"}}},
		{ID: "function:b", Label: "target", QualifiedName: "src/b.ts::target", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "src/b.ts"}}},
		{ID: "function:duplicate", Label: "target", QualifiedName: "src/duplicate.ts::target", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "src/duplicate.ts"}}},
		{ID: "function:duplicate", Label: "target", QualifiedName: "src/duplicate.ts::target", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "src/duplicate.ts"}}},
	}

	seeds := query.Rank(nodes, []string{"target"})
	if got, want := nodeIDs(seeds[0].Nodes), []string{"function:a", "function:b", "function:duplicate"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ranked seed IDs = %v, want %v", got, want)
	}
}

func TestRankMatchesTokenAndSubstring(t *testing.T) {
	nodes := []graph.Node{
		{ID: "function:token", Label: "runner", QualifiedName: "src/task-runner.ts::run-task", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "src/task-runner.ts"}}},
		{ID: "function:substring", Label: "otherTarget", QualifiedName: "src/other.ts::otherTarget", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "src/other.ts"}}},
	}

	seeds := query.Rank(nodes, []string{"task", "hertar"})
	if got, want := nodeIDs(seeds[0].Nodes), []string{"function:token"}; !reflect.DeepEqual(got, want) {
		t.Errorf("token seed IDs = %v, want %v", got, want)
	}
	if got, want := nodeIDs(seeds[1].Nodes), []string{"function:substring"}; !reflect.DeepEqual(got, want) {
		t.Errorf("substring seed IDs = %v, want %v", got, want)
	}
}

func TestRankSnapshotReadsNodesFromOnePublishedSnapshot(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	lookup := nodeLookupFunc(func(ctx context.Context, gotSnapshot storage.Snapshot, request storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
		if gotSnapshot != snapshot {
			t.Errorf("snapshot = %+v, want %+v", gotSnapshot, snapshot)
		}
		if request.Text != "main" || request.Limit <= 0 {
			t.Errorf("lookup request = %+v, want main and positive limit", request)
		}
		return []storage.NodeMatch{{Node: graph.Node{ID: "function:main", Label: "main", QualifiedName: "src/main.ts::main", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "src/main.ts"}}}}}, nil
	})

	seeds, err := query.RankSnapshot(context.Background(), lookup, snapshot, []string{"main"})
	if err != nil {
		t.Fatalf("rank snapshot: %v", err)
	}
	if got, want := nodeIDs(seeds[0].Nodes), []string{"function:main"}; !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot seed IDs = %v, want %v", got, want)
	}
}

func TestRankSnapshotPrefersExactMatches(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	exact := graph.Node{ID: "function:main", Label: "main", QualifiedName: "src/main.ts::main"}
	exactMatches := []storage.NodeMatch{
		{Node: exact},
		{Node: graph.Node{ID: "function:other", Label: "main", QualifiedName: "src/other.ts::main"}},
		{Node: graph.Node{ID: "function:third", Label: "main", QualifiedName: "src/third.ts::main"}},
		{Node: graph.Node{ID: "function:fourth", Label: "main", QualifiedName: "src/fourth.ts::main"}},
	}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			t.Fatal("broad lookup is called when exact lookup finds a match")
			return nil, nil
		}),
		exact: func(_ context.Context, gotSnapshot storage.Snapshot, identifier string) ([]storage.NodeMatch, error) {
			if gotSnapshot != snapshot {
				t.Errorf("snapshot = %+v, want %+v", gotSnapshot, snapshot)
			}
			if identifier != exact.QualifiedName {
				t.Errorf("exact lookup identifier = %q, want %q", identifier, exact.QualifiedName)
			}
			return exactMatches, nil
		},
	}

	seeds, err := query.RankSnapshot(context.Background(), lookup, snapshot, []string{exact.QualifiedName})
	if err != nil {
		t.Fatalf("rank snapshot: %v", err)
	}
	if got, want := nodeIDs(seeds[0].Nodes), []string{exact.ID, "function:other", "function:third"}; !reflect.DeepEqual(got, want) {
		t.Errorf("exact seed IDs = %v, want %v", got, want)
	}
}

func TestRankSnapshotFallsBackToBroadLookupAfterExactMiss(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	broad := graph.Node{ID: "function:main", Label: "main", QualifiedName: "src/main.ts::main"}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(_ context.Context, gotSnapshot storage.Snapshot, request storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			if gotSnapshot != snapshot {
				t.Errorf("snapshot = %+v, want %+v", gotSnapshot, snapshot)
			}
			if request.Text != "main" || request.Limit != 3 {
				t.Errorf("broad lookup request = %+v, want main with limit 3", request)
			}
			return []storage.NodeMatch{{Node: broad}}, nil
		}),
		exact: func(_ context.Context, _ storage.Snapshot, identifier string) ([]storage.NodeMatch, error) {
			if identifier != "main" {
				t.Errorf("exact lookup identifier = %q, want main", identifier)
			}
			return nil, nil
		},
	}

	seeds, err := query.RankSnapshot(context.Background(), lookup, snapshot, []string{"main"})
	if err != nil {
		t.Fatalf("rank snapshot: %v", err)
	}
	if got, want := nodeIDs(seeds[0].Nodes), []string{broad.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("broad fallback seed IDs = %v, want %v", got, want)
	}
}

func TestRankRetrievalsSnapshotUsesStructuredLexicalSearchAfterExactMiss(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	wantRequest := storage.LexicalSearchRequest{Text: "lookup exact nodes", TokenGroups: [][]string{{"lookup", "exact", "nodes"}}, Kinds: []graph.NodeKind{"function"}, ProjectIDs: []string{"project:app"}, Limit: 10}
	wantNode := graph.Node{ID: "function:lookup", Label: "LookupExactNodes"}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			t.Fatal("legacy broad lookup is called for structured retrieval")
			return nil, nil
		}),
		exact: func(_ context.Context, gotSnapshot storage.Snapshot, identifier string) ([]storage.NodeMatch, error) {
			if gotSnapshot != snapshot || identifier != wantRequest.Text {
				t.Errorf("exact lookup = {%+v, %q}, want {%+v, %q}", gotSnapshot, identifier, snapshot, wantRequest.Text)
			}
			return nil, nil
		},
	}
	searcher := lexicalSearcherFunc(func(_ context.Context, gotSnapshot storage.Snapshot, request storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		if gotSnapshot != snapshot || !reflect.DeepEqual(request, wantRequest) {
			t.Errorf("lexical search = {%+v, %+v}, want {%+v, %+v}", gotSnapshot, request, snapshot, wantRequest)
		}
		return []storage.LexicalMatch{{Node: wantNode, Score: 2, MatchedFields: []string{"identifierTokens"}}}, nil
	})

	seeds, err := query.RankRetrievalsSnapshot(context.Background(), lookup, searcher, snapshot, []storage.LexicalSearchRequest{wantRequest})
	if err != nil {
		t.Fatalf("rank structured retrieval: %v", err)
	}
	if got := nodeIDs(seeds[0].Nodes); !reflect.DeepEqual(got, []string{wantNode.ID}) {
		t.Errorf("structured seed IDs = %v, want %v", got, []string{wantNode.ID})
	}
}

func TestRankSeedRequestsSnapshotDiversifiesPathsAndExposesScores(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	requests := []query.SeedRequest{{
		Role:      "entity",
		Retrieval: storage.LexicalSearchRequest{Text: "lookup", Limit: 3},
	}}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			t.Fatal("legacy broad lookup is called for structured retrieval")
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) { return nil, nil },
	}
	searcher := lexicalSearcherFunc(func(_ context.Context, _ storage.Snapshot, request storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		if request.Limit <= requests[0].Retrieval.Limit {
			t.Errorf("candidate limit = %d, want more than selected limit %d", request.Limit, requests[0].Retrieval.Limit)
		}
		return []storage.LexicalMatch{
			{Node: graph.Node{ID: "function:first", Label: "lookup", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "src/noisy.go"}}}, Score: 10, MatchedFields: []string{"label"}},
			{Node: graph.Node{ID: "function:second", Label: "lookupMore", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "src/noisy.go"}}}, Score: 9, MatchedFields: []string{"label"}},
			{Node: graph.Node{ID: "function:diverse", Label: "lookupOther", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "pkg/diverse.go"}}}, Score: 8, MatchedFields: []string{"label"}},
		}, nil
	})

	seeds, err := query.RankSeedRequestsSnapshot(context.Background(), lookup, searcher, snapshot, requests)
	if err != nil {
		t.Fatalf("rank seed requests: %v", err)
	}
	if got, want := nodeIDs(seeds[0].Nodes), []string{"function:first", "function:diverse", "function:second"}; !reflect.DeepEqual(got, want) {
		t.Errorf("diversified seed IDs = %v, want %v", got, want)
	}
	if seeds[0].Role != "entity" || len(seeds[0].Rankings) != 3 {
		t.Fatalf("ranked seed set = %+v, want entity role and three rankings", seeds[0])
	}
	for _, ranking := range seeds[0].Rankings {
		if ranking.Score <= 0 || ranking.Components.Lexical <= 0 || ranking.Components.TokenCoverage <= 0 {
			t.Errorf("ranking = %+v, want positive total, lexical, and token-coverage scores", ranking)
		}
	}
}

func TestRankSeedRequestsSnapshotFusesSignalsDeterministically(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	requests := []query.SeedRequest{{
		Role: "caller",
		Retrieval: storage.LexicalSearchRequest{
			Text:        "lookup service",
			TokenGroups: [][]string{{"lookup", "service"}},
			Kinds:       []graph.NodeKind{"function"},
			ProjectIDs:  []string{"project:app"},
			Limit:       2,
		},
	}}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) { return nil, nil },
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		return []storage.LexicalMatch{
			{Node: graph.Node{ID: "variable:lookup", Kind: "variable", Label: "lookup", QualifiedName: "other::lookup", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "other/value.go"}}}, Score: 100, MatchedFields: []string{"label"}},
			{Node: graph.Node{ID: "function:lookup-service", Kind: "function", Label: "LookupService", QualifiedName: "app::LookupService", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "app/lookup_service.go"}}}, Score: 50, MatchedFields: []string{"label", "path", "identifierTokens"}},
		}, nil
	})

	first, err := query.RankSeedRequestsSnapshot(context.Background(), lookup, searcher, snapshot, requests)
	if err != nil {
		t.Fatalf("rank seed requests: %v", err)
	}
	second, err := query.RankSeedRequestsSnapshot(context.Background(), lookup, searcher, snapshot, requests)
	if err != nil {
		t.Fatalf("repeat rank seed requests: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated ranking differs:\nfirst  %+v\nsecond %+v", first, second)
	}
	if got, want := nodeIDs(first[0].Nodes), []string{"function:lookup-service", "variable:lookup"}; !reflect.DeepEqual(got, want) {
		t.Errorf("fused seed IDs = %v, want %v", got, want)
	}
	components := first[0].Rankings[0].Components
	if components.EntityRole <= 0 || components.NodeKind <= 0 || components.ProjectScope <= 0 || components.PathAffinity <= 0 || components.ReciprocalRank <= 0 {
		t.Errorf("fused components = %+v, want positive role, kind, scope, path, and reciprocal-rank scores", components)
	}
}

func TestRankSeedRequestsSnapshotPrioritizesExactSymbolLabel(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	request := query.SeedRequest{
		Role: "entity",
		Retrieval: storage.LexicalSearchRequest{
			Text:        "QuerySnapshot",
			TokenGroups: [][]string{{"query", "snapshot"}},
			Limit:       3,
		},
	}
	exactLabel := graph.Node{
		ID:            "function:QuerySnapshot",
		Kind:          "function",
		Label:         "QuerySnapshot",
		QualifiedName: "query.QuerySnapshot",
		Evidence:      graph.FactEvidence{Span: graph.SourceSpan{Path: "query/query.go"}},
	}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) { return nil, nil },
	}
	searcher := lexicalSearcherFunc(func(_ context.Context, _ storage.Snapshot, gotRequest storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		if gotRequest.Limit != 12 {
			t.Errorf("candidate limit = %d, want 12", gotRequest.Limit)
		}
		return []storage.LexicalMatch{
			{Node: graph.Node{ID: "variable:snapshot-a", Kind: "variable", Label: "snapshot", QualifiedName: "query.QuerySnapshot::snapshotA", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "query/a.go"}}}, Score: 10, MatchedFields: []string{"qualifiedName", "identifierTokens"}},
			{Node: graph.Node{ID: "variable:snapshot-b", Kind: "variable", Label: "snapshot", QualifiedName: "query.QuerySnapshot::snapshotB", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "query/b.go"}}}, Score: 9, MatchedFields: []string{"qualifiedName", "identifierTokens"}},
			{Node: graph.Node{ID: "variable:snapshot-c", Kind: "variable", Label: "snapshot", QualifiedName: "query.QuerySnapshot::snapshotC", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "query/c.go"}}}, Score: 8, MatchedFields: []string{"qualifiedName", "identifierTokens"}},
			{Node: graph.Node{ID: "variable:snapshot-d", Kind: "variable", Label: "snapshot", QualifiedName: "query.QuerySnapshot::snapshotD", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "query/d.go"}}}, Score: 7, MatchedFields: []string{"qualifiedName", "identifierTokens"}},
			{Node: exactLabel, Score: 1, MatchedFields: []string{"label", "qualifiedName", "identifierTokens"}},
		}, nil
	})

	first, err := query.RankSeedRequestsSnapshot(context.Background(), lookup, searcher, snapshot, []query.SeedRequest{request})
	if err != nil {
		t.Fatalf("rank exact-label seed: %v", err)
	}
	second, err := query.RankSeedRequestsSnapshot(context.Background(), lookup, searcher, snapshot, []query.SeedRequest{request})
	if err != nil {
		t.Fatalf("repeat rank exact-label seed: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated exact-label ranking differs:\nfirst  %+v\nsecond %+v", first, second)
	}
	if got := first[0].Nodes[0].ID; got != exactLabel.ID {
		t.Fatalf("first seed ID = %q, want exact label %q; seeds = %v", got, exactLabel.ID, nodeIDs(first[0].Nodes))
	}
	if first[0].Rankings[0].Components.Exact != 1 {
		t.Errorf("exact-label components = %+v, want exact score component 1", first[0].Rankings[0].Components)
	}
}

func TestRankSeedRequestsSnapshotPrefersAdapterNodesForSharedContractSlots(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	request := query.SeedRequest{Role: "left", Retrieval: storage.LexicalSearchRequest{Text: "postgres", TokenGroups: [][]string{{"postgres"}, {"pg"}}, Limit: 2}}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) { return nil, nil },
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		return []storage.LexicalMatch{
			{Node: graph.Node{ID: "function:migration", Kind: "function", Label: "transformLegacyPostgres", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "migrations/postgres.ts"}}}, Score: 10},
			{Node: graph.Node{ID: "class:adapter", Kind: "class", Label: "PgStore", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "adapters/pg-store.ts"}}}, Score: 9},
		}, nil
	})

	seeds, err := query.RankSeedRequestsSnapshot(context.Background(), lookup, searcher, snapshot, []query.SeedRequest{request})
	if err != nil {
		t.Fatalf("rank shared-contract slot: %v", err)
	}
	if got, want := nodeIDs(seeds[0].Nodes), []string{"class:adapter", "function:migration"}; !reflect.DeepEqual(got, want) {
		t.Errorf("shared-contract seeds = %v, want %v", got, want)
	}
}

func TestRankSeedRequestsSnapshotPrefersServiceEntityRole(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) { return nil, nil },
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		return []storage.LexicalMatch{
			{Node: graph.Node{ID: "function:index", Kind: "go:function", Label: "IndexHandler", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "cmd/index.go"}}}, Score: 10},
			{Node: graph.Node{ID: "type:index-service", Kind: "go:type", Label: "IndexService", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "query/index_service.go"}}}, Score: 5},
		}, nil
	})

	seeds, err := query.RankSeedRequestsSnapshot(context.Background(), lookup, searcher, snapshot, []query.SeedRequest{{
		Role:       "entity",
		EntityRole: "service",
		Retrieval:  storage.LexicalSearchRequest{Text: "index", TokenGroups: [][]string{{"index"}}, Limit: 2},
	}})
	if err != nil {
		t.Fatalf("rank service entity: %v", err)
	}
	if got, want := nodeIDs(seeds[0].Nodes), []string{"type:index-service", "function:index"}; !reflect.DeepEqual(got, want) {
		t.Errorf("service seed IDs = %v, want %v", got, want)
	}
}

func TestRankSeedRequestsSnapshotUsesExactMatchWhenAvailable(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	wantNode := graph.Node{ID: "function:main", Kind: "function", Label: "main", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "src/main.go"}}}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			t.Fatal("legacy broad lookup is called when exact lookup finds a match")
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) {
			return []storage.NodeMatch{{Node: wantNode}}, nil
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		t.Fatal("lexical search is called when exact lookup finds a match")
		return nil, nil
	})

	seeds, err := query.RankSeedRequestsSnapshot(context.Background(), lookup, searcher, snapshot, []query.SeedRequest{{
		Role:      "entity",
		Retrieval: storage.LexicalSearchRequest{Text: "main", Limit: 3},
	}})
	if err != nil {
		t.Fatalf("rank exact seed request: %v", err)
	}
	if got, want := nodeIDs(seeds[0].Nodes), []string{wantNode.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("exact seed IDs = %v, want %v", got, want)
	}
	ranking := seeds[0].Rankings[0]
	if ranking.Score != 1 || ranking.Components.Exact != 1 || ranking.Components.ReciprocalRank != 1 {
		t.Errorf("exact seed ranking = %+v, want score, exact, and reciprocal rank equal to 1", ranking)
	}
}

type exporterFunc func(context.Context, storage.Snapshot, storage.ExportRequest, storage.ExportSink) error

func (export exporterFunc) Export(ctx context.Context, snapshot storage.Snapshot, request storage.ExportRequest, sink storage.ExportSink) error {
	return export(ctx, snapshot, request, sink)
}

type nodeLookupFunc func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error)

func (lookup nodeLookupFunc) LookupNodes(ctx context.Context, snapshot storage.Snapshot, request storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
	return lookup(ctx, snapshot, request)
}

type lexicalSearcherFunc func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error)

func (search lexicalSearcherFunc) SearchNodes(ctx context.Context, snapshot storage.Snapshot, request storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
	return search(ctx, snapshot, request)
}

func nodeIDs(nodes []graph.Node) []string {
	ids := make([]string, len(nodes))
	for index, node := range nodes {
		ids[index] = node.ID
	}
	return ids
}
