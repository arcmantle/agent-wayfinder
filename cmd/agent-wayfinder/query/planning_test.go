package query

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"agent-wayfinder/query"
	"agent-wayfinder/storage"
	"agent-wayfinder/testkit"

	"github.com/spf13/cobra"
)

type settleRecordingSpendStore struct {
	settledReservationID int64
	settledAmount        float64
	settleError          error
}

func (store *settleRecordingSpendStore) ReserveSpend(context.Context, storage.SpendReservationRequest) (storage.SpendReservation, error) {
	return storage.SpendReservation{ID: 1, Amount: 30}, nil
}

func (store *settleRecordingSpendStore) SettleSpend(_ context.Context, reservationID int64, amount float64) error {
	store.settledReservationID = reservationID
	store.settledAmount = amount
	return store.settleError
}

func TestPlanQueryUsesLocalPlannerBeforeRemoteFallback(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"ollama","ollama":{}}}`,
	})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte(`{"message":{"content":"{\"schemaVersion\":1,\"intent\":\"called_by\",\"entities\":[\"helper\"],\"confidence\":\"high\"}"}}`))
	}))
	defer server.Close()
	t.Setenv("OLLAMA_HOST", server.URL)
	installFailingCopilotPlanner(t)

	command := &cobra.Command{}
	Configure(command)
	result, err := planQuery(command, workspace.Root, nil, &query.QueryPlan{Question: "Which code invokes helper?", Intent: query.IntentUnknown}, 2, 100, nil)
	if err != nil {
		t.Fatalf("plan query: %v", err)
	}
	if result.Metadata.Method != "ollama" || result.Plan.Intent != query.IntentCalledBy || result.CopilotMetric != nil {
		t.Errorf("planner result = %+v, want local called-by plan without a Copilot attempt", result)
	}
}

func TestPlanQuerySettlesExactCopilotCredits(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"copilot","copilot":{}},"spending":{"copilot":{"dailyAiCredits":30}}}`,
	})
	installCopilotPlannerWithUsage(t, `{"schemaVersion":1,"intent":"called_by","entities":["helper"]}`, 1)
	command := &cobra.Command{}
	Configure(command)
	store := &settleRecordingSpendStore{}

	result, err := planQuery(command, workspace.Root, store, &query.QueryPlan{Question: "Which code invokes helper?", Intent: query.IntentUnknown}, 2, 100, nil)
	if err != nil {
		t.Fatalf("plan query: %v", err)
	}
	if result.Metadata.Method != "copilot" || store.settledReservationID != 1 || store.settledAmount != 1 {
		t.Errorf("planner result = %+v, settlement = %+v; want Copilot plan and one-credit settlement", result, store)
	}
}

func TestPlanQuerySettlesExactClaudeCost(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"claude","claude":{}},"spending":{"claude":{"dailyUsd":30}}}`,
	})
	installClaudePlannerWithExactCost(t)
	command := &cobra.Command{}
	Configure(command)
	store := &settleRecordingSpendStore{}

	result, err := planQuery(command, workspace.Root, store, &query.QueryPlan{Question: "Which code invokes helper?", Intent: query.IntentUnknown}, 2, 100, nil)
	if err != nil {
		t.Fatalf("plan query: %v", err)
	}
	if result.Metadata.Method != "claude" || store.settledReservationID != 1 || store.settledAmount != 1.5 {
		t.Errorf("planner result = %+v, settlement = %+v; want Claude plan and $1.50 settlement", result, store)
	}
}

func TestPlanQueryRetainsCopilotReservationWithoutExactUsage(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"copilot","copilot":{}},"spending":{"copilot":{"dailyAiCredits":30}}}`,
	})
	installCopilotPlanner(t, `{"schemaVersion":1,"intent":"called_by","entities":["helper"]}`)
	command := &cobra.Command{}
	Configure(command)
	store := &settleRecordingSpendStore{}

	result, err := planQuery(command, workspace.Root, store, &query.QueryPlan{Question: "Which code invokes helper?", Intent: query.IntentUnknown}, 2, 100, nil)
	if err != nil {
		t.Fatalf("plan query: %v", err)
	}
	if result.Metadata.Method != "copilot" || store.settledReservationID != 0 {
		t.Errorf("planner result = %+v, settlement = %+v; want Copilot plan without settlement", result, store)
	}
}

func TestPlanQueryRetainsCopilotReservationAfterProviderFailure(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"copilot","copilot":{}},"spending":{"copilot":{"dailyAiCredits":30}}}`,
	})
	installFailingCopilotPlanner(t)
	command := &cobra.Command{}
	Configure(command)
	store := &settleRecordingSpendStore{}

	result, err := planQuery(command, workspace.Root, store, &query.QueryPlan{Question: "Which code invokes helper?", Intent: query.IntentUnknown}, 2, 100, nil)
	if err != nil {
		t.Fatalf("plan query: %v", err)
	}
	if result.CopilotMetadata.Available || store.settledReservationID != 0 {
		t.Errorf("planner result = %+v, settlement = %+v; want unavailable Copilot without settlement", result, store)
	}
}

func TestPlanQueryReturnsSettlementFailure(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"provider":"copilot","copilot":{}},"spending":{"copilot":{"dailyAiCredits":30}}}`,
	})
	installCopilotPlannerWithUsage(t, `{"schemaVersion":1,"intent":"called_by","entities":["helper"]}`, 1)
	command := &cobra.Command{}
	Configure(command)
	settlementFailure := errors.New("settlement unavailable")
	store := &settleRecordingSpendStore{settleError: settlementFailure}

	_, err := planQuery(command, workspace.Root, store, &query.QueryPlan{Question: "Which code invokes helper?", Intent: query.IntentUnknown}, 2, 100, nil)
	if !errors.Is(err, settlementFailure) {
		t.Errorf("plan query error = %v, want settlement failure", err)
	}
}

func installClaudePlannerWithExactCost(t *testing.T) {
	t.Helper()
	plannerDirectory := t.TempDir()
	plannerPath := filepath.Join(plannerDirectory, "claude")
	plannerScript := `#!/bin/sh
printf '%s\n' '{"result":"{\"schemaVersion\":1,\"intent\":\"called_by\",\"entities\":[\"helper\"]}","total_cost_usd":1.5}'
`
	if err := os.WriteFile(plannerPath, []byte(plannerScript), 0o755); err != nil {
		t.Fatalf("write Claude planner: %v", err)
	}
	t.Setenv("PATH", plannerDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
}
