package testkit

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRunGraphifyComparisonCorpusComparesCLIAndExportedFacts(t *testing.T) {
	workspace := NewWorkspace(t, map[string]string{
		"package.json":  `{"name":"comparison-fixture"}`,
		"src/helper.ts": "export function helper() { return 1; }\n",
		"src/main.ts":   "import { helper } from './helper'; export function main() { return helper(); }\n",
	})
	database := filepath.Join(t.TempDir(), "graph.db")

	command := exec.Command("go", "run", "-tags", "sqlite_fts5", "../cmd/agent-wayfinder", "index", "--database", database, "--format", "json", workspace.Root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("index comparison corpus workspace: %v\n%s", err, output)
	}

	command = exec.Command("go", "run", "-tags", "sqlite_fts5", "../cmd/agent-wayfinder", "export", "--database", database, "--format", "json", workspace.Root)
	candidate, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("export comparison corpus workspace: %v\n%s", err, candidate)
	}

	if err := RunGraphifyComparisonCorpus("typescript-import", candidate); err != nil {
		t.Fatal(err)
	}
}

func TestRunGraphifyComparisonCorpusComparesIndexDiagnostics(t *testing.T) {
	workspace := NewWorkspace(t, map[string]string{
		"package.json": `{"name":"comparison-fixture"}`,
		"src/main.ts":  "const support = import(moduleName);\n",
	})
	database := filepath.Join(t.TempDir(), "graph.db")

	command := exec.Command("go", "run", "-tags", "sqlite_fts5", "../cmd/agent-wayfinder", "index", "--database", database, "--format", "json", workspace.Root)
	candidate, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("index diagnostic comparison corpus workspace: %v\n%s", err, candidate)
	}

	if err := RunGraphifyIndexComparisonCorpus("typescript-diagnostic", candidate); err != nil {
		t.Fatal(err)
	}
}
