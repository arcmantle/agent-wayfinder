package query_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agent-wayfinder/graph"
	"agent-wayfinder/query"
	"agent-wayfinder/storage"
)

func TestQuerySnapshotReportsExternalProjectScopeBoundary(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	main := graph.Node{ID: "function:main", Label: "main", QualifiedName: "apps/app/src/main.ts::main"}
	helper := graph.Node{ID: "function:helper", Label: "helper", QualifiedName: "packages/library/src/helper.ts::helper"}
	lookup := nodeLookupFunc(func(_ context.Context, _ storage.Snapshot, request storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
		if request.Text != "main" || request.Limit <= 0 {
			t.Errorf("lookup request = %+v, want main and positive limit", request)
		}
		return []storage.NodeMatch{{Node: main}}, nil
	})
	traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
		return storage.TraversalResult{Facts: graph.Facts{
			Nodes: []graph.Node{main, helper},
			Edges: []graph.Edge{{SourceID: main.ID, TargetID: helper.ID, Relation: "calls"}},
		}, ScopeBoundary: &helper}, nil
	})

	result, err := query.QuerySnapshot(context.Background(), lookup, traverser, snapshot, query.Request{
		Terms:      []string{"main"},
		ProjectIDs: []string{"project:app"},
		MaxDepth:   2,
		MaxNodes:   3,
	})
	if err != nil {
		t.Fatalf("query published graph: %v", err)
	}
	if result.ScopeBoundary == nil || result.ScopeBoundary.Node.ID != helper.ID {
		t.Errorf("scope boundary = %+v, want external helper", result.ScopeBoundary)
	}
}

func TestQuerySnapshotRanksQuestionSlotsIndependently(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	left := graph.Node{ID: "package:postgres", Kind: "package", Label: "postgres", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "storage/postgres"}}}
	right := graph.Node{ID: "package:sqlite", Kind: "package", Label: "sqlite", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "storage/sqlite"}}}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) { return nil, nil },
	}
	searcher := lexicalSearcherFunc(func(_ context.Context, _ storage.Snapshot, request storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		switch request.Text {
		case "postgres":
			return []storage.LexicalMatch{{Node: left, Score: 2}}, nil
		case "sqlite":
			return []storage.LexicalMatch{{Node: right, Score: 2}}, nil
		default:
			t.Fatalf("unexpected slot retrieval %q", request.Text)
			return nil, nil
		}
	})
	var startNodeIDs []string
	traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
		startNodeIDs = append([]string(nil), request.StartNodeIDs...)
		return storage.TraversalResult{Facts: graph.Facts{Nodes: []graph.Node{left, right}}}, nil
	})

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{
		SeedRequests: []query.SeedRequest{
			{Role: "left", Retrieval: storage.LexicalSearchRequest{Text: "postgres", Limit: 1}},
			{Role: "right", Retrieval: storage.LexicalSearchRequest{Text: "sqlite", Limit: 1}},
		},
		MaxDepth: 1,
		MaxNodes: 10,
	})
	if err != nil {
		t.Fatalf("query question slots: %v", err)
	}
	if len(result.Seeds) != 2 || result.Seeds[0].Role != "left" || result.Seeds[1].Role != "right" {
		t.Fatalf("slot seeds = %+v, want separate left and right roles", result.Seeds)
	}
	if got, want := nodeIDs(result.Seeds[0].Nodes), []string{left.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("left seed IDs = %v, want %v", got, want)
	}
	if got, want := nodeIDs(result.Seeds[1].Nodes), []string{right.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("right seed IDs = %v, want %v", got, want)
	}
	if want := []string{left.ID, right.ID}; !reflect.DeepEqual(startNodeIDs, want) {
		t.Errorf("traversal start IDs = %v, want %v", startNodeIDs, want)
	}
}

func TestQuerySnapshotExecutesArchitecturalMoveComparisonInBothDirections(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	coreHost := graph.Node{ID: "package:core-host", Kind: "package", Label: "core-host", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "packages/core-host"}}}
	consumer := graph.Node{ID: "package:app", Kind: "package", Label: "app", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "apps/app"}}}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) { return nil, nil },
	}
	searcher := lexicalSearcherFunc(func(_ context.Context, _ storage.Snapshot, request storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		if len(request.TokenGroups) != 5 {
			t.Errorf("comparison token groups = %v, want five alternative concepts", request.TokenGroups)
		}
		return []storage.LexicalMatch{{Node: coreHost, Score: 2}}, nil
	})
	traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
		if request.Direction != storage.TraverseBoth {
			t.Errorf("comparison direction = %q, want both", request.Direction)
		}
		wantRelations := []graph.RelationKind{
			"imports_from", "go:imports_from", "javascript:imports_from", "typescript:imports_from",
			"requires", "go:requires", "javascript:requires", "typescript:requires",
			"depends_on", "go:depends_on", "javascript:depends_on", "typescript:depends_on",
		}
		if !reflect.DeepEqual(request.Relations, wantRelations) {
			t.Errorf("comparison relations = %v, want %v", request.Relations, wantRelations)
		}
		return storage.TraversalResult{Facts: graph.Facts{
			Nodes: []graph.Node{coreHost, consumer},
			Edges: []graph.Edge{{SourceID: consumer.ID, TargetID: coreHost.ID, Relation: "typescript:imports_from"}},
		}}, nil
	})
	plan := query.AnalyzeQuestion("Should Vite configuration move out of core-host into a core-vite package, or should shell generation move into a core-shell package? Compare dependency direction and consumers.")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
	if err != nil {
		t.Fatalf("execute architectural comparison: %v", err)
	}
	if len(result.Warnings) != 0 || len(result.Facts.Edges) != 1 || len(result.Evidence) != 1 {
		t.Errorf("comparison result = %+v, want consumer dependency evidence without warnings", result)
	}
}

func TestQuerySnapshotExecutesPathPlanAsAnOrderedDirectedProof(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	source := graph.Node{ID: "function:source", Kind: "function", Label: "source"}
	middle := graph.Node{ID: "function:middle", Kind: "function", Label: "middle"}
	target := graph.Node{ID: "function:target", Kind: "function", Label: "target"}
	other := graph.Node{ID: "function:other", Kind: "function", Label: "other"}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			t.Fatal("broad lookup is called after an exact match")
			return nil, nil
		}),
		exact: func(_ context.Context, _ storage.Snapshot, identifier string) ([]storage.NodeMatch, error) {
			switch identifier {
			case source.Label:
				return []storage.NodeMatch{{Node: source}}, nil
			case target.Label:
				return []storage.NodeMatch{{Node: target}}, nil
			default:
				t.Fatalf("unexpected exact lookup %q", identifier)
				return nil, nil
			}
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		t.Fatal("lexical search is called after an exact match")
		return nil, nil
	})
	traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
		if got, want := request.StartNodeIDs, []string{source.ID}; !reflect.DeepEqual(got, want) {
			t.Errorf("traversal start IDs = %v, want %v", got, want)
		}
		if request.Direction != storage.TraverseOutgoing {
			t.Errorf("direction = %q, want outgoing", request.Direction)
		}
		if got, want := request.Relations, []graph.RelationKind{"calls", "go:calls", "javascript:calls", "typescript:calls"}; !reflect.DeepEqual(got, want) {
			t.Errorf("relations = %v, want %v", got, want)
		}
		return storage.TraversalResult{Facts: graph.Facts{
			Nodes: []graph.Node{other, target, source, middle},
			Edges: []graph.Edge{
				{SourceID: source.ID, TargetID: other.ID, Relation: "calls"},
				{SourceID: middle.ID, TargetID: target.ID, Relation: "calls"},
				{SourceID: source.ID, TargetID: middle.ID, Relation: "calls"},
			},
		}}, nil
	})
	plan := query.AnalyzeQuestion("how does source reach target")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{
		Plan:     &plan,
		MaxDepth: plan.MaxDepth,
		MaxNodes: plan.MaxNodes,
	})
	if err != nil {
		t.Fatalf("execute path plan: %v", err)
	}
	if got, want := nodeIDs(result.Facts.Nodes), []string{source.ID, middle.ID, target.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("path node IDs = %v, want %v", got, want)
	}
	if got, want := result.Facts.Edges, []graph.Edge{
		{SourceID: source.ID, TargetID: middle.ID, Relation: "calls"},
		{SourceID: middle.ID, TargetID: target.ID, Relation: "calls"},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("path edges = %+v, want %+v", got, want)
	}
	if len(result.Evidence) != 1 {
		t.Fatalf("path evidence = %+v, want one ordered path group", result.Evidence)
	}
	evidence := result.Evidence[0]
	if evidence.Rank != 1 || evidence.SlotRole != "source+target" || evidence.Relation != "calls" || evidence.Distance != 2 || evidence.Score <= 0 || !strings.Contains(evidence.Reason, "source") || !strings.Contains(evidence.Reason, "target") {
		t.Errorf("path evidence provenance = %+v, want ranked source-to-target path", evidence)
	}
	if got, want := nodeIDs(evidence.Nodes), []string{source.ID, middle.ID, target.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("path evidence nodes = %v, want %v", got, want)
	}
	if got, want := result.Limits, []query.StageLimit{
		{Stage: "retrieval", SlotRole: "source", MaxNodes: 10},
		{Stage: "retrieval", SlotRole: "target", MaxNodes: 10},
		{Stage: "path", MaxDepth: 8, MaxNodes: 100},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("path limits = %+v, want %+v", got, want)
	}
}

func TestQuerySnapshotReportsAmbiguousPathEndpointsSeparately(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	sourceA := graph.Node{ID: "function:source-a", Kind: "function", Label: "source"}
	sourceB := graph.Node{ID: "function:source-b", Kind: "function", Label: "source"}
	targetA := graph.Node{ID: "function:target-a", Kind: "function", Label: "target"}
	targetB := graph.Node{ID: "function:target-b", Kind: "function", Label: "target"}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(_ context.Context, _ storage.Snapshot, identifier string) ([]storage.NodeMatch, error) {
			if identifier == "source" {
				return []storage.NodeMatch{{Node: sourceA}, {Node: sourceB}}, nil
			}
			return []storage.NodeMatch{{Node: targetA}, {Node: targetB}}, nil
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		t.Fatal("lexical search is called after exact matches")
		return nil, nil
	})
	traverser := traverserFunc(func(context.Context, storage.Snapshot, storage.TraversalRequest) (storage.TraversalResult, error) {
		t.Fatal("ambiguous path plan traversed the graph")
		return storage.TraversalResult{}, nil
	})
	plan := query.AnalyzeQuestion("find a path from source to target")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{
		Plan:     &plan,
		MaxDepth: plan.MaxDepth,
		MaxNodes: plan.MaxNodes,
	})
	if err != nil {
		t.Fatalf("execute ambiguous path plan: %v", err)
	}
	if len(result.Seeds) != 2 || result.Seeds[0].Role != "source" || result.Seeds[1].Role != "target" {
		t.Fatalf("endpoint candidates = %+v, want separate source and target slots", result.Seeds)
	}
	if got, want := nodeIDs(result.Seeds[0].Nodes), []string{sourceA.ID, sourceB.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("source candidate IDs = %v, want %v", got, want)
	}
	if got, want := nodeIDs(result.Seeds[1].Nodes), []string{targetA.ID, targetB.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("target candidate IDs = %v, want %v", got, want)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != "ambiguous_path_endpoints" {
		t.Fatalf("warnings = %+v, want ambiguous_path_endpoints", result.Warnings)
	}
	if got, want := result.Warnings[0].Suggestions, []string{
		"Run path from function:source-a to function:target-a.",
		"Run path from function:source-a to function:target-b.",
		"Run path from function:source-b to function:target-a.",
		"Run path from function:source-b to function:target-b.",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("suggestions = %v, want %v", got, want)
	}
}

func TestQuerySnapshotPathReportsOnlyTheEmptyTargetSlotWithoutTraversal(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	source := graph.Node{ID: "function:source", Kind: "function", Label: "source"}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(_ context.Context, _ storage.Snapshot, identifier string) ([]storage.NodeMatch, error) {
			if identifier == "source" {
				return []storage.NodeMatch{{Node: source}}, nil
			}
			return nil, nil
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		return nil, nil
	})
	traverser := traverserFunc(func(context.Context, storage.Snapshot, storage.TraversalRequest) (storage.TraversalResult, error) {
		t.Fatal("path plan with an empty target slot traversed the graph")
		return storage.TraversalResult{}, nil
	})
	plan := query.AnalyzeQuestion("find a path from source to missingTarget")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
	if err != nil {
		t.Fatalf("execute path plan: %v", err)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != "entity_not_found" || !strings.Contains(result.Warnings[0].Message, "target slot") {
		t.Fatalf("warnings = %+v, want only an empty target-slot warning", result.Warnings)
	}
	if got, want := nodeIDs(result.Seeds[0].Nodes), []string{source.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("source candidate IDs = %v, want %v", got, want)
	}
	if len(result.Seeds) != 2 || result.Seeds[1].Role != "target" || len(result.Seeds[1].Nodes) != 0 {
		t.Fatalf("seeds = %+v, want an empty target slot", result.Seeds)
	}
	if len(result.Limits) != 2 || result.Limits[0].SlotRole != "source" || result.Limits[1].SlotRole != "target" {
		t.Fatalf("limits = %+v, want source and target retrieval limits", result.Limits)
	}
	if len(result.Facts.Nodes) != 0 || len(result.Facts.Edges) != 0 || len(result.Impact) != 0 {
		t.Fatalf("result = %+v, want no answer evidence", result)
	}
}

func TestQuerySnapshotReportsNoDirectedPathWithExplicitFallbackSuggestion(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	source := graph.Node{ID: "function:source", Kind: "function", Label: "source"}
	target := graph.Node{ID: "function:target", Kind: "function", Label: "target"}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(_ context.Context, _ storage.Snapshot, identifier string) ([]storage.NodeMatch, error) {
			if identifier == source.Label {
				return []storage.NodeMatch{{Node: source}}, nil
			}
			return []storage.NodeMatch{{Node: target}}, nil
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		return nil, nil
	})
	traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
		if request.Direction != storage.TraverseOutgoing {
			t.Errorf("direction = %q, want outgoing", request.Direction)
		}
		return storage.TraversalResult{
			Facts:             graph.Facts{Nodes: []graph.Node{source}},
			TruncationReasons: []storage.TruncationReason{storage.TruncatedByDepthLimit},
		}, nil
	})
	plan := query.AnalyzeQuestion("can source reach target")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{
		Plan:     &plan,
		MaxDepth: plan.MaxDepth,
		MaxNodes: plan.MaxNodes,
	})
	if err != nil {
		t.Fatalf("execute reachability plan: %v", err)
	}
	if len(result.Facts.Nodes) != 0 || len(result.Facts.Edges) != 0 {
		t.Errorf("path facts = %+v, want no directed-path claim", result.Facts)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != "no_directed_path" {
		t.Fatalf("warnings = %+v, want no_directed_path", result.Warnings)
	}
	if got, want := result.Warnings[0].Suggestions, []string{"Run path from function:source to function:target with undirected fallback."}; !reflect.DeepEqual(got, want) {
		t.Errorf("suggestions = %v, want %v", got, want)
	}
	if got, want := result.TruncationReasons, []storage.TruncationReason{storage.TruncatedByDepthLimit}; !reflect.DeepEqual(got, want) {
		t.Errorf("truncation reasons = %v, want %v", got, want)
	}
}

func TestQuerySnapshotExecutesSharedContractPlanWithSeparateBudgetsAndCommonEvidence(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	left := graph.Node{ID: "package:postgres", Kind: "package", Label: "postgres", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "storage/postgres"}}}
	right := graph.Node{ID: "package:sqlite", Kind: "package", Label: "sqlite", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "storage/sqlite"}}}
	contract := graph.Node{ID: "interface:store", Kind: "interface", Label: "Store", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "storage/storage.go"}}}
	leftOnly := graph.Node{ID: "function:postgres-only", Kind: "function", Label: "postgresOnly", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "storage/postgres/postgres.go"}}}
	rightOnly := graph.Node{ID: "function:sqlite-only", Kind: "function", Label: "sqliteOnly", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "storage/sqlite/sqlite.go"}}}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			t.Fatal("broad lookup is called after an exact match")
			return nil, nil
		}),
		exact: func(_ context.Context, _ storage.Snapshot, identifier string) ([]storage.NodeMatch, error) {
			switch identifier {
			case "postgres":
				return []storage.NodeMatch{{Node: left}}, nil
			case "sqlite":
				return []storage.NodeMatch{{Node: right}}, nil
			default:
				t.Fatalf("unexpected exact lookup %q", identifier)
				return nil, nil
			}
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		t.Fatal("lexical search is called after an exact match")
		return nil, nil
	})
	var traversals []storage.TraversalRequest
	traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
		traversals = append(traversals, request)
		switch request.StartNodeIDs[0] {
		case left.ID:
			return storage.TraversalResult{Facts: graph.Facts{
				Nodes: []graph.Node{left, contract, leftOnly},
				Edges: []graph.Edge{
					{SourceID: left.ID, TargetID: contract.ID, Relation: "implements"},
					{SourceID: left.ID, TargetID: leftOnly.ID, Relation: "contains"},
					{SourceID: contract.ID, TargetID: leftOnly.ID, Relation: "contains"},
				},
			}, TruncationReasons: []storage.TruncationReason{storage.TruncatedByNodeLimit}}, nil
		case right.ID:
			return storage.TraversalResult{Facts: graph.Facts{
				Nodes: []graph.Node{right, contract, rightOnly},
				Edges: []graph.Edge{
					{SourceID: right.ID, TargetID: contract.ID, Relation: "implements"},
					{SourceID: right.ID, TargetID: rightOnly.ID, Relation: "contains"},
				},
			}}, nil
		default:
			t.Fatalf("unexpected traversal start IDs %v", request.StartNodeIDs)
			return storage.TraversalResult{}, nil
		}
	})
	plan := query.AnalyzeQuestion("what is the shared contract between postgres and sqlite")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: 3})
	if err != nil {
		t.Fatalf("execute shared-contract plan: %v", err)
	}
	if len(traversals) != 2 {
		t.Fatalf("traversal count = %d, want one traversal per entity slot", len(traversals))
	}
	for _, traversal := range traversals {
		if traversal.Direction != storage.TraverseBoth || traversal.MaxNodes != 3 {
			t.Errorf("traversal = %+v, want both directions and a separate 3-node budget", traversal)
		}
	}
	if len(result.Seeds) != 2 || result.Seeds[0].Role != "left" || result.Seeds[1].Role != "right" {
		t.Fatalf("slot seeds = %+v, want separate left and right roles", result.Seeds)
	}
	if got, want := nodeIDs(result.Facts.Nodes), []string{contract.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("common node IDs = %v, want %v", got, want)
	}
	if got, want := result.Facts.Edges, []graph.Edge{
		{SourceID: left.ID, TargetID: contract.ID, Relation: "implements"},
		{SourceID: right.ID, TargetID: contract.ID, Relation: "implements"},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("common supporting edges = %+v, want %+v", got, want)
	}
	if got, want := result.TruncationReasons, []storage.TruncationReason{storage.TruncatedByNodeLimit}; !reflect.DeepEqual(got, want) {
		t.Errorf("truncation reasons = %v, want %v", got, want)
	}
	if got, want := result.Limits, []query.StageLimit{
		{Stage: "retrieval", SlotRole: "left", MaxNodes: 10},
		{Stage: "retrieval", SlotRole: "right", MaxNodes: 10},
		{Stage: "intersection", SlotRole: "left", MaxDepth: 2, MaxNodes: 3, TruncationReasons: []storage.TruncationReason{storage.TruncatedByNodeLimit}},
		{Stage: "intersection", SlotRole: "right", MaxDepth: 2, MaxNodes: 3},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("intersection limits = %+v, want %+v", got, want)
	}
	if len(result.Evidence) != 1 {
		t.Fatalf("intersection evidence = %+v, want one shared group", result.Evidence)
	}
	evidence := result.Evidence[0]
	if evidence.Rank != 1 || evidence.Relation != "implements" || evidence.Components.SharedSupport != 2 || evidence.Score <= 0 || !strings.Contains(evidence.Reason, "left and right") {
		t.Errorf("intersection evidence provenance = %+v, want two-slot shared support", evidence)
	}
	if got, want := nodeIDs(evidence.Nodes), []string{contract.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("intersection evidence nodes = %v, want %v", got, want)
	}
}

func TestQuerySnapshotReportsNoCommonSharedContractEvidenceWithoutAClaim(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	left := graph.Node{ID: "package:postgres", Kind: "package", Label: "postgres", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "storage/postgres"}}}
	right := graph.Node{ID: "package:sqlite", Kind: "package", Label: "sqlite", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "storage/sqlite"}}}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(_ context.Context, _ storage.Snapshot, identifier string) ([]storage.NodeMatch, error) {
			if identifier == "postgres" {
				return []storage.NodeMatch{{Node: left}}, nil
			}
			return []storage.NodeMatch{{Node: right}}, nil
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		return nil, nil
	})
	traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
		if got, want := request.Relations, []graph.RelationKind{"implements", "go:implements", "javascript:implements", "typescript:implements", "contains", "defines", "references"}; !reflect.DeepEqual(got, want) {
			t.Errorf("relations = %v, want %v", got, want)
		}
		if request.StartNodeIDs[0] == left.ID {
			return storage.TraversalResult{Facts: graph.Facts{Nodes: []graph.Node{left}}}, nil
		}
		return storage.TraversalResult{Facts: graph.Facts{Nodes: []graph.Node{right}}}, nil
	})
	plan := query.AnalyzeQuestion("what is the shared contract between postgres and sqlite")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
	if err != nil {
		t.Fatalf("execute shared-contract plan: %v", err)
	}
	if len(result.Facts.Nodes) != 0 || len(result.Facts.Edges) != 0 {
		t.Errorf("facts = %+v, want no unsupported shared-contract claim", result.Facts)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != "no_common_evidence" {
		t.Fatalf("warnings = %+v, want no_common_evidence", result.Warnings)
	}
	if got, want := result.Warnings[0].Suggestions, []string{"Look up package:postgres.", "Look up package:sqlite."}; !reflect.DeepEqual(got, want) {
		t.Errorf("suggestions = %v, want %v", got, want)
	}
	if len(result.Seeds) != 2 || result.Seeds[0].Role != "left" || result.Seeds[1].Role != "right" {
		t.Errorf("slot candidates = %+v, want retained left and right candidates", result.Seeds)
	}
}

func TestQuerySnapshotReportsNoCommonEvidenceWhenOneIntersectionSlotIsEmpty(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	right := graph.Node{ID: "package:sqlite", Kind: "package", Label: "sqlite", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "storage/sqlite"}}}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) {
			return nil, nil
		},
	}
	searcher := lexicalSearcherFunc(func(_ context.Context, _ storage.Snapshot, request storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		if request.Text == "sqlite" {
			return []storage.LexicalMatch{{Node: right, Score: 1}}, nil
		}
		return nil, nil
	})
	traverser := traverserFunc(func(context.Context, storage.Snapshot, storage.TraversalRequest) (storage.TraversalResult, error) {
		t.Fatal("intersection traversal ran with an empty entity slot")
		return storage.TraversalResult{}, nil
	})
	plan := query.AnalyzeQuestion("what is the shared contract between postgres and sqlite")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
	if err != nil {
		t.Fatalf("execute shared-contract plan: %v", err)
	}
	if len(result.Seeds) != 2 || len(result.Seeds[0].Nodes) != 0 || len(result.Seeds[1].Nodes) != 1 {
		t.Fatalf("slot candidates = %+v, want empty left and retained right candidates", result.Seeds)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != "entity_not_found" || !strings.Contains(result.Warnings[0].Message, "left slot") {
		t.Fatalf("warnings = %+v, want empty left-slot diagnostic", result.Warnings)
	}
	if got, want := result.Warnings[0].Suggestions, []string{"Use --terms with \"postgres\" for literal lookup."}; !reflect.DeepEqual(got, want) {
		t.Errorf("suggestions = %v, want %v", got, want)
	}
	if got, want := result.Limits, []query.StageLimit{
		{Stage: "retrieval", SlotRole: "left", MaxNodes: 10},
		{Stage: "retrieval", SlotRole: "right", MaxNodes: 10},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("limits = %+v, want %+v", got, want)
	}
}

func TestQuerySnapshotTreatsMatchingSourcePathsAsCommonEvidence(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	left := graph.Node{ID: "package:postgres", Kind: "package", Label: "postgres", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "storage/postgres"}}}
	right := graph.Node{ID: "package:sqlite", Kind: "package", Label: "sqlite", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "storage/sqlite"}}}
	leftContract := graph.Node{ID: "test:postgres-contract", Kind: "test", Label: "StorageContract", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "storage/conformance/contract_test.go"}}}
	rightContract := graph.Node{ID: "test:sqlite-contract", Kind: "test", Label: "StorageContract", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "storage/conformance/contract_test.go"}}}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(_ context.Context, _ storage.Snapshot, identifier string) ([]storage.NodeMatch, error) {
			if identifier == "postgres" {
				return []storage.NodeMatch{{Node: left}}, nil
			}
			return []storage.NodeMatch{{Node: right}}, nil
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		return nil, nil
	})
	traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
		if request.StartNodeIDs[0] == left.ID {
			return storage.TraversalResult{Facts: graph.Facts{
				Nodes: []graph.Node{left, leftContract},
				Edges: []graph.Edge{{SourceID: left.ID, TargetID: leftContract.ID, Relation: "contains"}},
			}}, nil
		}
		return storage.TraversalResult{Facts: graph.Facts{
			Nodes: []graph.Node{right, rightContract},
			Edges: []graph.Edge{{SourceID: right.ID, TargetID: rightContract.ID, Relation: "contains"}},
		}}, nil
	})
	plan := query.AnalyzeQuestion("what is the shared contract between postgres and sqlite")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
	if err != nil {
		t.Fatalf("execute shared-contract plan: %v", err)
	}
	if got, want := nodeIDs(result.Facts.Nodes), []string{leftContract.ID, rightContract.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("common source-path node IDs = %v, want %v", got, want)
	}
	if len(result.Facts.Edges) != 2 || len(result.Warnings) != 0 {
		t.Errorf("result = %+v, want both source-path support edges without a warning", result)
	}
}

func TestQuerySnapshotRejectsMatchingSourcePathsWithoutContractNodes(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	left := graph.Node{ID: "class:left", Kind: "class", Label: "LeftAdapter"}
	right := graph.Node{ID: "class:right", Kind: "class", Label: "RightAdapter"}
	leftHelper := graph.Node{ID: "function:left-helper", Kind: "function", Label: "leftHelper", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "shared/helpers.go"}}}
	rightHelper := graph.Node{ID: "function:right-helper", Kind: "function", Label: "rightHelper", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "shared/helpers.go"}}}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(_ context.Context, _ storage.Snapshot, identifier string) ([]storage.NodeMatch, error) {
			if identifier == "postgres" {
				return []storage.NodeMatch{{Node: left}}, nil
			}
			return []storage.NodeMatch{{Node: right}}, nil
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		return nil, nil
	})
	traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
		if request.StartNodeIDs[0] == left.ID {
			return storage.TraversalResult{Facts: graph.Facts{Nodes: []graph.Node{left, leftHelper}, Edges: []graph.Edge{{SourceID: left.ID, TargetID: leftHelper.ID, Relation: "contains"}}}}, nil
		}
		return storage.TraversalResult{Facts: graph.Facts{Nodes: []graph.Node{right, rightHelper}, Edges: []graph.Edge{{SourceID: right.ID, TargetID: rightHelper.ID, Relation: "contains"}}}}, nil
	})
	plan := query.AnalyzeQuestion("what is the shared contract between postgres and sqlite")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
	if err != nil {
		t.Fatalf("execute shared-contract plan: %v", err)
	}
	if len(result.Evidence) != 0 || len(result.Warnings) != 1 || result.Warnings[0].Code != "no_common_evidence" {
		t.Errorf("result = %+v, want no_common_evidence", result)
	}
}

func TestQuerySnapshotTreatsMatchingContractIdentitiesAsCommonEvidence(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	left := graph.Node{ID: "package:postgres", Kind: "package", Label: "postgres"}
	right := graph.Node{ID: "package:sqlite", Kind: "package", Label: "sqlite"}
	leftContract := graph.Node{ID: "go:interface:postgres-store", Kind: "go:interface", Label: "Store", QualifiedName: "storage.Store", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "storage/postgres/store.go"}}}
	rightContract := graph.Node{ID: "typescript:interface:sqlite-store", Kind: "typescript:interface", Label: "Store", QualifiedName: "storage.Store", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "storage/sqlite/store.ts"}}}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(_ context.Context, _ storage.Snapshot, identifier string) ([]storage.NodeMatch, error) {
			if identifier == "postgres" {
				return []storage.NodeMatch{{Node: left}}, nil
			}
			return []storage.NodeMatch{{Node: right}}, nil
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		return nil, nil
	})
	traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
		if request.StartNodeIDs[0] == left.ID {
			return storage.TraversalResult{Facts: graph.Facts{
				Nodes: []graph.Node{left, leftContract},
				Edges: []graph.Edge{{SourceID: left.ID, TargetID: leftContract.ID, Relation: "implements"}},
			}}, nil
		}
		return storage.TraversalResult{Facts: graph.Facts{
			Nodes: []graph.Node{right, rightContract},
			Edges: []graph.Edge{{SourceID: right.ID, TargetID: rightContract.ID, Relation: "implements"}},
		}}, nil
	})
	plan := query.AnalyzeQuestion("what is the shared contract between postgres and sqlite")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
	if err != nil {
		t.Fatalf("execute shared-contract plan: %v", err)
	}
	if got, want := nodeIDs(result.Facts.Nodes), []string{leftContract.ID, rightContract.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("common contract node IDs = %v, want %v", got, want)
	}
	if len(result.Facts.Edges) != 2 || len(result.Warnings) != 0 {
		t.Errorf("result = %+v, want both contract support edges without a warning", result)
	}
}

func TestQuerySnapshotRejectsMatchingInterfaceLabelsWithDifferentQualifiedNames(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	left := graph.Node{ID: "class:left", Kind: "class", Label: "LeftAdapter"}
	right := graph.Node{ID: "class:right", Kind: "class", Label: "RightAdapter"}
	leftContract := graph.Node{ID: "interface:left-store", Kind: "interface", Label: "Store", QualifiedName: "left.Store", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "left/store.go"}}}
	rightContract := graph.Node{ID: "interface:right-store", Kind: "interface", Label: "Store", QualifiedName: "right.Store", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "right/store.go"}}}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(_ context.Context, _ storage.Snapshot, identifier string) ([]storage.NodeMatch, error) {
			if identifier == "postgres" {
				return []storage.NodeMatch{{Node: left}}, nil
			}
			return []storage.NodeMatch{{Node: right}}, nil
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		return nil, nil
	})
	traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
		if request.StartNodeIDs[0] == left.ID {
			return storage.TraversalResult{Facts: graph.Facts{Nodes: []graph.Node{left, leftContract}, Edges: []graph.Edge{{SourceID: left.ID, TargetID: leftContract.ID, Relation: "implements"}}}}, nil
		}
		return storage.TraversalResult{Facts: graph.Facts{Nodes: []graph.Node{right, rightContract}, Edges: []graph.Edge{{SourceID: right.ID, TargetID: rightContract.ID, Relation: "implements"}}}}, nil
	})
	plan := query.AnalyzeQuestion("what is the shared contract between postgres and sqlite")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
	if err != nil {
		t.Fatalf("execute shared-contract plan: %v", err)
	}
	if len(result.Evidence) != 0 || len(result.Warnings) != 1 || result.Warnings[0].Code != "no_common_evidence" {
		t.Errorf("result = %+v, want no_common_evidence", result)
	}
}

func TestQuerySnapshotUsesReferencedContractTestSuitesAsSharedEvidence(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	left := graph.Node{ID: "class:left", Kind: "typescript:class", Label: "LeftStore"}
	right := graph.Node{ID: "class:right", Kind: "typescript:class", Label: "RightStore"}
	leftAlternative := graph.Node{ID: "project:left", Kind: "project", Label: "LeftProject"}
	rightAlternative := graph.Node{ID: "project:right", Kind: "project", Label: "RightProject"}
	leftUse := graph.Node{ID: "function:left-use", Kind: "typescript:function", Label: "openLeftStore"}
	rightUse := graph.Node{ID: "function:right-use", Kind: "typescript:function", Label: "openRightStore"}
	leftSuite := graph.Node{ID: "file:left-contract", Kind: "file", Label: "packages/left/storage-driver-contract.test.ts", QualifiedName: "packages/left/storage-driver-contract.test.ts", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "packages/left/storage-driver-contract.test.ts"}}}
	rightSuite := graph.Node{ID: "file:right-contract", Kind: "file", Label: "packages/right/storage-driver-contract.test.ts", QualifiedName: "packages/right/storage-driver-contract.test.ts", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "packages/right/storage-driver-contract.test.ts"}}}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(_ context.Context, _ storage.Snapshot, identifier string) ([]storage.NodeMatch, error) {
			if identifier == "postgres" {
				return []storage.NodeMatch{{Node: left}, {Node: leftAlternative}}, nil
			}
			return []storage.NodeMatch{{Node: right}, {Node: rightAlternative}}, nil
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		return nil, nil
	})
	traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
		if !slices.Contains(request.Relations, graph.RelationKind("references")) {
			t.Errorf("relations = %v, want references", request.Relations)
		}
		if request.StartNodeIDs[0] == left.ID {
			return storage.TraversalResult{Facts: graph.Facts{Nodes: []graph.Node{left, leftUse, leftSuite}, Edges: []graph.Edge{{SourceID: leftUse.ID, TargetID: left.ID, Relation: "references"}, {SourceID: leftSuite.ID, TargetID: leftUse.ID, Relation: "contains"}}}}, nil
		}
		return storage.TraversalResult{Facts: graph.Facts{Nodes: []graph.Node{right, rightUse, rightSuite}, Edges: []graph.Edge{{SourceID: rightUse.ID, TargetID: right.ID, Relation: "references"}, {SourceID: rightSuite.ID, TargetID: rightUse.ID, Relation: "contains"}}}}, nil
	})
	plan := query.AnalyzeQuestion("what is the shared contract between postgres and sqlite")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
	if err != nil {
		t.Fatalf("execute shared-contract plan: %v", err)
	}
	if got, want := nodeIDs(result.Facts.Nodes), []string{leftSuite.ID, rightSuite.ID, leftUse.ID, rightUse.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("contract suite nodes = %v, want %v", got, want)
	}
	if len(result.Seeds[0].Nodes) != 2 || len(result.Seeds[1].Nodes) != 2 {
		t.Errorf("seeds = %+v, want retained alternative candidates", result.Seeds)
	}
	if len(result.Evidence) != 1 || len(result.Evidence[0].Edges) != 4 || len(result.Warnings) != 0 {
		t.Errorf("result = %+v, want referenced shared contract-suite evidence", result)
	}
}

func TestQuerySnapshotRejectsMatchingMethodsWithoutSharedContractProof(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	left := graph.Node{ID: "class:postgres", Kind: "typescript:class", Label: "PgStore"}
	right := graph.Node{ID: "class:sqlite", Kind: "typescript:class", Label: "SqliteStore"}
	leftMethod := graph.Node{ID: "method:postgres-create", Kind: "typescript:method", Label: "createEntity", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "storage/postgres/store.ts"}}}
	rightMethod := graph.Node{ID: "method:sqlite-create", Kind: "typescript:method", Label: "createEntity", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "storage/sqlite/store.ts"}}}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(_ context.Context, _ storage.Snapshot, identifier string) ([]storage.NodeMatch, error) {
			if identifier == "postgres" {
				return []storage.NodeMatch{{Node: left}}, nil
			}
			return []storage.NodeMatch{{Node: right}}, nil
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		return nil, nil
	})
	traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
		if !slices.Contains(request.Relations, graph.RelationKind("defines")) {
			t.Errorf("relations = %v, want defines", request.Relations)
		}
		if request.StartNodeIDs[0] == left.ID {
			return storage.TraversalResult{Facts: graph.Facts{
				Nodes: []graph.Node{left, leftMethod},
				Edges: []graph.Edge{{SourceID: left.ID, TargetID: leftMethod.ID, Relation: "defines"}},
			}}, nil
		}
		return storage.TraversalResult{Facts: graph.Facts{
			Nodes: []graph.Node{right, rightMethod},
			Edges: []graph.Edge{{SourceID: right.ID, TargetID: rightMethod.ID, Relation: "defines"}},
		}}, nil
	})
	plan := query.AnalyzeQuestion("what is the shared contract between postgres and sqlite")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
	if err != nil {
		t.Fatalf("execute shared-contract plan: %v", err)
	}
	if len(result.Facts.Nodes) != 0 || len(result.Facts.Edges) != 0 || len(result.Evidence) != 0 {
		t.Errorf("result = %+v, want no shared-contract evidence", result)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != "no_common_evidence" {
		t.Errorf("warnings = %+v, want no_common_evidence", result.Warnings)
	}
}

func TestQuerySnapshotExecutesCalledByPlanWithIncomingCallTraversal(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	target := graph.Node{ID: "function:target", Kind: "function", Label: "target"}
	caller := graph.Node{ID: "function:caller", Kind: "function", Label: "caller"}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			t.Fatal("broad lookup is called after an exact match")
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) {
			return []storage.NodeMatch{{Node: target}}, nil
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		t.Fatal("lexical search is called after an exact match")
		return nil, nil
	})
	traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
		if request.Direction != storage.TraverseIncoming {
			t.Errorf("direction = %q, want incoming", request.Direction)
		}
		if got, want := request.Relations, []graph.RelationKind{"calls", "go:calls", "javascript:calls", "typescript:calls"}; !reflect.DeepEqual(got, want) {
			t.Errorf("relations = %v, want %v", got, want)
		}
		return storage.TraversalResult{Facts: graph.Facts{
			Nodes: []graph.Node{target, caller},
			Edges: []graph.Edge{{SourceID: caller.ID, TargetID: target.ID, Relation: "calls"}},
		}}, nil
	})
	plan := query.AnalyzeQuestion("who calls target")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
	if err != nil {
		t.Fatalf("execute called-by plan: %v", err)
	}
	if len(result.Facts.Edges) != 1 || result.Facts.Edges[0].SourceID != caller.ID {
		t.Errorf("facts = %+v, want caller evidence", result.Facts)
	}
}

func TestQuerySnapshotExecutesCallsPlanWithOutgoingCallTraversal(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	caller := graph.Node{ID: "function:caller", Kind: "function", Label: "caller"}
	callee := graph.Node{ID: "function:callee", Kind: "function", Label: "callee"}
	distant := graph.Node{ID: "function:distant", Kind: "function", Label: "distant"}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			t.Fatal("broad lookup is called after an exact match")
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) {
			return []storage.NodeMatch{{Node: caller}}, nil
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		t.Fatal("lexical search is called after an exact match")
		return nil, nil
	})
	traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
		if request.Direction != storage.TraverseOutgoing {
			t.Errorf("direction = %q, want outgoing", request.Direction)
		}
		if got, want := request.Relations, []graph.RelationKind{"calls", "go:calls", "javascript:calls", "typescript:calls"}; !reflect.DeepEqual(got, want) {
			t.Errorf("relations = %v, want %v", got, want)
		}
		if request.MaxDepth != 2 || request.MaxNodes != 100 {
			t.Errorf("limits = {%d, %d}, want {2, 100}", request.MaxDepth, request.MaxNodes)
		}
		return storage.TraversalResult{Facts: graph.Facts{
			Nodes: []graph.Node{distant, caller, callee},
			Edges: []graph.Edge{
				{SourceID: callee.ID, TargetID: distant.ID, Relation: "calls"},
				{SourceID: caller.ID, TargetID: callee.ID, Relation: "calls"},
			},
		}, TruncationReasons: []storage.TruncationReason{storage.TruncatedByNodeLimit}}, nil
	})
	plan := query.AnalyzeQuestion("what does caller call")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{
		Plan:      &plan,
		Relations: []graph.RelationKind{"contains"},
		MaxDepth:  plan.MaxDepth,
		MaxNodes:  plan.MaxNodes,
	})
	if err != nil {
		t.Fatalf("execute calls plan: %v", err)
	}
	if len(result.Facts.Edges) != 2 {
		t.Errorf("facts = %+v, want callee evidence", result.Facts)
	}
	if got, want := result.TruncationReasons, []storage.TruncationReason{storage.TruncatedByNodeLimit}; !reflect.DeepEqual(got, want) {
		t.Errorf("truncation reasons = %v, want %v", got, want)
	}
	if len(result.Evidence) != 2 {
		t.Fatalf("evidence groups = %+v, want direct and distant calls groups", result.Evidence)
	}
	evidence := result.Evidence[0]
	if evidence.Rank != 1 || evidence.Score <= result.Evidence[1].Score || evidence.Distance != 1 || evidence.Reason == "" || evidence.SlotRole != "caller" || evidence.Relation != "calls" {
		t.Errorf("evidence provenance = %+v, want ranked caller calls evidence", evidence)
	}
	if evidence.Components.SeedRelevance <= 0 || evidence.Components.Distance <= 0 || evidence.Components.RelationFit <= 0 || evidence.Components.Confidence <= 0 || evidence.Components.SharedSupport != 1 || evidence.Components.PathDiversity != 1 {
		t.Errorf("evidence score components = %+v, want all standard traversal inputs", evidence.Components)
	}
	if got, want := nodeIDs(evidence.Nodes), []string{caller.ID, callee.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("evidence node IDs = %v, want %v", got, want)
	}
	if got, want := evidence.Edges, []graph.Edge{{SourceID: caller.ID, TargetID: callee.ID, Relation: "calls"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("evidence edges = %+v, want %+v", got, want)
	}
	if result.Evidence[1].Rank != 2 || result.Evidence[1].Distance != 2 || result.Evidence[1].Nodes[1].ID != distant.ID {
		t.Errorf("distant evidence = %+v, want second-rank distance-two call", result.Evidence[1])
	}
	if got, want := result.Limits, []query.StageLimit{
		{Stage: "retrieval", SlotRole: "caller", MaxNodes: 10},
		{Stage: "traversal", MaxDepth: 2, MaxNodes: 100, TruncationReasons: []storage.TruncationReason{storage.TruncatedByNodeLimit}},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("stage limits = %+v, want %+v", got, want)
	}
}

func TestQuerySnapshotExpandsLanguageSpecificCallRelationsForNeighbors(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	caller := graph.Node{ID: "function:caller", Kind: "function", Label: "caller"}
	callee := graph.Node{ID: "function:callee", Kind: "function", Label: "callee"}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			t.Fatal("broad lookup is called after an exact match")
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) {
			return []storage.NodeMatch{{Node: caller}}, nil
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		t.Fatal("lexical search is called after an exact match")
		return nil, nil
	})
	traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
		wantRelations := []graph.RelationKind{"calls", "go:calls", "javascript:calls", "typescript:calls"}
		if !reflect.DeepEqual(request.Relations, wantRelations) {
			t.Errorf("relations = %v, want %v", request.Relations, wantRelations)
		}
		return storage.TraversalResult{Facts: graph.Facts{
			Nodes: []graph.Node{caller, callee},
			Edges: []graph.Edge{{SourceID: caller.ID, TargetID: callee.ID, Relation: "go:calls"}},
		}}, nil
	})
	plan := query.AnalyzeQuestion("what does caller call")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
	if err != nil {
		t.Fatalf("execute calls plan: %v", err)
	}
	if len(result.Evidence) != 1 || result.Evidence[0].Relation != "go:calls" {
		t.Fatalf("evidence = %+v, want one language-specific call group", result.Evidence)
	}
}

func TestQuerySnapshotExecutesDependencyPlansWithDirectionCorrectImportEvidence(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	packageNode := graph.Node{ID: "package:query", Kind: "package", Label: "query"}
	dependency := graph.Node{ID: "package:storage", Kind: "package", Label: "storage"}
	dependent := graph.Node{ID: "package:cli", Kind: "package", Label: "cli"}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			t.Fatal("broad lookup is called after an exact match")
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) {
			return []storage.NodeMatch{{Node: packageNode}}, nil
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		t.Fatal("lexical search is called after an exact match")
		return nil, nil
	})
	execute := func(question string) query.Result {
		t.Helper()
		traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
			if request.MaxDepth != 2 || request.MaxNodes != 3 {
				t.Errorf("limits = {%d, %d}, want {2, 3}", request.MaxDepth, request.MaxNodes)
			}
			allowed := make(map[graph.RelationKind]bool, len(request.Relations))
			for _, relation := range request.Relations {
				allowed[relation] = true
			}
			facts := graph.Facts{Nodes: []graph.Node{packageNode}}
			for _, edge := range []graph.Edge{
				{SourceID: packageNode.ID, TargetID: dependency.ID, Relation: "go:imports_from"},
				{SourceID: dependent.ID, TargetID: packageNode.ID, Relation: "typescript:imports_from"},
				{SourceID: packageNode.ID, TargetID: dependent.ID, Relation: "go:calls"},
				{SourceID: packageNode.ID, TargetID: dependency.ID, Relation: "contains"},
			} {
				if !allowed[edge.Relation] {
					continue
				}
				if request.Direction == storage.TraverseOutgoing && edge.SourceID == packageNode.ID {
					facts.Nodes = append(facts.Nodes, dependency)
					facts.Edges = append(facts.Edges, edge)
				}
				if request.Direction == storage.TraverseIncoming && edge.TargetID == packageNode.ID {
					facts.Nodes = append(facts.Nodes, dependent)
					facts.Edges = append(facts.Edges, edge)
				}
			}
			return storage.TraversalResult{Facts: facts, TruncationReasons: []storage.TruncationReason{storage.TruncatedByNodeLimit}}, nil
		})
		plan := query.AnalyzeQuestion(question)
		result, err := query.QuerySnapshot(context.Background(), struct {
			exactNodeLookup
			lexicalSearcherFunc
		}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: 2, MaxNodes: 3})
		if err != nil {
			t.Fatalf("execute dependency plan: %v", err)
		}
		return result
	}

	dependencies := execute("which packages does query depend on")
	if got, want := nodeIDs(dependencies.Facts.Nodes), []string{packageNode.ID, dependency.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("dependency node IDs = %v, want %v", got, want)
	}
	if got, want := dependencies.Facts.Edges, []graph.Edge{{SourceID: packageNode.ID, TargetID: dependency.ID, Relation: "go:imports_from"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("dependency edges = %+v, want %+v", got, want)
	}

	dependents := execute("which packages use query")
	if got, want := nodeIDs(dependents.Facts.Nodes), []string{packageNode.ID, dependent.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("dependent node IDs = %v, want %v", got, want)
	}
	if got, want := dependents.Facts.Edges, []graph.Edge{{SourceID: dependent.ID, TargetID: packageNode.ID, Relation: "typescript:imports_from"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("dependent edges = %+v, want %+v", got, want)
	}
	if got, want := dependents.TruncationReasons, []storage.TruncationReason{storage.TruncatedByNodeLimit}; !reflect.DeepEqual(got, want) {
		t.Errorf("truncation reasons = %v, want %v", got, want)
	}
}

func TestQuerySnapshotExecutesImpactPlanWithRankedIncomingEvidence(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	changed := graph.Node{ID: "function:changed", Kind: "function", Label: "changed"}
	direct := graph.Node{ID: "function:direct", Kind: "function", Label: "direct"}
	distant := graph.Node{ID: "function:distant", Kind: "function", Label: "distant"}
	container := graph.Node{ID: "file:container", Kind: "file", Label: "container"}
	outgoing := graph.Node{ID: "function:outgoing", Kind: "function", Label: "outgoing"}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			t.Fatal("broad lookup is called after an exact match")
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) {
			return []storage.NodeMatch{{Node: changed}}, nil
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		t.Fatal("lexical search is called after an exact match")
		return nil, nil
	})
	traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
		if got, want := request.StartNodeIDs, []string{changed.ID}; !reflect.DeepEqual(got, want) {
			t.Errorf("start node IDs = %v, want %v", got, want)
		}
		if request.Direction != storage.TraverseIncoming {
			t.Errorf("direction = %q, want incoming", request.Direction)
		}
		if got, want := request.Relations, []graph.RelationKind{"references", "implements", "go:implements", "javascript:implements", "typescript:implements", "contains", "calls", "go:calls", "javascript:calls", "typescript:calls", "imports_from", "go:imports_from", "javascript:imports_from", "typescript:imports_from", "requires", "go:requires", "javascript:requires", "typescript:requires", "depends_on", "go:depends_on", "javascript:depends_on", "typescript:depends_on"}; !reflect.DeepEqual(got, want) {
			t.Errorf("relations = %v, want %v", got, want)
		}
		return storage.TraversalResult{Facts: graph.Facts{
			Nodes: []graph.Node{container, distant, changed, direct, outgoing},
			Edges: []graph.Edge{
				{SourceID: direct.ID, TargetID: changed.ID, Relation: "references"},
				{SourceID: distant.ID, TargetID: direct.ID, Relation: "references"},
				{SourceID: container.ID, TargetID: changed.ID, Relation: "contains"},
				{SourceID: changed.ID, TargetID: direct.ID, Relation: "calls"},
				{SourceID: changed.ID, TargetID: outgoing.ID, Relation: "depends_on"},
			},
		}, TruncationReasons: []storage.TruncationReason{storage.TruncatedByNodeLimit}}, nil
	})
	plan := query.AnalyzeQuestion("what is affected if changed changes")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
	if err != nil {
		t.Fatalf("execute impact plan: %v", err)
	}
	if got, want := impactNodeIDs(result.Impact), []string{direct.ID, distant.ID, container.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("ranked impact IDs = %v, want %v", got, want)
	}
	if result.Impact[0].Distance != 1 || result.Impact[1].Distance != 2 || result.Impact[2].Distance != 1 {
		t.Errorf("impact distances = %+v, want direct 1, distant 2, container 1", result.Impact)
	}
	if !(result.Impact[0].Score > result.Impact[1].Score && result.Impact[1].Score > result.Impact[2].Score) {
		t.Errorf("impact scores = %+v, want direct above distant above containment", result.Impact)
	}
	if got, want := result.TruncationReasons, []storage.TruncationReason{storage.TruncatedByNodeLimit}; !reflect.DeepEqual(got, want) {
		t.Errorf("truncation reasons = %v, want %v", got, want)
	}
}

func TestQuerySnapshotReportsAmbiguousExplainCandidatesWithoutTraversal(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	first := graph.Node{ID: "function:first", Kind: "function", Label: "helper", QualifiedName: "app::helper", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "app/helper.go"}}}
	second := graph.Node{ID: "function:second", Kind: "function", Label: "helper", QualifiedName: "other::helper", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "other/helper.go"}}}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) { return nil, nil },
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		return []storage.LexicalMatch{{Node: first, Score: 2}, {Node: second, Score: 1}}, nil
	})
	traverser := traverserFunc(func(context.Context, storage.Snapshot, storage.TraversalRequest) (storage.TraversalResult, error) {
		t.Fatal("ambiguous explain plan traversed the graph")
		return storage.TraversalResult{}, nil
	})
	plan := query.AnalyzeQuestion("explain helper")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{
		Plan:     &plan,
		MaxDepth: plan.MaxDepth,
		MaxNodes: plan.MaxNodes,
	})
	if err != nil {
		t.Fatalf("execute explain plan: %v", err)
	}
	if got, want := nodeIDs(result.Seeds[0].Nodes), []string{first.ID, second.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("candidate IDs = %v, want %v", got, want)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != "ambiguous_entity" {
		t.Fatalf("warnings = %+v, want ambiguous_entity", result.Warnings)
	}
	if got, want := result.Warnings[0].Suggestions, []string{"Explain function:first.", "Explain function:second."}; !reflect.DeepEqual(got, want) {
		t.Errorf("suggestions = %v, want %v", got, want)
	}
	if len(result.Facts.Nodes) != 0 || len(result.Facts.Edges) != 0 {
		t.Errorf("facts = %+v, want no answer evidence", result.Facts)
	}
}

func TestQuerySnapshotLookupReturnsRankedCandidatesWithoutTraversal(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	candidate := graph.Node{ID: "function:lookup", Kind: "function", Label: "Lookup", QualifiedName: "query.Lookup", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "query/lookup.go", StartLine: 12}}}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			t.Fatal("broad lookup is called after an exact match")
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) {
			return []storage.NodeMatch{{Node: candidate}}, nil
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		t.Fatal("lexical search is called after an exact match")
		return nil, nil
	})
	traverser := traverserFunc(func(context.Context, storage.Snapshot, storage.TraversalRequest) (storage.TraversalResult, error) {
		t.Fatal("lookup plan traversed the graph")
		return storage.TraversalResult{}, nil
	})
	plan := query.AnalyzeQuestion("where is query.Lookup")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
	if err != nil {
		t.Fatalf("execute lookup plan: %v", err)
	}
	if len(result.Seeds) != 1 || len(result.Seeds[0].Nodes) != 1 || result.Seeds[0].Nodes[0].Evidence.Span.Path != "query/lookup.go" {
		t.Fatalf("lookup candidates = %+v, want source-backed exact candidate", result.Seeds)
	}
	if len(result.Warnings) != 0 || len(result.Facts.Nodes) != 0 || len(result.Facts.Edges) != 0 {
		t.Errorf("lookup result = %+v, want candidates without warnings or neighborhood facts", result)
	}
}

func TestQuerySnapshotLookupReportsUnresolvedEntitiesWithoutTraversal(t *testing.T) {
	first := graph.Node{ID: "function:first", Kind: "function", Label: "helper", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "first/helper.go"}}}
	second := graph.Node{ID: "function:second", Kind: "function", Label: "helper", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "second/helper.go"}}}
	tests := []struct {
		name     string
		matches  []storage.LexicalMatch
		wantCode string
	}{
		{name: "absent", wantCode: "entity_not_found"},
		{name: "ambiguous", matches: []storage.LexicalMatch{{Node: first, Score: 2}, {Node: second, Score: 1}}, wantCode: "ambiguous_entity"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
			lookup := exactNodeLookup{
				nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
					return nil, nil
				}),
				exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) { return nil, nil },
			}
			searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
				return test.matches, nil
			})
			traverser := traverserFunc(func(context.Context, storage.Snapshot, storage.TraversalRequest) (storage.TraversalResult, error) {
				t.Fatal("unresolved lookup plan traversed the graph")
				return storage.TraversalResult{}, nil
			})
			plan := query.AnalyzeQuestion("where is helper")
			result, err := query.QuerySnapshot(context.Background(), struct {
				exactNodeLookup
				lexicalSearcherFunc
			}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
			if err != nil {
				t.Fatalf("execute lookup plan: %v", err)
			}
			if len(result.Warnings) != 1 || result.Warnings[0].Code != test.wantCode || len(result.Warnings[0].Suggestions) == 0 {
				t.Errorf("warnings = %+v, want %s with a next command", result.Warnings, test.wantCode)
			}
			if len(result.Seeds) != 1 || len(result.Seeds[0].Nodes) != len(test.matches) {
				t.Errorf("candidates = %+v, want %d ranked candidates", result.Seeds, len(test.matches))
			}
		})
	}
}

func TestQuerySnapshotCalledByReportsEmptyEntitySlotWithoutTraversal(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) { return nil, nil },
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		return nil, nil
	})
	traverser := traverserFunc(func(context.Context, storage.Snapshot, storage.TraversalRequest) (storage.TraversalResult, error) {
		t.Fatal("called-by plan with an empty entity slot traversed the graph")
		return storage.TraversalResult{}, nil
	})
	plan := query.AnalyzeQuestion("who calls lunarTelemetryGateway?")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
	if err != nil {
		t.Fatalf("execute called-by plan: %v", err)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != "entity_not_found" || len(result.Warnings[0].Suggestions) == 0 {
		t.Fatalf("warnings = %+v, want entity_not_found with a next command", result.Warnings)
	}
	if len(result.Seeds) != 1 || result.Seeds[0].Role != "callee" || len(result.Seeds[0].Nodes) != 0 {
		t.Fatalf("seeds = %+v, want an empty callee slot", result.Seeds)
	}
	if len(result.Limits) != 1 || result.Limits[0].SlotRole != "callee" {
		t.Fatalf("limits = %+v, want the callee retrieval limit", result.Limits)
	}
}

func TestQuerySnapshotOperatorsReportAllEmptyEntitySlotsWithoutTraversal(t *testing.T) {
	tests := []struct {
		name      string
		question  string
		wantRoles []string
	}{
		{name: "dependency", question: "what does the lunarTelemetryGateway package import?", wantRoles: []string{"dependent"}},
		{name: "impact", question: "what is affected if lunarTelemetryGateway changes?", wantRoles: []string{"changed"}},
		{name: "shared contract", question: "what is the shared contract between lunarBackend and solarAdapter", wantRoles: []string{"left", "right"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
			lookup := exactNodeLookup{
				nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
					return nil, nil
				}),
				exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) { return nil, nil },
			}
			searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
				return nil, nil
			})
			traverser := traverserFunc(func(context.Context, storage.Snapshot, storage.TraversalRequest) (storage.TraversalResult, error) {
				t.Fatalf("%s plan with empty entity slots traversed the graph", test.name)
				return storage.TraversalResult{}, nil
			})
			plan := query.AnalyzeQuestion(test.question)

			result, err := query.QuerySnapshot(context.Background(), struct {
				exactNodeLookup
				lexicalSearcherFunc
			}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
			if err != nil {
				t.Fatalf("execute %s plan: %v", test.name, err)
			}
			if len(result.Warnings) != len(test.wantRoles) || len(result.Seeds) != len(test.wantRoles) || len(result.Limits) != len(test.wantRoles) {
				t.Fatalf("result = %+v, want one warning, seed set, and limit per role %v", result, test.wantRoles)
			}
			for index, role := range test.wantRoles {
				if result.Warnings[index].Code != "entity_not_found" || !strings.Contains(result.Warnings[index].Message, role+" slot") || len(result.Warnings[index].Suggestions) == 0 {
					t.Errorf("warning %d = %+v, want actionable %s-slot warning", index, result.Warnings[index], role)
				}
				if result.Seeds[index].Role != role || len(result.Seeds[index].Nodes) != 0 || result.Limits[index].SlotRole != role {
					t.Errorf("slot %d = seed %+v, limit %+v; want empty %s slot", index, result.Seeds[index], result.Limits[index], role)
				}
			}
			if len(result.Facts.Nodes) != 0 || len(result.Facts.Edges) != 0 || len(result.Evidence) != 0 || len(result.Impact) != 0 || len(result.TruncationReasons) != 0 || result.ScopeBoundary != nil {
				t.Fatalf("result = %+v, want no answer or traversal evidence", result)
			}
		})
	}
}

func TestQuerySnapshotExplainReturnsDirectTypedNeighborhoodForExactEntity(t *testing.T) {
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	candidate := graph.Node{ID: "function:lookup", Kind: "function", Label: "Lookup", QualifiedName: "query.Lookup", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "query/lookup.go", StartLine: 12}}}
	caller := graph.Node{ID: "function:caller", Kind: "function", Label: "Caller"}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			t.Fatal("broad lookup is called after an exact match")
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) {
			return []storage.NodeMatch{{Node: candidate}}, nil
		},
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		t.Fatal("lexical search is called after an exact match")
		return nil, nil
	})
	traverser := traverserFunc(func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
		if got, want := request.StartNodeIDs, []string{candidate.ID}; !reflect.DeepEqual(got, want) {
			t.Errorf("start node IDs = %v, want %v", got, want)
		}
		if request.Direction != storage.TraverseBoth || request.MaxDepth != 1 {
			t.Errorf("traversal = %+v, want direct traversal in both directions", request)
		}
		return storage.TraversalResult{Facts: graph.Facts{
			Nodes: []graph.Node{candidate, caller},
			Edges: []graph.Edge{{SourceID: caller.ID, TargetID: candidate.ID, Relation: "calls"}},
		}}, nil
	})
	plan := query.AnalyzeQuestion("explain query.Lookup")

	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
	if err != nil {
		t.Fatalf("execute explain plan: %v", err)
	}
	if len(result.Warnings) != 0 || len(result.Facts.Nodes) != 2 || len(result.Facts.Edges) != 1 || result.Facts.Edges[0].Relation != "calls" {
		t.Errorf("explain result = %+v, want direct typed supporting facts", result)
	}
}

func TestQuerySnapshotReportsAbsentExplainEntityWithoutTraversal(t *testing.T) {
	result := executeExplainPlanWithMatches(t, "explain helper", nil, nil)
	if len(result.Seeds) != 1 || len(result.Seeds[0].Nodes) != 0 {
		t.Fatalf("candidates = %+v, want an empty entity slot", result.Seeds)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != "entity_not_found" || len(result.Warnings[0].Suggestions) == 0 {
		t.Fatalf("warnings = %+v, want entity_not_found with a next command", result.Warnings)
	}
}

func TestQuerySnapshotReportsWeakExplainCandidateWithoutTraversal(t *testing.T) {
	weak := graph.Node{ID: "function:possible", Kind: "function", Label: "helperMaybe", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "query/helper.go"}}}
	result := executeExplainPlanWithMatches(t, "explain helper service", []storage.LexicalMatch{{Node: weak, Score: 1}}, nil)
	if got, want := nodeIDs(result.Seeds[0].Nodes), []string{weak.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate IDs = %v, want %v", got, want)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != "weak_entity_match" || len(result.Warnings[0].Suggestions) == 0 {
		t.Fatalf("warnings = %+v, want weak_entity_match with a next command", result.Warnings)
	}
}

func TestQuerySnapshotExplainsUniqueFullCoverageLexicalCandidate(t *testing.T) {
	candidate := graph.Node{ID: "function:lookup", Kind: "function", Label: "LookupExactNodes", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "query/rank.go"}}}
	result := executeExplainPlanWithMatches(t, "explain LookupExactNodes", []storage.LexicalMatch{{Node: candidate, Score: 1}}, func(request storage.TraversalRequest) storage.TraversalResult {
		return storage.TraversalResult{Facts: graph.Facts{Nodes: []graph.Node{candidate}}}
	})
	if len(result.Warnings) != 0 || len(result.Facts.Nodes) != 1 || result.Facts.Nodes[0].ID != candidate.ID {
		t.Errorf("explain result = %+v, want unique full-coverage lexical evidence", result)
	}
}

func executeExplainPlanWithMatches(t *testing.T, question string, matches []storage.LexicalMatch, traverse func(storage.TraversalRequest) storage.TraversalResult) query.Result {
	t.Helper()
	snapshot := storage.Snapshot{Workspace: "workspace", Version: 7}
	lookup := exactNodeLookup{
		nodeLookupFunc: nodeLookupFunc(func(context.Context, storage.Snapshot, storage.NodeLookupRequest) ([]storage.NodeMatch, error) {
			return nil, nil
		}),
		exact: func(context.Context, storage.Snapshot, string) ([]storage.NodeMatch, error) { return nil, nil },
	}
	searcher := lexicalSearcherFunc(func(context.Context, storage.Snapshot, storage.LexicalSearchRequest) ([]storage.LexicalMatch, error) {
		return matches, nil
	})
	traverser := traverserFunc(func(context.Context, storage.Snapshot, storage.TraversalRequest) (storage.TraversalResult, error) {
		if traverse == nil {
			t.Fatal("unresolved explain plan traversed the graph")
		}
		return storage.TraversalResult{}, nil
	})
	if traverse != nil {
		traverser = func(_ context.Context, _ storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
			return traverse(request), nil
		}
	}
	plan := query.AnalyzeQuestion(question)
	result, err := query.QuerySnapshot(context.Background(), struct {
		exactNodeLookup
		lexicalSearcherFunc
	}{exactNodeLookup: lookup, lexicalSearcherFunc: searcher}, traverser, snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
	if err != nil {
		t.Fatalf("execute explain plan: %v", err)
	}
	return result
}

func impactNodeIDs(evidence []query.ImpactEvidence) []string {
	ids := make([]string, len(evidence))
	for index, item := range evidence {
		ids[index] = item.Node.ID
	}
	return ids
}
