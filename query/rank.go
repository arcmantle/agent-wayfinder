package query

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"agent-wayfinder/graph"
	"agent-wayfinder/storage"
)

const maxSeedsPerTerm = 3

const questionCandidateMultiplier = 4

type SeedRequest struct {
	Role      string                       `json:"role"`
	Retrieval storage.LexicalSearchRequest `json:"retrieval"`
}

type SeedScoreComponents struct {
	Exact          float64 `json:"exact"`
	Lexical        float64 `json:"lexical"`
	EntityRole     float64 `json:"entityRole"`
	NodeKind       float64 `json:"nodeKind"`
	ProjectScope   float64 `json:"projectScope"`
	PathAffinity   float64 `json:"pathAffinity"`
	TokenCoverage  float64 `json:"tokenCoverage"`
	ReciprocalRank float64 `json:"reciprocalRank"`
}

type SeedRanking struct {
	NodeID     string              `json:"nodeId"`
	Score      float64             `json:"score"`
	Components SeedScoreComponents `json:"components"`
}

type SeedSet struct {
	Role     string        `json:"role,omitempty"`
	Term     string        `json:"term"`
	Nodes    []graph.Node  `json:"nodes"`
	Rankings []SeedRanking `json:"rankings,omitempty"`
}

type ExplainResult struct {
	Candidates     []graph.Node
	RemainderCount int
	Explanation    *storage.Explanation
}

func (result ExplainResult) CandidateIDs() []string {
	ids := make([]string, len(result.Candidates))
	for index, candidate := range result.Candidates {
		ids[index] = candidate.ID
	}
	return ids
}

func Rank(nodes []graph.Node, terms []string) []SeedSet {
	seeds := make([]SeedSet, 0, len(terms))
	for _, term := range terms {
		seeds = append(seeds, SeedSet{Term: term, Nodes: rankTerm(nodes, term)})
	}
	return seeds
}

func RankSnapshot(ctx context.Context, lookup storage.NodeLookup, snapshot storage.Snapshot, terms []string) ([]SeedSet, error) {
	if lookup == nil {
		return nil, fmt.Errorf("rank published graph: node lookup is required")
	}

	seeds := make([]SeedSet, 0, len(terms))
	for _, term := range terms {
		matches, err := lookupTermMatches(ctx, lookup, snapshot, term, maxSeedsPerTerm)
		if err != nil {
			return nil, fmt.Errorf("rank published graph: %w", err)
		}
		nodes := make([]graph.Node, len(matches))
		for index, match := range matches {
			nodes[index] = match.Node
		}
		seeds = append(seeds, SeedSet{Term: term, Nodes: nodes})
	}
	return seeds, nil
}

func RankRetrievalsSnapshot(ctx context.Context, lookup storage.NodeLookup, searcher storage.LexicalSearcher, snapshot storage.Snapshot, requests []storage.LexicalSearchRequest) ([]SeedSet, error) {
	if lookup == nil || searcher == nil {
		return nil, fmt.Errorf("rank structured retrieval: node lookup and lexical search are required")
	}
	seeds := make([]SeedSet, 0, len(requests))
	for _, request := range requests {
		if request.Text == "" || request.Limit <= 0 {
			return nil, fmt.Errorf("rank structured retrieval: search text and positive limit are required")
		}
		matches, err := lookupExactMatches(ctx, lookup, snapshot, request.Text, request.Limit)
		if err != nil {
			return nil, fmt.Errorf("rank structured retrieval: %w", err)
		}
		nodes := make([]graph.Node, 0, request.Limit)
		if len(matches) > 0 {
			for _, match := range matches {
				nodes = append(nodes, match.Node)
			}
		} else {
			lexicalMatches, err := searcher.SearchNodes(ctx, snapshot, request)
			if err != nil {
				return nil, fmt.Errorf("rank structured retrieval: %w", err)
			}
			for _, match := range lexicalMatches {
				nodes = append(nodes, match.Node)
			}
		}
		seeds = append(seeds, SeedSet{Term: request.Text, Nodes: nodes})
	}
	return seeds, nil
}

func RankSeedRequestsSnapshot(ctx context.Context, lookup storage.NodeLookup, searcher storage.LexicalSearcher, snapshot storage.Snapshot, requests []SeedRequest) ([]SeedSet, error) {
	if lookup == nil || searcher == nil {
		return nil, fmt.Errorf("rank seed requests: node lookup and lexical search are required")
	}
	seeds := make([]SeedSet, 0, len(requests))
	for _, seedRequest := range requests {
		request := seedRequest.Retrieval
		if request.Text == "" || request.Limit <= 0 {
			return nil, fmt.Errorf("rank seed requests: search text and positive limit are required")
		}
		matches, err := lookupExactMatches(ctx, lookup, snapshot, request.Text, request.Limit)
		if err != nil {
			return nil, fmt.Errorf("rank seed requests: %w", err)
		}
		ranked := make([]rankedSeed, 0, request.Limit)
		if len(matches) > 0 {
			for _, match := range matches {
				ranked = append(ranked, rankedSeed{
					node:       match.Node,
					score:      1,
					components: SeedScoreComponents{Exact: 1, ReciprocalRank: 1},
				})
			}
		} else {
			candidateRequest := request
			candidateRequest.Limit *= questionCandidateMultiplier
			lexicalMatches, err := searcher.SearchNodes(ctx, snapshot, candidateRequest)
			if err != nil {
				return nil, fmt.Errorf("rank seed requests: %w", err)
			}
			for _, match := range lexicalMatches {
				ranked = append(ranked, rankedSeed{
					node: match.Node,
					components: SeedScoreComponents{
						Exact:         seedLabelExactness(request, match.Node),
						Lexical:       match.Score,
						EntityRole:    seedRoleFit(seedRequest.Role, match.Node.Kind),
						NodeKind:      seedKindFit(request.Kinds, match.Node.Kind),
						ProjectScope:  seedProjectScopeFit(request.ProjectIDs),
						PathAffinity:  seedTextCoverage(request, match.Node.Evidence.Span.Path),
						TokenCoverage: seedTokenCoverage(request, match.Node),
					},
				})
			}
			ranked = fuseRankedSeeds(ranked)
		}
		ranked = diversifyRankedSeeds(ranked, request.Limit)
		seedSet := SeedSet{Role: seedRequest.Role, Term: request.Text, Nodes: make([]graph.Node, len(ranked)), Rankings: make([]SeedRanking, len(ranked))}
		for index, candidate := range ranked {
			seedSet.Nodes[index] = candidate.node
			seedSet.Rankings[index] = SeedRanking{NodeID: candidate.node.ID, Score: candidate.score, Components: candidate.components}
		}
		seeds = append(seeds, seedSet)
	}
	return seeds, nil
}

type rankedSeed struct {
	node       graph.Node
	score      float64
	components SeedScoreComponents
}

func fuseRankedSeeds(candidates []rankedSeed) []rankedSeed {
	if len(candidates) == 0 {
		return candidates
	}
	maximumLexical := 0.0
	for _, candidate := range candidates {
		maximumLexical = max(maximumLexical, candidate.components.Lexical)
	}
	if maximumLexical > 0 {
		for index := range candidates {
			candidates[index].components.Lexical /= maximumLexical
		}
	}
	componentValues := [][]float64{
		seedComponentValues(candidates, func(components SeedScoreComponents) float64 { return components.Exact }),
		seedComponentValues(candidates, func(components SeedScoreComponents) float64 { return components.Lexical }),
		seedComponentValues(candidates, func(components SeedScoreComponents) float64 { return components.EntityRole }),
		seedComponentValues(candidates, func(components SeedScoreComponents) float64 { return components.NodeKind }),
		seedComponentValues(candidates, func(components SeedScoreComponents) float64 { return components.ProjectScope }),
		seedComponentValues(candidates, func(components SeedScoreComponents) float64 { return components.PathAffinity }),
		seedComponentValues(candidates, func(components SeedScoreComponents) float64 { return components.TokenCoverage }),
	}
	for _, values := range componentValues {
		ranks := seedComponentRanks(candidates, values)
		for index, rank := range ranks {
			if rank == 0 {
				continue
			}
			candidates[index].components.ReciprocalRank += 1 / float64(60+rank)
		}
	}
	for index := range candidates {
		candidates[index].score = candidates[index].components.Exact + candidates[index].components.ReciprocalRank
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		if candidates[left].score != candidates[right].score {
			return candidates[left].score > candidates[right].score
		}
		if candidates[left].node.QualifiedName != candidates[right].node.QualifiedName {
			return candidates[left].node.QualifiedName < candidates[right].node.QualifiedName
		}
		if candidates[left].node.Label != candidates[right].node.Label {
			return candidates[left].node.Label < candidates[right].node.Label
		}
		return candidates[left].node.ID < candidates[right].node.ID
	})
	return candidates
}

func seedComponentValues(candidates []rankedSeed, value func(SeedScoreComponents) float64) []float64 {
	values := make([]float64, len(candidates))
	for index, candidate := range candidates {
		values[index] = value(candidate.components)
	}
	return values
}

func seedComponentRanks(candidates []rankedSeed, values []float64) []int {
	indexes := make([]int, len(candidates))
	for index := range indexes {
		indexes[index] = index
	}
	sort.SliceStable(indexes, func(left, right int) bool {
		leftIndex, rightIndex := indexes[left], indexes[right]
		if values[leftIndex] != values[rightIndex] {
			return values[leftIndex] > values[rightIndex]
		}
		return candidates[leftIndex].node.ID < candidates[rightIndex].node.ID
	})
	ranks := make([]int, len(candidates))
	for position, index := range indexes {
		if values[index] <= 0 {
			continue
		}
		rank := position + 1
		if position > 0 && values[index] == values[indexes[position-1]] {
			rank = ranks[indexes[position-1]]
		}
		ranks[index] = rank
	}
	return ranks
}

func diversifyRankedSeeds(candidates []rankedSeed, limit int) []rankedSeed {
	selected := make([]rankedSeed, 0, min(len(candidates), limit))
	deferred := make([]rankedSeed, 0, len(candidates))
	seenNodes := make(map[string]struct{}, len(candidates))
	seenPaths := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if _, found := seenNodes[candidate.node.ID]; found {
			continue
		}
		seenNodes[candidate.node.ID] = struct{}{}
		path := candidate.node.Evidence.Span.Path
		if _, found := seenPaths[path]; found {
			deferred = append(deferred, candidate)
			continue
		}
		seenPaths[path] = struct{}{}
		selected = append(selected, candidate)
		if len(selected) == limit {
			return selected
		}
	}
	for _, candidate := range deferred {
		selected = append(selected, candidate)
		if len(selected) == limit {
			break
		}
	}
	return selected
}

func seedTokenCoverage(request storage.LexicalSearchRequest, node graph.Node) float64 {
	return seedTextCoverage(request, strings.Join([]string{node.ID, node.Label, node.QualifiedName, node.Evidence.Span.Path}, " "))
}

func seedLabelExactness(request storage.LexicalSearchRequest, node graph.Node) float64 {
	if node.Label == request.Text {
		return 1
	}
	return 0
}

func seedTextCoverage(request storage.LexicalSearchRequest, value string) float64 {
	tokens := make([]string, 0)
	for _, group := range request.TokenGroups {
		tokens = append(tokens, group...)
	}
	if len(tokens) == 0 {
		tokens = strings.Fields(strings.ToLower(request.Text))
	}
	if len(tokens) == 0 {
		return 0
	}
	haystack := strings.ToLower(value)
	matched := 0
	for _, token := range tokens {
		if strings.Contains(haystack, strings.ToLower(token)) {
			matched++
		}
	}
	return float64(matched) / float64(len(tokens))
}

func seedRoleFit(role string, kind graph.NodeKind) float64 {
	baseKind := string(kind)
	if separator := strings.LastIndex(baseKind, ":"); separator >= 0 {
		baseKind = baseKind[separator+1:]
	}
	switch role {
	case "caller", "callee":
		if baseKind == "function" || baseKind == "method" {
			return 1
		}
		return 0
	case "dependent", "dependency":
		if baseKind == "package" || baseKind == "module" || baseKind == "file" || baseKind == "project" {
			return 1
		}
		return 0
	case "left", "right":
		if baseKind == "class" || baseKind == "interface" || baseKind == "type_alias" || baseKind == "type" || baseKind == "package" || baseKind == "module" || baseKind == "file" || baseKind == "project" {
			return 1
		}
		return 0
	default:
		return 1
	}
}

func seedKindFit(kinds []graph.NodeKind, kind graph.NodeKind) float64 {
	if len(kinds) == 0 {
		return 0
	}
	for _, expected := range kinds {
		if expected == kind {
			return 1
		}
	}
	return 0
}

func seedProjectScopeFit(projectIDs []string) float64 {
	if len(projectIDs) > 0 {
		return 1
	}
	return 0
}

func lookupExactMatches(ctx context.Context, lookup storage.NodeLookup, snapshot storage.Snapshot, text string, limit int) ([]storage.NodeMatch, error) {
	exactLookup, supported := lookup.(storage.ExactNodeLookup)
	if !supported {
		return nil, nil
	}
	matches, err := exactLookup.LookupExactNodes(ctx, snapshot, text)
	if err != nil {
		return nil, err
	}
	return matches[:min(len(matches), limit)], nil
}

func ExplainSnapshot(ctx context.Context, lookup storage.NodeLookup, explainer storage.Explainer, snapshot storage.Snapshot, term string) (ExplainResult, error) {
	if explainer == nil {
		return ExplainResult{}, fmt.Errorf("explain published graph: explainer is required")
	}
	if lookup == nil {
		return ExplainResult{}, fmt.Errorf("explain published graph: node lookup is required")
	}
	matches, err := lookupTermMatches(ctx, lookup, snapshot, term, maxSeedsPerTerm+1)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("explain published graph: %w", err)
	}
	candidates := make([]graph.Node, len(matches))
	for index, match := range matches {
		candidates[index] = match.Node
	}
	if len(candidates) != 1 {
		return ExplainResult{
			Candidates:     candidates[:min(len(candidates), maxSeedsPerTerm)],
			RemainderCount: max(0, len(candidates)-maxSeedsPerTerm),
		}, nil
	}
	explanation, err := explainer.Explain(ctx, snapshot, storage.ExplainRequest{NodeID: candidates[0].ID})
	if err != nil {
		return ExplainResult{}, fmt.Errorf("explain published graph: %w", err)
	}
	return ExplainResult{Explanation: &explanation}, nil
}

func lookupTermMatches(ctx context.Context, lookup storage.NodeLookup, snapshot storage.Snapshot, term string, limit int) ([]storage.NodeMatch, error) {
	if exactLookup, supported := lookup.(storage.ExactNodeLookup); supported {
		matches, err := exactLookup.LookupExactNodes(ctx, snapshot, term)
		if err != nil {
			return nil, err
		}
		if len(matches) > 0 {
			return matches[:min(len(matches), limit)], nil
		}
	}
	return lookup.LookupNodes(ctx, snapshot, storage.NodeLookupRequest{Text: term, Limit: limit})
}

type nodeCollector struct {
	nodes []graph.Node
}

func (collector *nodeCollector) WriteNode(node graph.Node) error {
	collector.nodes = append(collector.nodes, node)
	return nil
}

func (*nodeCollector) WriteEdge(graph.Edge) error {
	return nil
}

type rankedNode struct {
	node graph.Node
	rank int
}

func rankTerm(nodes []graph.Node, term string) []graph.Node {
	matches := rankTermMatches(nodes, term)
	return matches[:min(len(matches), maxSeedsPerTerm)]
}

func rankTermMatches(nodes []graph.Node, term string) []graph.Node {
	if term == "" {
		return nil
	}

	matches := make([]rankedNode, 0, len(nodes))
	for _, node := range nodes {
		rank, matched := matchRank(node, term)
		if matched {
			matches = append(matches, rankedNode{node: node, rank: rank})
		}
	}
	sort.Slice(matches, func(left, right int) bool {
		if matches[left].rank != matches[right].rank {
			return matches[left].rank < matches[right].rank
		}
		if matches[left].node.QualifiedName != matches[right].node.QualifiedName {
			return matches[left].node.QualifiedName < matches[right].node.QualifiedName
		}
		if matches[left].node.Label != matches[right].node.Label {
			return matches[left].node.Label < matches[right].node.Label
		}
		return matches[left].node.ID < matches[right].node.ID
	})

	selected := make([]graph.Node, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		if _, found := seen[match.node.ID]; found {
			continue
		}
		seen[match.node.ID] = struct{}{}
		selected = append(selected, match.node)
	}
	return selected
}

func matchRank(node graph.Node, term string) (int, bool) {
	if node.ID == term {
		return 0, true
	}
	if node.QualifiedName == term {
		return 1, true
	}
	if node.Label == term {
		return 2, true
	}

	query := strings.ToLower(term)
	if strings.HasPrefix(strings.ToLower(node.Label), query) || hasTokenPrefix(node.Label, query) || hasTokenPrefix(node.QualifiedName, query) {
		return 3, true
	}
	if strings.Contains(strings.ToLower(node.Evidence.Span.Path), query) {
		return 4, true
	}
	if strings.Contains(strings.ToLower(node.ID), query) || strings.Contains(strings.ToLower(node.QualifiedName), query) || strings.Contains(strings.ToLower(node.Label), query) {
		return 5, true
	}
	return 0, false
}

func hasTokenPrefix(value, term string) bool {
	for _, token := range strings.FieldsFunc(value, func(character rune) bool {
		return !unicode.IsLetter(character) && !unicode.IsDigit(character)
	}) {
		if strings.HasPrefix(strings.ToLower(token), term) {
			return true
		}
	}
	return false
}
