.DEFAULT_GOAL := help
.PHONY: audit build clean coverage-check docs-audit fmt fmt-check help lint modernize-check test test/cover test-cross-compile test-integration test-integration-cover test-integration-live test-integration-smoke tidy vuln

## build: build all packages
build:
	go build ./...

## test: run tests with race detector and coverage
test:
	go test -race -shuffle=on -coverprofile=coverage.out -covermode=atomic ./...

## coverage-check: require 100% statement coverage
coverage-check: test
	@go tool cover -func=coverage.out | awk 'BEGIN { found = 0 } /^total:/ { found = 1; if ($$3 != "100.0%") { printf "total coverage %s, want 100.0%%\n", $$3; exit 1 } } END { if (!found) { print "missing total coverage line"; exit 1 } }'

## test-integration-smoke: run live integration tests that do not spend model tokens
test-integration-smoke:
	ACP_GO_CODEX_RUN_INTEGRATION=1 go test -race -count=1 -tags=integration -timeout=300s -parallel=4 -v ./integration/...

## test-integration-live: run full live integration tests
test-integration-live:
	ACP_GO_CODEX_RUN_INTEGRATION=1 ACP_GO_CODEX_RUN_LIVE_TOKENS=1 go test -race -count=1 -tags=integration -timeout=900s -parallel=4 -v ./integration/...

## test-integration: alias for full live integration tests
test-integration: test-integration-live

## test-integration-cover: run full live integration tests with compiled binary coverage
test-integration-cover:
	rm -rf .tmp/integration-cover coverage-integration.out
	mkdir -p .tmp/integration-cover/data
	go build -cover -coverpkg=./... -o .tmp/integration-cover/acp-go-codex ./cmd/acp-go-codex
	ACP_GO_CODEX_RUN_INTEGRATION=1 ACP_GO_CODEX_RUN_LIVE_TOKENS=1 ACP_GO_CODEX_AGENT_BINARY=$$(pwd)/.tmp/integration-cover/acp-go-codex GOCOVERDIR=$$(pwd)/.tmp/integration-cover/data go test -race -count=1 -tags=integration -timeout=900s -parallel=4 -v ./integration/...
	go tool covdata percent -i=.tmp/integration-cover/data
	go tool covdata textfmt -i=.tmp/integration-cover/data -o coverage-integration.out

## lint: run golangci-lint
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run ./...

## fmt: format code with golangci-lint
fmt:
	gofmt -w $$(find . -name '*.go' -not -path './.git/*')
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 fmt ./...

## fmt-check: require gofmt-clean Go files
fmt-check:
	@test -z "$$(gofmt -l .)"

## tidy: verify module files are tidy
tidy:
	go mod tidy -diff

## vuln: run govulncheck from the go.mod tool directive
# golang.org/x/vuln v1.4.0 panics in x/tools SSA on Go 1.26 generics;
# keep the tool directive pinned at v1.5.0 or newer.
vuln:
	go tool govulncheck ./...

## test-cross-compile: compile platform-specific test branches
test-cross-compile:
	rm -rf .tmp/cross
	mkdir -p .tmp/cross
	GOOS=linux GOARCH=amd64 go test -c -o .tmp/cross/codex-linux.test ./internal/codex
	GOOS=darwin GOARCH=arm64 go test -c -o .tmp/cross/codex-darwin.test ./internal/codex
	GOOS=windows GOARCH=amd64 go build ./...
	GOOS=freebsd GOARCH=amd64 go build ./...
	GOOS=openbsd GOARCH=amd64 go build ./...

## modernize-check: preview Go modernizations without changing files
modernize-check:
	go fix -n ./...

## docs-audit: check public docs and examples for removed public terms
docs-audit:
	@! rg -n 'opencode acp|proxy|compatibility|deprecated|legacy|migration|session/import|sdkMessage|emitRawSDKMessages|setGoal|goals|"_meta"\s*:\s*\{[^}]*"mode"|NES|SSE MCP|mcpCapabilities\.acp|^  "codex"\s*:' README.md doc.go docs.json docs examples cmd/acp-go-codex/*.go

## audit: run local checks
audit: fmt-check lint build test coverage-check test-cross-compile tidy vuln modernize-check docs-audit
	go mod verify

## clean: remove build artifacts
clean:
	rm -rf .tmp coverage.out coverage-integration.out coverage-summary.txt

## test/cover: open HTML coverage report
test/cover: test
	go tool cover -html=coverage.out

## help: show this help
help:
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' | sed -e 's/^/ /'
