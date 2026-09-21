# Agent Wayfinder Command Reference

Load this reference when you need exact CLI syntax or flags.

## Install And Launch

Install the released CLI with Node.js 18 or later:

```bash
npm install --global agent-wayfinder
agent-wayfinder install
agent-wayfinder --help
a-wayfinder --help
```

The canonical scoped npm package is `@arcmantle/agent-wayfinder`.

`agent-wayfinder install` writes the skill to
`~/.agents/skills/agent-wayfinder`. Use `agent-wayfinder install --project` to
write it to `./.agents/skills/agent-wayfinder` in the current project. Running
the command again updates the installed skill.

In the Agent Wayfinder source repository, this command runs the current source:

```bash
go run ./cmd/agent-wayfinder --help
```

Use one launcher consistently in a command sequence.

## MCP

When an Agent Wayfinder MCP server is available, use its `query`, `path`,
`explain`, and `export` tools instead of the matching CLI commands. Tool input
uses the same workspace, database, traversal limit, project, and relation
values described below. Tool results use the same JSON result envelope.

The MCP server is read-only. Use the CLI for `install`, `index`, and `indexer`.
To start the stdio server directly, run:

```bash
agent-wayfinder mcp
```

## Database

The default database is:

```text
<WORKSPACE>/.agent-wayfinder/graph.db
```

Commands resolve `WORKSPACE` and `--database` to absolute paths. Use the same workspace and database path for indexing and queries.

## Index

Build or refresh the published graph:

```bash
agent-wayfinder index WORKSPACE [--database PATH] [--format text|json]
```

`index` creates the default database directory when needed. JSON output includes the graph version, publication time, workspace, and extraction diagnostics.

## Query

Question mode interprets one architecture question and returns a deterministic query plan with ranked evidence:

```bash
agent-wayfinder query WORKSPACE QUESTION \
  [--question] \
  [--show-plan] \
  [--database PATH] \
  [--format text|json] \
  [--max-depth 2] \
  [--max-nodes 100] \
  [--project PROJECT_ID]... \
  [--relation RELATION]...
```

Quote a question so that the shell passes it as one argument. One argument with sentence signals enters question mode automatically. Use `--question` to require question mode. Question-mode JSON includes `schemaVersion`, `interpretation`, `evidence`, `limits`, `warnings`, and `suggestions` in the result envelope. Use `--show-plan` to include the interpreted plan in text output.

When confidence is low or retrieval is weak, inspect the ranked candidates and follow the returned suggestions. Use exact identifiers, separate terms, `path`, or `explain` for the next check. Do not treat weak evidence as an answer claim.

Explicit TERM mode keeps literal lookup behavior:

```bash
agent-wayfinder query WORKSPACE TERM... \
  [--terms] \
  [--database PATH] \
  [--format text|json] \
  [--max-depth 2] \
  [--max-nodes 100] \
  [--project PROJECT_ID]... \
  [--relation RELATION]...
```

Pass literal terms separately. Multiple terms enter TERM mode automatically. Use `--terms` when one literal term contains sentence signals. Exact lookup keeps precedence, and each term can select up to three seed nodes.

`--question` and `--terms` cannot be used together. Use `--project` and `--relation` only with IDs or relation names found in graph output. Repeating either filter adds allowed values.

## Path

Find a shortest directed path between two node queries:

```bash
agent-wayfinder path WORKSPACE SOURCE TARGET \
  [--database PATH] \
  [--format text|json] \
  [--max-depth 8] \
  [--max-nodes 100] \
  [--project PROJECT_ID]... \
  [--relation RELATION]... \
  [--undirected]
```

The source and target are each one argument. Quote names that contain spaces. Use `--undirected` only after the directed path is absent and an undirected structural connection is useful.

## Explain

Explain one exact or unambiguous node and its relationships:

```bash
agent-wayfinder explain WORKSPACE NODE [--database PATH] [--format text|json]
```

When lookup is ambiguous, the result contains up to three candidates and a remaining-candidate count. Rerun with an exact candidate ID.

## Export

Export all nodes and edges from the published snapshot:

```bash
agent-wayfinder export WORKSPACE [--database PATH] [--format text|json]
```

Use export for interoperability or full-graph inspection. Prefer bounded commands for normal agent retrieval.

## Indexer Process

Control the workspace indexer process:

```bash
agent-wayfinder indexer serve WORKSPACE [--format text|json]
agent-wayfinder indexer start WORKSPACE [--format text|json]
agent-wayfinder indexer status WORKSPACE [--format text|json]
agent-wayfinder indexer stop WORKSPACE [--format text|json]
```

`serve` runs the process in the foreground. `start` launches the same process in the background. The current CLI wiring reports lifecycle status, but it does not connect a file watcher, reconciler, or graph publisher. It does not replace `index` for graph creation or refresh. The process exits after five minutes without a status or stop request.

## Output And Errors

Text results start with the graph version and UTC publication time. JSON results use this envelope:

```json
{
  "graphVersion": 1,
  "publishedAt": "2026-08-31T00:00:00Z",
  "result": {}
}
```

An invalid argument exits with code `2`. Other failures, including an unavailable published graph, exit with code `1` and write an `error:` line to standard error.