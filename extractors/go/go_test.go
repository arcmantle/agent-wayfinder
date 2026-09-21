package goextractor

import (
	"context"
	"testing"

	"agent-wayfinder/extractor"
	"agent-wayfinder/graph"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

func TestLanguageParsesGo(t *testing.T) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(Language()); err != nil {
		t.Fatalf("set Go language: %v", err)
	}

	tree := parser.Parse([]byte("package fixture\n\nfunc main() {}\n"), nil)
	defer tree.Close()
	if tree.RootNode().HasError() {
		t.Fatal("Go language did not parse valid source")
	}
}

func TestExtractProducesPackageAndFunctionFacts(t *testing.T) {
	contribution, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "src/greet.go",
		Contents:   []byte("package fixture\n\nfunc Greet() {}\n"),
	})
	if err != nil {
		t.Fatalf("extract Go facts: %v", err)
	}

	facts := contribution.Facts()
	if len(facts.Nodes) != 4 {
		t.Fatalf("node count = %d, want 4", len(facts.Nodes))
	}
	if len(facts.Edges) != 3 {
		t.Fatalf("edge count = %d, want 3", len(facts.Edges))
	}

	fileSpan := graph.SourceSpan{Path: "src/greet.go", StartLine: 1, StartColumn: 1, EndLine: 4, EndColumn: 1}
	packageSpan := graph.SourceSpan{Path: "src/greet.go", StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 16}
	functionSpan := graph.SourceSpan{Path: "src/greet.go", StartLine: 3, StartColumn: 1, EndLine: 3, EndColumn: 16}
	wantFileID := graph.NewNodeID("file", fileSpan)
	wantPackageID := graph.NewNodeID(PackageNodeKind, packageSpan)
	wantFunctionID := graph.NewNodeID(FunctionNodeKind, functionSpan)

	if got := facts.Nodes[0]; got.ID != "project:fixture" || got.Kind != "project" || got.Label != "project:fixture" || got.QualifiedName != "project:fixture" {
		t.Errorf("project node = %+v, want project query names", got)
	}
	if got := facts.Nodes[1]; got.ID != wantFileID || got.Kind != "file" || got.Label != "src/greet.go" || got.QualifiedName != "src/greet.go" {
		t.Errorf("file node = %+v, want file node %q", got, wantFileID)
	}
	if got := facts.Nodes[2]; got.ID != wantPackageID || got.Kind != PackageNodeKind || got.Label != "fixture" || got.QualifiedName != "src/greet.go::fixture" {
		t.Errorf("package node = %+v, want package node %q", got, wantPackageID)
	}
	if got := facts.Nodes[3]; got.ID != wantFunctionID || got.Kind != FunctionNodeKind || got.Label != "Greet" || got.QualifiedName != "src/greet.go::fixture.Greet" {
		t.Errorf("function node = %+v, want function node %q", got, wantFunctionID)
	}
	if got := facts.Edges[0]; got.SourceID != "project:fixture" || got.TargetID != wantFileID || got.Relation != "contains" {
		t.Errorf("containment edge = %+v", got)
	}
	if got := facts.Edges[1]; got.SourceID != wantFileID || got.TargetID != wantPackageID || got.Relation != "defines" {
		t.Errorf("package definition edge = %+v", got)
	}
	if got := facts.Edges[2]; got.SourceID != wantPackageID || got.TargetID != wantFunctionID || got.Relation != "defines" {
		t.Errorf("function definition edge = %+v", got)
	}
}

func TestExtractProducesTypeAndMethodFacts(t *testing.T) {
	contribution, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "src/service.go",
		Contents:   []byte("package fixture\n\ntype Service struct{}\n\nfunc (Service) Run() {}\n"),
	})
	if err != nil {
		t.Fatalf("extract Go facts: %v", err)
	}

	packageID := findNodeID(t, contribution.Facts(), PackageNodeKind, "fixture")
	typeID := findNodeID(t, contribution.Facts(), TypeNodeKind, "Service")
	methodID := findNodeID(t, contribution.Facts(), MethodNodeKind, "Run")
	if !hasFactEdge(contribution.Facts(), packageID, typeID, "defines") {
		t.Errorf("facts = %+v, want package definition for Service", contribution.Facts())
	}
	if !hasFactEdge(contribution.Facts(), packageID, methodID, "defines") {
		t.Errorf("facts = %+v, want package definition for Run", contribution.Facts())
	}

	for _, node := range contribution.Facts().Nodes {
		switch node.ID {
		case typeID:
			if node.QualifiedName != "src/service.go::fixture.Service" {
				t.Errorf("type qualified name = %q, want src/service.go::fixture.Service", node.QualifiedName)
			}
		case methodID:
			if node.QualifiedName != "src/service.go::fixture.Service.Run" {
				t.Errorf("method qualified name = %q, want src/service.go::fixture.Service.Run", node.QualifiedName)
			}
		}
	}
}

func TestExtractProvidesCatalogUnitSourceData(t *testing.T) {
	contribution, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "src/validator.go",
		Contents:   []byte("package fixture\n\n// ValidateToken checks a signed access token.\nfunc ValidateToken(token string) error { return nil }\n"),
	})
	if err != nil {
		t.Fatalf("extract Go facts: %v", err)
	}

	units := contribution.CatalogUnits()
	if len(units) != 1 {
		t.Fatalf("catalog unit count = %d, want 1", len(units))
	}
	unit := units[0]
	if unit.Name != "ValidateToken" || unit.Kind != FunctionNodeKind || unit.Signature != "func ValidateToken(token string) error" {
		t.Errorf("catalog unit = %+v, want function source data", unit)
	}
	if len(unit.Comments) != 1 || unit.Comments[0] != "ValidateToken checks a signed access token." {
		t.Errorf("catalog comments = %#v, want one declaration comment", unit.Comments)
	}
	if len(unit.IdentifierTokens) != 2 || unit.IdentifierTokens[0] != "validate" || unit.IdentifierTokens[1] != "token" {
		t.Errorf("catalog identifier tokens = %#v, want validate and token", unit.IdentifierTokens)
	}
}

func TestExtractProducesVariableAndLocalReferenceFacts(t *testing.T) {
	contribution, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "src/main.go",
		Contents:   []byte("package fixture\n\nfunc helper() int { return 1 }\n\nfunc main() int {\n\tvalue := helper()\n\treturn value\n}\n"),
	})
	if err != nil {
		t.Fatalf("extract Go facts: %v", err)
	}

	helperID := findNodeID(t, contribution.Facts(), FunctionNodeKind, "helper")
	mainID := findNodeID(t, contribution.Facts(), FunctionNodeKind, "main")
	variableID := findNodeID(t, contribution.Facts(), VariableNodeKind, "value")
	if !hasFactEdge(contribution.Facts(), mainID, variableID, "defines") {
		t.Errorf("facts = %+v, want main definition for value", contribution.Facts())
	}
	if !hasFactEdge(contribution.Facts(), mainID, helperID, "references") {
		t.Errorf("facts = %+v, want main reference to helper", contribution.Facts())
	}
	if !hasFactEdge(contribution.Facts(), mainID, helperID, CallsRelation) {
		t.Errorf("facts = %+v, want main call to helper", contribution.Facts())
	}
	if !hasReferenceEvidence(contribution.Facts(), mainID, helperID, graph.SourceSpan{Path: "src/main.go", StartLine: 6, StartColumn: 11, EndLine: 6, EndColumn: 17}) {
		t.Errorf("facts = %+v, want helper reference evidence at the call site", contribution.Facts())
	}
}

func TestResolvePagePreservesLocalCallFacts(t *testing.T) {
	contribution, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "src/main.go",
		Contents:   []byte("package fixture\n\nfunc helper() int { return 1 }\n\nfunc main() int {\n\treturn helper()\n}\n"),
	})
	if err != nil {
		t.Fatalf("extract Go facts: %v", err)
	}

	resolution, err := ResolvePage(context.Background(), []extractor.Contribution{contribution}, "project:fixture", pageResolverIndex{}, extractor.ResolverFileView{})
	if err != nil {
		t.Fatalf("resolve Go page: %v", err)
	}

	helperID := findNodeID(t, contribution.Facts(), FunctionNodeKind, "helper")
	mainID := findNodeID(t, contribution.Facts(), FunctionNodeKind, "main")
	if !hasFactEdge(resolution.Facts(), mainID, helperID, CallsRelation) {
		t.Errorf("resolved facts = %+v, want local call fact", resolution.Facts())
	}
}

func TestResolveWithFileViewResolvesLocalMethodSelectorCalls(t *testing.T) {
	contribution, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "src/store.go",
		Contents:   []byte("package fixture\n\ntype Store struct{}\n\nfunc (Store) LookupNodes() {}\n\nfunc run(store Store) { store.LookupNodes() }\n"),
	})
	if err != nil {
		t.Fatalf("extract Go facts: %v", err)
	}

	resolution, err := ResolveWithFileView([]extractor.Contribution{contribution}, extractor.ResolverFileView{})
	if err != nil {
		t.Fatalf("resolve Go facts: %v", err)
	}

	runID := findNodeID(t, contribution.Facts(), FunctionNodeKind, "run")
	methodID := findNodeID(t, contribution.Facts(), MethodNodeKind, "LookupNodes")
	if !hasFactEdge(resolution.Facts(), runID, methodID, CallsRelation) {
		t.Errorf("resolved facts = %+v, want method call fact", resolution.Facts())
	}
}

func TestExtractScopesLocalVariableReferencesToTheirFunction(t *testing.T) {
	contribution, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "src/values.go",
		Contents:   []byte("package fixture\n\nfunc first() int {\n\tvalue := 1\n\treturn value\n}\n\nfunc second() int {\n\tvalue := 2\n\treturn value\n}\n"),
	})
	if err != nil {
		t.Fatalf("extract Go facts: %v", err)
	}

	firstID := findNodeID(t, contribution.Facts(), FunctionNodeKind, "first")
	secondID := findNodeID(t, contribution.Facts(), FunctionNodeKind, "second")
	firstValueID := findNodeIDBySpan(t, contribution.Facts(), VariableNodeKind, "value", graph.SourceSpan{Path: "src/values.go", StartLine: 4, StartColumn: 2, EndLine: 4, EndColumn: 7})
	secondValueID := findNodeIDBySpan(t, contribution.Facts(), VariableNodeKind, "value", graph.SourceSpan{Path: "src/values.go", StartLine: 9, StartColumn: 2, EndLine: 9, EndColumn: 7})
	if !hasFactEdge(contribution.Facts(), firstID, firstValueID, "references") {
		t.Errorf("facts = %+v, want first reference to first value", contribution.Facts())
	}
	if !hasFactEdge(contribution.Facts(), secondID, secondValueID, "references") {
		t.Errorf("facts = %+v, want second reference to second value", contribution.Facts())
	}
}

func TestExtractSkipsBlankIdentifierVariables(t *testing.T) {
	contribution, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "src/blank.go",
		Contents:   []byte("package fixture\n\nfunc value() int {\n\t_, result := 1, 2\n\treturn result\n}\n"),
	})
	if err != nil {
		t.Fatalf("extract Go facts: %v", err)
	}

	for _, node := range contribution.Facts().Nodes {
		if node.Kind == VariableNodeKind && node.Label == "_" {
			t.Errorf("facts = %+v, must not declare blank identifier", contribution.Facts())
		}
	}
	findNodeID(t, contribution.Facts(), VariableNodeKind, "result")
}

func TestResolveWithFileViewResolvesModuleLocalImport(t *testing.T) {
	helper, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "internal/helper/helper.go",
		Contents:   []byte("package helper\n\nfunc Help() {}\n"),
	})
	if err != nil {
		t.Fatalf("extract helper Go facts: %v", err)
	}
	main, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "cmd/main.go",
		Contents:   []byte("package main\n\nimport \"example.com/fixture/internal/helper\"\n\nfunc Main() { helper.Help() }\n"),
	})
	if err != nil {
		t.Fatalf("extract main Go facts: %v", err)
	}
	view, err := extractor.NewResolverFileView(".", map[string][]byte{
		"go.mod": []byte("module example.com/fixture\n"),
	})
	if err != nil {
		t.Fatalf("create resolver file view: %v", err)
	}

	resolution, err := ResolveWithFileView([]extractor.Contribution{main, helper}, view)
	if err != nil {
		t.Fatalf("resolve Go imports: %v", err)
	}
	mainFileID := findNodeID(t, main.Facts(), "file", "cmd/main.go")
	helperFileID := findNodeID(t, helper.Facts(), "file", "internal/helper/helper.go")
	helperID := findNodeID(t, helper.Facts(), FunctionNodeKind, "Help")
	if !hasFactEdge(resolution.Facts(), mainFileID, helperFileID, ImportsFromRelation) {
		t.Errorf("facts = %+v, want package import fact", resolution.Facts())
	}
	if !hasFactEdge(resolution.Facts(), mainFileID, helperID, ImportsFromRelation) {
		t.Errorf("facts = %+v, want imported exported surface fact", resolution.Facts())
	}
}

func TestResolvePageUsesResolverIndexForCrossPageImport(t *testing.T) {
	helper, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "internal/helper/helper.go",
		Contents:   []byte("package helper\n\nfunc Help() {}\n"),
	})
	if err != nil {
		t.Fatalf("extract helper Go facts: %v", err)
	}
	main, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "cmd/main.go",
		Contents:   []byte("package main\n\nimport \"example.com/fixture/internal/helper\"\n\nfunc Main() { helper.Help() }\n"),
	})
	if err != nil {
		t.Fatalf("extract main Go facts: %v", err)
	}
	view, err := extractor.NewResolverFileView(".", map[string][]byte{"go.mod": []byte("module example.com/fixture\n")})
	if err != nil {
		t.Fatalf("create resolver file view: %v", err)
	}
	resolution, err := ResolvePage(context.Background(), []extractor.Contribution{main}, "project:fixture", pageResolverIndex{packages: map[string][]extractor.ResolverTarget{
		"internal/helper": {pageResolverTarget(helper)},
	}}, view)
	if err != nil {
		t.Fatalf("resolve Go page: %v", err)
	}
	mainFileID := findNodeID(t, main.Facts(), "file", "cmd/main.go")
	helperFileID := findNodeID(t, helper.Facts(), "file", "internal/helper/helper.go")
	helperID := findNodeID(t, helper.Facts(), FunctionNodeKind, "Help")
	if !hasFactEdge(resolution.Facts(), mainFileID, helperFileID, ImportsFromRelation) {
		t.Errorf("facts = %+v, want indexed package import fact", resolution.Facts())
	}
	if !hasFactEdge(resolution.Facts(), mainFileID, helperID, ImportsFromRelation) {
		t.Errorf("facts = %+v, want indexed exported surface import fact", resolution.Facts())
	}
}

func TestResolvePageUsesResolverIndexForCrossPagePackageCall(t *testing.T) {
	helper, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "internal/helper/helper.go",
		Contents:   []byte("package helper\n\nfunc Help() {}\n"),
	})
	if err != nil {
		t.Fatalf("extract helper Go facts: %v", err)
	}
	main, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "cmd/main.go",
		Contents:   []byte("package main\n\nimport \"example.com/fixture/internal/helper\"\n\nfunc Main() { helper.Help() }\n"),
	})
	if err != nil {
		t.Fatalf("extract main Go facts: %v", err)
	}
	view, err := extractor.NewResolverFileView(".", map[string][]byte{"go.mod": []byte("module example.com/fixture\n")})
	if err != nil {
		t.Fatalf("create resolver file view: %v", err)
	}
	reads := 0
	resolution, err := ResolvePage(context.Background(), []extractor.Contribution{main}, "project:fixture", pageResolverIndex{
		packages: map[string][]extractor.ResolverTarget{
			"internal/helper": {pageResolverTarget(helper)},
		},
		packagePageReads: &reads,
	}, view)
	if err != nil {
		t.Fatalf("resolve Go page: %v", err)
	}
	mainID := findNodeID(t, main.Facts(), FunctionNodeKind, "Main")
	helperID := findNodeID(t, helper.Facts(), FunctionNodeKind, "Help")
	if !hasFactEdge(resolution.Facts(), mainID, helperID, CallsRelation) {
		t.Errorf("facts = %+v, want indexed package call fact", resolution.Facts())
	}
	if reads != 2 {
		t.Errorf("resolver package page reads = %d, want one data page and one terminal page", reads)
	}
}

func TestResolvePageUsesResolverIndexForCrossPageInterfaceImplementation(t *testing.T) {
	contract, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "service/contract.go",
		Contents:   []byte("package service\n\ntype Runner interface {\n\tRun()\n}\n"),
	})
	if err != nil {
		t.Fatalf("extract contract Go facts: %v", err)
	}
	service, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "service/service.go",
		Contents:   []byte("package service\n\ntype Worker struct{}\n\nfunc (Worker) Run() {}\n"),
	})
	if err != nil {
		t.Fatalf("extract service Go facts: %v", err)
	}
	resolution, err := ResolvePage(context.Background(), []extractor.Contribution{service}, "project:fixture", pageResolverIndex{packages: map[string][]extractor.ResolverTarget{
		"service": {pageResolverTarget(contract), pageResolverTarget(service)},
	}}, extractor.ResolverFileView{})
	if err != nil {
		t.Fatalf("resolve Go page: %v", err)
	}
	workerID := findNodeID(t, service.Facts(), TypeNodeKind, "Worker")
	runnerID := findNodeID(t, contract.Facts(), TypeNodeKind, "Runner")
	if !hasFactEdge(resolution.Facts(), workerID, runnerID, ImplementsRelation) {
		t.Errorf("facts = %+v, want Worker to implement indexed Runner", resolution.Facts())
	}
}

func TestResolvePageUsesResolverIndexForCrossPageInterfaceEmbedding(t *testing.T) {
	base, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "service/base.go",
		Contents:   []byte("package service\n\ntype Reader interface {\n\tRead()\n}\n"),
	})
	if err != nil {
		t.Fatalf("extract base Go facts: %v", err)
	}
	combined, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "service/combined.go",
		Contents:   []byte("package service\n\ntype ReadWriter interface {\n\tReader\n\tWrite()\n}\n"),
	})
	if err != nil {
		t.Fatalf("extract combined Go facts: %v", err)
	}
	resolution, err := ResolvePage(context.Background(), []extractor.Contribution{combined}, "project:fixture", pageResolverIndex{packages: map[string][]extractor.ResolverTarget{
		"service": {pageResolverTarget(base), pageResolverTarget(combined)},
	}}, extractor.ResolverFileView{})
	if err != nil {
		t.Fatalf("resolve Go page: %v", err)
	}
	readWriterID := findNodeID(t, combined.Facts(), TypeNodeKind, "ReadWriter")
	readerID := findNodeID(t, base.Facts(), TypeNodeKind, "Reader")
	if !hasFactEdge(resolution.Facts(), readWriterID, readerID, EmbedsRelation) {
		t.Errorf("facts = %+v, want ReadWriter to embed indexed Reader", resolution.Facts())
	}
}

type pageResolverIndex struct {
	packages         map[string][]extractor.ResolverTarget
	packagePageReads *int
}

func (index pageResolverIndex) ResolverTarget(context.Context, extractor.ResolverTargetRequest) (extractor.ResolverTarget, bool, error) {
	return extractor.ResolverTarget{}, false, nil
}

func (index pageResolverIndex) ResolverPackagePage(_ context.Context, request extractor.ResolverPackagePageRequest) ([]extractor.ResolverTarget, error) {
	if index.packagePageReads != nil {
		*index.packagePageReads++
	}
	targets := make([]extractor.ResolverTarget, 0, request.Limit)
	for _, target := range index.packages[request.PackagePath] {
		if target.SourcePath <= request.AfterSourcePath {
			continue
		}
		targets = append(targets, target)
		if len(targets) == request.Limit {
			break
		}
	}
	return targets, nil
}

func pageResolverTarget(contribution extractor.Contribution) extractor.ResolverTarget {
	return extractor.ResolverTarget{
		ProjectID:            "project:fixture",
		SourcePath:           contribution.SourcePath(),
		Metadata:             contribution.Metadata(),
		Nodes:                contribution.Facts().Nodes,
		UnresolvedReferences: contribution.UnresolvedReferences(),
		SymbolReferences:     contribution.SymbolReferences(),
		ExportedSurfaces:     contribution.ExportedSurfaces(),
		Diagnostics:          contribution.Diagnostics(),
	}
}

func TestResolveWithFileViewAddsCrossFileInterfaceImplementation(t *testing.T) {
	contract, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "service/contract.go",
		Contents:   []byte("package service\n\ntype Runner interface {\n\tRun()\n}\n"),
	})
	if err != nil {
		t.Fatalf("extract contract Go facts: %v", err)
	}
	service, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "service/service.go",
		Contents:   []byte("package service\n\ntype Worker struct{}\n\nfunc (Worker) Run() {}\n"),
	})
	if err != nil {
		t.Fatalf("extract service Go facts: %v", err)
	}
	view, err := extractor.NewResolverFileView(".", map[string][]byte{
		"go.mod": []byte("module example.com/fixture\n"),
	})
	if err != nil {
		t.Fatalf("create resolver file view: %v", err)
	}

	resolution, err := ResolveWithFileView([]extractor.Contribution{service, contract}, view)
	if err != nil {
		t.Fatalf("resolve Go relationships: %v", err)
	}
	workerID := findNodeID(t, service.Facts(), TypeNodeKind, "Worker")
	runnerID := findNodeID(t, contract.Facts(), TypeNodeKind, "Runner")
	if !hasFactEdge(resolution.Facts(), workerID, runnerID, ImplementsRelation) {
		t.Errorf("facts = %+v, want Worker to implement Runner", resolution.Facts())
	}
}

func TestResolveWithFileViewAddsCrossFileInterfaceEmbedding(t *testing.T) {
	base, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "service/base.go",
		Contents:   []byte("package service\n\ntype Reader interface {\n\tRead()\n}\n"),
	})
	if err != nil {
		t.Fatalf("extract base Go facts: %v", err)
	}
	combined, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "service/combined.go",
		Contents:   []byte("package service\n\ntype ReadWriter interface {\n\tReader\n\tWrite()\n}\n"),
	})
	if err != nil {
		t.Fatalf("extract combined Go facts: %v", err)
	}
	view, err := extractor.NewResolverFileView(".", map[string][]byte{
		"go.mod": []byte("module example.com/fixture\n"),
	})
	if err != nil {
		t.Fatalf("create resolver file view: %v", err)
	}

	resolution, err := ResolveWithFileView([]extractor.Contribution{combined, base}, view)
	if err != nil {
		t.Fatalf("resolve Go relationships: %v", err)
	}
	readWriterID := findNodeID(t, combined.Facts(), TypeNodeKind, "ReadWriter")
	readerID := findNodeID(t, base.Facts(), TypeNodeKind, "Reader")
	if !hasFactEdge(resolution.Facts(), readWriterID, readerID, EmbedsRelation) {
		t.Errorf("facts = %+v, want ReadWriter to embed Reader", resolution.Facts())
	}
}

func TestResolveWithFileViewAddsCrossFilePackageCall(t *testing.T) {
	helper, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "internal/helper/helper.go",
		Contents:   []byte("package helper\n\nfunc Help() {}\n"),
	})
	if err != nil {
		t.Fatalf("extract helper Go facts: %v", err)
	}
	main, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "cmd/main.go",
		Contents:   []byte("package main\n\nimport \"example.com/fixture/internal/helper\"\n\nfunc Main() { helper.Help() }\n"),
	})
	if err != nil {
		t.Fatalf("extract main Go facts: %v", err)
	}
	view, err := extractor.NewResolverFileView(".", map[string][]byte{
		"go.mod": []byte("module example.com/fixture\n"),
	})
	if err != nil {
		t.Fatalf("create resolver file view: %v", err)
	}

	resolution, err := ResolveWithFileView([]extractor.Contribution{main, helper}, view)
	if err != nil {
		t.Fatalf("resolve Go relationships: %v", err)
	}
	mainID := findNodeID(t, main.Facts(), FunctionNodeKind, "Main")
	helperID := findNodeID(t, helper.Facts(), FunctionNodeKind, "Help")
	if !hasFactEdge(resolution.Facts(), mainID, helperID, CallsRelation) {
		t.Errorf("facts = %+v, want Main to call Help", resolution.Facts())
	}
}

func TestResolveWithFileViewReportsUnsupportedPackageTypeCall(t *testing.T) {
	helper, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "internal/helper/helper.go",
		Contents:   []byte("package helper\n\ntype Factory struct{}\n"),
	})
	if err != nil {
		t.Fatalf("extract helper Go facts: %v", err)
	}
	main, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "cmd/main.go",
		Contents:   []byte("package main\n\nimport \"example.com/fixture/internal/helper\"\n\nfunc Main() { helper.Factory() }\n"),
	})
	if err != nil {
		t.Fatalf("extract main Go facts: %v", err)
	}
	view, err := extractor.NewResolverFileView(".", map[string][]byte{
		"go.mod": []byte("module example.com/fixture\n"),
	})
	if err != nil {
		t.Fatalf("create resolver file view: %v", err)
	}

	resolution, err := ResolveWithFileView([]extractor.Contribution{main, helper}, view)
	if err != nil {
		t.Fatalf("resolve Go relationships: %v", err)
	}
	for _, diagnostic := range resolution.Diagnostics() {
		if diagnostic.Message == `Go call "helper.Factory" from "cmd/main.go" is unsupported or ambiguous` {
			return
		}
	}
	t.Errorf("diagnostics = %+v, want unsupported package type call diagnostic", resolution.Diagnostics())
}

func TestResolveWithFileViewReportsUnsupportedReceiverDispatch(t *testing.T) {
	service, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "service/service.go",
		Contents:   []byte("package service\n\ntype Worker struct{}\n\nfunc (Worker) Run() {}\n\nfunc Start() {\n\tworker := Worker{}\n\tworker.Run()\n}\n"),
	})
	if err != nil {
		t.Fatalf("extract service Go facts: %v", err)
	}
	view, err := extractor.NewResolverFileView(".", map[string][]byte{
		"go.mod": []byte("module example.com/fixture\n"),
	})
	if err != nil {
		t.Fatalf("create resolver file view: %v", err)
	}

	resolution, err := ResolveWithFileView([]extractor.Contribution{service}, view)
	if err != nil {
		t.Fatalf("resolve Go relationships: %v", err)
	}
	for _, diagnostic := range resolution.Diagnostics() {
		if diagnostic.Message == `Go call "worker.Run" from "service/service.go" is unsupported or ambiguous` {
			return
		}
	}
	t.Errorf("diagnostics = %+v, want unsupported receiver dispatch diagnostic", resolution.Diagnostics())
}

func TestResolveWithFileViewReportsAmbiguousPackageCall(t *testing.T) {
	first, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "internal/helper/first.go",
		Contents:   []byte("package helper\n\nfunc Help() {}\n"),
	})
	if err != nil {
		t.Fatalf("extract first helper Go facts: %v", err)
	}
	second, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "internal/helper/second.go",
		Contents:   []byte("package helper\n\nfunc Help() {}\n"),
	})
	if err != nil {
		t.Fatalf("extract second helper Go facts: %v", err)
	}
	main, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "cmd/main.go",
		Contents:   []byte("package main\n\nimport \"example.com/fixture/internal/helper\"\n\nfunc Main() { helper.Help() }\n"),
	})
	if err != nil {
		t.Fatalf("extract main Go facts: %v", err)
	}
	view, err := extractor.NewResolverFileView(".", map[string][]byte{
		"go.mod": []byte("module example.com/fixture\n"),
	})
	if err != nil {
		t.Fatalf("create resolver file view: %v", err)
	}

	resolution, err := ResolveWithFileView([]extractor.Contribution{main, first, second}, view)
	if err != nil {
		t.Fatalf("resolve Go relationships: %v", err)
	}
	for _, diagnostic := range resolution.Diagnostics() {
		if diagnostic.Message == `Go call "helper.Help" from "cmd/main.go" is unsupported or ambiguous` {
			return
		}
	}
	t.Errorf("diagnostics = %+v, want ambiguous package call diagnostic", resolution.Diagnostics())
}

func TestResolveWithFileViewKeepsExternalImportsOutOfLocalFacts(t *testing.T) {
	main, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "cmd/main.go",
		Contents:   []byte("package main\n\nimport \"fmt\"\n\nfunc Main() { fmt.Println(\"fixture\") }\n"),
	})
	if err != nil {
		t.Fatalf("extract main Go facts: %v", err)
	}
	view, err := extractor.NewResolverFileView(".", map[string][]byte{
		"go.mod": []byte("module example.com/fixture\n"),
	})
	if err != nil {
		t.Fatalf("create resolver file view: %v", err)
	}

	resolution, err := ResolveWithFileView([]extractor.Contribution{main}, view)
	if err != nil {
		t.Fatalf("resolve Go imports: %v", err)
	}
	if len(resolution.Facts().Edges) != 0 {
		t.Errorf("facts = %+v, want no local import facts for standard library package", resolution.Facts())
	}
	if len(resolution.Diagnostics()) != 0 {
		t.Errorf("diagnostics = %+v, want no external package diagnostic", resolution.Diagnostics())
	}
}

func TestResolveWithFileViewReportsUnresolvedModuleLocalPackage(t *testing.T) {
	main, err := Extract(extractor.Source{
		ProjectID:  "project:fixture",
		SourcePath: "cmd/main.go",
		Contents:   []byte("package main\n\nimport \"example.com/fixture/missing\"\n"),
	})
	if err != nil {
		t.Fatalf("extract main Go facts: %v", err)
	}
	view, err := extractor.NewResolverFileView(".", map[string][]byte{
		"go.mod": []byte("module example.com/fixture\n"),
	})
	if err != nil {
		t.Fatalf("create resolver file view: %v", err)
	}

	resolution, err := ResolveWithFileView([]extractor.Contribution{main}, view)
	if err != nil {
		t.Fatalf("resolve Go imports: %v", err)
	}
	for _, diagnostic := range resolution.Diagnostics() {
		if diagnostic.Message == `Go package "example.com/fixture/missing" from "cmd/main.go" is not indexed` {
			return
		}
	}
	t.Errorf("diagnostics = %+v, want unresolved package diagnostic", resolution.Diagnostics())
}

func findNodeID(t *testing.T, facts graph.Facts, kind graph.NodeKind, label string) string {
	t.Helper()
	for _, node := range facts.Nodes {
		if node.Kind == kind && node.Label == label {
			return node.ID
		}
	}
	t.Fatalf("facts = %+v, want node %s %q", facts, kind, label)
	return ""
}

func findNodeIDBySpan(t *testing.T, facts graph.Facts, kind graph.NodeKind, label string, span graph.SourceSpan) string {
	t.Helper()
	for _, node := range facts.Nodes {
		if node.Kind == kind && node.Label == label && node.Evidence.Span == span {
			return node.ID
		}
	}
	t.Fatalf("facts = %+v, want node %s %q at %+v", facts, kind, label, span)
	return ""
}

func hasFactEdge(facts graph.Facts, sourceID, targetID string, relation graph.RelationKind) bool {
	for _, edge := range facts.Edges {
		if edge.SourceID == sourceID && edge.TargetID == targetID && edge.Relation == relation {
			return true
		}
	}
	return false
}

func hasReferenceEvidence(facts graph.Facts, sourceID, targetID string, span graph.SourceSpan) bool {
	for _, edge := range facts.Edges {
		if edge.SourceID == sourceID && edge.TargetID == targetID && edge.Relation == "references" && edge.Evidence.Span == span {
			return true
		}
	}
	return false
}
