.DEFAULT_GOAL := help

GOLANGCI_LINT_VERSION ?= v2.12.2
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: audit build clean coverage-check docs-audit fmt fmt-check help lint modernize-check test test/cover test-cross-compile test-integration-attended test-integration-cover test-integration-keystore test-integration-live test-integration-native-browser test-integration-smoke tidy vuln

## build: build all packages
build:
	go build ./...

GO_TEST_TIMEOUT ?= 40m

## test: run unit tests with race detector and shuffled order
test:
	go test -race -shuffle=on -timeout=$(GO_TEST_TIMEOUT) ./...

## coverage-check: run shuffled race tests and report statement coverage
coverage-check:
	go test -race -shuffle=on -coverprofile=coverage.out -covermode=atomic -timeout=$(GO_TEST_TIMEOUT) ./...
	@awk 'NR > 1 && $$(NF - 1) > 0 { found = 1 } END { if (!found) { print "coverage profile has no statement blocks"; exit 1 } }' coverage.out
	@report=$$(go tool cover -func=coverage.out) || exit $$?; printf '%s\n' "$$report" | awk '/^total:/ { found = 1; if ($$3 !~ /^[0-9]+([.][0-9]+)?%$$/) { print "invalid total coverage line"; exit 1 } printf "total coverage %s\n", $$3 } END { if (!found) { print "missing total coverage line"; exit 1 } }'

## test-integration-smoke: run live integration tests that do not spend model tokens
test-integration-smoke:
	ACP_GO_CODEX_RUN_LIVE_TOKENS=0 ACP_GO_CODEX_RUN_ATTENDED=0 ACP_GO_CODEX_RUN_KEYSTORE=0 ACP_GO_CODEX_RUN_INTEGRATION=1 go test -race -count=1 -tags=integration -timeout=300s -parallel=4 -v ./integration/... ./internal/codex

## test-integration-live: run full live integration tests
test-integration-live:
	ACP_GO_CODEX_RUN_ATTENDED=0 ACP_GO_CODEX_RUN_KEYSTORE=0 ACP_GO_CODEX_RUN_INTEGRATION=1 ACP_GO_CODEX_RUN_LIVE_TOKENS=1 go test -race -count=1 -tags=integration -timeout=900s -parallel=4 -v ./integration/... ./internal/codex

## test-integration-attended: run provider-auth flows a human must approve in real time
test-integration-attended:
	@set -eu; dir=$$(mktemp -d); trap 'rm -rf "$$dir"' EXIT HUP INT TERM; \
	dir=$$(cd "$$dir" && pwd); \
	export ACP_GO_CODEX_RUN_INTEGRATION=1 ACP_GO_CODEX_RUN_ATTENDED=1 ACP_GO_CODEX_RUN_LIVE_TOKENS=0 ACP_GO_CODEX_RUN_KEYSTORE=0; \
	go test -race -c -tags=integration -o "$$dir/integration.test" ./integration; \
	"$$dir/integration.test" -test.list '^TestAttendedProviderAuth' >"$$dir/selected"; \
	expected=$$(grep -Ec '^TestAttendedProviderAuth' "$$dir/selected" || true); \
	[ "$$expected" -gt 0 ] || { echo 'attended selector discovered no tests'; exit 1; }; \
	{ status=0; (cd integration && "$$dir/integration.test" -test.v -test.count=1 -test.timeout=1200s -test.run '^TestAttendedProviderAuth') 2>&1 || status=$$?; echo "$$status" >"$$dir/status"; } | tee "$$dir/output"; \
	status=$$(cat "$$dir/status"); passed=$$(grep -Ec '^--- PASS: TestAttendedProviderAuth' "$$dir/output" || true); \
	skipped=$$(grep -Ec '^[[:space:]]*--- SKIP:' "$$dir/output" || true); empty=$$(grep -c 'no tests to run' "$$dir/output" || true); \
	[ "$$status" -eq 0 ] || exit "$$status"; \
	[ "$$passed" -eq "$$expected" ] || { echo "attended tests passed $$passed of $$expected"; exit 1; }; \
	[ "$$skipped" -eq 0 ] || { echo 'attended test skipped'; exit 1; }; \
	[ "$$empty" -eq 0 ] || { echo 'attended selector ran no tests'; exit 1; }

## test-integration-native-browser: run current Codex login offline and trace every browser launcher
test-integration-native-browser:
	@log=$$(mktemp); rc=$$(mktemp); \
	{ (set -eu; export ACP_GO_CODEX_RUN_LIVE_TOKENS=0 ACP_GO_CODEX_RUN_ATTENDED=0 ACP_GO_CODEX_RUN_KEYSTORE=0 ACP_GO_CODEX_RUN_INTEGRATION=1; case "$$(uname -m)" in x86_64) goarch=amd64; platform=linux/amd64 ;; arm64|aarch64) goarch=arm64; platform=linux/arm64 ;; *) echo "unsupported native-browser architecture: $$(uname -m)" >&2; exit 1 ;; esac; \
	integration/browser_canary/prepare.sh; \
	CGO_ENABLED=0 GOOS=linux GOARCH="$$goarch" go test -c -tags=integration,browsercanary -o .tmp/browser-canary/browser-canary.test ./cmd/acp-go-codex; \
	docker build --platform "$$platform" --tag acp-go-codex-browser-canary --file integration/browser_canary/Dockerfile .; \
	docker run --rm --platform "$$platform" --network none --user 4242:4242 --env ACP_GO_CODEX_RUN_INTEGRATION=1 --tmpfs /tmp:rw,exec,uid=4242,gid=4242,size=256m --tmpfs /home/canary:rw,exec,uid=4242,gid=4242,mode=0700,size=128m --tmpfs /canary/scratch:rw,exec,uid=4242,gid=4242,mode=0700,size=128m acp-go-codex-browser-canary); echo $$? >"$$rc"; } 2>&1 | tee "$$log"; \
	status=$$(cat "$$rc"); passed=$$(grep -Ec '^--- PASS: TestRealNativeBrowserContainment ' "$$log" || true); skipped=$$(grep -Ec '^[[:space:]]*--- SKIP: TestRealNativeBrowserContainment(/| )' "$$log" || true); empty=$$(grep -Ec 'no tests to run' "$$log" || true); \
	rm -f "$$log" "$$rc"; \
	[ "$$status" -eq 0 ] || exit "$$status"; \
	[ "$$passed" -eq 1 ] || { echo "native browser pass count $$passed, want exactly 1"; exit 1; }; \
	[ "$$skipped" -eq 0 ] || { echo 'required native browser canary skipped'; exit 1; }; \
	[ "$$empty" -eq 0 ] || { echo 'required native browser selector ran no tests'; exit 1; }

## test-integration-keystore: run the three-configuration credential-residence matrix
test-integration-keystore:
	ACP_GO_CODEX_RUN_LIVE_TOKENS=0 ACP_GO_CODEX_RUN_ATTENDED=0 ACP_GO_CODEX_RUN_INTEGRATION=1 ACP_GO_CODEX_RUN_KEYSTORE=1 go test -race -count=1 -tags=integration -timeout=900s -v -run TestKeystore ./...

## test-integration-cover: run token-free live integration tests with compiled binary coverage
test-integration-cover:
	@set -eu; mkdir -p .tmp; dir=$$(mktemp -d "$$(pwd)/.tmp/integration-cover.XXXXXX"); trap 'rm -rf "$$dir"' EXIT HUP INT TERM; \
	mkdir "$$dir/data"; \
	go build -cover -coverpkg=./... -o "$$dir/acp-go-codex" ./cmd/acp-go-codex; \
	{ status=0; ACP_GO_CODEX_RUN_LIVE_TOKENS=0 ACP_GO_CODEX_RUN_ATTENDED=0 ACP_GO_CODEX_RUN_KEYSTORE=0 ACP_GO_CODEX_RUN_INTEGRATION=1 ACP_GO_CODEX_AGENT_BINARY="$$dir/acp-go-codex" GOCOVERDIR="$$dir/data" go test -race -count=1 -tags=integration -timeout=900s -parallel=4 -v ./integration/... ./internal/codex 2>&1 || status=$$?; echo "$$status" >"$$dir/status"; } | tee "$$dir/output"; \
	status=$$(cat "$$dir/status"); [ "$$status" -eq 0 ] || exit "$$status"; \
	passed=$$(grep -Ec '^--- PASS: TestBinarySmokeInitializeClose ' "$$dir/output" || true); \
	[ "$$passed" -eq 1 ] || { echo 'compiled initialize/close proof did not pass'; exit 1; }; \
	[ -n "$$(find "$$dir/data" -name 'covcounters.*' -type f -size +0c -print -quit)" ] || { echo 'compiled adapter produced no coverage counters'; exit 1; }; \
	go tool covdata percent -i="$$dir/data"; \
	go tool covdata textfmt -i="$$dir/data" -o coverage-integration.out

## lint: run pinned golangci-lint
lint:
	$(GOLANGCI_LINT) run --timeout=10m --allow-parallel-runners ./...

## fmt: format code with golangci-lint
fmt:
	gofmt -w $$(find . -name '*.go' -not -path './.git/*')
	$(GOLANGCI_LINT) fmt ./...

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
	GOOS=darwin GOARCH=arm64 go test -c -o .tmp/cross/codex-cmd-darwin.test ./cmd/acp-go-codex
	GOOS=darwin GOARCH=arm64 go build ./...
	GOOS=windows GOARCH=amd64 go test -c -o .tmp/cross/codex-root-windows.test .
	GOOS=windows GOARCH=amd64 go test -c -o .tmp/cross/codex-windows.test ./internal/codex
	GOOS=windows GOARCH=amd64 go test -c -o .tmp/cross/codex-cmd-windows.test ./cmd/acp-go-codex
	GOOS=windows GOARCH=amd64 go build ./...
	GOOS=freebsd GOARCH=amd64 go build ./...
	GOOS=openbsd GOARCH=amd64 go build ./...
	GOOS=netbsd GOARCH=amd64 go build ./...

## modernize-check: check Go modernizations without changing files
modernize-check:
	go fix -diff ./...

## docs-audit: check public docs, examples, required files, and CLI flags
docs-audit:
	@missing=0; for file in README.md doc.go docs.json example_test.go AGENTS.md docs/overview.mdx docs/core/sessions.mdx docs/core/prompt-streaming.mdx docs/features/authentication.mdx docs/features/elicitation.mdx docs/features/mcp.mdx docs/features/models-config.mdx docs/features/permissions.mdx docs/features/raw-events.mdx docs/features/session-store.mdx docs/get-started/examples.mdx docs/get-started/install.mdx docs/get-started/quickstart.mdx docs/get-started/run-modes.mdx docs/operations/observability.mdx docs/operations/security.mdx docs/operations/troubleshooting.mdx docs/reference/acp-methods.mdx docs/reference/cli.mdx docs/reference/go-api.mdx docs/reference/meta.mdx docs/reference/updates.mdx examples/minimal-client/main.go examples/resume-from-file/main.go examples/interactive-chat/main.go; do if [ ! -f "$$file" ]; then echo "missing required docs file: $$file"; missing=1; fi; done; exit $$missing
	@for flag in -path -home -scratch-dir -provider-auth-root -provider-auth-direct-home -model -debug -version; do rg -q -- "$$flag" docs/reference/cli.mdx cmd/acp-go-codex/main.go || { echo "missing CLI flag in docs/code: $$flag"; exit 1; }; done

## audit: run local checks
audit: fmt-check lint build coverage-check test-cross-compile tidy vuln modernize-check docs-audit
	go mod verify

## clean: remove build artifacts
clean:
	rm -rf .tmp coverage.out coverage-integration.out coverage-summary.txt

## test/cover: open HTML coverage report
test/cover: coverage-check
	go tool cover -html=coverage.out

## help: show this help
help:
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' | sed -e 's/^/ /'
