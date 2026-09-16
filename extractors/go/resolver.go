package goextractor

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"agent-wayfinder/extractor"
	"agent-wayfinder/graph"
)

const (
	ImportsFromRelation graph.RelationKind = "go:imports_from"
	ImplementsRelation  graph.RelationKind = "go:implements"
	EmbedsRelation      graph.RelationKind = "go:embeds"
	CallsRelation       graph.RelationKind = "go:calls"
)

type Resolution struct {
	facts       graph.Facts
	diagnostics []extractor.Diagnostic
}

func (resolution Resolution) Facts() graph.Facts {
	return graph.Facts{
		Nodes: append([]graph.Node(nil), resolution.facts.Nodes...),
		Edges: append([]graph.Edge(nil), resolution.facts.Edges...),
	}
}

func (resolution Resolution) Diagnostics() []extractor.Diagnostic {
	return append([]extractor.Diagnostic(nil), resolution.diagnostics...)
}

func ResolverVocabulary() (graph.Vocabulary, error) {
	return graph.NewVocabulary(graph.VocabularyDefinition{
		NodeKinds: []graph.NodeKind{"file", TypeNodeKind, FunctionNodeKind, MethodNodeKind, VariableNodeKind},
		Relations: []graph.RelationDefinition{
			{
				Kind: ImportsFromRelation,
				Endpoints: []graph.EndpointRule{
					{Source: "file", Target: "file"},
					{Source: "file", Target: TypeNodeKind},
					{Source: "file", Target: FunctionNodeKind},
					{Source: "file", Target: MethodNodeKind},
					{Source: "file", Target: VariableNodeKind},
				},
			},
			{Kind: ImplementsRelation, Endpoints: []graph.EndpointRule{{Source: TypeNodeKind, Target: TypeNodeKind}}},
			{Kind: EmbedsRelation, Endpoints: []graph.EndpointRule{{Source: TypeNodeKind, Target: TypeNodeKind}}},
			{Kind: CallsRelation, Endpoints: []graph.EndpointRule{
				{Source: FunctionNodeKind, Target: FunctionNodeKind},
				{Source: FunctionNodeKind, Target: MethodNodeKind},
				{Source: MethodNodeKind, Target: FunctionNodeKind},
				{Source: MethodNodeKind, Target: MethodNodeKind},
			}},
		},
	})
}

func ResolveWithFileView(contributions []extractor.Contribution, view extractor.ResolverFileView) (Resolution, error) {
	modulePath, _ := modulePath(view)

	files := make(map[string]graph.Node, len(contributions))
	nodes := make(map[string]graph.Node)
	packages := make(map[string]string, len(contributions))
	surfaces := make(map[string][]extractor.ExportedSurface, len(contributions))
	for _, contribution := range contributions {
		file, found := fileFact(contribution)
		if !found {
			return Resolution{}, fmt.Errorf("resolve Go contribution %q: file fact is required", contribution.SourcePath())
		}
		files[contribution.SourcePath()] = file
		for _, node := range contribution.Facts().Nodes {
			if node.Kind == PackageNodeKind {
				packages[contribution.SourcePath()] = node.Label
			}
			if isResolverNodeKind(node.Kind) {
				nodes[node.ID] = node
			}
		}
		surfaces[contribution.SourcePath()] = contribution.ExportedSurfaces()
	}

	paths := make([]string, 0, len(files))
	for sourcePath := range files {
		paths = append(paths, sourcePath)
	}
	sort.Strings(paths)

	resolution := Resolution{facts: graph.Facts{Nodes: make([]graph.Node, 0, len(nodes))}}
	for _, node := range nodes {
		resolution.facts.Nodes = append(resolution.facts.Nodes, node)
	}
	appendLocalCallFacts(contributions, &resolution)
	for _, sourcePath := range paths {
		contribution := contributionForPath(contributions, sourcePath)
		for _, reference := range contribution.UnresolvedReferences() {
			targetPaths := packageFiles(reference.Target, modulePath, files, packages)
			if len(targetPaths) == 0 {
				if strings.HasPrefix(reference.Target, modulePath+"/") || reference.Target == modulePath {
					resolution.diagnostics = append(resolution.diagnostics, extractor.Diagnostic{Severity: extractor.DiagnosticWarning, Message: fmt.Sprintf("Go package %q from %q is not indexed", reference.Target, sourcePath)})
				}
				continue
			}
			for _, targetPath := range targetPaths {
				resolution.facts.Edges = append(resolution.facts.Edges, graph.Edge{SourceID: reference.SourceID, TargetID: files[targetPath].ID, Relation: ImportsFromRelation, Evidence: files[sourcePath].Evidence})
				for _, surface := range surfaces[targetPath] {
					resolution.facts.Edges = append(resolution.facts.Edges, graph.Edge{SourceID: reference.SourceID, TargetID: surface.NodeID, Relation: ImportsFromRelation, Evidence: files[sourcePath].Evidence})
				}
			}
		}
	}
	appendPackageCallFacts(contributions, modulePath, files, packages, surfaces, &resolution)
	appendLocalMethodCallFacts(contributions, &resolution)
	appendImplementationFacts(contributions, &resolution)

	sort.Slice(resolution.facts.Edges, func(left, right int) bool {
		if resolution.facts.Edges[left].SourceID != resolution.facts.Edges[right].SourceID {
			return resolution.facts.Edges[left].SourceID < resolution.facts.Edges[right].SourceID
		}
		return resolution.facts.Edges[left].TargetID < resolution.facts.Edges[right].TargetID
	})
	vocabulary, err := ResolverVocabulary()
	if err != nil {
		return Resolution{}, fmt.Errorf("get Go resolver vocabulary: %w", err)
	}
	if err := vocabulary.Validate(resolution.facts); err != nil {
		return Resolution{}, fmt.Errorf("validate Go resolution: %w", err)
	}
	return resolution, nil
}

func ResolvePage(ctx context.Context, contributions []extractor.Contribution, projectID string, index extractor.ResolverIndex, view extractor.ResolverFileView) (Resolution, error) {
	if projectID == "" || index == nil {
		return Resolution{}, fmt.Errorf("resolve Go page: project and resolver index are required")
	}
	index = extractor.NewPageResolverIndex(index)
	modulePath, _ := modulePath(view)
	files := make(map[string]graph.Node, len(contributions))
	byPath := make(map[string]extractor.Contribution, len(contributions))
	nodes := make(map[string]graph.Node)
	for _, contribution := range contributions {
		file, found := fileFact(contribution)
		if !found {
			return Resolution{}, fmt.Errorf("resolve Go contribution %q: file fact is required", contribution.SourcePath())
		}
		files[contribution.SourcePath()] = file
		byPath[contribution.SourcePath()] = contribution
		for _, node := range contribution.Facts().Nodes {
			if isResolverNodeKind(node.Kind) {
				nodes[node.ID] = node
			}
		}
	}
	paths := make([]string, 0, len(files))
	for sourcePath := range files {
		paths = append(paths, sourcePath)
	}
	sort.Strings(paths)
	resolution := Resolution{}
	appendLocalCallFacts(contributions, &resolution)
	for _, sourcePath := range paths {
		contribution := byPath[sourcePath]
		for _, reference := range contribution.UnresolvedReferences() {
			packagePath, found := modulePackagePath(reference.Target, modulePath)
			if !found {
				continue
			}
			foundTarget := false
			err := visitPackageTargets(ctx, index, projectID, packagePath, func(target extractor.ResolverTarget) error {
				file, found := targetFile(target.Nodes)
				if !found {
					return fmt.Errorf("resolve Go target %q: file fact is required", target.SourcePath)
				}
				for _, node := range target.Nodes {
					if isResolverNodeKind(node.Kind) {
						nodes[node.ID] = node
					}
				}
				foundTarget = true
				resolution.facts.Edges = append(resolution.facts.Edges, graph.Edge{SourceID: reference.SourceID, TargetID: file.ID, Relation: ImportsFromRelation, Evidence: files[sourcePath].Evidence})
				for _, surface := range target.ExportedSurfaces {
					resolution.facts.Edges = append(resolution.facts.Edges, graph.Edge{SourceID: reference.SourceID, TargetID: surface.NodeID, Relation: ImportsFromRelation, Evidence: files[sourcePath].Evidence})
				}
				return nil
			})
			if err != nil {
				return Resolution{}, err
			}
			if !foundTarget {
				resolution.diagnostics = append(resolution.diagnostics, extractor.Diagnostic{Severity: extractor.DiagnosticWarning, Message: fmt.Sprintf("Go package %q from %q is not indexed", reference.Target, sourcePath)})
			}
		}
		for _, reference := range contribution.SymbolReferences() {
			if reference.Relation != CallsRelation {
				continue
			}
			packageName, symbolName, found := strings.Cut(reference.Target, ".")
			if !found {
				continue
			}
			matches := make([]string, 0)
			matchingImport := false
			for _, imported := range contribution.UnresolvedReferences() {
				if path.Base(imported.Target) != packageName {
					continue
				}
				matchingImport = true
				packagePath, found := modulePackagePath(imported.Target, modulePath)
				if !found {
					continue
				}
				err := visitPackageTargets(ctx, index, projectID, packagePath, func(target extractor.ResolverTarget) error {
					for _, node := range target.Nodes {
						if isResolverNodeKind(node.Kind) {
							nodes[node.ID] = node
						}
					}
					for _, surface := range target.ExportedSurfaces {
						if surface.Name == symbolName && callableTargetSurface(surface.NodeID, target.Nodes) {
							matches = append(matches, surface.NodeID)
						}
					}
					return nil
				})
				if err != nil {
					return Resolution{}, err
				}
			}
			if !matchingImport || len(matches) != 1 {
				resolution.diagnostics = append(resolution.diagnostics, extractor.Diagnostic{Severity: extractor.DiagnosticWarning, Message: fmt.Sprintf("Go call %q from %q is unsupported or ambiguous", reference.Target, sourcePath)})
				continue
			}
			resolution.facts.Edges = append(resolution.facts.Edges, graph.Edge{SourceID: reference.SourceID, TargetID: matches[0], Relation: CallsRelation, Evidence: reference.Evidence})
		}
	}
	if err := appendImportedMethodCallFacts(ctx, index, projectID, modulePath, contributions, &resolution); err != nil {
		return Resolution{}, err
	}
	if err := appendPageImplementationFacts(ctx, index, projectID, modulePath, contributions, nodes, &resolution); err != nil {
		return Resolution{}, err
	}
	resolution.facts.Nodes = make([]graph.Node, 0, len(nodes))
	for _, node := range nodes {
		resolution.facts.Nodes = append(resolution.facts.Nodes, node)
	}
	sort.Slice(resolution.facts.Nodes, func(left, right int) bool {
		return resolution.facts.Nodes[left].ID < resolution.facts.Nodes[right].ID
	})
	sort.Slice(resolution.facts.Edges, func(left, right int) bool {
		if resolution.facts.Edges[left].SourceID != resolution.facts.Edges[right].SourceID {
			return resolution.facts.Edges[left].SourceID < resolution.facts.Edges[right].SourceID
		}
		return resolution.facts.Edges[left].TargetID < resolution.facts.Edges[right].TargetID
	})
	vocabulary, err := ResolverVocabulary()
	if err != nil {
		return Resolution{}, fmt.Errorf("get Go resolver vocabulary: %w", err)
	}
	if err := vocabulary.Validate(resolution.facts); err != nil {
		return Resolution{}, fmt.Errorf("validate Go page resolution: %w", err)
	}
	return resolution, nil
}

func appendLocalCallFacts(contributions []extractor.Contribution, resolution *Resolution) {
	for _, contribution := range contributions {
		for _, edge := range contribution.Facts().Edges {
			if edge.Relation == CallsRelation {
				resolution.facts.Edges = append(resolution.facts.Edges, edge)
			}
		}
	}
}

func appendLocalMethodCallFacts(contributions []extractor.Contribution, resolution *Resolution) {
	methodsByName := make(map[string][]string)
	for _, contribution := range contributions {
		for _, node := range contribution.Facts().Nodes {
			if node.Kind == MethodNodeKind {
				methodsByName[node.Label] = append(methodsByName[node.Label], node.ID)
			}
		}
	}
	for _, contribution := range contributions {
		importedPackages := make(map[string]struct{})
		for _, imported := range contribution.UnresolvedReferences() {
			importedPackages[path.Base(imported.Target)] = struct{}{}
		}
		for _, reference := range contribution.SymbolReferences() {
			if reference.Relation != CallsRelation {
				continue
			}
			receiver, method, found := strings.Cut(reference.Target, ".")
			if !found {
				continue
			}
			if _, imported := importedPackages[receiver]; imported {
				continue
			}
			for _, targetID := range methodsByName[method] {
				resolution.facts.Edges = append(resolution.facts.Edges, graph.Edge{
					SourceID: reference.SourceID,
					TargetID: targetID,
					Relation: CallsRelation,
					Evidence: reference.Evidence,
				})
			}
		}
	}
}

func appendImportedMethodCallFacts(ctx context.Context, index extractor.ResolverIndex, projectID, modulePath string, contributions []extractor.Contribution, resolution *Resolution) error {
	for _, contribution := range contributions {
		importedPackages := make(map[string]struct{})
		for _, imported := range contribution.UnresolvedReferences() {
			importedPackages[path.Base(imported.Target)] = struct{}{}
		}
		for _, reference := range contribution.SymbolReferences() {
			if reference.Relation != CallsRelation {
				continue
			}
			receiver, method, found := strings.Cut(reference.Target, ".")
			if !found {
				continue
			}
			if _, imported := importedPackages[receiver]; imported {
				continue
			}
			seen := make(map[string]struct{})
			for _, imported := range contribution.UnresolvedReferences() {
				packagePath, inModule := modulePackagePath(imported.Target, modulePath)
				if !inModule {
					continue
				}
				if err := visitPackageTargets(ctx, index, projectID, packagePath, func(target extractor.ResolverTarget) error {
					for _, node := range target.Nodes {
						if node.Kind != MethodNodeKind || node.Label != method {
							continue
						}
						if _, duplicate := seen[node.ID]; duplicate {
							continue
						}
						seen[node.ID] = struct{}{}
						resolution.facts.Edges = append(resolution.facts.Edges, graph.Edge{
							SourceID: reference.SourceID,
							TargetID: node.ID,
							Relation: CallsRelation,
							Evidence: reference.Evidence,
						})
					}
					return nil
				}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

const resolverPackagePageSize = 128

func visitPackageTargets(ctx context.Context, index extractor.ResolverIndex, projectID, packagePath string, visit func(extractor.ResolverTarget) error) error {
	after := ""
	for {
		page, err := index.ResolverPackagePage(ctx, extractor.ResolverPackagePageRequest{
			ProjectID:       projectID,
			Language:        "go",
			PackagePath:     packagePath,
			AfterSourcePath: after,
			Limit:           resolverPackagePageSize,
		})
		if err != nil {
			return fmt.Errorf("read Go resolver package %q: %w", packagePath, err)
		}
		if len(page) == 0 {
			return nil
		}
		for _, target := range page {
			after = target.SourcePath
			if err := visit(target); err != nil {
				return err
			}
		}
	}
}

func modulePackagePath(importPath, modulePath string) (string, bool) {
	if modulePath == "" || (importPath != modulePath && !strings.HasPrefix(importPath, modulePath+"/")) {
		return "", false
	}
	if importPath == modulePath {
		return ".", true
	}
	return strings.TrimPrefix(importPath, modulePath+"/"), true
}

func callableTargetSurface(nodeID string, nodes []graph.Node) bool {
	for _, node := range nodes {
		if node.ID == nodeID {
			return node.Kind == FunctionNodeKind || node.Kind == MethodNodeKind
		}
	}
	return false
}

func appendPageImplementationFacts(ctx context.Context, index extractor.ResolverIndex, projectID, modulePath string, contributions []extractor.Contribution, nodes map[string]graph.Node, resolution *Resolution) error {
	for _, contribution := range contributions {
		packageName := packageName(contribution.Facts().Nodes)
		if packageName == "" {
			continue
		}
		packagePath := path.Dir(contribution.SourcePath())
		for _, current := range contribution.Facts().Nodes {
			if current.Kind != TypeNodeKind {
				continue
			}
			methods, err := packageMethods(ctx, index, projectID, packagePath, packageName, current.Label, nodes)
			if err != nil {
				return err
			}
			if err := appendImplementedInterfaces(ctx, index, projectID, packagePath, packageName, current, methods, nodes, resolution); err != nil {
				return err
			}
			if err := appendEmbeddedInterfaces(ctx, index, projectID, packagePath, packageName, current, contribution.SymbolReferences(), nodes, resolution); err != nil {
				return err
			}
			if err := appendImportedInterfaceImplementations(ctx, index, projectID, modulePath, contribution, current, methods, nodes, resolution); err != nil {
				return err
			}
		}
	}
	return nil
}

func appendImportedInterfaceImplementations(ctx context.Context, index extractor.ResolverIndex, projectID, modulePath string, contribution extractor.Contribution, current graph.Node, methods map[string]struct{}, nodes map[string]graph.Node, resolution *Resolution) error {
	seen := make(map[string]struct{})
	for _, imported := range contribution.UnresolvedReferences() {
		packagePath, found := modulePackagePath(imported.Target, modulePath)
		if !found {
			continue
		}
		if err := visitPackageTargets(ctx, index, projectID, packagePath, func(target extractor.ResolverTarget) error {
			for _, node := range target.Nodes {
				if isResolverNodeKind(node.Kind) {
					nodes[node.ID] = node
				}
			}
			for _, node := range target.Nodes {
				if node.Kind != TypeNodeKind || node.ID == current.ID {
					continue
				}
				requirements := relationReferencesForSource(target.SymbolReferences, node.ID, ImplementsRelation)
				if len(requirements) == 0 || !implementsAll(methods, requirements) {
					continue
				}
				if _, duplicate := seen[node.ID]; duplicate {
					continue
				}
				seen[node.ID] = struct{}{}
				resolution.facts.Edges = append(resolution.facts.Edges, graph.Edge{
					SourceID: current.ID,
					TargetID: node.ID,
					Relation: ImplementsRelation,
					Evidence: current.Evidence,
				})
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func packageMethods(ctx context.Context, index extractor.ResolverIndex, projectID, packagePath, expectedPackageName, typeName string, nodes map[string]graph.Node) (map[string]struct{}, error) {
	methods := make(map[string]struct{})
	err := visitPackageTargets(ctx, index, projectID, packagePath, func(target extractor.ResolverTarget) error {
		if expectedPackageName != packageName(target.Nodes) {
			return nil
		}
		for _, node := range target.Nodes {
			if isResolverNodeKind(node.Kind) {
				nodes[node.ID] = node
			}
			if node.Kind == MethodNodeKind && receiverTypeFromQualifiedName(node.QualifiedName) == typeName {
				methods[node.Label] = struct{}{}
			}
		}
		return nil
	})
	return methods, err
}

func appendImplementedInterfaces(ctx context.Context, index extractor.ResolverIndex, projectID, packagePath, expectedPackageName string, current graph.Node, methods map[string]struct{}, nodes map[string]graph.Node, resolution *Resolution) error {
	return visitPackageTargets(ctx, index, projectID, packagePath, func(target extractor.ResolverTarget) error {
		if expectedPackageName != packageName(target.Nodes) {
			return nil
		}
		for _, node := range target.Nodes {
			if isResolverNodeKind(node.Kind) {
				nodes[node.ID] = node
			}
			if node.Kind != TypeNodeKind || node.ID == current.ID {
				continue
			}
			requirements := relationReferencesForSource(target.SymbolReferences, node.ID, ImplementsRelation)
			if implementsAll(methods, requirements) {
				resolution.facts.Edges = append(resolution.facts.Edges, graph.Edge{SourceID: current.ID, TargetID: node.ID, Relation: ImplementsRelation, Evidence: current.Evidence})
			}
		}
		return nil
	})
}

func appendEmbeddedInterfaces(ctx context.Context, index extractor.ResolverIndex, projectID, packagePath, expectedPackageName string, current graph.Node, references []extractor.SymbolReference, nodes map[string]graph.Node, resolution *Resolution) error {
	references = relationReferencesForSource(references, current.ID, EmbedsRelation)
	if len(references) == 0 {
		return nil
	}
	return visitPackageTargets(ctx, index, projectID, packagePath, func(target extractor.ResolverTarget) error {
		if expectedPackageName != packageName(target.Nodes) {
			return nil
		}
		for _, node := range target.Nodes {
			if isResolverNodeKind(node.Kind) {
				nodes[node.ID] = node
			}
			if node.Kind != TypeNodeKind || node.ID == current.ID {
				continue
			}
			for _, reference := range references {
				if node.Label == reference.Target {
					resolution.facts.Edges = append(resolution.facts.Edges, graph.Edge{SourceID: current.ID, TargetID: node.ID, Relation: EmbedsRelation, Evidence: reference.Evidence})
				}
			}
		}
		return nil
	})
}

func packageName(nodes []graph.Node) string {
	for _, node := range nodes {
		if node.Kind == PackageNodeKind {
			return node.Label
		}
	}
	return ""
}

func relationReferencesForSource(references []extractor.SymbolReference, sourceID string, relation graph.RelationKind) []extractor.SymbolReference {
	matching := make([]extractor.SymbolReference, 0)
	for _, reference := range references {
		if reference.SourceID == sourceID && reference.Relation == relation {
			matching = append(matching, reference)
		}
	}
	return matching
}

func targetFile(nodes []graph.Node) (graph.Node, bool) {
	for _, node := range nodes {
		if node.Kind == "file" {
			return node, true
		}
	}
	return graph.Node{}, false
}

func appendPackageCallFacts(contributions []extractor.Contribution, modulePath string, files map[string]graph.Node, packages map[string]string, surfaces map[string][]extractor.ExportedSurface, resolution *Resolution) {
	for _, contribution := range contributions {
		for _, reference := range contribution.SymbolReferences() {
			if reference.Relation != CallsRelation {
				continue
			}
			packageName, symbolName, found := strings.Cut(reference.Target, ".")
			if !found {
				continue
			}
			matches := make([]string, 0)
			matchingImport := false
			resolvedPackage := false
			for _, imported := range contribution.UnresolvedReferences() {
				if path.Base(imported.Target) != packageName {
					continue
				}
				matchingImport = true
				targetPaths := packageFiles(imported.Target, modulePath, files, packages)
				if len(targetPaths) == 0 {
					continue
				}
				resolvedPackage = true
				for _, targetPath := range targetPaths {
					for _, surface := range surfaces[targetPath] {
						if surface.Name == symbolName && callableSurface(surface.NodeID, contributions) {
							matches = append(matches, surface.NodeID)
						}
					}
				}
			}
			if !matchingImport {
				resolution.diagnostics = append(resolution.diagnostics, extractor.Diagnostic{
					Severity: extractor.DiagnosticWarning,
					Message:  fmt.Sprintf("Go call %q from %q is unsupported or ambiguous", reference.Target, contribution.SourcePath()),
				})
				continue
			}
			if !resolvedPackage {
				continue
			}
			if len(matches) != 1 {
				resolution.diagnostics = append(resolution.diagnostics, extractor.Diagnostic{
					Severity: extractor.DiagnosticWarning,
					Message:  fmt.Sprintf("Go call %q from %q is unsupported or ambiguous", reference.Target, contribution.SourcePath()),
				})
				continue
			}
			resolution.facts.Edges = append(resolution.facts.Edges, graph.Edge{
				SourceID: reference.SourceID,
				TargetID: matches[0],
				Relation: CallsRelation,
				Evidence: reference.Evidence,
			})
		}
	}
}

func callableSurface(nodeID string, contributions []extractor.Contribution) bool {
	for _, contribution := range contributions {
		for _, node := range contribution.Facts().Nodes {
			if node.ID != nodeID {
				continue
			}
			return node.Kind == FunctionNodeKind || node.Kind == MethodNodeKind
		}
	}
	return false
}

func appendImplementationFacts(contributions []extractor.Contribution, resolution *Resolution) {
	methodsByType := make(map[string]map[string]struct{})
	typesByPackage := make(map[string][]graph.Node)
	interfaces := make(map[string][]extractor.SymbolReference)
	packageByNodeID := make(map[string]string)

	for _, contribution := range contributions {
		packageName := ""
		for _, node := range contribution.Facts().Nodes {
			if node.Kind == PackageNodeKind {
				packageName = node.Label
			}
			if node.Kind == TypeNodeKind {
				typesByPackage[packageName] = append(typesByPackage[packageName], node)
				packageByNodeID[node.ID] = packageName
			}
			if node.Kind == MethodNodeKind {
				typeName := receiverTypeFromQualifiedName(node.QualifiedName)
				if typeName != "" {
					key := packageName + "\x00" + typeName
					if methodsByType[key] == nil {
						methodsByType[key] = make(map[string]struct{})
					}
					methodsByType[key][node.Label] = struct{}{}
				}
			}
		}
		for _, reference := range contribution.SymbolReferences() {
			if reference.Relation == ImplementsRelation || reference.Relation == EmbedsRelation {
				interfaces[reference.SourceID] = append(interfaces[reference.SourceID], reference)
			}
		}
	}

	for _, types := range typesByPackage {
		for _, contract := range types {
			references, isInterface := interfaces[contract.ID]
			if !isInterface {
				continue
			}
			requirements := relationReferences(references, ImplementsRelation)
			for _, implementation := range types {
				key := packageByNodeID[implementation.ID] + "\x00" + implementation.Label
				if implementation.ID == contract.ID || !implementsAll(methodsByType[key], requirements) {
					continue
				}
				resolution.facts.Edges = append(resolution.facts.Edges, graph.Edge{
					SourceID: implementation.ID,
					TargetID: contract.ID,
					Relation: ImplementsRelation,
					Evidence: implementation.Evidence,
				})
			}
			for _, reference := range relationReferences(references, EmbedsRelation) {
				for _, target := range types {
					if target.Label != reference.Target {
						continue
					}
					resolution.facts.Edges = append(resolution.facts.Edges, graph.Edge{
						SourceID: contract.ID,
						TargetID: target.ID,
						Relation: EmbedsRelation,
						Evidence: reference.Evidence,
					})
				}
			}
		}
	}
}

func relationReferences(references []extractor.SymbolReference, relation graph.RelationKind) []extractor.SymbolReference {
	matching := make([]extractor.SymbolReference, 0, len(references))
	for _, reference := range references {
		if reference.Relation == relation {
			matching = append(matching, reference)
		}
	}
	return matching
}

func isResolverNodeKind(kind graph.NodeKind) bool {
	switch kind {
	case "file", TypeNodeKind, FunctionNodeKind, MethodNodeKind, VariableNodeKind:
		return true
	default:
		return false
	}
}

func implementsAll(methods map[string]struct{}, requirements []extractor.SymbolReference) bool {
	if len(requirements) == 0 {
		return false
	}
	for _, requirement := range requirements {
		if _, found := methods[requirement.Target]; !found {
			return false
		}
	}
	return true
}

func receiverTypeFromQualifiedName(qualifiedName string) string {
	parts := strings.Split(qualifiedName, ".")
	if len(parts) < 3 {
		return ""
	}
	return parts[len(parts)-2]
}

func modulePath(view extractor.ResolverFileView) (string, bool) {
	contents, found := view.File("go.mod")
	if !found {
		return "", false
	}
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1], true
		}
	}
	return "", false
}

func packageFiles(importPath, modulePath string, files map[string]graph.Node, packages map[string]string) []string {
	if modulePath == "" {
		return nil
	}
	if importPath != modulePath && !strings.HasPrefix(importPath, modulePath+"/") {
		return nil
	}
	directory := "."
	if importPath != modulePath {
		directory = strings.TrimPrefix(importPath, modulePath+"/")
	}
	paths := make([]string, 0)
	for sourcePath := range files {
		if path.Dir(sourcePath) == directory && packages[sourcePath] != "" {
			paths = append(paths, sourcePath)
		}
	}
	sort.Strings(paths)
	return paths
}

func contributionForPath(contributions []extractor.Contribution, sourcePath string) extractor.Contribution {
	for _, contribution := range contributions {
		if contribution.SourcePath() == sourcePath {
			return contribution
		}
	}
	return extractor.Contribution{}
}

func fileFact(contribution extractor.Contribution) (graph.Node, bool) {
	for _, node := range contribution.Facts().Nodes {
		if node.Kind == "file" {
			return node, true
		}
	}
	return graph.Node{}, false
}
