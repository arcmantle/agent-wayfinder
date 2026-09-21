package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"agent-wayfinder/cli"
	"agent-wayfinder/cmd/agent-wayfinder/catalog"
	"agent-wayfinder/cmd/agent-wayfinder/index"
	cmd "agent-wayfinder/cmd/agent-wayfinder/internal/command"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/spf13/cobra"

	"agent-wayfinder/testkit"
)

func runTestCommand(arguments []string, standardOutput, standardError io.Writer) int {
	if len(arguments) == 0 {
		return cmd.WriteError(standardError, cli.NewInvalidArgumentError("expected test command"))
	}
	exitCode := 0
	var command *cobra.Command
	switch arguments[0] {
	case "index":
		command = index.New(standardOutput, standardError, &exitCode)
	case "catalog":
		command = catalog.New(standardOutput, standardError, &exitCode)
	default:
		return runCommand(arguments, standardOutput, standardError)
	}
	command.SetOut(standardOutput)
	command.SetErr(standardError)
	command.SetArgs(arguments[1:])
	if err := command.Execute(); err != nil {
		return cmd.WriteError(standardError, cli.NewInvalidArgumentError(err.Error()))
	}
	return exitCode
}

func TestMCPServerListsToolsAndRunsQuery(t *testing.T) {
	var receivedCalls [][]string
	runCommand := func(arguments []string, standardOutput, _ io.Writer) int {
		receivedCalls = append(receivedCalls, append([]string(nil), arguments...))
		fmt.Fprintln(standardOutput, `{"graphVersion":7,"publishedAt":"2026-08-31T12:00:00Z","result":{"nodes":[],"catalog":{"matches":[]}}}`)
		return 0
	}
	mcpClient, err := client.NewInProcessClient(newMCPServer(runCommand))
	if err != nil {
		t.Fatalf("create MCP client: %v", err)
	}
	t.Cleanup(func() { _ = mcpClient.Close() })
	ctx := context.Background()
	if err := mcpClient.Start(ctx); err != nil {
		t.Fatalf("start MCP client: %v", err)
	}
	if _, err := mcpClient.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		ClientInfo:      mcp.Implementation{Name: "agent-wayfinder-test", Version: "1.0.0"},
	}}); err != nil {
		t.Fatalf("initialize MCP client: %v", err)
	}

	tools, err := mcpClient.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("list MCP tools: %v", err)
	}
	toolNames := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		toolNames = append(toolNames, tool.Name)
	}
	sort.Strings(toolNames)
	if want := []string{"explain", "export", "path", "query"}; !reflect.DeepEqual(toolNames, want) {
		t.Errorf("MCP tools = %v, want %v", toolNames, want)
	}

	result, err := mcpClient.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "query",
		Arguments: map[string]any{
			"workspace": "/workspace",
			"terms":     []string{"AuthService", "TokenStore"},
			"maxDepth":  3,
			"projects":  []string{"api"},
			"relations": []string{"calls"},
		},
	}})
	if err != nil {
		t.Fatalf("call query tool: %v", err)
	}
	if result.IsError {
		t.Fatalf("query tool returned an error: %+v", result.Content)
	}
	wantArguments := []string{"query", "--format", "json", "--max-depth", "3", "--max-nodes", "100", "--project", "api", "--relation", "calls", "/workspace", "AuthService", "TokenStore"}
	if !reflect.DeepEqual(receivedCalls[0], wantArguments) {
		t.Errorf("query command arguments = %v, want %v", receivedCalls[0], wantArguments)
	}
	structuredContent, ok := result.StructuredContent.(map[string]any)
	if !ok || structuredContent["graphVersion"] != float64(7) {
		t.Errorf("structured content = %#v, want graph version 7", result.StructuredContent)
	}
	resultContent, resultFound := structuredContent["result"].(map[string]any)
	if !resultFound || resultContent["catalog"] == nil {
		t.Errorf("structured content = %#v, want catalog query evidence", result.StructuredContent)
	}

	capabilityResult, err := mcpClient.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "query",
		Arguments: map[string]any{
			"workspace": "/workspace",
			"terms":     []string{"Does this workspace validate access tokens?"},
		},
	}})
	if err != nil {
		t.Fatalf("call capability query tool: %v", err)
	}
	if capabilityResult.IsError {
		t.Fatalf("capability query tool returned an error: %+v", capabilityResult.Content)
	}
	if want := []string{"query", "--format", "json", "--max-depth", "2", "--max-nodes", "100", "/workspace", "Does this workspace validate access tokens?"}; !reflect.DeepEqual(receivedCalls[1], want) {
		t.Errorf("capability query arguments = %v, want %v", receivedCalls[1], want)
	}

	toolCalls := []struct {
		name      string
		arguments map[string]any
		want      []string
	}{
		{
			name:      "path",
			arguments: map[string]any{"workspace": "/workspace", "source": "API", "target": "Store", "undirected": true, "maxNodes": 20, "database": "/tmp/graph.db"},
			want:      []string{"path", "--format", "json", "--max-depth", "8", "--max-nodes", "20", "--undirected", "--database", "/tmp/graph.db", "/workspace", "API", "Store"},
		},
		{
			name:      "explain",
			arguments: map[string]any{"workspace": "/workspace", "node": "API"},
			want:      []string{"explain", "--format", "json", "/workspace", "API"},
		},
		{
			name:      "export",
			arguments: map[string]any{"workspace": "/workspace"},
			want:      []string{"export", "--format", "json", "/workspace"},
		},
	}
	for callIndex, toolCall := range toolCalls {
		result, err := mcpClient.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: toolCall.name, Arguments: toolCall.arguments}})
		if err != nil {
			t.Fatalf("call %s tool: %v", toolCall.name, err)
		}
		if result.IsError {
			t.Errorf("%s tool returned an error: %+v", toolCall.name, result.Content)
		}
		if got := receivedCalls[callIndex+2]; !reflect.DeepEqual(got, toolCall.want) {
			t.Errorf("%s command arguments = %v, want %v", toolCall.name, got, toolCall.want)
		}
	}
}

func TestMCPToolReturnsCommandFailureAsToolError(t *testing.T) {
	handler := commandToolHandler(func(_ []string, _, standardError io.Writer) int {
		fmt.Fprintln(standardError, "error: no published graph")
		return 1
	}, exportMCPArguments)

	result, err := handler(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{
		Arguments: map[string]any{"workspace": "/workspace"},
	}})
	if err != nil {
		t.Fatalf("run MCP tool handler: %v", err)
	}
	if !result.IsError {
		t.Fatalf("tool result = %+v, want command failure", result)
	}
}

func TestMCPQueryReturnsGeneratorLabeledCatalogEvidence(t *testing.T) {
	workspace := testkit.NewWorkspace(t, map[string]string{
		"package.json": `{"name":"fixture"}`,
		"src/token.ts": "export function validateAccessToken(token: string) { return token.length > 0; }",
	})
	database := filepath.Join(t.TempDir(), "state", "graph.db")
	standardOutput := &bytes.Buffer{}
	standardError := &bytes.Buffer{}
	if exitCode := runTestCommand([]string{"index", "--database", database, workspace.Root}, standardOutput, standardError); exitCode != 0 {
		t.Fatalf("run index command: exit code %d, error %s", exitCode, standardError.String())
	}
	if exitCode := runTestCommand([]string{"catalog", "--database", database, workspace.Root}, standardOutput, standardError); exitCode != 0 {
		t.Fatalf("run catalog command: exit code %d, error %s", exitCode, standardError.String())
	}

	mcpClient, err := client.NewInProcessClient(newMCPServer(runTestCommand))
	if err != nil {
		t.Fatalf("create MCP client: %v", err)
	}
	t.Cleanup(func() { _ = mcpClient.Close() })
	ctx := context.Background()
	if err := mcpClient.Start(ctx); err != nil {
		t.Fatalf("start MCP client: %v", err)
	}
	if _, err := mcpClient.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		ClientInfo:      mcp.Implementation{Name: "agent-wayfinder-test", Version: "1.0.0"},
	}}); err != nil {
		t.Fatalf("initialize MCP client: %v", err)
	}

	result, err := mcpClient.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "query",
		Arguments: map[string]any{
			"workspace": workspace.Root,
			"database":  database,
			"terms":     []string{"Does this workspace validate access tokens?"},
		},
	}})
	if err != nil {
		t.Fatalf("call query tool: %v", err)
	}
	if result.IsError {
		t.Fatalf("query tool result = %+v, want catalog evidence", result.Content)
	}
	structuredContent, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content = %#v, want JSON object", result.StructuredContent)
	}
	resultContent, ok := structuredContent["result"].(map[string]any)
	if !ok {
		t.Fatalf("structured result = %#v, want JSON object", structuredContent)
	}
	catalog, ok := resultContent["catalog"].(map[string]any)
	if !ok {
		t.Fatalf("catalog result = %#v, want JSON object", resultContent["catalog"])
	}
	matches, ok := catalog["matches"].([]any)
	if !ok || len(matches) == 0 {
		t.Fatalf("catalog matches = %#v, want one catalog match", catalog["matches"])
	}
	match, ok := matches[0].(map[string]any)
	if !ok {
		t.Fatalf("catalog match = %#v, want JSON object", matches[0])
	}
	evidence, ok := match["evidence"].([]any)
	if !ok || len(evidence) != 1 {
		t.Fatalf("catalog evidence = %#v, want one deterministic synopsis", match["evidence"])
	}
	synopsis, ok := evidence[0].(map[string]any)
	if !ok || synopsis["generator"] != "deterministic" || synopsis["synopsis"] == "" {
		t.Errorf("catalog synopsis = %#v, want labeled deterministic text", evidence[0])
	}
}
