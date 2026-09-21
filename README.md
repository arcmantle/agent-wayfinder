# agent-wayfinder

`agent-wayfinder` is a local-first, continuously updated code graph for AI agents.

## Install

Install the released command-line tool from npm:

```bash
npm install --global agent-wayfinder
wayfind install
wayfind --help
awf --help
```

`@arcmantle/agent-wayfinder` is also published as the canonical scoped package.
`wayfind` is the preferred command. `awf`, `agent-wayfinder`, and `a-wayfinder`
remain available as command aliases.

The npm package downloads a checksum-verified native binary for Windows x64,
Linux x64, and macOS arm64. It requires Node.js 18 or later during installation.

## Local Development

The local SQLite catalog includes FTS5 and sqlite-vec by default. Use the
Makefile targets for development commands:

```bash
make build
make test
make run ARGS='query . "where is the query.go file" --format json'
```

When a direct Go command is necessary, use:

```bash
CGO_ENABLED=1 go run ./cmd/agent-wayfinder query . "where is the query.go file" --format json
```

## Catalog Configuration

Catalog configuration is in the indexed workspace's
`.agent-wayfinder/config.json` file. It controls embeddings and optional
catalog synopses. It also controls question planning through its `planning`
object. The file is the only workspace configuration file for these features.

### Embeddings

Catalog embeddings use the local Ollama HTTP API. Agent Wayfinder does not run
the Ollama command-line tool, but Ollama must be installed, running, and have
the selected model available.

Install Ollama and download the default embedding model:

```bash
brew install --cask ollama
ollama pull qwen3-embedding:4b
```

Catalog embeddings are disabled by default. Enable them explicitly with the
default `qwen3-embedding:4b` model:

```bash
agent-wayfinder index --catalog-embeddings /path/to/workspace
```

Set `OLLAMA_HOST` to use another Ollama host. The value can include a scheme,
or only a host and port:

```bash
export OLLAMA_HOST=http://localhost:11434
```

To enable embeddings for a workspace or use a larger model, set
`embeddingEnabled` and `embeddingModel` in the indexed workspace's
`.agent-wayfinder/config.json` file:

```bash
ollama pull qwen3-embedding:8b
```

```json
{
  "embeddingEnabled": true,
  "embeddingModel": "qwen3-embedding:8b",
  "embeddingProcessLimit": 2
}
```

If Ollama or the selected model is unavailable, indexing continues and local
lexical catalog retrieval remains available. Explicit embedding calls use one
host thread, a 10-second timeout, and a 4 MiB response limit. Catalog work
sends up to 32 inputs per request and runs at most two requests concurrently.
Set `embeddingProcessLimit` to `1` on a constrained host. Catalog work keeps
the model loaded for one catalog pass and sends `keep_alive: 0` when that pass
completes. One-off query embeddings unload the model after their request.

### Synopses

Deterministic synopses always run. An optional provider can create a more
specific capability synopsis from the catalog metadata and bounded declaration
source. The provider is disabled by default.

#### Ollama

```json
{
  "synopsis": {
    "provider": "ollama",
    "sourceLimit": 8192
  },
  "ollama": {
    "model": "qwen3:8b",
    "endpoint": "http://127.0.0.1:11434"
  }
}
```

Put this configuration in the indexed workspace's
`.agent-wayfinder/config.json` file. `sourceLimit` is the maximum declaration
source size in bytes for each synopsis request. It must be a positive integer.
Use `copilot`, `ollama`, or `claude` for `synopsis.provider`.

`ollama.model` selects the generation model and `ollama.endpoint` sets the
Ollama HTTP endpoint. This model is separate from `embeddingModel`.

#### Copilot

```json
{
  "synopsis": {
    "provider": "copilot",
    "sourceLimit": 4096
  },
  "copilot": {
    "enabled": true,
    "maxAiCredits": 30,
    "processLimit": 1,
    "path": "copilot"
  }
}
```

`copilot.maxAiCredits` is the maximum AI credits for each catalog synopsis
request. It must be at least `30`; the default is `30`. `processLimit` limits
concurrent requests. This credit cap is separate from
`planning.copilot.maxAiCredits` for question planning.

#### Claude

Set the catalog provider in `.agent-wayfinder/config.json`:

```json
{
  "synopsis": {
    "provider": "claude",
    "sourceLimit": 4096
  }
}
```

Also enable and configure Claude in the same `.agent-wayfinder/config.json`
file:

```json
{
  "planning": {
    "claude": {
      "enabled": true,
      "model": "sonnet",
      "maxBudgetUsd": 0.25,
      "timeout": "30s"
    }
  }
}
```

`planning.claude.maxBudgetUsd` applies to both Claude question planning and
Claude catalog synopses.

Use `--catalog-synopsis-provider` and `--catalog-synopsis-source-limit` to
override the workspace configuration for one `catalog` command. Copilot and
Ollama also have the `--catalog-copilot-*` and `--catalog-ollama-*` overrides.
For example:

```bash
agent-wayfinder catalog --catalog-synopsis-provider copilot --catalog-copilot --catalog-copilot-max-ai-credits 30 /path/to/workspace
```

The selected provider receives catalog metadata and at most `sourceLimit` bytes
of local declaration source. Treat this as a code-sharing boundary when the
selected provider is not local. Copilot and Claude run without tools. Ollama
synopsis generation uses the configured endpoint.

Provider failures do not stop catalog generation. The catalog retains its
deterministic synopsis and lexical retrieval remains available. When embeddings
are enabled, Wayfinder embeds stored deterministic and optional synopsis text;
it does not embed raw declaration source.

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

### Question Planner Configuration

Question planner settings are non-secret. The query command reads them from the
workspace `.agent-wayfinder/config.json` file and from environment variables. Ollama,
Claude, and Copilot planners have separate configuration sections.

```json
{
  "planning": {
    "ollama": {
      "enabled": true,
      "model": "qwen3:8b",
      "timeout": "30s"
    },
    "copilot": {
      "enabled": true,
      "model": "auto",
      "maxAiCredits": 30,
      "tokenBudget": 4096,
      "timeout": "30s"
    },
    "claude": {
      "enabled": true,
      "model": "sonnet",
      "maxBudgetUsd": 0.25,
      "timeout": "30s"
    }
  }
}
```

Use `--copilot-max-ai-credits` to override the Copilot planning credit cap for
one `query` command. This setting does not change the catalog synopsis cap.
Use `--claude-max-budget-usd` to override the Claude planning budget for one
`query` command.

Use `WAYFINDER_COPILOT_ENABLED`, `WAYFINDER_COPILOT_MODEL`,
`WAYFINDER_COPILOT_MAX_AI_CREDITS`, `WAYFINDER_COPILOT_TOKEN_BUDGET`, and
`WAYFINDER_COPILOT_TIMEOUT` to override file settings. Defaults are `false`,
`gpt-5.6-luna`, `30`, `4096`, and `30s`. When this default model is unavailable,
the planner retries once with `auto`. An explicit model from a file, environment
variable, or command flag does not retry with `auto`. Luna uses high reasoning
effort. Timeout accepts a Go duration or integer seconds. Credit values must be
integers of at least `30`; token values must be positive integers. Timeout must
be greater than zero and no more than 30 seconds. Unknown settings inside
`planning.ollama`, `planning.claude`, or `planning.copilot` and invalid values
return an error. The catalog command accepts the `planning` object and ignores
it because it is only used by query planning.

The Ollama planner is disabled by default. When enabled, it uses a fixed
512-token context, 128-token output limit, one host thread, no reasoning, and
an 8 KiB response limit. It uses `keep_alive: 0` so Ollama unloads the model
when the request completes.

### Copilot Planner Metrics

`agent-wayfinder copilot-metrics [WORKSPACE]` shows a local daily ledger and
monthly total for the current UTC month. It uses the current directory when
`WORKSPACE` is not given. Use `--month YYYY-MM` for another UTC month or
`--day YYYY-MM-DD` for one UTC day. Use `--verbose` for token, cache, timing,
and byte details. The local database
stores request time, configured and actual model, credit limit, outcome,
durations, byte counts, input/output/cache/reasoning token totals, AIU, used
premium-request credits, and request totals. Usage values come from Copilot
CLI's final `--usage-output-file` report. It does not store prompts, responses,
source text, credentials, or other request content. Copilot-supplied values are
marked exact; unavailable values remain unavailable. `costUsd` is a derived
estimate: used credits divided by 100. Existing records without a final usage
report are not backfilled and are omitted from the metrics report.

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
AGENT_WAYFINDER_CLIENTFLEX_ROOT=/path/to/ClientFlex CGO_ENABLED=1 go test ./acceptance -run '^TestClientFlexAcceptanceIndexesIntoTemporaryDatabase$' -v
```
