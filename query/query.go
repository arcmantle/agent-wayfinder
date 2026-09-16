package query

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"agent-wayfinder/graph"
	"agent-wayfinder/storage"
)

type Request struct {
	Plan         *QueryPlan
	Terms        []string
	Retrievals   []storage.LexicalSearchRequest
	SeedRequests []SeedRequest
	ProjectIDs   []string
	Relations    []graph.RelationKind
	MaxDepth     int
	MaxNodes     int
}

type Result struct {
	Seeds             []SeedSet
	Facts             graph.Facts
	Impact            []ImpactEvidence
	Evidence          []EvidenceGroup
	Limits            []StageLimit
	Warnings          []PlanWarning
	TruncationReasons []storage.TruncationReason
	ScopeBoundary     *ScopeBoundary
}

type EvidenceGroup struct {
	Rank       int                     `json:"rank"`
	Score      float64                 `json:"score"`
	Components EvidenceScoreComponents `json:"components"`
	Reason     string                  `json:"reason"`
	SlotRole   string                  `json:"slotRole"`
	Relation   graph.RelationKind      `json:"relation,omitempty"`
	Distance   int                     `json:"distance"`
	Nodes      []graph.Node            `json:"nodes"`
	Edges      []graph.Edge            `json:"edges"`
}

type EvidenceScoreComponents struct {
	SeedRelevance float64 `json:"seedRelevance"`
	Distance      float64 `json:"distance"`
	RelationFit   float64 `json:"relationFit"`
	Confidence    float64 `json:"confidence"`
	SharedSupport float64 `json:"sharedSupport"`
	PathDiversity float64 `json:"pathDiversity"`
}

type StageLimit struct {
	Stage             string                     `json:"stage"`
	SlotRole          string                     `json:"slotRole,omitempty"`
	MaxDepth          int                        `json:"maxDepth"`
	MaxNodes          int                        `json:"maxNodes"`
	TruncationReasons []storage.TruncationReason `json:"truncationReasons,omitempty"`
}

type ImpactEvidence struct {
	Node     graph.Node         `json:"node"`
	Relation graph.RelationKind `json:"relation"`
	Distance int                `json:"distance"`
	Score    float64            `json:"score"`
}

func QuerySnapshot(ctx context.Context, lookup storage.NodeLookup, traverser storage.Traverser, snapshot storage.Snapshot, request Request) (Result, error) {
	if lookup == nil {
		return Result{}, fmt.Errorf("query published graph: node lookup is required")
	}
	if traverser == nil {
		return Result{}, fmt.Errorf("query published graph: traverser is required")
	}
	if request.Plan == nil && len(request.Terms) == 0 && len(request.Retrievals) == 0 && len(request.SeedRequests) == 0 || request.MaxDepth < 0 || request.MaxNodes <= 0 {
		return Result{}, fmt.Errorf("query published graph: terms, nonnegative maximum depth, and positive maximum nodes are required")
	}

	var seeds []SeedSet
	var err error
	if request.Plan != nil {
		searcher, supported := lookup.(storage.LexicalSearcher)
		if !supported {
			return Result{}, fmt.Errorf("query published graph: lexical search is required for structured retrieval")
		}
		seedRequests := make([]SeedRequest, len(request.Plan.EntitySlots))
		for index, slot := range request.Plan.EntitySlots {
			seedRequests[index] = SeedRequest{Role: slot.Role, Retrieval: slot.Retrieval}
		}
		seeds, err = RankSeedRequestsSnapshot(ctx, lookup, searcher, snapshot, seedRequests)
	} else if len(request.SeedRequests) > 0 {
		searcher, supported := lookup.(storage.LexicalSearcher)
		if !supported {
			return Result{}, fmt.Errorf("query published graph: lexical search is required for structured retrieval")
		}
		seeds, err = RankSeedRequestsSnapshot(ctx, lookup, searcher, snapshot, request.SeedRequests)
	} else if len(request.Retrievals) > 0 {
		searcher, supported := lookup.(storage.LexicalSearcher)
		if !supported {
			return Result{}, fmt.Errorf("query published graph: lexical search is required for structured retrieval")
		}
		seeds, err = RankRetrievalsSnapshot(ctx, lookup, searcher, snapshot, request.Retrievals)
	} else {
		seeds, err = RankSnapshot(ctx, lookup, snapshot, request.Terms)
	}
	if err != nil {
		return Result{}, fmt.Errorf("query published graph: %w", err)
	}
	if request.Plan != nil {
		warnings := emptyEntitySlotWarnings(request.Plan, seeds)
		if len(warnings) > 0 {
			return Result{
				Seeds:    seeds,
				Evidence: retrievalEvidence(request.Plan, seeds),
				Limits:   retrievalLimits(request.Plan),
				Warnings: warnings,
			}, nil
		}
	}
	if request.Plan != nil && (request.Plan.Operator == OperatorLookup || request.Plan.Operator == OperatorExplain) {
		warning := entityResolutionWarning(request.Plan.Operator, seeds)
		if request.Plan.Operator == OperatorLookup || warning != nil {
			result := Result{Seeds: seeds, Evidence: retrievalEvidence(request.Plan, seeds), Limits: retrievalLimits(request.Plan)}
			if warning != nil {
				result.Warnings = []PlanWarning{*warning}
			}
			return result, nil
		}
	}
	startNodeIDCount := 0
	for _, seedSet := range seeds {
		startNodeIDCount += len(seedSet.Nodes)
	}
	startNodeIDs := make([]string, 0, startNodeIDCount)
	for _, seedSet := range seeds {
		for _, node := range seedSet.Nodes {
			startNodeIDs = append(startNodeIDs, node.ID)
		}
	}
	if len(startNodeIDs) == 0 {
		result := Result{Seeds: seeds}
		if request.Plan != nil {
			result.Limits = retrievalLimits(request.Plan)
		}
		return result, nil
	}
	if request.Plan != nil && request.Plan.Operator == OperatorIntersection {
		return executeIntersectionPlan(ctx, traverser, snapshot, request, seeds)
	}
	if request.Plan != nil && request.Plan.Operator == OperatorPath {
		return executePathPlan(ctx, traverser, snapshot, request, seeds)
	}

	direction := storage.TraverseOutgoing
	maximumDepth := request.MaxDepth
	relations := append([]graph.RelationKind(nil), request.Relations...)
	if request.Plan != nil {
		switch request.Plan.Operator {
		case OperatorExplain:
			direction = storage.TraverseBoth
			maximumDepth = min(maximumDepth, 1)
		case OperatorNeighbors:
			direction = request.Plan.Direction
			relations = append([]graph.RelationKind(nil), request.Plan.AllowedRelations...)
			if request.Plan.Intent == IntentCalls || request.Plan.Intent == IntentCalledBy {
				relations = callRelations(relations)
			}
			if request.Plan.Intent == IntentDependencies || request.Plan.Intent == IntentDependents || request.Plan.Intent == IntentDependencyComparison {
				relations = dependencyRelations(relations)
			}
		case OperatorImpact:
			direction = storage.TraverseIncoming
			relations = impactRelations(request.Plan.AllowedRelations)
		}
	}
	traversal, err := traverser.Traverse(ctx, snapshot, storage.TraversalRequest{
		StartNodeIDs: startNodeIDs,
		ProjectIDs:   append([]string(nil), request.ProjectIDs...),
		Direction:    direction,
		Relations:    relations,
		MaxDepth:     maximumDepth,
		MaxNodes:     request.MaxNodes,
	})
	if err != nil {
		return Result{}, fmt.Errorf("query published graph: %w", err)
	}
	var boundary *ScopeBoundary
	if traversal.ScopeBoundary != nil {
		boundary = &ScopeBoundary{Node: *traversal.ScopeBoundary}
	}
	result := Result{
		Seeds:             seeds,
		Facts:             traversal.Facts,
		TruncationReasons: traversal.TruncationReasons,
		ScopeBoundary:     boundary,
	}
	if request.Plan != nil {
		result.Evidence = traversalEvidence(request.Plan, seeds, traversal.Facts)
		result.Limits = append(retrievalLimits(request.Plan), StageLimit{
			Stage:             "traversal",
			MaxDepth:          maximumDepth,
			MaxNodes:          request.MaxNodes,
			TruncationReasons: append([]storage.TruncationReason(nil), traversal.TruncationReasons...),
		})
	}
	if request.Plan != nil && request.Plan.Operator == OperatorImpact {
		result.Impact = rankImpactEvidence(traversal.Facts, startNodeIDs)
	}
	return result, nil
}

func retrievalEvidence(plan *QueryPlan, seeds []SeedSet) []EvidenceGroup {
	evidence := make([]EvidenceGroup, 0)
	for _, seedSet := range seeds {
		for index, node := range seedSet.Nodes {
			seedRelevance := 1.0
			if index < len(seedSet.Rankings) {
				seedRelevance = seedSet.Rankings[index].Score
			}
			components := EvidenceScoreComponents{
				SeedRelevance: seedRelevance,
				Distance:      1,
				RelationFit:   1,
				Confidence:    plan.Confidence,
				SharedSupport: 1,
				PathDiversity: 1,
			}
			evidence = append(evidence, EvidenceGroup{
				Score:      evidenceScore(components),
				Components: components,
				Reason:     fmt.Sprintf("Matched the %s slot during ranked retrieval.", seedSet.Role),
				SlotRole:   seedSet.Role,
				Nodes:      []graph.Node{node},
			})
		}
	}
	sort.Slice(evidence, func(left, right int) bool {
		if evidence[left].Score != evidence[right].Score {
			return evidence[left].Score > evidence[right].Score
		}
		return evidence[left].Nodes[0].ID < evidence[right].Nodes[0].ID
	})
	for index := range evidence {
		evidence[index].Rank = index + 1
	}
	return evidence
}

func retrievalLimits(plan *QueryPlan) []StageLimit {
	limits := make([]StageLimit, len(plan.EntitySlots))
	for index, slot := range plan.EntitySlots {
		limits[index] = StageLimit{Stage: "retrieval", SlotRole: slot.Role, MaxNodes: slot.Retrieval.Limit}
	}
	return limits
}

func callRelations(relations []graph.RelationKind) []graph.RelationKind {
	return expandLanguageRelations(relations, "calls")
}

func dependencyRelations(relations []graph.RelationKind) []graph.RelationKind {
	expanded := make([]graph.RelationKind, 0, len(relations)*4)
	for _, relation := range relations {
		expanded = append(expanded, expandLanguageRelations([]graph.RelationKind{relation}, relation)...)
	}
	return expanded
}

func impactRelations(relations []graph.RelationKind) []graph.RelationKind {
	expanded := make([]graph.RelationKind, 0, len(relations)*4)
	for _, relation := range relations {
		switch relation {
		case "implements", "calls", "imports_from", "requires", "depends_on":
			expanded = append(expanded, expandLanguageRelations([]graph.RelationKind{relation}, relation)...)
		default:
			expanded = append(expanded, relation)
		}
	}
	return expanded
}

func expandLanguageRelations(relations []graph.RelationKind, generic graph.RelationKind) []graph.RelationKind {
	expanded := make([]graph.RelationKind, 0, len(relations)+3)
	for _, relation := range relations {
		expanded = append(expanded, relation)
		if relation == generic {
			expanded = append(expanded, "go:"+generic, "javascript:"+generic, "typescript:"+generic)
		}
	}
	return expanded
}

func traversalEvidence(plan *QueryPlan, seeds []SeedSet, facts graph.Facts) []EvidenceGroup {
	if len(facts.Nodes) == 0 && len(facts.Edges) == 0 {
		return nil
	}
	slotRole := ""
	seedRelevance := 1.0
	if len(seeds) > 0 {
		slotRole = seeds[0].Role
		if len(seeds[0].Rankings) > 0 {
			seedRelevance = seeds[0].Rankings[0].Score
		}
	}
	nodesByID := make(map[string]graph.Node, len(facts.Nodes))
	for _, node := range facts.Nodes {
		nodesByID[node.ID] = node
	}
	type groupKey struct {
		distance int
		relation graph.RelationKind
	}
	groups := make(map[groupKey][]graph.Edge)
	visited := make(map[string]struct{})
	frontier := make([]string, 0)
	for _, seedSet := range seeds {
		for _, node := range seedSet.Nodes {
			if _, found := visited[node.ID]; found {
				continue
			}
			visited[node.ID] = struct{}{}
			frontier = append(frontier, node.ID)
		}
	}
	edges := append([]graph.Edge(nil), facts.Edges...)
	sortEdges(edges)
	for distance := 1; len(frontier) > 0; distance++ {
		next := make([]string, 0)
		for _, currentID := range frontier {
			for _, edge := range edges {
				neighborID, matched := traversalNeighbor(plan.Direction, currentID, edge)
				if !matched {
					continue
				}
				if _, found := visited[neighborID]; found {
					continue
				}
				visited[neighborID] = struct{}{}
				next = append(next, neighborID)
				groups[groupKey{distance: distance, relation: edge.Relation}] = append(groups[groupKey{distance: distance, relation: edge.Relation}], edge)
			}
		}
		frontier = next
	}
	evidence := make([]EvidenceGroup, 0, len(groups))
	for key, groupEdges := range groups {
		groupNodes := nodesForEdges(nodesByID, groupEdges)
		components := EvidenceScoreComponents{
			SeedRelevance: seedRelevance,
			Distance:      1 / float64(key.distance),
			RelationFit:   relationFit(plan.AllowedRelations, key.relation),
			Confidence:    plan.Confidence,
			SharedSupport: 1,
			PathDiversity: float64(len(groupEdges)),
		}
		evidence = append(evidence, EvidenceGroup{
			Score:      evidenceScore(components),
			Components: components,
			Reason:     fmt.Sprintf("Matched the %s slot through %s at distance %d.", slotRole, key.relation, key.distance),
			SlotRole:   slotRole,
			Relation:   key.relation,
			Distance:   key.distance,
			Nodes:      groupNodes,
			Edges:      groupEdges,
		})
	}
	sort.Slice(evidence, func(left, right int) bool {
		if evidence[left].Score != evidence[right].Score {
			return evidence[left].Score > evidence[right].Score
		}
		if evidence[left].Distance != evidence[right].Distance {
			return evidence[left].Distance < evidence[right].Distance
		}
		return evidence[left].Relation < evidence[right].Relation
	})
	for index := range evidence {
		evidence[index].Rank = index + 1
	}
	return evidence
}

func traversalNeighbor(direction storage.TraversalDirection, currentID string, edge graph.Edge) (string, bool) {
	if direction != storage.TraverseIncoming && edge.SourceID == currentID {
		return edge.TargetID, true
	}
	if direction != storage.TraverseOutgoing && edge.TargetID == currentID {
		return edge.SourceID, true
	}
	return "", false
}

func nodesForEdges(nodesByID map[string]graph.Node, edges []graph.Edge) []graph.Node {
	nodes := make([]graph.Node, 0, len(edges)+1)
	seen := make(map[string]struct{}, len(edges)+1)
	for _, edge := range edges {
		for _, nodeID := range []string{edge.SourceID, edge.TargetID} {
			if _, found := seen[nodeID]; found {
				continue
			}
			node, found := nodesByID[nodeID]
			if !found {
				continue
			}
			seen[nodeID] = struct{}{}
			nodes = append(nodes, node)
		}
	}
	return nodes
}

func sortEdges(edges []graph.Edge) {
	sort.Slice(edges, func(left, right int) bool {
		if edges[left].SourceID != edges[right].SourceID {
			return edges[left].SourceID < edges[right].SourceID
		}
		if edges[left].TargetID != edges[right].TargetID {
			return edges[left].TargetID < edges[right].TargetID
		}
		return edges[left].Relation < edges[right].Relation
	})
}

func relationFit(allowed []graph.RelationKind, relation graph.RelationKind) float64 {
	for _, candidate := range allowed {
		if relation == candidate || strings.HasSuffix(string(relation), ":"+string(candidate)) {
			return 1
		}
	}
	return 0.5
}

func evidenceScore(components EvidenceScoreComponents) float64 {
	return components.SeedRelevance * components.Distance * components.RelationFit * components.Confidence * components.SharedSupport * components.PathDiversity
}

func executePathPlan(ctx context.Context, traverser storage.Traverser, snapshot storage.Snapshot, request Request, seeds []SeedSet) (Result, error) {
	if len(seeds) != 2 {
		return Result{}, fmt.Errorf("query published graph path: exactly two entity slots are required")
	}
	sources := pathEndpointCandidates(seeds[0])
	targets := pathEndpointCandidates(seeds[1])
	if len(sources) != 1 || len(targets) != 1 {
		if !pathCandidatesNeedDisambiguation(seeds) {
			warning := PlanWarning{
				Code:    "ambiguous_path_endpoints",
				Message: "The path endpoints are ambiguous; no graph operation was executed.",
			}
			for _, source := range seeds[0].Nodes {
				for _, target := range seeds[1].Nodes {
					warning.Suggestions = append(warning.Suggestions, fmt.Sprintf("Run path from %s to %s.", source.ID, target.ID))
				}
			}
			return Result{Seeds: seeds, Warnings: []PlanWarning{warning}}, nil
		}
	}
	if len(sources) == 0 || len(targets) == 0 {
		warning := PlanWarning{
			Code:    "ambiguous_path_endpoints",
			Message: "The path endpoints are ambiguous; no graph operation was executed.",
		}
		for _, source := range seeds[0].Nodes {
			for _, target := range seeds[1].Nodes {
				warning.Suggestions = append(warning.Suggestions, fmt.Sprintf("Run path from %s to %s.", source.ID, target.ID))
			}
		}
		return Result{Seeds: seeds, Warnings: []PlanWarning{warning}}, nil
	}
	var traversal storage.TraversalResult
	var path PathResult
	var foundPath bool
	for _, source := range sources {
		currentTraversal, err := traverser.Traverse(ctx, snapshot, storage.TraversalRequest{
			StartNodeIDs: []string{source.ID},
			ProjectIDs:   append([]string(nil), request.ProjectIDs...),
			Direction:    storage.TraverseOutgoing,
			Relations:    callRelations(request.Plan.AllowedRelations),
			MaxDepth:     request.MaxDepth,
			MaxNodes:     request.MaxNodes,
		})
		if err != nil {
			return Result{}, fmt.Errorf("query published graph path: %w", err)
		}
		traversal = currentTraversal
		for _, target := range targets {
			candidate := directedPath(currentTraversal.Facts, source.ID, target.ID)
			if len(candidate.Nodes) == 0 {
				continue
			}
			path = candidate
			foundPath = true
			break
		}
		if foundPath {
			break
		}
	}
	result := Result{
		Seeds:             seeds,
		Facts:             graph.Facts{Nodes: path.Nodes, Edges: path.Edges},
		TruncationReasons: traversal.TruncationReasons,
		Limits: append(retrievalLimits(request.Plan), StageLimit{
			Stage:             "path",
			MaxDepth:          request.MaxDepth,
			MaxNodes:          request.MaxNodes,
			TruncationReasons: append([]storage.TruncationReason(nil), traversal.TruncationReasons...),
		}),
	}
	if len(path.Nodes) == 0 {
		if len(sources) == 0 || len(targets) == 0 {
			return result, nil
		}
		result.Warnings = []PlanWarning{{
			Code:        "no_directed_path",
			Message:     "No directed path was found; no reachability claim was produced.",
			Suggestions: []string{fmt.Sprintf("Run path from %s to %s with undirected fallback.", sources[0].ID, targets[0].ID)},
		}}
	}
	if len(path.Nodes) > 0 && traversal.ScopeBoundary != nil {
		result.ScopeBoundary = &ScopeBoundary{Node: *traversal.ScopeBoundary}
	}
	if len(path.Nodes) > 0 {
		components := EvidenceScoreComponents{
			SeedRelevance: seedSetRelevance(seeds),
			Distance:      1 / float64(max(1, len(path.Edges))),
			RelationFit:   1,
			Confidence:    request.Plan.Confidence,
			SharedSupport: float64(len(seeds)),
			PathDiversity: 1,
		}
		relation := graph.RelationKind("")
		if len(path.Edges) > 0 {
			relation = path.Edges[0].Relation
		}
		result.Evidence = []EvidenceGroup{{
			Rank:       1,
			Score:      evidenceScore(components),
			Components: components,
			Reason:     fmt.Sprintf("The %s slot reaches the %s slot through a directed path.", seeds[0].Role, seeds[1].Role),
			SlotRole:   seeds[0].Role + "+" + seeds[1].Role,
			Relation:   relation,
			Distance:   len(path.Edges),
			Nodes:      append([]graph.Node(nil), path.Nodes...),
			Edges:      append([]graph.Edge(nil), path.Edges...),
		}}
	}
	return result, nil
}

func pathEndpointCandidates(seed SeedSet) []graph.Node {
	if len(seed.Nodes) == 0 || len(seed.Rankings) != len(seed.Nodes) {
		return append([]graph.Node(nil), seed.Nodes...)
	}
	exact := make([]graph.Node, 0, len(seed.Nodes))
	for index, ranking := range seed.Rankings {
		if ranking.Components.Exact > 0 && ranking.Components.Lexical > 0 {
			exact = append(exact, seed.Nodes[index])
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return append([]graph.Node(nil), seed.Nodes...)
}

func pathCandidatesNeedDisambiguation(seeds []SeedSet) bool {
	for _, seed := range seeds {
		for _, ranking := range seed.Rankings {
			if ranking.Components.Lexical > 0 {
				return true
			}
		}
	}
	return false
}

func seedSetRelevance(seeds []SeedSet) float64 {
	relevance := 1.0
	for _, seedSet := range seeds {
		if len(seedSet.Rankings) > 0 {
			relevance *= seedSet.Rankings[0].Score
		}
	}
	return relevance
}

func executeIntersectionPlan(ctx context.Context, traverser storage.Traverser, snapshot storage.Snapshot, request Request, seeds []SeedSet) (Result, error) {
	traversals := make([]storage.TraversalResult, len(seeds))
	for index, seedSet := range seeds {
		if len(seedSet.Nodes) == 0 {
			return noCommonIntersectionResult(request.Plan, seeds, nil, nil), nil
		}
		startNodeIDs := make([]string, len(seedSet.Nodes))
		for nodeIndex, node := range seedSet.Nodes {
			startNodeIDs[nodeIndex] = node.ID
		}
		traversal, err := traverser.Traverse(ctx, snapshot, storage.TraversalRequest{
			StartNodeIDs: startNodeIDs,
			ProjectIDs:   append([]string(nil), request.ProjectIDs...),
			Direction:    request.Plan.Direction,
			Relations:    sharedContractRelations(request.Plan.AllowedRelations),
			MaxDepth:     request.MaxDepth,
			MaxNodes:     request.MaxNodes,
		})
		if err != nil {
			return Result{}, fmt.Errorf("query published graph intersection slot %q: %w", seedSet.Role, err)
		}
		traversals[index] = traversal
	}
	if len(traversals) != 2 {
		return Result{}, fmt.Errorf("query published graph intersection: exactly two entity slots are required")
	}

	rightNodeIDs := make(map[string]struct{}, len(traversals[1].Facts.Nodes))
	rightNodesByPath := make(map[string][]graph.Node)
	rightNodesByContractIdentity := make(map[string][]graph.Node)
	for _, node := range traversals[1].Facts.Nodes {
		rightNodeIDs[node.ID] = struct{}{}
		if node.Evidence.Span.Path != "" && isContractNode(node) {
			rightNodesByPath[node.Evidence.Span.Path] = append(rightNodesByPath[node.Evidence.Span.Path], node)
		}
		for _, identity := range contractIdentityKeys(node) {
			rightNodesByContractIdentity[identity] = append(rightNodesByContractIdentity[identity], node)
		}
	}
	commonNodeIDs := make(map[string]struct{})
	commonNodes := make([]graph.Node, 0)
	for _, node := range traversals[0].Facts.Nodes {
		_, commonID := rightNodeIDs[node.ID]
		matchingPathNodes := []graph.Node(nil)
		if isContractNode(node) {
			matchingPathNodes = rightNodesByPath[node.Evidence.Span.Path]
		}
		matchingIdentityNodes := make([]graph.Node, 0)
		for _, identity := range contractIdentityKeys(node) {
			matchingIdentityNodes = append(matchingIdentityNodes, rightNodesByContractIdentity[identity]...)
		}
		if !commonID && (node.Evidence.Span.Path == "" || len(matchingPathNodes) == 0) && len(matchingIdentityNodes) == 0 {
			continue
		}
		if _, duplicate := commonNodeIDs[node.ID]; duplicate {
			continue
		}
		commonNodeIDs[node.ID] = struct{}{}
		commonNodes = append(commonNodes, node)
		for _, matchingNode := range append(matchingPathNodes, matchingIdentityNodes...) {
			if _, duplicate := commonNodeIDs[matchingNode.ID]; duplicate {
				continue
			}
			commonNodeIDs[matchingNode.ID] = struct{}{}
			commonNodes = append(commonNodes, matchingNode)
		}
	}
	sort.Slice(commonNodes, func(left, right int) bool { return commonNodes[left].ID < commonNodes[right].ID })

	proofNodesByID := make(map[string]graph.Node, len(commonNodes))
	for _, node := range commonNodes {
		proofNodesByID[node.ID] = node
	}
	commonEdges := make([]graph.Edge, 0)
	commonEdgeKeys := make(map[string]struct{})
	for traversalIndex, traversal := range traversals {
		for _, commonNode := range commonNodes {
			var proof PathResult
			proofRootID := ""
			for _, seed := range seeds[traversalIndex].Nodes {
				proof = undirectedPath(traversal.Facts, seed.ID, commonNode.ID)
				if len(proof.Edges) > 0 {
					proofRootID = seed.ID
					break
				}
			}
			if len(proof.Edges) == 0 {
				continue
			}
			for _, node := range proof.Nodes {
				if node.ID != proofRootID {
					proofNodesByID[node.ID] = node
				}
			}
			for _, edge := range proof.Edges {
				key := pathEdgeKey(edge)
				if _, found := commonEdgeKeys[key]; found {
					continue
				}
				commonEdgeKeys[key] = struct{}{}
				commonEdges = append(commonEdges, edge)
			}
		}
	}
	commonNodes = commonNodes[:0]
	for _, node := range proofNodesByID {
		commonNodes = append(commonNodes, node)
	}
	sort.Slice(commonNodes, func(left, right int) bool { return commonNodes[left].ID < commonNodes[right].ID })
	sort.Slice(commonEdges, func(left, right int) bool {
		if commonEdges[left].SourceID != commonEdges[right].SourceID {
			return commonEdges[left].SourceID < commonEdges[right].SourceID
		}
		if commonEdges[left].TargetID != commonEdges[right].TargetID {
			return commonEdges[left].TargetID < commonEdges[right].TargetID
		}
		return commonEdges[left].Relation < commonEdges[right].Relation
	})

	truncationSet := make(map[storage.TruncationReason]struct{})
	for _, traversal := range traversals {
		for _, reason := range traversal.TruncationReasons {
			truncationSet[reason] = struct{}{}
		}
	}
	truncationReasons := make([]storage.TruncationReason, 0, len(truncationSet))
	for reason := range truncationSet {
		truncationReasons = append(truncationReasons, reason)
	}
	sort.Slice(truncationReasons, func(left, right int) bool { return truncationReasons[left] < truncationReasons[right] })
	result := Result{
		Seeds:             seeds,
		Facts:             graph.Facts{Nodes: commonNodes, Edges: commonEdges},
		TruncationReasons: truncationReasons,
		Limits:            append(retrievalLimits(request.Plan), make([]StageLimit, len(traversals))...),
	}
	for index, traversal := range traversals {
		result.Limits[len(request.Plan.EntitySlots)+index] = StageLimit{
			Stage:             "intersection",
			SlotRole:          seeds[index].Role,
			MaxDepth:          request.MaxDepth,
			MaxNodes:          request.MaxNodes,
			TruncationReasons: append([]storage.TruncationReason(nil), traversal.TruncationReasons...),
		}
	}
	if len(commonNodes) == 0 {
		warning := PlanWarning{
			Code:    "no_common_evidence",
			Message: "No common graph evidence was found for the two entity slots; no shared-contract claim was produced.",
		}
		for _, seedSet := range seeds {
			for _, node := range seedSet.Nodes {
				warning.Suggestions = append(warning.Suggestions, suggestionForCandidate(OperatorLookup, node.ID))
			}
		}
		result.Warnings = []PlanWarning{warning}
		return result, nil
	}
	seedRelevance := 1.0
	for _, seedSet := range seeds {
		if len(seedSet.Rankings) > 0 {
			seedRelevance *= seedSet.Rankings[0].Score
		}
	}
	relation := graph.RelationKind("")
	if len(commonEdges) > 0 {
		relation = commonEdges[0].Relation
	}
	components := EvidenceScoreComponents{
		SeedRelevance: seedRelevance,
		Distance:      1,
		RelationFit:   relationFit(request.Plan.AllowedRelations, relation),
		Confidence:    request.Plan.Confidence,
		SharedSupport: float64(len(seeds)),
		PathDiversity: float64(len(commonEdges)),
	}
	result.Evidence = []EvidenceGroup{{
		Rank:       1,
		Score:      evidenceScore(components),
		Components: components,
		Reason:     fmt.Sprintf("Common evidence is supported by the %s and %s slots through %s.", seeds[0].Role, seeds[1].Role, relation),
		SlotRole:   seeds[0].Role + "+" + seeds[1].Role,
		Relation:   relation,
		Distance:   1,
		Nodes:      append([]graph.Node(nil), commonNodes...),
		Edges:      append([]graph.Edge(nil), commonEdges...),
	}}
	return result, nil
}

func sharedContractRelations(relations []graph.RelationKind) []graph.RelationKind {
	return append(expandLanguageRelations(relations, "implements"), "defines", "references")
}

func contractIdentityKeys(node graph.Node) []string {
	if key := contractSuiteIdentity(node); key != "" {
		return []string{key}
	}
	if !isContractNode(node) || node.QualifiedName == "" {
		return nil
	}
	return []string{"qualified:" + strings.ToLower(node.QualifiedName)}
}

func contractSuiteIdentity(node graph.Node) string {
	if node.Kind != "file" {
		return ""
	}
	name := strings.ToLower(path.Base(node.Evidence.Span.Path))
	if !strings.Contains(name, "contract") || !strings.Contains(name, "test") {
		return ""
	}
	return "contract-suite:" + name
}

func isContractNode(node graph.Node) bool {
	kind := strings.ToLower(string(node.Kind))
	return strings.Contains(kind, "interface") || strings.Contains(kind, "contract") || strings.Contains(kind, "test") || contractSuiteIdentity(node) != ""
}

func noCommonIntersectionResult(plan *QueryPlan, seeds []SeedSet, nodes []graph.Node, edges []graph.Edge) Result {
	result := Result{
		Seeds:  seeds,
		Facts:  graph.Facts{Nodes: nodes, Edges: edges},
		Limits: retrievalLimits(plan),
	}
	warning := PlanWarning{
		Code:    "no_common_evidence",
		Message: "No common graph evidence was found for the two entity slots; no shared-contract claim was produced.",
	}
	for _, seedSet := range seeds {
		for _, node := range seedSet.Nodes {
			warning.Suggestions = append(warning.Suggestions, suggestionForCandidate(OperatorLookup, node.ID))
		}
	}
	result.Warnings = []PlanWarning{warning}
	return result
}

func rankImpactEvidence(facts graph.Facts, startNodeIDs []string) []ImpactEvidence {
	nodes := make(map[string]graph.Node, len(facts.Nodes))
	for _, node := range facts.Nodes {
		nodes[node.ID] = node
	}
	type impactReach struct {
		distance int
		relation graph.RelationKind
	}
	reached := make(map[string]impactReach, len(facts.Nodes))
	frontier := append([]string(nil), startNodeIDs...)
	for _, nodeID := range frontier {
		reached[nodeID] = impactReach{}
	}
	for distance := 1; len(frontier) > 0; distance++ {
		next := make([]string, 0)
		for _, currentID := range frontier {
			for _, edge := range facts.Edges {
				if edge.TargetID != currentID {
					continue
				}
				if _, found := reached[edge.SourceID]; found {
					continue
				}
				reached[edge.SourceID] = impactReach{distance: distance, relation: edge.Relation}
				next = append(next, edge.SourceID)
			}
		}
		frontier = next
	}
	evidence := make([]ImpactEvidence, 0, len(reached))
	for nodeID, reach := range reached {
		if reach.distance == 0 {
			continue
		}
		node, found := nodes[nodeID]
		if !found {
			continue
		}
		evidence = append(evidence, ImpactEvidence{
			Node:     node,
			Relation: reach.relation,
			Distance: reach.distance,
			Score:    impactRelationWeight(reach.relation) / float64(reach.distance),
		})
	}
	sort.Slice(evidence, func(left, right int) bool {
		if evidence[left].Score != evidence[right].Score {
			return evidence[left].Score > evidence[right].Score
		}
		if evidence[left].Distance != evidence[right].Distance {
			return evidence[left].Distance < evidence[right].Distance
		}
		return evidence[left].Node.ID < evidence[right].Node.ID
	})
	return evidence
}

func impactRelationWeight(relation graph.RelationKind) float64 {
	switch relation {
	case "contains":
		return 0.25
	case "implements":
		return 0.9
	default:
		return 1
	}
}

func emptyEntitySlotWarnings(plan *QueryPlan, seeds []SeedSet) []PlanWarning {
	warnings := make([]PlanWarning, 0, len(plan.EntitySlots))
	for index, slot := range plan.EntitySlots {
		if index < len(seeds) && len(seeds[index].Nodes) > 0 {
			continue
		}
		warnings = append(warnings, PlanWarning{
			Code:        "entity_not_found",
			Message:     fmt.Sprintf("No entity matched the %s slot; no graph operation was executed.", slot.Role),
			Suggestions: []string{fmt.Sprintf("Use --terms with %q for literal lookup.", slot.Text)},
		})
	}
	return warnings
}

func entityResolutionWarning(operator ExecutionOperator, seeds []SeedSet) *PlanWarning {
	if len(seeds) != 1 || len(seeds[0].Nodes) == 0 {
		return &PlanWarning{
			Code:        "entity_not_found",
			Message:     "No entity matched the question; no graph operation was executed.",
			Suggestions: []string{"Use --terms with the entity text for literal lookup."},
		}
	}
	if len(seeds[0].Nodes) > 1 {
		warning := &PlanWarning{
			Code:    "ambiguous_entity",
			Message: "The entity is ambiguous; no graph operation was executed.",
		}
		for _, candidate := range seeds[0].Nodes {
			warning.Suggestions = append(warning.Suggestions, suggestionForCandidate(operator, candidate.ID))
		}
		return warning
	}
	if len(seeds[0].Rankings) != 1 || seeds[0].Rankings[0].Components.Exact == 0 && seeds[0].Rankings[0].Components.TokenCoverage < 1 {
		return &PlanWarning{
			Code:        "weak_entity_match",
			Message:     "Only a weak entity match was found; no graph operation was executed.",
			Suggestions: []string{suggestionForCandidate(operator, seeds[0].Nodes[0].ID)},
		}
	}
	return nil
}

func suggestionForCandidate(operator ExecutionOperator, nodeID string) string {
	if operator == OperatorExplain {
		return "Explain " + nodeID + "."
	}
	return "Look up " + nodeID + "."
}
