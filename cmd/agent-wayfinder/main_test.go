package main

import (
	"strings"
	"testing"
)

func TestCommandRuns(t *testing.T) {
	standardOutput := &strings.Builder{}
	standardError := &strings.Builder{}
	if exitCode := run(nil, standardOutput, standardError); exitCode != 0 {
		t.Fatalf("run command: exit code %d, error %s", exitCode, standardError.String())
	}
}

func TestCommandHelpListsPublicCommands(t *testing.T) {
	standardOutput := &strings.Builder{}
	standardError := &strings.Builder{}

	if exitCode := run([]string{"--help"}, standardOutput, standardError); exitCode != 0 {
		t.Fatalf("run help command: exit code %d, error %s", exitCode, standardError.String())
	}

	output := standardOutput.String()
	for _, command := range []string{"install", "index", "query", "path", "explain", "export", "indexer", "benchmark", "mcp"} {
		if !strings.Contains(output, command) {
			t.Errorf("help output = %q, want public command %q", output, command)
		}
	}
}

func TestCommandRejectsUnsupportedOutputFormat(t *testing.T) {
	standardOutput := &strings.Builder{}
	standardError := &strings.Builder{}

	if exitCode := run([]string{"--format", "yaml"}, standardOutput, standardError); exitCode != 2 {
		t.Errorf("exit code = %d, want 2", exitCode)
	}
}
