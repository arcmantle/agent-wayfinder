# Development

- Use `make run ARGS='...'` to run the local CLI.
- Use `make build`, `make test`, and `make lint` for local validation.
- The SQLite catalog includes FTS5 and sqlite-vec by default.
- When a direct Go command is necessary, use `CGO_ENABLED=1 go run ./cmd/agent-wayfinder ...`.