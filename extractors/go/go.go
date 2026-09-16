package goextractor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"

	"agent-wayfinder/extractor"
	"agent-wayfinder/graph"

	sitter "github.com/tree-sitter/go-tree-sitter"
	gogrammar "github.com/tree-sitter/tree-sitter-go/bindings/go"
)

const Version = "v0"

const (
	PackageNodeKind  graph.NodeKind = "go:package"
	TypeNodeKind     graph.NodeKind = "go:type"
	FunctionNodeKind graph.NodeKind = "go:function"
	MethodNodeKind   graph.NodeKind = "go:method"
	VariableNodeKind graph.NodeKind = "go:variable"
)

func New() extractor.Extractor {
	return extractorDefinition{}
}

func Language() *sitter.Language {
	return sitter.NewLanguage(gogrammar.Language())
}

type extractorDefinition struct{}

type declaration struct {
	id            string
	name          string
	qualifiedName string
	node          *sitter.Node
	nameStart     uint
	nameEnd       uint
}

func Extract(source extractor.Source) (extractor.Contribution, error) {
	if source.ProjectID == "" {
		return extractor.Contribution{}, fmt.Errorf("Go source project ID is empty")
	}
	if source.SourcePath == "" {
		return extractor.Contribution{}, fmt.Errorf("Go source path is empty")
	}

	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(Language()); err != nil {
		return extractor.Contribution{}, fmt.Errorf("set Go language: %w", err)
	}

	tree := parser.Parse(source.Contents, nil)
	defer tree.Close()
	root := tree.RootNode()
	if root.HasError() {
		return extractor.Contribution{}, fmt.Errorf("parse Go source %q", source.SourcePath)
	}

	vocabulary, err := New().Vocabulary()
	if err != nil {
		return extractor.Contribution{}, fmt.Errorf("get Go vocabulary: %w", err)
	}

	fileEvidence := evidenceFor(source, root)
	fileID := graph.NewNodeID("file", fileEvidence.Span)
	facts := graph.Facts{
		Nodes: []graph.Node{
			{ID: source.ProjectID, Kind: "project", Label: source.ProjectID, QualifiedName: source.ProjectID, Evidence: fileEvidence},
			{ID: fileID, Kind: "file", Label: source.SourcePath, QualifiedName: source.SourcePath, Evidence: fileEvidence},
		},
		Edges: []graph.Edge{{
			SourceID: source.ProjectID,
			TargetID: fileID,
			Relation: "contains",
			Evidence: fileEvidence,
		}},
	}

	var packageClause *sitter.Node
	for childIndex := uint(0); childIndex < root.NamedChildCount(); childIndex++ {
		child := root.NamedChild(childIndex)
		if child.Kind() == "package_clause" {
			packageClause = child
			break
		}
	}
	if packageClause == nil {
		return extractor.Contribution{}, fmt.Errorf("Go source %q has no package clause", source.SourcePath)
	}
	if packageClause.NamedChildCount() != 1 {
		return extractor.Contribution{}, fmt.Errorf("Go source %q has no package name", source.SourcePath)
	}
	packageName := packageClause.NamedChild(0)
	packageEvidence := evidenceFor(source, packageClause)
	packageID := graph.NewNodeID(PackageNodeKind, packageEvidence.Span)
	packageQualifiedName := source.SourcePath + "::" + packageName.Utf8Text(source.Contents)
	facts.Nodes = append(facts.Nodes, graph.Node{
		ID:            packageID,
		Kind:          PackageNodeKind,
		Label:         packageName.Utf8Text(source.Contents),
		QualifiedName: packageQualifiedName,
		Evidence:      packageEvidence,
	})
	facts.Edges = append(facts.Edges, graph.Edge{
		SourceID: fileID,
		TargetID: packageID,
		Relation: "defines",
		Evidence: packageEvidence,
	})

	declarations := make([]declaration, 0)
	owners := make([]declaration, 0)
	symbolReferences := make([]extractor.SymbolReference, 0)
	moduleReferences := collectModuleReferences(source, root, fileID)
	for childIndex := uint(0); childIndex < root.NamedChildCount(); childIndex++ {
		child := root.NamedChild(childIndex)
		switch child.Kind() {
		case "type_declaration":
			for specIndex := uint(0); specIndex < child.NamedChildCount(); specIndex++ {
				specification := child.NamedChild(specIndex)
				if specification.Kind() != "type_spec" {
					continue
				}
				name := specification.ChildByFieldName("name")
				if name == nil {
					continue
				}
				declaration := appendDeclaration(source, &facts, packageID, packageQualifiedName, TypeNodeKind, name.Utf8Text(source.Contents), specification, name)
				declarations = append(declarations, declaration)
				if typeNode := specification.ChildByFieldName("type"); typeNode != nil && typeNode.Kind() == "interface_type" {
					for methodIndex := uint(0); methodIndex < typeNode.NamedChildCount(); methodIndex++ {
						method := typeNode.NamedChild(methodIndex)
						if method.Kind() != "method_elem" {
							continue
						}
						methodName := method.ChildByFieldName("name")
						if methodName == nil {
							continue
						}
						declarations = append(declarations, appendDeclaration(source, &facts, declaration.id, declaration.qualifiedName, MethodNodeKind, methodName.Utf8Text(source.Contents), method, methodName))
					}
					symbolReferences = append(symbolReferences, interfaceMethodReferences(source, declaration, typeNode)...)
				}
			}
		case "var_declaration":
			declarations = append(declarations, appendVariables(source, &facts, packageID, packageQualifiedName, child)...)
		case "function_declaration", "method_declaration":
			name := child.ChildByFieldName("name")
			if name == nil {
				continue
			}
			qualifiedName := packageQualifiedName
			kind := FunctionNodeKind
			if child.Kind() == "method_declaration" {
				receiverType, found := receiverTypeName(source, child.ChildByFieldName("receiver"))
				if !found {
					continue
				}
				qualifiedName += "." + receiverType
				kind = MethodNodeKind
			}
			declaration := appendDeclaration(source, &facts, packageID, qualifiedName, kind, name.Utf8Text(source.Contents), child, name)
			declarations = append(declarations, declaration)
			owners = append(owners, declaration)
		}
	}
	localDeclarations := make(map[string][]declaration, len(owners))
	for _, owner := range owners {
		locals := appendLocalVariables(source, &facts, owner)
		localDeclarations[owner.id] = locals
		declarations = append(declarations, locals...)
	}
	appendLocalReferences(source, owners, declarations, localDeclarations, &facts)
	for _, owner := range owners {
		symbolReferences = append(symbolReferences, packageCallReferences(source, owner)...)
	}

	return extractor.NewContribution(vocabulary, extractor.ContributionInput{
		ProjectID:            source.ProjectID,
		SourcePath:           source.SourcePath,
		Metadata:             New().Metadata(),
		Facts:                facts,
		UnresolvedReferences: moduleReferences,
		SymbolReferences:     symbolReferences,
		ExportedSurfaces:     exportedSurfaces(declarations),
	})
}

func interfaceMethodReferences(source extractor.Source, declaration declaration, interfaceType *sitter.Node) []extractor.SymbolReference {
	references := make([]extractor.SymbolReference, 0)
	for childIndex := uint(0); childIndex < interfaceType.NamedChildCount(); childIndex++ {
		method := interfaceType.NamedChild(childIndex)
		switch method.Kind() {
		case "method_elem":
			name := method.ChildByFieldName("name")
			if name == nil {
				continue
			}
			references = append(references, extractor.SymbolReference{
				SourceID: declaration.id,
				Target:   name.Utf8Text(source.Contents),
				Relation: ImplementsRelation,
				Evidence: evidenceFor(source, name),
			})
		case "type_elem":
			name := embeddedTypeName(source, method)
			if name == nil {
				continue
			}
			references = append(references, extractor.SymbolReference{
				SourceID: declaration.id,
				Target:   name.Utf8Text(source.Contents),
				Relation: EmbedsRelation,
				Evidence: evidenceFor(source, name),
			})
		}
	}
	return references
}

func embeddedTypeName(source extractor.Source, node *sitter.Node) *sitter.Node {
	if node.Kind() == "type_identifier" {
		return node
	}
	for childIndex := uint(0); childIndex < node.NamedChildCount(); childIndex++ {
		if name := embeddedTypeName(source, node.NamedChild(childIndex)); name != nil {
			return name
		}
	}
	return nil
}

func packageCallReferences(source extractor.Source, owner declaration) []extractor.SymbolReference {
	references := make([]extractor.SymbolReference, 0)
	var visit func(*sitter.Node)
	visit = func(node *sitter.Node) {
		if node != owner.node && (node.Kind() == "function_declaration" || node.Kind() == "method_declaration") {
			return
		}
		if node.Kind() == "call_expression" {
			function := node.ChildByFieldName("function")
			if function != nil && function.Kind() == "selector_expression" {
				operand := function.ChildByFieldName("operand")
				field := function.ChildByFieldName("field")
				if operand != nil && operand.Kind() == "identifier" && field != nil && field.Kind() == "field_identifier" {
					references = append(references, extractor.SymbolReference{
						SourceID: owner.id,
						Target:   operand.Utf8Text(source.Contents) + "." + field.Utf8Text(source.Contents),
						Relation: CallsRelation,
						Evidence: evidenceFor(source, field),
					})
				}
			}
		}
		for childIndex := uint(0); childIndex < node.NamedChildCount(); childIndex++ {
			visit(node.NamedChild(childIndex))
		}
	}
	visit(owner.node)
	return references
}

func collectModuleReferences(source extractor.Source, root *sitter.Node, fileID string) []extractor.UnresolvedReference {
	references := make([]extractor.UnresolvedReference, 0)
	var visit func(*sitter.Node)
	visit = func(node *sitter.Node) {
		if node.Kind() == "import_spec" {
			pathNode := node.ChildByFieldName("path")
			if pathNode != nil {
				value, err := strconv.Unquote(pathNode.Utf8Text(source.Contents))
				if err == nil {
					references = append(references, extractor.UnresolvedReference{
						SourceID: fileID,
						Target:   value,
						Kind:     extractor.ModuleReferenceImport,
					})
				}
			}
		}
		for childIndex := uint(0); childIndex < node.NamedChildCount(); childIndex++ {
			visit(node.NamedChild(childIndex))
		}
	}
	visit(root)
	return references
}

func exportedSurfaces(declarations []declaration) []extractor.ExportedSurface {
	surfaces := make([]extractor.ExportedSurface, 0)
	for _, declaration := range declarations {
		if declaration.name == "" || declaration.name[0] < 'A' || declaration.name[0] > 'Z' {
			continue
		}
		surfaces = append(surfaces, extractor.ExportedSurface{NodeID: declaration.id, Name: declaration.name})
	}
	return surfaces
}

func appendDeclaration(source extractor.Source, facts *graph.Facts, parentID, parentQualifiedName string, kind graph.NodeKind, name string, node, nameNode *sitter.Node) declaration {
	evidence := evidenceFor(source, node)
	id := graph.NewNodeID(kind, evidence.Span)
	facts.Nodes = append(facts.Nodes, graph.Node{
		ID:            id,
		Kind:          kind,
		Label:         name,
		QualifiedName: parentQualifiedName + "." + name,
		Evidence:      evidence,
	})
	facts.Edges = append(facts.Edges, graph.Edge{
		SourceID: parentID,
		TargetID: id,
		Relation: "defines",
		Evidence: evidence,
	})
	return declaration{
		id:            id,
		name:          name,
		qualifiedName: parentQualifiedName + "." + name,
		node:          node,
		nameStart:     nameNode.StartByte(),
		nameEnd:       nameNode.EndByte(),
	}
}

func appendVariables(source extractor.Source, facts *graph.Facts, parentID, parentQualifiedName string, node *sitter.Node) []declaration {
	declarations := make([]declaration, 0)
	for childIndex := uint(0); childIndex < node.NamedChildCount(); childIndex++ {
		child := node.NamedChild(childIndex)
		if child.Kind() != "var_spec" {
			continue
		}
		declarations = append(declarations, appendVariablesFromNames(source, facts, parentID, parentQualifiedName, child)...)
	}
	return declarations
}

func appendLocalVariables(source extractor.Source, facts *graph.Facts, owner declaration) []declaration {
	declarations := make([]declaration, 0)
	var visit func(*sitter.Node)
	visit = func(node *sitter.Node) {
		if node != owner.node && (node.Kind() == "function_declaration" || node.Kind() == "method_declaration") {
			return
		}
		switch node.Kind() {
		case "short_var_declaration", "var_spec":
			declarations = append(declarations, appendVariablesFromNames(source, facts, owner.id, owner.qualifiedName, node)...)
		}
		for childIndex := uint(0); childIndex < node.NamedChildCount(); childIndex++ {
			visit(node.NamedChild(childIndex))
		}
	}
	visit(owner.node)
	return declarations
}

func appendVariablesFromNames(source extractor.Source, facts *graph.Facts, parentID, parentQualifiedName string, node *sitter.Node) []declaration {
	if node.NamedChildCount() == 0 {
		return nil
	}
	identifiers := identifiersIn(node.NamedChild(0))
	declarations := make([]declaration, 0, len(identifiers))
	for _, identifier := range identifiers {
		name := identifier.Utf8Text(source.Contents)
		if name == "_" {
			continue
		}
		declarations = append(declarations, appendDeclaration(source, facts, parentID, parentQualifiedName, VariableNodeKind, name, identifier, identifier))
	}
	return declarations
}

func identifiersIn(node *sitter.Node) []*sitter.Node {
	identifiers := make([]*sitter.Node, 0)
	var visit func(*sitter.Node)
	visit = func(current *sitter.Node) {
		if current.Kind() == "identifier" {
			identifiers = append(identifiers, current)
			return
		}
		for childIndex := uint(0); childIndex < current.NamedChildCount(); childIndex++ {
			visit(current.NamedChild(childIndex))
		}
	}
	visit(node)
	return identifiers
}

func appendLocalReferences(source extractor.Source, owners, declarations []declaration, localDeclarations map[string][]declaration, facts *graph.Facts) {
	packageDeclarations := make([]declaration, 0, len(declarations))
	for _, declaration := range declarations {
		if declaration.node.Kind() != "identifier" {
			packageDeclarations = append(packageDeclarations, declaration)
		}
	}

	declarationNames := make(map[uint]uint, len(declarations))
	for _, declaration := range declarations {
		declarationNames[declaration.nameStart] = declaration.nameEnd
	}

	for _, owner := range owners {
		byName := make(map[string]declaration, len(packageDeclarations)+len(localDeclarations[owner.id]))
		for _, declaration := range packageDeclarations {
			byName[declaration.name] = declaration
		}
		for _, declaration := range localDeclarations[owner.id] {
			byName[declaration.name] = declaration
		}
		appendReferencesFromNode(source, owner.node, owner.id, byName, declarationNames, facts)
	}
}

func appendReferencesFromNode(source extractor.Source, node *sitter.Node, sourceID string, byName map[string]declaration, declarationNames map[uint]uint, facts *graph.Facts) {
	for childIndex := uint(0); childIndex < node.NamedChildCount(); childIndex++ {
		child := node.NamedChild(childIndex)
		if child.Kind() == "function_declaration" || child.Kind() == "method_declaration" {
			continue
		}
		if child.Kind() == "identifier" || child.Kind() == "type_identifier" {
			if end, isDeclarationName := declarationNames[child.StartByte()]; !isDeclarationName || end != child.EndByte() {
				if target, found := byName[child.Utf8Text(source.Contents)]; found {
					facts.Edges = append(facts.Edges, graph.Edge{
						SourceID: sourceID,
						TargetID: target.id,
						Relation: "references",
						Evidence: evidenceFor(source, child),
					})
					if isLocalCallTarget(node, child, target) {
						facts.Edges = append(facts.Edges, graph.Edge{
							SourceID: sourceID,
							TargetID: target.id,
							Relation: CallsRelation,
							Evidence: evidenceFor(source, child),
						})
					}
				}
			}
		}
		appendReferencesFromNode(source, child, sourceID, byName, declarationNames, facts)
	}
}

func isLocalCallTarget(parent, child *sitter.Node, target declaration) bool {
	if parent.Kind() != "call_expression" || target.node.Kind() != "function_declaration" && target.node.Kind() != "method_declaration" {
		return false
	}
	function := parent.ChildByFieldName("function")
	return function != nil && function.Kind() == "identifier" && function.StartByte() == child.StartByte() && function.EndByte() == child.EndByte()
}

func receiverTypeName(source extractor.Source, receiver *sitter.Node) (string, bool) {
	if receiver == nil {
		return "", false
	}
	var findTypeName func(*sitter.Node) (string, bool)
	findTypeName = func(node *sitter.Node) (string, bool) {
		if node.Kind() == "type_identifier" {
			return node.Utf8Text(source.Contents), true
		}
		for childIndex := uint(0); childIndex < node.NamedChildCount(); childIndex++ {
			if name, found := findTypeName(node.NamedChild(childIndex)); found {
				return name, true
			}
		}
		return "", false
	}
	return findTypeName(receiver)
}

func (extractorDefinition) Metadata() extractor.Metadata {
	return extractor.Metadata{
		Name:       "go",
		Version:    Version,
		Extensions: []string{".go"},
	}
}

func (extractorDefinition) Vocabulary() (graph.Vocabulary, error) {
	return extractor.NewLanguageVocabulary("go", []graph.NodeKind{
		PackageNodeKind,
		TypeNodeKind,
		FunctionNodeKind,
		MethodNodeKind,
		VariableNodeKind,
	})
}

func evidenceFor(source extractor.Source, node *sitter.Node) graph.FactEvidence {
	start := node.StartPosition()
	end := node.EndPosition()
	hash := sha256.Sum256(source.Contents)

	return graph.FactEvidence{
		Span: graph.SourceSpan{
			Path:        source.SourcePath,
			StartLine:   int(start.Row) + 1,
			StartColumn: int(start.Column) + 1,
			EndLine:     int(end.Row) + 1,
			EndColumn:   int(end.Column) + 1,
		},
		FileHash:   "sha256:" + hex.EncodeToString(hash[:]),
		Extractor:  "go@" + Version,
		Provenance: "static",
		Confidence: graph.ConfidenceExtracted,
	}
}
