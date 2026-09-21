.PHONY: build run test acceptance lint check

AGENT_WAYFINDER_PACKAGES := $(shell go list -e ./... | grep -v '^agent-wayfinder/reference')

build:
	CGO_ENABLED=1 go build ./cmd/agent-wayfinder

run:
	CGO_ENABLED=1 go run ./cmd/agent-wayfinder $(ARGS)

test:
	CGO_ENABLED=1 go test $(AGENT_WAYFINDER_PACKAGES)

acceptance:
	CGO_ENABLED=1 go test $(AGENT_WAYFINDER_PACKAGES)

lint:
	CGO_ENABLED=1 go vet $(AGENT_WAYFINDER_PACKAGES)

check: build acceptance lint