package query

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"agent-wayfinder/query"
	"agent-wayfinder/testkit"

	"github.com/spf13/cobra"
)

func TestPlanQueryUsesLocalPlannerBeforeRemoteFallback(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		".agent-wayfinder/config.json": `{"planning":{"ollama":{"enabled":true},"copilot":{"enabled":true}}}`,
	})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte(`{"message":{"content":"{\"schemaVersion\":1,\"intent\":\"called_by\",\"entities\":[\"helper\"],\"confidence\":\"high\"}"}}`))
	}))
	defer server.Close()
	t.Setenv("OLLAMA_HOST", server.URL)
	installFailingCopilotPlanner(t)

	command := &cobra.Command{}
	Configure(command)
	result, err := planQuery(command, workspace.Root, &query.QueryPlan{Question: "Which code invokes helper?", Intent: query.IntentUnknown}, 2, 100, nil)
	if err != nil {
		t.Fatalf("plan query: %v", err)
	}
	if result.Metadata.Method != "ollama" || result.Plan.Intent != query.IntentCalledBy || result.CopilotMetric != nil {
		t.Errorf("planner result = %+v, want local called-by plan without a Copilot attempt", result)
	}
}
