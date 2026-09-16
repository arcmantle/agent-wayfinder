.PHONY: build test acceptance lint check

AGENT_WAYFINDER_PACKAGES := $(shell go list -e ./... | grep -v '^agent-wayfinder/reference')

build:
	CGO_ENABLED=1 go build -tags sqlite_fts5 ./cmd/agent-wayfinder

test:
	CGO_ENABLED=1 go test -tags sqlite_fts5 $(AGENT_WAYFINDER_PACKAGES)

acceptance:
	CGO_ENABLED=1 go test -tags sqlite_fts5 $(AGENT_WAYFINDER_PACKAGES)

lint:
	CGO_ENABLED=1 go vet -tags sqlite_fts5 $(AGENT_WAYFINDER_PACKAGES)

check: build acceptance lint