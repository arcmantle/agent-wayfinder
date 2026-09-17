# agent-wayfinder

`agent-wayfinder` is a local-first, continuously updated code graph for AI agents.

## Install

Install the released command-line tool from npm:

```bash
npm install --global agent-wayfinder
agent-wayfinder install
agent-wayfinder --help
a-wayfinder --help
```

`@arcmantle/agent-wayfinder` is also published as the canonical scoped package.

The npm package downloads a checksum-verified native binary for Windows x64,
Linux x64, and macOS arm64. It requires Node.js 18 or later during installation.

## Agent Skill

The project includes a cross-framework skill at
[cmd/agent-wayfinder/skill_assets/SKILL.md](cmd/agent-wayfinder/skill_assets/SKILL.md).
Run `agent-wayfinder install --project` to install the bundled copy in another
project instead of the user-level default.
Use `/agent-wayfinder` to index a workspace, query code relationships, trace a
path, or explain a graph node. The skill queries the local graph first and then
requires source inspection and focused validation before code changes.

## MCP Server

Start the read-only Model Context Protocol server over standard input and output:

```bash
agent-wayfinder mcp
```

The server provides `query`, `path`, `explain`, and `export` tools. Each tool
returns the same graph version, publication time, result data, and errors as its
JSON CLI command.

The VS Code provider in
[npm/vscode-mcp-provider](npm/vscode-mcp-provider) registers this server as
`Agent Wayfinder`. It runs `agent-wayfinder mcp` in the first workspace folder
by default. Use `agentWayfinder.mcp.command` and `agentWayfinder.mcp.args` to
override the executable or arguments.

## Publish A Release

The release workflow runs when a signed-off version tag is pushed. It builds
native binaries, creates a GitHub release and checksum manifest, then publishes
the npm package. Before the first release, add an npm automation token with
publish access for both package names as the organization `NPM_PUBLISH` secret.

Prospector calculates the package version from the release tag:

```bash
git tag vVERSION
git push origin vVERSION
```

It is intended to work beside `agent-issues`:

- `agent-issues` records work state, plans, dependencies, and decisions.
- `agent-wayfinder` records code structure, relationships, change impact, and architecture context.

The goal is to make broad codebase questions fast and evidence-based without replacing source inspection, language-server navigation, tests, or Git.

## Why This Exists

Graphify demonstrates the value of a code graph for AI workflows, but its JSON-first design is not ideal for a continuously updated monorepo index:

- Queries load a full graph JSON document and scan nodes before traversal.
- Graph outputs are useful interchange artifacts but weak primary storage.
- Per-package graphs require separate updates and a merge step.
- Reports and visualization do not need to update on every edit.

`agent-wayfinder` will use Graphify as a behavioral reference while taking a database-first, Go-native approach to indexing and updates.

## Product Principles

- Local-first SQLite storage, with optional deployed PostgreSQL.
- One storage contract and migration model for SQLite and PostgreSQL.
- `graph.json` is an export and snapshot format, not the query store.
- Source code remains authoritative. The graph is retrieval and planning evidence.
- Every extracted fact records source path, source location, content hash, extractor version, timestamp, and confidence.
- Exact lookup stays with LSP and text search. The graph serves architecture, dependency, impact, and data-flow questions.
- Generated reports and HTML are delayed or explicit work, not part of the edit hot path.

## Core Design

### Extractor Packages

Language extractors are statically linked, isolated Go packages. Each package
under `extractors/<language>` owns one language's parser integration, file
matching, version, and local-fact mapping. `extractors/registry` is the single
composition point that imports and registers those packages. Extractors depend
on shared contracts only; they do not depend on each other or on the registry.

v0 uses the Go Tree-sitter runtime with JavaScript and TypeScript grammar
bindings. Local checks and CI require a C compiler and run with `CGO_ENABLED=1`.

### Storage

The initial relational model should include:

```text
projects
files
file_versions
nodes
edges
extractions
graph_snapshots
query_results
```

Required indexes include node labels, file references, and both edge endpoints. SQLite FTS5 will support local node lookup. PostgreSQL full-text search can provide the deployed equivalent. Vector search is optional and should not be required for the first version.

### Continuous Indexing

A long-running Go service watches project files, normalizes events, and debounces bursts from editors.

```text
filesystem event
  -> normalize and debounce paths
  -> identify added, changed, and deleted files
  -> extract affected files
  -> transactionally replace file nodes and edges
  -> publish a new queryable graph version
```

Use a periodic reconciliation scan to recover from missed or reordered filesystem events.

Target timings:

- File extraction and graph update: debounced at about 250-1000 ms.
- Clustering, metrics, reports, and HTML: after 30-60 seconds of inactivity, or by explicit command.

### Querying

Queries should use indexed node lookup followed by bounded graph traversal:

```text
architecture question
  -> deterministic query plan
  -> exact and full-text node search
  -> ranked start nodes by entity role
  -> bounded graph execution
  -> answer-ready evidence with source spans, scores, limits, and warnings
```

Pass one quoted sentence to question mode. JSON output includes the interpreted
plan, confidence, ranked evidence, stage limits, warnings, suggestions, graph
version, and publication time:

```bash
agent-wayfinder query . "What is the shared contract between postgres and sqlite?" --format json
```

One sentence enters question mode automatically. Use `--question` to require
question mode or `--show-plan` to show the plan in text output. Low-confidence
and weak results include candidates and follow-up commands instead of an answer
claim.

### Copilot Configuration

Copilot settings are non-secret. The query command reads them from a workspace
root `.wayfinder` JSON file and from environment variables. A later CLI slice
adds the flags and enables Copilot planning.

```json
{
  "copilot": {
    "enabled": true,
    "model": "auto",
    "maxAiCredits": 1,
    "tokenBudget": 4096,
    "timeout": "30s"
  }
}
```

Use `WAYFINDER_COPILOT_ENABLED`, `WAYFINDER_COPILOT_MODEL`,
`WAYFINDER_COPILOT_MAX_AI_CREDITS`, `WAYFINDER_COPILOT_TOKEN_BUDGET`, and
`WAYFINDER_COPILOT_TIMEOUT` to override file settings. Defaults are `false`,
`auto`, `1`, `4096`, and `30s`. Timeout accepts a Go duration or integer
seconds. Credit and token values must be positive integers. Timeout must be
greater than zero and no more than 30 seconds. Unknown settings inside
`copilot` and invalid values return an error. Unrelated top-level `.wayfinder`
settings remain valid.

Pass separate terms for legacy literal lookup. Use `--terms` when one literal
term contains spaces or punctuation:

```bash
agent-wayfinder query . AuthService token src/auth --format json
agent-wayfinder query . "Auth Service" --terms --format json
```

Use `path` when both endpoints are known. Directed traversal is the default;
`--undirected` is only an explicit structural fallback:

```bash
agent-wayfinder path . AuthService TokenStore --format json
```

Use `explain` for one exact or unambiguous node. If the result is ambiguous,
rerun it with one returned node ID:

```bash
agent-wayfinder explain . AuthService --format json
```

## Relationship To AI Workflows

An AI agent should run deterministic question mode before it plans exact
follow-up queries. It must read the plan, confidence, warnings, graph version,
limits, truncation, and ranked evidence. It must separate graph-supported facts
from inference and inspect current source before a technical claim. Tests and
Git remain the validation sources for code changes.

The graph can be stale between index updates. Query results must identify the graph version and source evidence.

## Reference Implementation

The `reference/` directory contains a shallow clone of Graphify at the time this project was created. It is a behavioral oracle, not a code template.

Use it to create golden fixtures and compare:

- node and edge identities
- source locations and confidence metadata
- extraction output
- incremental replacement and deletion behavior
- query, path, and explain results

Do not port Python files one-for-one. Port externally visible behavior through small, tested vertical slices.

## Initial Delivery Plan

1. Define the Go module, storage interface, SQLite implementation, and schema migrations.
2. Extract TypeScript and JavaScript fixtures into stable nodes and edges.
3. Add indexed `query`, `path`, and `explain` commands.
4. Replace changed-file graph facts and remove deleted-file graph facts transactionally.
5. Add a workspace-level index so one update covers all configured packages.
6. Add a file watcher with debounce, queueing, and reconciliation.
7. Add delayed clustering, reports, HTML, and JSON exports.
8. Add optional semantic extraction and PostgreSQL support.

## Out Of Scope For The First Version

- Image, video, and audio extraction.
- Required cloud LLM access.
- Vector search as the main retrieval method.
- Instant report regeneration on every file edit.
- Full export parity before the core index and update loop is reliable.

## Validation Strategy

The Go implementation should use the Python reference as a behavioral oracle. For each fixture, compare normalized graph JSON and query results. Test SQLite by default and run the same storage contract tests against PostgreSQL in CI.

## Release Validation

Run the supported v0 release gate with:

```bash
make acceptance
```

This command runs the storage conformance, extraction, indexing, query, path, explain, export, lifecycle, fixture, and Graphify comparison checks.

## ClientFlex Acceptance

The ClientFlex acceptance test is opt-in because the corpus is private and external. It uses a temporary SQLite database and does not write graph artifacts to the corpus.

```bash
AGENT_WAYFINDER_CLIENTFLEX_ROOT=/path/to/ClientFlex CGO_ENABLED=1 go test -tags sqlite_fts5 ./acceptance -run '^TestClientFlexAcceptanceIndexesIntoTemporaryDatabase$' -v
```
