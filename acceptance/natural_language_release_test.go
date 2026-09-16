package acceptance_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"agent-wayfinder/benchmark"
	"agent-wayfinder/graph"
	"agent-wayfinder/index"
	"agent-wayfinder/query"
	"agent-wayfinder/storage"
	"agent-wayfinder/storage/sqlite"
)

const releaseRootEnvironment = "AGENT_WAYFINDER_RELEASE_ROOT"
const sharedContractRootEnvironment = "AGENT_WAYFINDER_SHARED_CONTRACT_ROOT"

const reportedSharedContractQuestion = "what is the shared contract between postgres and sqlite"

type releaseQuestionResult struct {
	Question          string                     `json:"question"`
	Plan              query.QueryPlan            `json:"plan"`
	RecallAt10Hit     bool                       `json:"recallAt10Hit"`
	Evaluation        releaseQuestionEvaluation  `json:"evaluation"`
	PlanningLookupNS  int64                      `json:"planningLookupNs"`
	TraversalNS       int64                      `json:"traversalNs"`
	RankedIDs         [][]string                 `json:"rankedIds"`
	EvidenceGroups    int                        `json:"evidenceGroups"`
	TruncationReasons []storage.TruncationReason `json:"truncationReasons,omitempty"`
}

type releaseAcceptanceRecord struct {
	CorpusWorkspace      string                  `json:"corpusWorkspace"`
	SharedWorkspace      string                  `json:"sharedContractWorkspace"`
	GraphVersion         storage.GraphVersion    `json:"graphVersion"`
	QuestionCount        int                     `json:"questionCount"`
	RecallAt10           float64                 `json:"recallAt10"`
	IntentAccuracy       float64                 `json:"intentAccuracy"`
	P95PlanningLookup    time.Duration           `json:"p95PlanningLookup"`
	P95Traversal         time.Duration           `json:"p95Traversal"`
	StableOutputHash     string                  `json:"stableOutputHash"`
	Results              []releaseQuestionResult `json:"results"`
	SharedContractResult releaseQuestionResult   `json:"sharedContractResult"`
}

type releaseQuestionEvaluation struct {
	Passed                 bool                   `json:"passed"`
	AcceptableEvidence     bool                   `json:"acceptableEvidence"`
	SourceEvidence         bool                   `json:"sourceEvidence"`
	EntityRoleChecks       []releaseContractCheck `json:"entityRoleChecks"`
	RequiredRelationChecks []releaseContractCheck `json:"requiredRelationChecks"`
	ForbiddenClaimChecks   []releaseContractCheck `json:"forbiddenClaimChecks"`
	Failures               []string               `json:"failures,omitempty"`
}

type releaseContractCheck struct {
	Expected string `json:"expected"`
	Passed   bool   `json:"passed"`
}

func evaluateReleaseQuestion(question benchmark.ArchitectureQuestion, seeds []query.SeedSet, result query.Result) releaseQuestionEvaluation {
	resolvedSlots := make(map[string]query.SeedSet)
	for _, seedSet := range seeds {
		if len(seedSet.Nodes) > 0 {
			resolvedSlots[seedSet.Role] = seedSet
		}
	}
	foundRelations := make(map[graph.RelationKind]bool)
	for _, group := range result.Evidence {
		if group.Relation != "" {
			foundRelations[group.Relation] = true
		}
		for _, edge := range group.Edges {
			foundRelations[edge.Relation] = true
		}
	}
	evaluation := releaseQuestionEvaluation{
		Passed:             true,
		AcceptableEvidence: releaseRecallHit(question, seeds),
		SourceEvidence:     hasSourceBackedEvidence(result.Evidence),
	}
	if !evaluation.AcceptableEvidence {
		evaluation.Passed = false
		evaluation.Failures = append(evaluation.Failures, "acceptable seed or path not found")
	}
	if !evaluation.SourceEvidence {
		evaluation.Passed = false
		evaluation.Failures = append(evaluation.Failures, "evidence without a source path")
	}
	for _, entityRole := range question.EntityRoles {
		seedSet, resolved := resolvedSlots[entityRole.Role]
		passed := resolved && seedSetMatchesEntity(seedSet, entityRole.Value)
		if question.NoMatch {
			passed = !resolved
		}
		evaluation.EntityRoleChecks = append(evaluation.EntityRoleChecks, releaseContractCheck{Expected: entityRole.Role + ":" + entityRole.Value, Passed: passed})
		if !passed {
			evaluation.Passed = false
			evaluation.Failures = append(evaluation.Failures, "required entity slot not resolved: "+entityRole.Role)
		}
	}
	for _, required := range question.RequiredRelations {
		passed := hasWarningCode(result.Warnings, "no_directed_path") || releaseRelationFound(foundRelations, required)
		evaluation.RequiredRelationChecks = append(evaluation.RequiredRelationChecks, releaseContractCheck{Expected: string(required), Passed: passed})
		if !passed {
			evaluation.Passed = false
			evaluation.Failures = append(evaluation.Failures, "required relation not found: "+string(required))
		}
	}
	for _, forbiddenClaim := range question.ForbiddenClaims {
		passed := !resultContainsClaim(result, forbiddenClaim)
		evaluation.ForbiddenClaimChecks = append(evaluation.ForbiddenClaimChecks, releaseContractCheck{Expected: forbiddenClaim, Passed: passed})
		if !passed {
			evaluation.Passed = false
			evaluation.Failures = append(evaluation.Failures, "forbidden claim returned: "+forbiddenClaim)
		}
	}
	return evaluation
}

func releaseRelationFound(found map[graph.RelationKind]bool, required graph.RelationKind) bool {
	for relation, present := range found {
		if present && (relation == required || strings.HasSuffix(string(relation), ":"+string(required))) {
			return true
		}
	}
	return false
}

func hasWarningCode(warnings []query.PlanWarning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}

func seedSetMatchesEntity(seedSet query.SeedSet, expected string) bool {
	expected = normalizeIdentity(expected)
	if strings.Contains(normalizeIdentity(seedSet.Term), expected) {
		return true
	}
	for _, node := range seedSet.Nodes {
		for _, identity := range []string{node.ID, node.Label, node.QualifiedName, node.Evidence.Span.Path} {
			if strings.Contains(normalizeIdentity(identity), expected) {
				return true
			}
		}
	}
	return false
}

func normalizeIdentity(value string) string {
	return strings.Map(func(character rune) rune {
		if character >= 'A' && character <= 'Z' {
			return character + ('a' - 'A')
		}
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			return character
		}
		return -1
	}, value)
}

func hasSourceBackedEvidence(groups []query.EvidenceGroup) bool {
	for _, group := range groups {
		for _, node := range group.Nodes {
			if node.Evidence.Span.Path == "" {
				return false
			}
		}
		for _, edge := range group.Edges {
			if edge.Evidence.Span.Path == "" {
				return false
			}
		}
	}
	return true
}

func resultContainsClaim(result query.Result, claim string) bool {
	claim = strings.ToLower(strings.TrimSpace(claim))
	for _, group := range result.Evidence {
		if strings.Contains(strings.ToLower(group.Reason), claim) {
			return true
		}
	}
	for _, warning := range result.Warnings {
		if strings.Contains(strings.ToLower(warning.Message), claim) {
			return true
		}
	}
	return false
}

func TestReleaseQuestionOracleRejectsWrongRelationEvidence(t *testing.T) {
	question := benchmark.ArchitectureQuestion{
		Text:              "what calls QuerySnapshot?",
		Intent:            benchmark.IntentCalledBy,
		EntityRoles:       []benchmark.QuestionEntityRole{{Role: "callee", Value: "QuerySnapshot"}},
		AcceptableSeeds:   []string{"QuerySnapshot"},
		RequiredRelations: []graph.RelationKind{"calls"},
		ForbiddenClaims:   []string{"Import edges prove a function call."},
	}
	seeds := []query.SeedSet{{Role: "callee", Nodes: []graph.Node{{ID: "QuerySnapshot", Label: "QuerySnapshot"}}}}
	result := query.Result{Evidence: []query.EvidenceGroup{{
		SlotRole: "callee",
		Relation: "imports_from",
		Nodes:    []graph.Node{{ID: "QuerySnapshot", Label: "QuerySnapshot", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "query/query.go"}}}},
		Edges:    []graph.Edge{{SourceID: "caller", TargetID: "QuerySnapshot", Relation: "imports_from", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "query/query.go"}}}},
	}}}

	evaluation := evaluateReleaseQuestion(question, seeds, result)
	if evaluation.Passed || len(evaluation.Failures) == 0 {
		t.Fatalf("evaluation = %+v, want a wrong-relation failure", evaluation)
	}
}

func TestReleaseQuestionOracleAcceptsLanguageSpecificRequiredRelation(t *testing.T) {
	question := benchmark.ArchitectureQuestion{
		Text:              "what calls QuerySnapshot?",
		Intent:            benchmark.IntentCalls,
		EntityRoles:       []benchmark.QuestionEntityRole{{Role: "caller", Value: "QuerySnapshot"}},
		AcceptableSeeds:   []string{"QuerySnapshot"},
		RequiredRelations: []graph.RelationKind{"calls"},
	}
	seeds := []query.SeedSet{{Role: "caller", Nodes: []graph.Node{{ID: "QuerySnapshot", Label: "QuerySnapshot"}}}}
	result := query.Result{Evidence: []query.EvidenceGroup{{
		SlotRole: "caller",
		Relation: "go:calls",
		Nodes:    []graph.Node{{ID: "QuerySnapshot", Label: "QuerySnapshot", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "query/query.go"}}}},
		Edges:    []graph.Edge{{SourceID: "QuerySnapshot", TargetID: "helper", Relation: "go:calls", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "query/query.go"}}}},
	}}}

	evaluation := evaluateReleaseQuestion(question, seeds, result)
	if !evaluation.Passed {
		t.Fatalf("evaluation = %+v, want language-specific relation to satisfy calls", evaluation)
	}
}

func TestReleaseQuestionOracleRejectsMissingRequiredSlot(t *testing.T) {
	question := benchmark.ArchitectureQuestion{
		Text:              "find a path from runQuery to Traverse",
		Intent:            benchmark.IntentPath,
		EntityRoles:       []benchmark.QuestionEntityRole{{Role: "source", Value: "runQuery"}, {Role: "target", Value: "Traverse"}},
		AcceptableSeeds:   []string{"runQuery", "Traverse"},
		RequiredRelations: []graph.RelationKind{"calls"},
		ForbiddenClaims:   []string{"An undirected path proves directed flow."},
	}
	seeds := []query.SeedSet{{Role: "source", Nodes: []graph.Node{{ID: "runQuery", Label: "runQuery"}}}, {Role: "target"}}
	result := query.Result{Evidence: []query.EvidenceGroup{{
		SlotRole: "source",
		Relation: "calls",
		Nodes:    []graph.Node{{ID: "runQuery", Label: "runQuery", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "cmd/agent-wayfinder/main.go"}}}},
		Edges:    []graph.Edge{{SourceID: "runQuery", TargetID: "helper", Relation: "calls", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "cmd/agent-wayfinder/main.go"}}}},
	}}}

	evaluation := evaluateReleaseQuestion(question, seeds, result)
	if evaluation.Passed || len(evaluation.Failures) == 0 {
		t.Fatalf("evaluation = %+v, want a missing-slot failure", evaluation)
	}
}

func TestReleaseQuestionOracleRejectsWrongEntityInRequiredSlot(t *testing.T) {
	question := benchmark.ArchitectureQuestion{
		Text:            "find a path from runQuery to Traverse",
		Intent:          benchmark.IntentPath,
		EntityRoles:     []benchmark.QuestionEntityRole{{Role: "source", Value: "runQuery"}, {Role: "target", Value: "Traverse"}},
		AcceptableSeeds: []string{"runQuery", "Traverse"},
	}
	seeds := []query.SeedSet{
		{Role: "source", Nodes: []graph.Node{{ID: "runQuery", Label: "runQuery"}}},
		{Role: "target", Nodes: []graph.Node{{ID: "Unrelated", Label: "Unrelated"}}},
	}

	evaluation := evaluateReleaseQuestion(question, seeds, query.Result{})
	if evaluation.Passed || evaluation.EntityRoleChecks[1].Passed {
		t.Fatalf("evaluation = %+v, want the target entity check to fail", evaluation)
	}
}

func TestReleaseQuestionOracleAcceptsExpectedNoMatch(t *testing.T) {
	question := benchmark.ArchitectureQuestion{
		Text:            "what calls the lunarTelemetryGateway?",
		Intent:          benchmark.IntentCalledBy,
		EntityRoles:     []benchmark.QuestionEntityRole{{Role: "callee", Value: "lunarTelemetryGateway"}},
		ForbiddenClaims: []string{"The missing symbol exists."},
		NoMatch:         true,
	}
	seeds := []query.SeedSet{{Role: "callee", Term: "lunarTelemetryGateway"}}

	evaluation := evaluateReleaseQuestion(question, seeds, query.Result{})
	if !evaluation.Passed || !evaluation.EntityRoleChecks[0].Passed {
		t.Fatalf("evaluation = %+v, want an expected no-match pass", evaluation)
	}
}

func TestReleaseQuestionOracleRejectsForbiddenClaim(t *testing.T) {
	const forbiddenClaim = "Incoming callers are outgoing calls."
	question := benchmark.ArchitectureQuestion{
		Text:              "who calls QuerySnapshot?",
		Intent:            benchmark.IntentCalledBy,
		EntityRoles:       []benchmark.QuestionEntityRole{{Role: "callee", Value: "QuerySnapshot"}},
		AcceptableSeeds:   []string{"QuerySnapshot"},
		RequiredRelations: []graph.RelationKind{"calls"},
		ForbiddenClaims:   []string{forbiddenClaim},
	}
	seeds := []query.SeedSet{{Role: "callee", Nodes: []graph.Node{{ID: "QuerySnapshot", Label: "QuerySnapshot"}}}}
	result := query.Result{Evidence: []query.EvidenceGroup{{
		SlotRole: "callee",
		Relation: "calls",
		Reason:   forbiddenClaim,
		Nodes:    []graph.Node{{ID: "QuerySnapshot", Label: "QuerySnapshot", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "query/query.go"}}}},
		Edges:    []graph.Edge{{SourceID: "caller", TargetID: "QuerySnapshot", Relation: "calls", Evidence: graph.FactEvidence{Span: graph.SourceSpan{Path: "query/query.go"}}}},
	}}}

	evaluation := evaluateReleaseQuestion(question, seeds, result)
	if evaluation.Passed || len(evaluation.Failures) == 0 {
		t.Fatalf("evaluation = %+v, want a forbidden-claim failure", evaluation)
	}
}

func TestReleaseQuestionOracleAcceptsExplicitNoPathWithoutAClaim(t *testing.T) {
	question := benchmark.ArchitectureQuestion{
		Text:              "how does main reach QuerySnapshot?",
		Intent:            benchmark.IntentPath,
		EntityRoles:       []benchmark.QuestionEntityRole{{Role: "source", Value: "main"}, {Role: "target", Value: "QuerySnapshot"}},
		AcceptableSeeds:   []string{"main", "QuerySnapshot"},
		RequiredRelations: []graph.RelationKind{"calls"},
		ForbiddenClaims:   []string{"A path exists when no directed evidence is returned."},
	}
	seeds := []query.SeedSet{
		{Role: "source", Nodes: []graph.Node{{ID: "main", Label: "main"}}},
		{Role: "target", Nodes: []graph.Node{{ID: "QuerySnapshot", Label: "QuerySnapshot"}}},
	}
	result := query.Result{Warnings: []query.PlanWarning{{Code: "no_directed_path", Message: "No directed path was found; no reachability claim was produced."}}}

	evaluation := evaluateReleaseQuestion(question, seeds, result)
	if !evaluation.Passed {
		t.Fatalf("evaluation = %+v, want explicit no-path warning without a claim to pass", evaluation)
	}
}

func TestNaturalLanguageReleaseAcceptance(t *testing.T) {
	root := releaseRoot(t)
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "release.db"))
	if err != nil {
		t.Fatalf("open release database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	indexed, err := index.Index(ctx, store, index.Request{Root: root})
	if err != nil {
		t.Fatalf("index release workspace: %v", err)
	}
	questions := benchmark.ArchitectureQuestions()
	fixedQuestions := filterQuestions(questions, false)
	sharedQuestions := sharedContractReleaseQuestions()
	_ = runReleaseQuestions(t, ctx, store, indexed.Snapshot, fixedQuestions)
	first := runReleaseQuestions(t, ctx, store, indexed.Snapshot, fixedQuestions)
	second := runReleaseQuestions(t, ctx, store, indexed.Snapshot, fixedQuestions)
	sharedRoot := releaseRootFromEnvironment(t, sharedContractRootEnvironment, root)
	sharedStore, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "shared-contract.db"))
	if err != nil {
		t.Fatalf("open shared-contract database: %v", err)
	}
	t.Cleanup(func() { _ = sharedStore.Close() })
	sharedIndexed, err := index.Index(ctx, sharedStore, index.Request{Root: sharedRoot})
	if err != nil {
		t.Fatalf("index shared-contract workspace: %v", err)
	}
	_ = runReleaseQuestions(t, ctx, sharedStore, sharedIndexed.Snapshot, sharedQuestions)
	sharedFirst := runReleaseQuestions(t, ctx, sharedStore, sharedIndexed.Snapshot, sharedQuestions)
	sharedSecond := runReleaseQuestions(t, ctx, sharedStore, sharedIndexed.Snapshot, sharedQuestions)
	first = mergeReleaseAcceptanceRecords(first, sharedFirst)
	second = mergeReleaseAcceptanceRecords(second, sharedSecond)
	if first.StableOutputHash != second.StableOutputHash {
		t.Fatalf("release output hashes differ: %q and %q", first.StableOutputHash, second.StableOutputHash)
	}
	first.CorpusWorkspace = root
	first.GraphVersion = indexed.Snapshot.Version
	if first.RecallAt10 < 0.90 {
		t.Errorf("release Recall@10 = %.2f, want at least 0.90", first.RecallAt10)
	}
	if first.IntentAccuracy != 1 {
		t.Errorf("release intent accuracy = %.2f, want 1.00", first.IntentAccuracy)
	}
	if first.P95PlanningLookup >= 100*time.Millisecond {
		t.Errorf("release p95 deterministic planning plus lexical lookup = %s, want below 100ms", first.P95PlanningLookup)
	}
	first.SharedWorkspace = sharedRoot
	for _, result := range sharedFirst.Results {
		if result.Question == reportedSharedContractQuestion {
			first.SharedContractResult = result
			break
		}
	}
	record, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal release acceptance record: %v", err)
	}
	t.Logf("Natural-language release acceptance: %s", record)
}

func filterQuestions(questions []benchmark.ArchitectureQuestion, shared bool) []benchmark.ArchitectureQuestion {
	filtered := make([]benchmark.ArchitectureQuestion, 0, len(questions))
	for _, question := range questions {
		if (question.Intent == benchmark.IntentSharedContract) == shared {
			filtered = append(filtered, question)
		}
	}
	return filtered
}

func mergeReleaseAcceptanceRecords(first, second releaseAcceptanceRecord) releaseAcceptanceRecord {
	results := append(append([]releaseQuestionResult(nil), first.Results...), second.Results...)
	merged := releaseAcceptanceRecord{
		QuestionCount:     first.QuestionCount + second.QuestionCount,
		Results:           results,
		RecallAt10:        first.RecallAt10*float64(first.QuestionCount) + second.RecallAt10*float64(second.QuestionCount),
		IntentAccuracy:    first.IntentAccuracy*float64(first.QuestionCount) + second.IntentAccuracy*float64(second.QuestionCount),
		P95PlanningLookup: releaseResultsPercentile(results, func(result releaseQuestionResult) time.Duration { return time.Duration(result.PlanningLookupNS) }),
		P95Traversal:      releaseResultsPercentile(results, func(result releaseQuestionResult) time.Duration { return time.Duration(result.TraversalNS) }),
	}
	if merged.QuestionCount > 0 {
		merged.RecallAt10 /= float64(merged.QuestionCount)
		merged.IntentAccuracy /= float64(merged.QuestionCount)
	}
	merged.StableOutputHash = releaseOutputHash(results)
	return merged
}

func releaseResultsPercentile(results []releaseQuestionResult, value func(releaseQuestionResult) time.Duration) time.Duration {
	durations := make([]time.Duration, 0, len(results))
	for _, result := range results {
		durations = append(durations, value(result))
	}
	return releasePercentile(durations, 0.95)
}

func runSharedContractAcceptance(t *testing.T, ctx context.Context, root string) releaseQuestionResult {
	t.Helper()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "shared-contract.db"))
	if err != nil {
		t.Fatalf("open shared-contract database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	indexed, err := index.Index(ctx, store, index.Request{Root: root})
	if err != nil {
		t.Fatalf("index shared-contract workspace: %v", err)
	}
	question := sharedContractReleaseQuestion()
	plan := query.AnalyzeQuestion(reportedSharedContractQuestion)
	started := time.Now()
	seeds, err := query.RankSeedRequestsSnapshot(ctx, store, store, indexed.Snapshot, seedRequests(plan))
	planningLookupDuration := time.Since(started)
	if err != nil {
		t.Fatalf("rank reported shared-contract question: %v", err)
	}
	traverser := &measuredTraverser{delegate: store}
	result, err := query.QuerySnapshot(ctx, store, traverser, indexed.Snapshot, query.Request{Plan: &plan, MaxDepth: plan.MaxDepth, MaxNodes: plan.MaxNodes})
	if err != nil {
		t.Fatalf("execute reported shared-contract question: %v", err)
	}
	assertSourceBackedEvidence(t, reportedSharedContractQuestion, result.Evidence)
	evaluation := evaluateReleaseQuestion(question, seeds, result)
	if !evaluation.Passed {
		t.Errorf("reported shared-contract question failed acceptance: %v", evaluation.Failures)
	}
	return releaseQuestionResult{
		Question:          question.Text,
		Plan:              plan,
		RecallAt10Hit:     releaseRecallHit(question, seeds),
		Evaluation:        evaluation,
		PlanningLookupNS:  planningLookupDuration.Nanoseconds(),
		TraversalNS:       traverser.elapsed.Nanoseconds(),
		RankedIDs:         releaseRankedIDs(seeds),
		EvidenceGroups:    len(result.Evidence),
		TruncationReasons: append([]storage.TruncationReason(nil), result.TruncationReasons...),
	}
}

func releaseQuestion(text string) benchmark.ArchitectureQuestion {
	for _, question := range benchmark.ArchitectureQuestions() {
		if question.Text == text {
			return question
		}
	}
	panic("release question is not in the benchmark corpus: " + text)
}

func sharedContractReleaseQuestion() benchmark.ArchitectureQuestion {
	for _, question := range sharedContractReleaseQuestions() {
		if question.Text == reportedSharedContractQuestion {
			return question
		}
	}
	panic("reported shared-contract question is not in the benchmark corpus")
}

func sharedContractReleaseQuestions() []benchmark.ArchitectureQuestion {
	questions := make([]benchmark.ArchitectureQuestion, 0)
	for _, question := range filterQuestions(benchmark.ArchitectureQuestions(), true) {
		question.AcceptableSeeds = []string{"PgStore", "SqliteStore", "StorageDriver", "Store"}
		question.AcceptablePaths = []string{"packages/api-pg/src", "packages/api-local/src", "packages/core/src"}
		question.RequiredRelations = []graph.RelationKind{"implements"}
		questions = append(questions, question)
	}
	return questions
}

func runReleaseQuestions(t *testing.T, ctx context.Context, store *sqlite.Store, snapshot storage.Snapshot, questions []benchmark.ArchitectureQuestion) releaseAcceptanceRecord {
	t.Helper()
	record := releaseAcceptanceRecord{QuestionCount: len(questions), Results: make([]releaseQuestionResult, 0, len(questions))}
	planningLookupDurations := make([]time.Duration, 0, len(questions))
	traversalDurations := make([]time.Duration, 0, len(questions))
	recallHits := 0
	intentHits := 0
	for _, question := range questions {
		started := time.Now()
		plan := query.AnalyzeQuestion(question.Text)
		seeds, err := query.RankSeedRequestsSnapshot(ctx, store, store, snapshot, seedRequests(plan))
		planningLookupDuration := time.Since(started)
		if err != nil {
			t.Fatalf("rank release question %q: %v", question.Text, err)
		}
		traverser := &measuredTraverser{delegate: store}
		result, err := query.QuerySnapshot(ctx, store, traverser, snapshot, query.Request{
			Plan:     &plan,
			MaxDepth: plan.MaxDepth,
			MaxNodes: plan.MaxNodes,
		})
		if err != nil {
			t.Fatalf("execute release question %q: %v", question.Text, err)
		}
		assertSourceBackedEvidence(t, question.Text, result.Evidence)

		hit := releaseRecallHit(question, seeds)
		evaluation := evaluateReleaseQuestion(question, seeds, result)
		if !evaluation.Passed {
			t.Errorf("release question %q failed acceptance: %v", question.Text, evaluation.Failures)
		}
		if hit {
			recallHits++
		}
		if string(plan.Intent) == string(question.Intent) {
			intentHits++
		}
		planningLookupDurations = append(planningLookupDurations, planningLookupDuration)
		traversalDurations = append(traversalDurations, traverser.elapsed)
		record.Results = append(record.Results, releaseQuestionResult{
			Question:          question.Text,
			Plan:              plan,
			RecallAt10Hit:     hit,
			Evaluation:        evaluation,
			PlanningLookupNS:  planningLookupDuration.Nanoseconds(),
			TraversalNS:       traverser.elapsed.Nanoseconds(),
			RankedIDs:         releaseRankedIDs(seeds),
			EvidenceGroups:    len(result.Evidence),
			TruncationReasons: append([]storage.TruncationReason(nil), result.TruncationReasons...),
		})
	}
	record.RecallAt10 = float64(recallHits) / float64(len(questions))
	record.IntentAccuracy = float64(intentHits) / float64(len(questions))
	record.P95PlanningLookup = releasePercentile(planningLookupDurations, 0.95)
	record.P95Traversal = releasePercentile(traversalDurations, 0.95)
	record.StableOutputHash = releaseOutputHash(record.Results)
	return record
}

type measuredTraverser struct {
	delegate storage.Traverser
	elapsed  time.Duration
}

func (traverser *measuredTraverser) Traverse(ctx context.Context, snapshot storage.Snapshot, request storage.TraversalRequest) (storage.TraversalResult, error) {
	started := time.Now()
	result, err := traverser.delegate.Traverse(ctx, snapshot, request)
	traverser.elapsed += time.Since(started)
	return result, err
}

func seedRequests(plan query.QueryPlan) []query.SeedRequest {
	requests := make([]query.SeedRequest, len(plan.EntitySlots))
	for index, slot := range plan.EntitySlots {
		requests[index] = query.SeedRequest{Role: slot.Role, Retrieval: slot.Retrieval}
	}
	return requests
}

func releaseRecallHit(question benchmark.ArchitectureQuestion, seeds []query.SeedSet) bool {
	nodes := make([]graph.Node, 0)
	for _, seedSet := range seeds {
		nodes = append(nodes, seedSet.Nodes...)
	}
	if question.NoMatch {
		return len(nodes) == 0
	}
	for _, node := range nodes[:min(len(nodes), 10)] {
		for _, acceptable := range question.AcceptableSeeds {
			if node.ID == acceptable || node.Label == acceptable || node.QualifiedName == acceptable {
				return true
			}
		}
		for _, acceptable := range question.AcceptablePaths {
			path := filepath.ToSlash(node.Evidence.Span.Path)
			acceptablePath := filepath.ToSlash(acceptable)
			if path == acceptablePath || strings.HasPrefix(path, acceptablePath+"/") || filepath.Dir(path) == acceptablePath {
				return true
			}
		}
	}
	return false
}

func assertSourceBackedEvidence(t *testing.T, question string, groups []query.EvidenceGroup) {
	t.Helper()
	for _, group := range groups {
		for _, node := range group.Nodes {
			if node.Evidence.Span.Path == "" {
				t.Errorf("release question %q returned node %q without a source path", question, node.ID)
			}
		}
		for _, edge := range group.Edges {
			if edge.Evidence.Span.Path == "" {
				t.Errorf("release question %q returned edge %q -> %q without a source path", question, edge.SourceID, edge.TargetID)
			}
		}
	}
}

func releaseRankedIDs(seeds []query.SeedSet) [][]string {
	result := make([][]string, len(seeds))
	for index, seedSet := range seeds {
		result[index] = make([]string, len(seedSet.Rankings))
		for rankingIndex, ranking := range seedSet.Rankings {
			result[index][rankingIndex] = ranking.NodeID
		}
	}
	return result
}

func releasePercentile(values []time.Duration, percentile float64) time.Duration {
	ordered := append([]time.Duration(nil), values...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	index := int(float64(len(ordered))*percentile+0.999999) - 1
	return ordered[max(0, index)]
}

func releaseOutputHash(results []releaseQuestionResult) string {
	stable := make([]struct {
		Plan       query.QueryPlan           `json:"plan"`
		RankedIDs  [][]string                `json:"rankedIds"`
		Evaluation releaseQuestionEvaluation `json:"evaluation"`
	}, len(results))
	for index, result := range results {
		stable[index].Plan = result.Plan
		stable[index].RankedIDs = result.RankedIDs
		stable[index].Evaluation = result.Evaluation
	}
	contents, err := json.Marshal(stable)
	if err != nil {
		panic(err)
	}
	hash := sha256.Sum256(contents)
	return "sha256:" + hex.EncodeToString(hash[:])
}

func releaseRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv(releaseRootEnvironment)
	if root == "" {
		t.Skipf("set %s to run the natural-language release acceptance test", releaseRootEnvironment)
	}
	return releaseRootFromEnvironment(t, releaseRootEnvironment, "")
}

func releaseRootFromEnvironment(t *testing.T, name, fallback string) string {
	t.Helper()
	root := os.Getenv(name)
	if root == "" {
		root = fallback
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("resolve release root: %v", err)
	}
	info, err := os.Stat(absoluteRoot)
	if err != nil {
		t.Fatalf("stat release root %q: %v", absoluteRoot, err)
	}
	if !info.IsDir() {
		t.Fatalf("release root %q is not a directory", absoluteRoot)
	}
	return absoluteRoot
}
