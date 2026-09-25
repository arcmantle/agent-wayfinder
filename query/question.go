package query

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"

	"agent-wayfinder/graph"
	"agent-wayfinder/storage"
)

const QueryPlanSchemaVersion = 1

type Intent string

const (
	IntentUnknown              Intent = "unknown"
	IntentLookup               Intent = "lookup"
	IntentExplain              Intent = "explain"
	IntentCalls                Intent = "calls"
	IntentCalledBy             Intent = "called_by"
	IntentDependencies         Intent = "dependencies"
	IntentDependents           Intent = "dependents"
	IntentPath                 Intent = "path"
	IntentReachability         Intent = "reachability"
	IntentSharedContract       Intent = "shared_contract"
	IntentImpact               Intent = "impact"
	IntentDependencyComparison Intent = "dependency_comparison"
	IntentCapability           Intent = "capability"
)

type ExecutionOperator string

const (
	OperatorLookup       ExecutionOperator = "lookup"
	OperatorExplain      ExecutionOperator = "explain"
	OperatorNeighbors    ExecutionOperator = "neighbors"
	OperatorPath         ExecutionOperator = "path"
	OperatorIntersection ExecutionOperator = "intersection"
	OperatorImpact       ExecutionOperator = "impact"
)

type RetrievalRequest = storage.LexicalSearchRequest

type EntitySlot struct {
	Role       string           `json:"role"`
	EntityRole string           `json:"entityRole,omitempty"`
	Text       string           `json:"text"`
	Retrieval  RetrievalRequest `json:"retrieval"`
}

type PlanWarning struct {
	Code        string   `json:"code"`
	Message     string   `json:"message"`
	Suggestions []string `json:"suggestions,omitempty"`
}

type QueryPlan struct {
	SchemaVersion    int                        `json:"schemaVersion"`
	Question         string                     `json:"question"`
	Tokens           []string                   `json:"tokens"`
	NormalizedTerms  []string                   `json:"normalizedTerms"`
	QuotedPhrases    []string                   `json:"quotedPhrases,omitempty"`
	IgnoredStopWords []string                   `json:"ignoredStopWords,omitempty"`
	RelationHints    []string                   `json:"relationHints,omitempty"`
	DirectionHints   []string                   `json:"directionHints,omitempty"`
	Intent           Intent                     `json:"intent"`
	Confidence       float64                    `json:"confidence"`
	EntitySlots      []EntitySlot               `json:"entitySlots"`
	AllowedRelations []graph.RelationKind       `json:"allowedRelations,omitempty"`
	Direction        storage.TraversalDirection `json:"direction"`
	Operator         ExecutionOperator          `json:"operator"`
	MaxDepth         int                        `json:"maxDepth"`
	MaxNodes         int                        `json:"maxNodes"`
	Warnings         []PlanWarning              `json:"warnings"`
}

type plannerResponse struct {
	SchemaVersion int      `json:"schemaVersion"`
	Intent        Intent   `json:"intent"`
	Entities      []string `json:"entities"`
}

type copilotPlannerResponse = plannerResponse

type localPlannerResponse struct {
	plannerResponse
	Confidence string `json:"confidence"`
}

type copilotPlannerIntent struct {
	operator  ExecutionOperator
	direction storage.TraversalDirection
	roles     []string
	relations []graph.RelationKind
	maxDepth  int
}

var copilotPlannerIntents = map[Intent]copilotPlannerIntent{
	IntentLookup:         {operator: OperatorLookup, direction: storage.TraverseBoth, roles: []string{"entity"}, maxDepth: 2},
	IntentExplain:        {operator: OperatorExplain, direction: storage.TraverseBoth, roles: []string{"entity"}, maxDepth: 2},
	IntentCalls:          {operator: OperatorNeighbors, direction: storage.TraverseOutgoing, roles: []string{"caller"}, relations: []graph.RelationKind{"calls"}, maxDepth: 2},
	IntentCalledBy:       {operator: OperatorNeighbors, direction: storage.TraverseIncoming, roles: []string{"callee"}, relations: []graph.RelationKind{"calls"}, maxDepth: 2},
	IntentDependencies:   {operator: OperatorNeighbors, direction: storage.TraverseOutgoing, roles: []string{"dependent"}, relations: []graph.RelationKind{"imports_from", "requires", "depends_on"}, maxDepth: 2},
	IntentDependents:     {operator: OperatorNeighbors, direction: storage.TraverseIncoming, roles: []string{"dependency"}, relations: []graph.RelationKind{"imports_from", "requires", "depends_on"}, maxDepth: 2},
	IntentPath:           {operator: OperatorPath, direction: storage.TraverseOutgoing, roles: []string{"source", "target"}, relations: []graph.RelationKind{"calls"}, maxDepth: 8},
	IntentReachability:   {operator: OperatorPath, direction: storage.TraverseOutgoing, roles: []string{"source", "target"}, relations: []graph.RelationKind{"calls"}, maxDepth: 8},
	IntentSharedContract: {operator: OperatorIntersection, direction: storage.TraverseBoth, roles: []string{"left", "right"}, relations: []graph.RelationKind{"implements", "contains"}, maxDepth: 2},
	IntentImpact:         {operator: OperatorImpact, direction: storage.TraverseIncoming, roles: []string{"changed"}, relations: []graph.RelationKind{"references", "implements", "contains", "calls", "imports_from", "requires", "depends_on"}, maxDepth: 2},
}

// ParseCopilotPlannerResponse validates a bounded Copilot planner response.
func ParseCopilotPlannerResponse(response []byte) (QueryPlan, error) {
	var plannerResponse copilotPlannerResponse
	if err := decodePlannerResponse("Copilot", response, &plannerResponse); err != nil {
		return QueryPlan{}, err
	}
	return validatePlannerResponse("Copilot", plannerResponse)
}

// ParseLocalPlannerResponse validates a bounded local planner response.
func ParseLocalPlannerResponse(response []byte) (QueryPlan, error) {
	var responseSchema localPlannerResponse
	if err := decodePlannerResponse("local", response, &responseSchema); err != nil {
		return QueryPlan{}, err
	}
	if responseSchema.Confidence != "high" {
		return QueryPlan{}, fmt.Errorf("parse local planner response: confidence must be high")
	}
	return validatePlannerResponse("local", responseSchema.plannerResponse)
}

// ParseClaudePlannerResponse validates a bounded Claude planner response.
func ParseClaudePlannerResponse(response []byte) (QueryPlan, error) {
	var plannerResponse plannerResponse
	if err := decodePlannerResponse("Claude", response, &plannerResponse); err != nil {
		return QueryPlan{}, err
	}
	return validatePlannerResponse("Claude", plannerResponse)
}

func decodePlannerResponse(planner string, response []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(response)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("parse %s planner response: %w", planner, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("parse %s planner response: multiple JSON values", planner)
	}
	return nil
}

func validatePlannerResponse(planner string, plannerResponse plannerResponse) (QueryPlan, error) {
	if plannerResponse.SchemaVersion != QueryPlanSchemaVersion {
		return QueryPlan{}, fmt.Errorf("parse %s planner response: unsupported schema version %d", planner, plannerResponse.SchemaVersion)
	}
	intent, found := copilotPlannerIntents[plannerResponse.Intent]
	if !found {
		return QueryPlan{}, fmt.Errorf("parse %s planner response: unsupported intent %q", planner, plannerResponse.Intent)
	}
	if len(plannerResponse.Entities) != len(intent.roles) {
		return QueryPlan{}, fmt.Errorf("parse %s planner response: intent %q requires %d entities", planner, plannerResponse.Intent, len(intent.roles))
	}
	for _, entity := range plannerResponse.Entities {
		if cleanEntityText(entity) == "" {
			return QueryPlan{}, fmt.Errorf("parse %s planner response: entity text is required", planner)
		}
	}
	return copilotQueryPlan(plannerResponse.Intent, intent, plannerResponse.Entities), nil
}

func copilotQueryPlan(intent Intent, specification copilotPlannerIntent, entities []string) QueryPlan {
	question := strings.Join(entities, " and ")
	slots := make([]EntitySlot, len(entities))
	for index, entity := range entities {
		slots[index] = entitySlot(specification.roles[index], cleanEntityText(entity))
	}
	return QueryPlan{
		SchemaVersion:    QueryPlanSchemaVersion,
		Question:         question,
		Tokens:           questionTokens(question),
		NormalizedTerms:  lowercaseTokens(questionTokens(question)),
		Intent:           intent,
		Confidence:       1,
		EntitySlots:      slots,
		AllowedRelations: append([]graph.RelationKind(nil), specification.relations...),
		RelationHints:    relationHints(specification.relations),
		DirectionHints:   []string{string(specification.direction)},
		Direction:        specification.direction,
		Operator:         specification.operator,
		MaxDepth:         specification.maxDepth,
		MaxNodes:         100,
		Warnings:         []PlanWarning{},
	}
}

type questionRule struct {
	pattern        *regexp.Regexp
	intent         Intent
	operator       ExecutionOperator
	direction      storage.TraversalDirection
	roles          []string
	captureIndexes []int
	relations      []graph.RelationKind
	maximumDepth   int
}

var questionRules = []questionRule{
	rule(`^what is the (?:shared|common) contracts? between (.+?) and (.+?)$`, IntentSharedContract, OperatorIntersection, storage.TraverseBoth, []string{"left", "right"}, nil, []graph.RelationKind{"implements", "contains"}),
	rule(`^what do (.+?) and (.+?) have in common$`, IntentSharedContract, OperatorIntersection, storage.TraverseBoth, []string{"left", "right"}, nil, []graph.RelationKind{"implements", "contains"}),
	rule(`^which interfaces are common to (.+?) and (?:a )?(.+?)$`, IntentSharedContract, OperatorIntersection, storage.TraverseBoth, []string{"left", "right"}, nil, []graph.RelationKind{"implements", "contains"}),
	rule(`^show the common storage contracts for (.+?) and (.+?)$`, IntentSharedContract, OperatorIntersection, storage.TraverseBoth, []string{"left", "right"}, nil, []graph.RelationKind{"implements", "contains"}),
	rule(`^what is affected if (.+?) changes$`, IntentImpact, OperatorImpact, storage.TraverseIncoming, []string{"changed"}, nil, []graph.RelationKind{"references", "implements", "contains", "calls", "imports_from", "requires", "depends_on"}),
	rule(`^what will break if (.+?) changes$`, IntentImpact, OperatorImpact, storage.TraverseIncoming, []string{"changed"}, nil, []graph.RelationKind{"references", "implements", "contains", "calls", "imports_from", "requires", "depends_on"}),
	rule(`^impact of changing (.+?)$`, IntentImpact, OperatorImpact, storage.TraverseIncoming, []string{"changed"}, nil, []graph.RelationKind{"references", "implements", "contains", "calls", "imports_from", "requires", "depends_on"}),
	rule(`^find a path from (.+?) to (.+?)$`, IntentPath, OperatorPath, storage.TraverseOutgoing, []string{"source", "target"}, nil, []graph.RelationKind{"calls"}),
	rule(`^how do i get from (.+?) to (.+?)$`, IntentPath, OperatorPath, storage.TraverseOutgoing, []string{"source", "target"}, nil, []graph.RelationKind{"calls"}),
	rule(`^how does (.+?) reach (.+?)$`, IntentPath, OperatorPath, storage.TraverseOutgoing, []string{"source", "target"}, nil, []graph.RelationKind{"calls"}),
	rule(`^trace (.+?) from (.+?)$`, IntentPath, OperatorPath, storage.TraverseOutgoing, []string{"source", "target"}, []int{2, 1}, []graph.RelationKind{"calls"}),
	rule(`^can (.+?) reach (.+?)$`, IntentReachability, OperatorPath, storage.TraverseOutgoing, []string{"source", "target"}, nil, []graph.RelationKind{"calls"}),
	rule(`^does (.+?) flow to (.+?)$`, IntentReachability, OperatorPath, storage.TraverseOutgoing, []string{"source", "target"}, nil, []graph.RelationKind{"calls"}),
	rule(`^is (.+?) connected to (.+?) through calls$`, IntentReachability, OperatorPath, storage.TraverseOutgoing, []string{"source", "target"}, nil, []graph.RelationKind{"calls"}),
	rule(`^who calls (.+?)$`, IntentCalledBy, OperatorNeighbors, storage.TraverseIncoming, []string{"callee"}, nil, []graph.RelationKind{"calls"}),
	rule(`^which functions call (.+?)$`, IntentCalledBy, OperatorNeighbors, storage.TraverseIncoming, []string{"callee"}, nil, []graph.RelationKind{"calls"}),
	rule(`^show callers of (.+?)$`, IntentCalledBy, OperatorNeighbors, storage.TraverseIncoming, []string{"callee"}, nil, []graph.RelationKind{"calls"}),
	rule(`^where is (.+?) called by other code$`, IntentCalledBy, OperatorNeighbors, storage.TraverseIncoming, []string{"callee"}, nil, []graph.RelationKind{"calls"}),
	rule(`^what calls (?:the )?(.+?)$`, IntentCalledBy, OperatorNeighbors, storage.TraverseIncoming, []string{"callee"}, nil, []graph.RelationKind{"calls"}),
	rule(`^what does (.+?) call$`, IntentCalls, OperatorNeighbors, storage.TraverseOutgoing, []string{"caller"}, nil, []graph.RelationKind{"calls"}),
	rule(`^which functions does (.+?) invoke$`, IntentCalls, OperatorNeighbors, storage.TraverseOutgoing, []string{"caller"}, nil, []graph.RelationKind{"calls"}),
	rule(`^show calls from (.+?)$`, IntentCalls, OperatorNeighbors, storage.TraverseOutgoing, []string{"caller"}, nil, []graph.RelationKind{"calls"}),
	rule(`^what does the (.+?) package import$`, IntentDependencies, OperatorNeighbors, storage.TraverseOutgoing, []string{"dependent"}, nil, []graph.RelationKind{"imports_from", "requires", "depends_on"}),
	rule(`^which packages does (.+?) depend on$`, IntentDependencies, OperatorNeighbors, storage.TraverseOutgoing, []string{"dependent"}, nil, []graph.RelationKind{"imports_from", "requires", "depends_on"}),
	rule(`^what modules are required by (.+?)$`, IntentDependencies, OperatorNeighbors, storage.TraverseOutgoing, []string{"dependent"}, nil, []graph.RelationKind{"imports_from", "requires", "depends_on"}),
	rule(`^what depends on the (.+?) package$`, IntentDependents, OperatorNeighbors, storage.TraverseIncoming, []string{"dependency"}, nil, []graph.RelationKind{"imports_from", "requires", "depends_on"}),
	rule(`^which modules depend on (.+?)$`, IntentDependents, OperatorNeighbors, storage.TraverseIncoming, []string{"dependency"}, nil, []graph.RelationKind{"imports_from", "requires", "depends_on"}),
	rule(`^find users of (.+?)$`, IntentDependents, OperatorNeighbors, storage.TraverseIncoming, []string{"dependency"}, nil, []graph.RelationKind{"references", "imports_from"}),
	rule(`^which packages use (.+?)$`, IntentDependents, OperatorNeighbors, storage.TraverseIncoming, []string{"dependency"}, nil, []graph.RelationKind{"imports_from", "requires", "depends_on"}),
	rule(`^explain (.+?)$`, IntentExplain, OperatorExplain, storage.TraverseBoth, []string{"entity"}, nil, nil),
	rule(`^what does (.+?) do$`, IntentExplain, OperatorExplain, storage.TraverseBoth, []string{"entity"}, nil, nil),
	rule(`^describe the (.+?) package$`, IntentExplain, OperatorExplain, storage.TraverseBoth, []string{"entity"}, nil, nil),
	rule(`^where is (.+?)$`, IntentLookup, OperatorLookup, storage.TraverseBoth, []string{"entity"}, nil, nil),
	rule(`^find (.+?)$`, IntentLookup, OperatorLookup, storage.TraverseBoth, []string{"entity"}, nil, nil),
	rule(`^show me (.+?)$`, IntentLookup, OperatorLookup, storage.TraverseBoth, []string{"entity"}, nil, nil),
}

var architecturalMoveComparisonPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^should (.+?) move out of (.+?) into (?:a )?(.+?)(?: package)?, or should (.+?) move into (?:a )?(.+?)(?: package)?\?? compare dependenc(?:y|ies)(?: direction| directions)? and consumers?$`),
	regexp.MustCompile(`(?i)^should (.+?) move out of (.+?) into (?:a )?(.+?)(?: package)?, or should (.+?) move into (?:a )?(.+?)(?: package)?\?? compare consumers? and dependenc(?:y|ies)(?: direction| directions)?$`),
}

var folderLookupPattern = regexp.MustCompile(`(?i)^where is (?:the )?(.+?) folder$`)

var packageExplainPattern = regexp.MustCompile(`(?i)^describe the (.+?) package$`)

var sourceQualifiedExplainPattern = regexp.MustCompile(`(?i)^explain (?:the )?(.+?) in ((?:[a-z0-9_.-]+/)+[a-z0-9_.-]+\.[a-z0-9_.-]+)$`)

var fileLookupPattern = regexp.MustCompile(`(?i)^where is (?:the )?(.+?) file$`)

var classExplainPattern = regexp.MustCompile(`(?i)^explain (?:the )?(.+?) class$`)

var serviceExplainPattern = regexp.MustCompile(`(?i)^explain (?:the )?(.+?) service$`)

var capabilityQuestionPattern = regexp.MustCompile(`(?i)^(?:does|can) (?:this |the )?workspace\b.+$`)

func rule(pattern string, intent Intent, operator ExecutionOperator, direction storage.TraversalDirection, roles []string, captureIndexes []int, relations []graph.RelationKind) questionRule {
	if captureIndexes == nil {
		captureIndexes = make([]int, len(roles))
		for index := range roles {
			captureIndexes[index] = index + 1
		}
	}
	maximumDepth := 2
	if operator == OperatorPath {
		maximumDepth = 8
	}
	return questionRule{
		pattern:        regexp.MustCompile(`(?i)` + pattern),
		intent:         intent,
		operator:       operator,
		direction:      direction,
		roles:          roles,
		captureIndexes: captureIndexes,
		relations:      relations,
		maximumDepth:   maximumDepth,
	}
}

func AnalyzeQuestion(question string) QueryPlan {
	tokens := questionTokens(question)
	normalized, ignored := normalizeQuestionTokens(tokens)
	plan := QueryPlan{
		SchemaVersion:    QueryPlanSchemaVersion,
		Question:         question,
		Tokens:           tokens,
		NormalizedTerms:  normalized,
		QuotedPhrases:    quotedPhrases(question),
		IgnoredStopWords: ignored,
		Intent:           IntentUnknown,
		Direction:        storage.TraverseBoth,
		Operator:         OperatorLookup,
		MaxDepth:         2,
		MaxNodes:         100,
		Warnings:         []PlanWarning{},
	}

	normalizedQuestion := strings.TrimRight(strings.TrimSpace(question), "?!. ")
	if matches := folderLookupPattern.FindStringSubmatch(normalizedQuestion); matches != nil {
		plan.Intent = IntentLookup
		plan.Confidence = 1
		plan.EntitySlots = []EntitySlot{entitySlotWithEntityRole("entity", "folder", cleanEntityText(matches[1]))}
		return plan
	}
	if matches := packageExplainPattern.FindStringSubmatch(normalizedQuestion); matches != nil {
		plan.Intent = IntentExplain
		plan.Confidence = 1
		plan.Operator = OperatorExplain
		plan.EntitySlots = []EntitySlot{entitySlotWithEntityRole("entity", "package", cleanEntityText(matches[1]))}
		return plan
	}
	if matches := sourceQualifiedExplainPattern.FindStringSubmatch(normalizedQuestion); matches != nil {
		plan.Intent = IntentExplain
		plan.Confidence = 1
		plan.Operator = OperatorExplain
		plan.EntitySlots = []EntitySlot{entitySlotWithSourcePath("entity", cleanEntityText(matches[1]), cleanEntityText(matches[2]))}
		return plan
	}
	if matches := fileLookupPattern.FindStringSubmatch(normalizedQuestion); matches != nil {
		plan.Intent = IntentLookup
		plan.Confidence = 1
		plan.EntitySlots = []EntitySlot{entitySlotWithEntityRole("entity", "file", cleanEntityText(matches[1]))}
		return plan
	}
	if matches := classExplainPattern.FindStringSubmatch(normalizedQuestion); matches != nil {
		plan.Intent = IntentExplain
		plan.Confidence = 1
		plan.Operator = OperatorExplain
		plan.EntitySlots = []EntitySlot{entitySlotWithEntityRole("entity", "class", cleanEntityText(matches[1]))}
		return plan
	}
	if matches := serviceExplainPattern.FindStringSubmatch(normalizedQuestion); matches != nil {
		plan.Intent = IntentExplain
		plan.Confidence = 1
		plan.Operator = OperatorExplain
		plan.EntitySlots = []EntitySlot{entitySlotWithEntityRole("entity", "service", cleanEntityText(matches[1]))}
		return plan
	}
	if capabilityQuestionPattern.MatchString(normalizedQuestion) {
		plan.Intent = IntentCapability
		plan.Confidence = 1
		return plan
	}
	if matches := matchArchitecturalMoveComparison(normalizedQuestion); matches != nil {
		groups := make([][]string, 0, len(matches)-1)
		concepts := make([]string, 0, len(matches)-1)
		for _, match := range matches[1:] {
			concept := cleanEntityText(match)
			concepts = append(concepts, concept)
			groups = append(groups, lowercaseTokens(questionTokens(concept)))
		}
		relations := []graph.RelationKind{"imports_from", "requires", "depends_on"}
		plan.Intent = IntentDependencyComparison
		plan.Confidence = 1
		plan.Operator = OperatorNeighbors
		plan.Direction = storage.TraverseBoth
		plan.AllowedRelations = relations
		plan.RelationHints = relationHints(relations)
		plan.DirectionHints = []string{string(storage.TraverseBoth)}
		plan.EntitySlots = []EntitySlot{{
			Role: "comparison",
			Text: strings.Join(concepts, ", "),
			Retrieval: RetrievalRequest{
				Text:        strings.Join(concepts, ", "),
				TokenGroups: groups,
				Limit:       10,
			},
		}}
		return plan
	}
	for _, candidate := range questionRules {
		matches := candidate.pattern.FindStringSubmatch(normalizedQuestion)
		if matches == nil {
			continue
		}
		plan.Intent = candidate.intent
		plan.Confidence = 1
		plan.Operator = candidate.operator
		plan.Direction = candidate.direction
		plan.AllowedRelations = append([]graph.RelationKind(nil), candidate.relations...)
		plan.RelationHints = relationHints(candidate.relations)
		plan.DirectionHints = []string{string(candidate.direction)}
		plan.MaxDepth = candidate.maximumDepth
		plan.EntitySlots = make([]EntitySlot, len(candidate.roles))
		for index, role := range candidate.roles {
			plan.EntitySlots[index] = entitySlot(role, cleanEntityText(matches[candidate.captureIndexes[index]]))
		}
		return plan
	}
	plan.Confidence = 0.1
	plan.EntitySlots = []EntitySlot{{
		Role: "entity",
		Text: question,
		Retrieval: RetrievalRequest{
			Text:        question,
			TokenGroups: [][]string{append([]string(nil), plan.NormalizedTerms...)},
			Limit:       10,
		},
	}}
	plan.Warnings = []PlanWarning{{
		Code:        "unknown_intent",
		Message:     "The question grammar is not in the supported intent set.",
		Suggestions: []string{"Use --terms with the normalized terms for literal lookup."},
	}}
	return plan
}

func matchArchitecturalMoveComparison(question string) []string {
	for _, pattern := range architecturalMoveComparisonPatterns {
		if matches := pattern.FindStringSubmatch(question); len(matches) == 6 {
			return matches
		}
	}
	return nil
}

func entitySlot(role, text string) EntitySlot {
	return entitySlotWithEntityRole(role, "", text)
}

func entitySlotWithSourcePath(role, text, sourcePath string) EntitySlot {
	slot := entitySlot(role, text)
	slot.Retrieval.SourcePath = sourcePath
	return slot
}

func entitySlotWithEntityRole(role, entityRole, text string) EntitySlot {
	tokens := questionTokens(text)
	tokenGroups := [][]string{lowercaseTokens(tokens)}
	var kinds []graph.NodeKind
	normalizedText := strings.ToLower(strings.TrimSpace(text))
	switch strings.ToLower(strings.Join(tokens, "")) {
	case "postgres", "postgresql":
		tokenGroups = append(tokenGroups, []string{"pg"})
	}
	if normalizedText == "sqlite" {
		if len(tokenGroups[0]) != 1 || tokenGroups[0][0] != "sqlite" {
			tokenGroups = append(tokenGroups, []string{"sqlite"})
		}
	}
	if strings.Contains(normalizedText, "postgresql adapter") || strings.Contains(normalizedText, "postgres adapter") {
		tokenGroups = [][]string{{"postgres"}, {"pg"}, {"adapter"}}
		kinds = []graph.NodeKind{"typescript:class", "typescript:interface", "typescript:type_alias", "go:type"}
	}
	if normalizedText == "store" {
		tokenGroups = [][]string{{"store"}, {"storage", "driver"}, {"storage"}, {"driver"}}
		kinds = []graph.NodeKind{"typescript:class", "typescript:interface", "typescript:type_alias", "go:type"}
	}
	if (role == "left" || role == "right") && len(kinds) == 0 {
		kinds = []graph.NodeKind{"typescript:class", "typescript:interface", "typescript:type_alias", "go:type"}
	}
	if entityRole == "folder" || entityRole == "file" {
		kinds = []graph.NodeKind{"file"}
	}
	if entityRole == "package" {
		kinds = []graph.NodeKind{"go:package"}
	}
	if entityRole == "class" || entityRole == "service" {
		kinds = []graph.NodeKind{"typescript:class", "javascript:class", "go:type"}
	}
	return EntitySlot{
		Role:       role,
		EntityRole: entityRole,
		Text:       text,
		Retrieval: RetrievalRequest{
			Text:        text,
			TokenGroups: tokenGroups,
			Kinds:       kinds,
			Limit:       10,
		},
	}
}

func cleanEntityText(text string) string {
	return strings.Trim(strings.TrimSpace(text), `"'`)
}

func questionTokens(text string) []string {
	characters := []rune(text)
	tokens := make([]string, 0)
	start := -1
	flush := func(end int) {
		if start >= 0 {
			tokens = append(tokens, string(characters[start:end]))
			start = -1
		}
	}
	for index, character := range characters {
		if !unicode.IsLetter(character) && !unicode.IsDigit(character) {
			flush(index)
			continue
		}
		if start < 0 {
			start = index
			continue
		}
		previous := characters[index-1]
		nextIsLower := index+1 < len(characters) && unicode.IsLower(characters[index+1])
		if unicode.IsUpper(character) && (unicode.IsLower(previous) || unicode.IsDigit(previous) || unicode.IsUpper(previous) && nextIsLower) {
			flush(index)
			start = index
		}
	}
	flush(len(characters))
	return tokens
}

func lowercaseTokens(tokens []string) []string {
	terms := make([]string, len(tokens))
	for index, token := range tokens {
		terms[index] = strings.ToLower(token)
	}
	return terms
}

var questionStopWords = map[string]struct{}{
	"a": {}, "an": {}, "are": {}, "by": {}, "describe": {}, "do": {}, "does": {},
	"explain": {}, "find": {}, "for": {}, "from": {}, "functions": {}, "how": {},
	"is": {}, "me": {}, "modules": {}, "of": {}, "package": {}, "packages": {},
	"show": {}, "the": {}, "to": {}, "what": {}, "where": {}, "which": {}, "who": {},
}

var controlledSynonyms = map[string]string{
	"affected": "impact",
	"caller":   "called_by",
	"callers":  "called_by",
	"common":   "shared",
	"depends":  "dependencies",
	"flow":     "path",
	"flows":    "path",
	"import":   "dependencies",
	"imports":  "dependencies",
	"invoke":   "calls",
	"invokes":  "calls",
	"reaches":  "path",
	"required": "dependencies",
	"uses":     "dependencies",
}

func normalizeQuestionTokens(tokens []string) ([]string, []string) {
	terms := make([]string, 0, len(tokens))
	ignored := make([]string, 0)
	for _, token := range tokens {
		normalized := strings.ToLower(token)
		if _, stopWord := questionStopWords[normalized]; stopWord {
			ignored = append(ignored, normalized)
			continue
		}
		if synonym, found := controlledSynonyms[normalized]; found {
			normalized = synonym
		}
		terms = append(terms, normalized)
	}
	if len(ignored) == 0 {
		ignored = nil
	}
	return terms, ignored
}

func relationHints(relations []graph.RelationKind) []string {
	if len(relations) == 0 {
		return nil
	}
	hints := make([]string, len(relations))
	for index, relation := range relations {
		hints[index] = string(relation)
	}
	return hints
}

var quotedPhrasePattern = regexp.MustCompile(`"([^"]+)"`)

func quotedPhrases(question string) []string {
	matches := quotedPhrasePattern.FindAllStringSubmatch(question, -1)
	if len(matches) == 0 {
		return nil
	}
	phrases := make([]string, len(matches))
	for index, match := range matches {
		phrases[index] = match[1]
	}
	return phrases
}
