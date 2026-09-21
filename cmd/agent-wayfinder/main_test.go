package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestCommandRuns(t *testing.T) {
	command := exec.Command("go", "run", ".")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run command: %v\n%s", err, output)
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
