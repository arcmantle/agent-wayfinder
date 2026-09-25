package query_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"agent-wayfinder/benchmark"
	"agent-wayfinder/graph"
	"agent-wayfinder/query"
	"agent-wayfinder/storage"
)

func TestAnalyzeQuestionPlansSharedContractIntersection(t *testing.T) {
	const question = "what is the shared contract between postgres and sqlite"

	first := query.AnalyzeQuestion(question)
	second := query.AnalyzeQuestion(question)

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated plans differ:\nfirst:  %+v\nsecond: %+v", first, second)
	}
	if first.SchemaVersion != 1 || first.Question != question {
		t.Errorf("plan identity = {%d, %q}, want {1, %q}", first.SchemaVersion, first.Question, question)
	}
	if first.Intent != query.IntentSharedContract || first.Operator != query.OperatorIntersection || first.Direction != storage.TraverseBoth {
		t.Errorf("plan operation = {%q, %q, %q}, want shared contract intersection in both directions", first.Intent, first.Operator, first.Direction)
	}
	wantKinds := []graph.NodeKind{"typescript:class", "typescript:interface", "typescript:type_alias", "go:type"}
	wantSlots := []query.EntitySlot{
		{Role: "left", Text: "postgres", Retrieval: query.RetrievalRequest{Text: "postgres", TokenGroups: [][]string{{"postgres"}, {"pg"}}, Kinds: wantKinds, Limit: 10}},
		{Role: "right", Text: "sqlite", Retrieval: query.RetrievalRequest{Text: "sqlite", TokenGroups: [][]string{{"sqlite"}}, Kinds: wantKinds, Limit: 10}},
	}
	if !reflect.DeepEqual(first.EntitySlots, wantSlots) {
		t.Errorf("entity slots = %+v, want %+v", first.EntitySlots, wantSlots)
	}
	if first.Confidence <= 0 || len(first.Warnings) != 0 {
		t.Errorf("confidence and warnings = {%v, %+v}, want positive confidence without warnings", first.Confidence, first.Warnings)
	}
}

func TestAnalyzeQuestionPlansFolderRoleRetrieval(t *testing.T) {
	plan := query.AnalyzeQuestion("where is the storage folder")

	if plan.Intent != query.IntentLookup || len(plan.EntitySlots) != 1 {
		t.Fatalf("plan = %+v, want one lookup entity slot", plan)
	}
	slot := plan.EntitySlots[0]
	if slot.Role != "entity" || slot.EntityRole != "folder" || slot.Text != "storage" {
		t.Errorf("slot = %+v, want storage folder entity", slot)
	}
	if got, want := slot.Retrieval, (query.RetrievalRequest{Text: "storage", TokenGroups: [][]string{{"storage"}}, Kinds: []graph.NodeKind{"file"}, Limit: 10}); !reflect.DeepEqual(got, want) {
		t.Errorf("folder retrieval = %+v, want %+v", got, want)
	}
}

func TestAnalyzeQuestionPlansPackageRoleRetrieval(t *testing.T) {
	plan := query.AnalyzeQuestion("describe the query package")

	if plan.Intent != query.IntentExplain || len(plan.EntitySlots) != 1 {
		t.Fatalf("plan = %+v, want one explain entity slot", plan)
	}
	slot := plan.EntitySlots[0]
	if slot.Role != "entity" || slot.EntityRole != "package" || slot.Text != "query" {
		t.Errorf("slot = %+v, want query package entity", slot)
	}
	if got, want := slot.Retrieval.Kinds, []graph.NodeKind{"go:package"}; !reflect.DeepEqual(got, want) {
		t.Errorf("package kinds = %v, want %v", got, want)
	}
}

func TestAnalyzeQuestionPlansFileRoleRetrieval(t *testing.T) {
	plan := query.AnalyzeQuestion("where is the query.go file")

	if plan.Intent != query.IntentLookup || len(plan.EntitySlots) != 1 {
		t.Fatalf("plan = %+v, want one lookup entity slot", plan)
	}
	slot := plan.EntitySlots[0]
	if slot.Role != "entity" || slot.EntityRole != "file" || slot.Text != "query.go" {
		t.Errorf("slot = %+v, want query.go file entity", slot)
	}
	if got, want := slot.Retrieval.Kinds, []graph.NodeKind{"file"}; !reflect.DeepEqual(got, want) {
		t.Errorf("file kinds = %v, want %v", got, want)
	}
}

func TestAnalyzeQuestionPlansClassRoleRetrieval(t *testing.T) {
	plan := query.AnalyzeQuestion("explain the QueryPlan class")

	if plan.Intent != query.IntentExplain || len(plan.EntitySlots) != 1 {
		t.Fatalf("plan = %+v, want one explain entity slot", plan)
	}
	slot := plan.EntitySlots[0]
	if slot.Role != "entity" || slot.EntityRole != "class" || slot.Text != "QueryPlan" {
		t.Errorf("slot = %+v, want QueryPlan class entity", slot)
	}
	wantKinds := []graph.NodeKind{"typescript:class", "javascript:class", "go:type"}
	if !reflect.DeepEqual(slot.Retrieval.Kinds, wantKinds) {
		t.Errorf("class kinds = %v, want %v", slot.Retrieval.Kinds, wantKinds)
	}
}

func TestAnalyzeQuestionPlansServiceRoleRetrieval(t *testing.T) {
	plan := query.AnalyzeQuestion("explain the index service")

	if plan.Intent != query.IntentExplain || len(plan.EntitySlots) != 1 {
		t.Fatalf("plan = %+v, want one explain entity slot", plan)
	}
	slot := plan.EntitySlots[0]
	if slot.Role != "entity" || slot.EntityRole != "service" || slot.Text != "index" {
		t.Errorf("slot = %+v, want index service entity", slot)
	}
	wantKinds := []graph.NodeKind{"typescript:class", "javascript:class", "go:type"}
	if !reflect.DeepEqual(slot.Retrieval.Kinds, wantKinds) {
		t.Errorf("service kinds = %v, want %v", slot.Retrieval.Kinds, wantKinds)
	}
}

func TestAnalyzeQuestionPlansSourceQualifiedExplain(t *testing.T) {
	plan := query.AnalyzeQuestion("Explain StartProcess in cmd/agent-wayfinder/catalog/command.go")

	if plan.Intent != query.IntentExplain || plan.Operator != query.OperatorExplain || len(plan.EntitySlots) != 1 {
		t.Fatalf("plan = %+v, want one explain entity slot", plan)
	}
	slot := plan.EntitySlots[0]
	wantSlot := query.EntitySlot{
		Role: "entity",
		Text: "StartProcess",
		Retrieval: query.RetrievalRequest{
			Text:        "StartProcess",
			TokenGroups: [][]string{{"start", "process"}},
			SourcePath:  "cmd/agent-wayfinder/catalog/command.go",
			Limit:       10,
		},
	}
	if !reflect.DeepEqual(slot, wantSlot) {
		t.Errorf("entity slot = %+v, want %+v", slot, wantSlot)
	}
}

func TestAnalyzeQuestionPlansWorkspaceCapability(t *testing.T) {
	plan := query.AnalyzeQuestion("Does this workspace validate access tokens?")

	if plan.Intent != query.IntentCapability || plan.Confidence != 1 || len(plan.EntitySlots) != 0 {
		t.Errorf("capability plan = %+v, want a confident catalog-only capability plan", plan)
	}
}

func TestParseCopilotPlannerResponseProtocolAcceptsAllowlistedIntent(t *testing.T) {
	plan, err := query.ParseCopilotPlannerResponse([]byte(`{
		"schemaVersion": 1,
		"intent": "shared_contract",
		"entities": ["postgres", "sqlite"]
	}`))
	if err != nil {
		t.Fatalf("parse planner response: %v", err)
	}
	if plan.Intent != query.IntentSharedContract || plan.Operator != query.OperatorIntersection || plan.Direction != storage.TraverseBoth {
		t.Errorf("plan operation = {%q, %q, %q}, want shared-contract intersection in both directions", plan.Intent, plan.Operator, plan.Direction)
	}
	if len(plan.EntitySlots) != 2 || plan.EntitySlots[0].Text != "postgres" || plan.EntitySlots[1].Text != "sqlite" {
		t.Errorf("entity slots = %+v, want postgres and sqlite", plan.EntitySlots)
	}
}

func TestParseCopilotPlannerResponseProtocolDerivesCallsPlan(t *testing.T) {
	plan, err := query.ParseCopilotPlannerResponse([]byte(`{
		"schemaVersion": 1,
		"intent": "calls",
		"entities": ["runQuery"]
	}`))
	if err != nil {
		t.Fatalf("parse planner response: %v", err)
	}
	if plan.Operator != query.OperatorNeighbors || plan.Direction != storage.TraverseOutgoing || !reflect.DeepEqual(plan.AllowedRelations, []graph.RelationKind{"calls"}) {
		t.Errorf("plan = %+v, want an outgoing calls plan", plan)
	}
	if len(plan.EntitySlots) != 1 || plan.EntitySlots[0].Role != "caller" || plan.EntitySlots[0].Text != "runQuery" {
		t.Errorf("entity slots = %+v, want runQuery as the caller", plan.EntitySlots)
	}
}

func TestParseLocalPlannerResponseDerivesTrustedCallsPlan(t *testing.T) {
	plan, err := query.ParseLocalPlannerResponse([]byte(`{
		"schemaVersion": 1,
		"intent": "calls",
		"entities": ["runQuery"],
		"confidence": "high"
	}`))
	if err != nil {
		t.Fatalf("parse local planner response: %v", err)
	}
	if plan.Operator != query.OperatorNeighbors || plan.Direction != storage.TraverseOutgoing || !reflect.DeepEqual(plan.AllowedRelations, []graph.RelationKind{"calls"}) {
		t.Errorf("plan = %+v, want a trusted outgoing calls plan", plan)
	}
	if plan.MaxDepth != 2 || plan.MaxNodes != 100 {
		t.Errorf("plan limits = {%d, %d}, want {2, 100}", plan.MaxDepth, plan.MaxNodes)
	}
}

func TestParseLocalPlannerResponseRejectsLowConfidence(t *testing.T) {
	_, err := query.ParseLocalPlannerResponse([]byte(`{
		"schemaVersion": 1,
		"intent": "calls",
		"entities": ["runQuery"],
		"confidence": "low"
	}`))
	if err == nil {
		t.Error("parse local planner response succeeded, want low-confidence rejection")
	}
}

func TestParseCopilotPlannerResponseProtocolRejectsUnsafeResponses(t *testing.T) {
	tests := []struct {
		name     string
		response string
	}{
		{name: "invalid JSON", response: `{`},
		{name: "unknown field", response: `{"schemaVersion":1,"intent":"calls","entities":["runQuery"],"relations":["contains"]}`},
		{name: "model limit", response: `{"schemaVersion":1,"intent":"calls","entities":["runQuery"],"maxNodes":10000}`},
		{name: "unsupported version", response: `{"schemaVersion":2,"intent":"calls","entities":["runQuery"]}`},
		{name: "unsupported intent", response: `{"schemaVersion":1,"intent":"delete","entities":["runQuery"]}`},
		{name: "excess entities", response: `{"schemaVersion":1,"intent":"calls","entities":["one","two"]}`},
		{name: "empty entity", response: `{"schemaVersion":1,"intent":"calls","entities":[""]}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := query.ParseCopilotPlannerResponse([]byte(test.response)); err == nil {
				t.Error("parse planner response succeeded, want an error")
			}
		})
	}
}

func TestAnalyzeQuestionPlansIncomingImpactRelations(t *testing.T) {
	plan := query.AnalyzeQuestion("what is affected if changed changes")

	if plan.Intent != query.IntentImpact || plan.Operator != query.OperatorImpact || plan.Direction != storage.TraverseIncoming {
		t.Errorf("plan operation = {%q, %q, %q}, want incoming impact", plan.Intent, plan.Operator, plan.Direction)
	}
	want := []graph.RelationKind{"references", "implements", "contains", "calls", "imports_from", "requires", "depends_on"}
	if !reflect.DeepEqual(plan.AllowedRelations, want) {
		t.Errorf("impact relations = %v, want %v", plan.AllowedRelations, want)
	}
}

func TestAnalyzeQuestionRecognizesImpactParaphrase(t *testing.T) {
	const question = "what will break if storage changes"
	plan := query.AnalyzeQuestion(question)
	if !reflect.DeepEqual(plan, query.AnalyzeQuestion(question)) {
		t.Fatal("repeated plans differ")
	}

	if plan.Intent != query.IntentImpact || plan.Operator != query.OperatorImpact || plan.Direction != storage.TraverseIncoming {
		t.Errorf("plan operation = {%q, %q, %q}, want incoming impact", plan.Intent, plan.Operator, plan.Direction)
	}
	wantRelations := []graph.RelationKind{"references", "implements", "contains", "calls", "imports_from", "requires", "depends_on"}
	if !reflect.DeepEqual(plan.AllowedRelations, wantRelations) {
		t.Errorf("relations = %v, want %v", plan.AllowedRelations, wantRelations)
	}
	if len(plan.EntitySlots) != 1 || plan.EntitySlots[0].Role != "changed" || plan.EntitySlots[0].Text != "storage" {
		t.Errorf("entity slots = %+v, want storage as the changed entity", plan.EntitySlots)
	}
}

func TestAnalyzeQuestionRecognizesControlledParaphrases(t *testing.T) {
	tests := []struct {
		question  string
		intent    query.Intent
		operator  query.ExecutionOperator
		direction storage.TraversalDirection
		roles     []string
		texts     []string
		relations []graph.RelationKind
	}{
		{"which modules depend on storage", query.IntentDependents, query.OperatorNeighbors, storage.TraverseIncoming, []string{"dependency"}, []string{"storage"}, []graph.RelationKind{"imports_from", "requires", "depends_on"}},
		{"which functions call runQuery", query.IntentCalledBy, query.OperatorNeighbors, storage.TraverseIncoming, []string{"callee"}, []string{"runQuery"}, []graph.RelationKind{"calls"}},
		{"what do postgres and sqlite have in common", query.IntentSharedContract, query.OperatorIntersection, storage.TraverseBoth, []string{"left", "right"}, []string{"postgres", "sqlite"}, []graph.RelationKind{"implements", "contains"}},
		{"how do I get from source to target", query.IntentPath, query.OperatorPath, storage.TraverseOutgoing, []string{"source", "target"}, []string{"source", "target"}, []graph.RelationKind{"calls"}},
	}

	for _, test := range tests {
		t.Run(test.question, func(t *testing.T) {
			plan := query.AnalyzeQuestion(test.question)
			if !reflect.DeepEqual(plan, query.AnalyzeQuestion(test.question)) {
				t.Fatal("repeated plans differ")
			}
			if plan.Intent != test.intent || plan.Operator != test.operator || plan.Direction != test.direction {
				t.Errorf("plan operation = {%q, %q, %q}, want {%q, %q, %q}", plan.Intent, plan.Operator, plan.Direction, test.intent, test.operator, test.direction)
			}
			if !reflect.DeepEqual(plan.AllowedRelations, test.relations) {
				t.Errorf("relations = %v, want %v", plan.AllowedRelations, test.relations)
			}
			if len(plan.EntitySlots) != len(test.roles) {
				t.Fatalf("entity slots = %+v, want roles %v", plan.EntitySlots, test.roles)
			}
			for index, slot := range plan.EntitySlots {
				if slot.Role != test.roles[index] || slot.Text != test.texts[index] {
					t.Errorf("entity slot %d = {%q, %q}, want {%q, %q}", index, slot.Role, slot.Text, test.roles[index], test.texts[index])
				}
			}
		})
	}
}

func TestAnalyzeQuestionUsesPortableSharedContractEntityTokens(t *testing.T) {
	shared := query.AnalyzeQuestion("what is the shared contract between PostgreSQL and sqlite")
	if got, want := shared.EntitySlots[0].Retrieval.TokenGroups, [][]string{{"postgre", "sql"}, {"pg"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("shared-contract PostgreSQL groups = %v, want %v", got, want)
	}

	lookup := query.AnalyzeQuestion("where is postgres")
	if got, want := lookup.EntitySlots[0].Retrieval.TokenGroups, [][]string{{"postgres"}, {"pg"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("lookup PostgreSQL groups = %v, want %v", got, want)
	}

	sqlite := query.AnalyzeQuestion("show the common storage contracts for SQLite and Postgres")
	if got := sqlite.EntitySlots[0].Retrieval.TokenGroups; !reflect.DeepEqual(got, [][]string{{"sq", "lite"}, {"sqlite"}}) {
		t.Errorf("shared-contract SQLite groups = %v, want SQLite alias group", got)
	}

	adapter := query.AnalyzeQuestion("which interfaces are common to Store and a PostgreSQL adapter?")
	if got := adapter.EntitySlots[0].Retrieval.TokenGroups; !reflect.DeepEqual(got, [][]string{{"store"}, {"storage", "driver"}, {"storage"}, {"driver"}}) {
		t.Errorf("shared-contract Store groups = %v, want portable store groups", got)
	}
	if len(adapter.EntitySlots[0].Retrieval.Kinds) == 0 {
		t.Error("shared-contract Store retrieval has no portable node-kind filter")
	}
	if got := adapter.EntitySlots[1].Retrieval.TokenGroups; !reflect.DeepEqual(got, [][]string{{"postgres"}, {"pg"}, {"adapter"}}) {
		t.Errorf("shared-contract adapter groups = %v, want portable adapter groups", got)
	}
}

func TestAnalyzeQuestionIdentifiesCorpusIntentsAndEntityRoles(t *testing.T) {
	for _, question := range benchmark.ArchitectureQuestions() {
		t.Run(question.Text, func(t *testing.T) {
			plan := query.AnalyzeQuestion(question.Text)
			if string(plan.Intent) != string(question.Intent) {
				t.Errorf("intent = %q, want %q", plan.Intent, question.Intent)
			}
			if len(plan.EntitySlots) != len(question.EntityRoles) {
				t.Fatalf("entity slots = %+v, want roles %+v", plan.EntitySlots, question.EntityRoles)
			}
			for index, role := range question.EntityRoles {
				slot := plan.EntitySlots[index]
				if slot.Role != role.Role || slot.Text != role.Value {
					t.Errorf("entity slot %d = {%q, %q}, want {%q, %q}", index, slot.Role, slot.Text, role.Role, role.Value)
				}
			}
		})
	}
}

func TestAnalyzeQuestionNormalizesLexicalFormsAfterEntityExtraction(t *testing.T) {
	tests := []struct {
		question          string
		wantTerms         []string
		wantIgnored       []string
		wantQuotedPhrases []string
		wantSlotTokens    []string
	}{
		{
			question:       "show me storage/sqlite/sqlite.go",
			wantTerms:      []string{"storage", "sqlite", "sqlite", "go"},
			wantIgnored:    []string{"show", "me"},
			wantSlotTokens: []string{"storage", "sqlite", "sqlite", "go"},
		},
		{
			question:          `Explain "LookupExactNodes"`,
			wantTerms:         []string{"lookup", "exact", "nodes"},
			wantIgnored:       []string{"explain"},
			wantQuotedPhrases: []string{"LookupExactNodes"},
			wantSlotTokens:    []string{"lookup", "exact", "nodes"},
		},
		{
			question:       "which functions does runQuery invoke",
			wantTerms:      []string{"run", "query", "calls"},
			wantIgnored:    []string{"which", "functions", "does"},
			wantSlotTokens: []string{"run", "query"},
		},
		{
			question:       "find query_snapshot",
			wantTerms:      []string{"query", "snapshot"},
			wantIgnored:    []string{"find"},
			wantSlotTokens: []string{"query", "snapshot"},
		},
	}

	for _, test := range tests {
		t.Run(test.question, func(t *testing.T) {
			plan := query.AnalyzeQuestion(test.question)
			if !reflect.DeepEqual(plan.NormalizedTerms, test.wantTerms) {
				t.Errorf("normalized terms = %v, want %v", plan.NormalizedTerms, test.wantTerms)
			}
			if !reflect.DeepEqual(plan.IgnoredStopWords, test.wantIgnored) {
				t.Errorf("ignored stop words = %v, want %v", plan.IgnoredStopWords, test.wantIgnored)
			}
			if !reflect.DeepEqual(plan.QuotedPhrases, test.wantQuotedPhrases) {
				t.Errorf("quoted phrases = %v, want %v", plan.QuotedPhrases, test.wantQuotedPhrases)
			}
			if got := plan.EntitySlots[0].Retrieval.TokenGroups[0]; !reflect.DeepEqual(got, test.wantSlotTokens) {
				t.Errorf("slot tokens = %v, want %v", got, test.wantSlotTokens)
			}
		})
	}
}

func TestAnalyzeQuestionReturnsStableLexicalFallbackForUnknownGrammar(t *testing.T) {
	const question = "compare QuerySnapshot with storage"
	plan := query.AnalyzeQuestion(question)

	if plan.Intent != query.IntentUnknown || plan.Confidence <= 0 || plan.Confidence >= 0.5 {
		t.Errorf("unknown plan intent and confidence = {%q, %v}, want unknown with low confidence", plan.Intent, plan.Confidence)
	}
	if len(plan.EntitySlots) != 1 || plan.EntitySlots[0].Role != "entity" || !reflect.DeepEqual(plan.EntitySlots[0].Retrieval.TokenGroups, [][]string{{"compare", "query", "snapshot", "with", "storage"}}) {
		t.Errorf("fallback slots = %+v, want one lexical entity slot", plan.EntitySlots)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != "unknown_intent" || len(plan.Warnings[0].Suggestions) == 0 {
		t.Errorf("warnings = %+v, want unknown-intent warning with a suggestion", plan.Warnings)
	}

	firstJSON, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal query plan: %v", err)
	}
	secondJSON, err := json.Marshal(query.AnalyzeQuestion(question))
	if err != nil {
		t.Fatalf("marshal repeated query plan: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Errorf("repeated JSON plans differ:\n%s\n%s", firstJSON, secondJSON)
	}
	var decoded query.QueryPlan
	if err := json.Unmarshal(firstJSON, &decoded); err != nil {
		t.Fatalf("unmarshal query plan: %v", err)
	}
	if !reflect.DeepEqual(decoded, plan) {
		t.Errorf("decoded plan = %+v, want %+v", decoded, plan)
	}
}

func TestAnalyzeQuestionPlansArchitecturalMoveComparison(t *testing.T) {
	const question = "Should Vite configuration move out of core-host into a core-vite package, or should shell generation move into a core-shell package? Compare dependency direction and consumers."
	plan := query.AnalyzeQuestion(question)

	if plan.Intent != query.IntentDependencyComparison || plan.Operator != query.OperatorNeighbors || plan.Direction != storage.TraverseBoth {
		t.Errorf("plan operation = {%q, %q, %q}, want dependency comparison in both directions", plan.Intent, plan.Operator, plan.Direction)
	}
	wantRelations := []graph.RelationKind{"imports_from", "requires", "depends_on"}
	if !reflect.DeepEqual(plan.AllowedRelations, wantRelations) {
		t.Errorf("comparison relations = %v, want %v", plan.AllowedRelations, wantRelations)
	}
	wantGroups := [][]string{
		{"vite", "configuration"},
		{"core", "host"},
		{"core", "vite"},
		{"shell", "generation"},
		{"core", "shell"},
	}
	if len(plan.EntitySlots) != 1 || plan.EntitySlots[0].Role != "comparison" || !reflect.DeepEqual(plan.EntitySlots[0].Retrieval.TokenGroups, wantGroups) {
		t.Errorf("comparison slots = %+v, want one alternative-topic slot with groups %v", plan.EntitySlots, wantGroups)
	}
	if plan.Confidence < 0.5 || len(plan.Warnings) != 0 {
		t.Errorf("confidence and warnings = {%v, %+v}, want recognized comparison without warnings", plan.Confidence, plan.Warnings)
	}
}

func TestAnalyzeQuestionAcceptsArchitecturalComparisonWordingVariants(t *testing.T) {
	questions := []string{
		"Should Vite configuration move out of core-host into core-vite, or should shell generation move into core-shell? Compare dependency directions and consumer.",
		"Should Vite configuration move out of core-host into a core-vite package, or should shell generation move into a core-shell package? Compare consumers and dependencies.",
	}
	for _, question := range questions {
		t.Run(question, func(t *testing.T) {
			plan := query.AnalyzeQuestion(question)
			if plan.Intent != query.IntentDependencyComparison || len(plan.EntitySlots) != 1 {
				t.Errorf("plan = %+v, want one recognized dependency-comparison slot", plan)
			}
		})
	}
}
