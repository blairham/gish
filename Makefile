BINARY_NAME := koi
BUILD_DIR := build
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --verify --quiet HEAD 2>/dev/null || echo "none")
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)"

.PHONY: all build clean test test-cover lint fmt vet tidy install check sync check-versions proto

all: build

build:
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/koi

install:
	go install $(LDFLAGS) ./cmd/koi

clean:
	rm -rf $(BUILD_DIR) coverage.out coverage.html
	go clean

test:
	go test -v -race ./...

test-cover:
	go test -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

lint:
	go tool golangci-lint run ./...

vet:
	go vet ./...

fmt:
	go tool gofumpt -w .

tidy:
	go mod tidy

check: fmt vet test

# Startup latency: prints exec-to-first-prompt timings (#37). The CI
# regression gate lives in cmd/koi/startup_test.go.
bench-startup:
	go test -run TestStartupBudget -v ./cmd/koi/ | grep 'startup runs'

.PHONY: bench
bench: ## Regenerate docs/bench.md: startup table + keystroke p50/p99 (#102)
	go test ./internal/bench/ -run TestBenchmarkReport -update -v

.PHONY: compat
compat: ## Regenerate docs/compat.md from a live bash-vs-koi run (#101)
	go test ./internal/compat/ -run TestCompatScoreboard -update -v

.PHONY: compat-check
compat-check: ## Fail if the corpus passes fewer cases than published
	go test ./internal/compat/ -run 'TestCompatScoreboard|TestCorpusIsWellFormed'

.PHONY: bash-suite
bash-suite: ## Fetch bash's own tests/ and publish docs/bash-suite.md (#211)
	./scripts/fetch-bash-tests.sh
	KOI_GATES=1 go test ./internal/compat/ -run TestBashSuite -update-suite -timeout 30m -v

.PHONY: bash-suite-check
bash-suite-check: ## Run the bash suite without republishing (harness sanity only)
	./scripts/fetch-bash-tests.sh
	# -v is load-bearing: the run reports its delta against the published
	# page with t.Logf, and go test discards Logf output from a passing test
	# unless -v is set. Without it this target prints "ok" and the drift it
	# exists to surface stays invisible — which is exactly how it shipped.
	KOI_GATES=1 go test -v ./internal/compat/ -run 'TestBashSuite|TestSuiteSummary|TestBashSuiteDoc|TestPublishedSuite' -timeout 30m

.PHONY: paste-gate
paste-gate: ## Regenerate docs/interactive-compat.md: paste + source gates (#161) and the ecosystem matrix (#159)
	KOI_GATES=1 go test ./internal/compat/ -run TestInteractiveGates -update -v

.PHONY: paste-gate-check
paste-gate-check: ## Fail if a pasted construct, an init script, or an installed tool regressed
	KOI_GATES=1 go test ./internal/compat/ -run 'TestInteractiveGates|TestInteractiveCorporaAreWellFormed'

.PHONY: agent-gate
agent-gate: ## Regenerate the open-gap table in docs/agents.md from a live run (#208)
	go test ./internal/compat/ -run TestAgentGapsDoc -update-agent -v

.PHONY: agent-gate-check
agent-gate-check: ## Run the agent gate: regressions fail, filed gaps report, a stale marker fails
	go test ./internal/compat/ -run 'TestAgentGate|TestAgentKnownGaps|TestAgentGapsDoc' -v

# Regenerate pkg/pluginapi/v1 from proto/koi/plugin/v1. Needs protoc plus
# protoc-gen-go and protoc-gen-go-grpc on PATH.
proto:
	protoc --proto_path=proto \
		--go_out=. --go_opt=module=github.com/blairham/koi-shell \
		--go-grpc_out=. --go-grpc_opt=module=github.com/blairham/koi-shell \
		proto/koi/plugin/v1/*.proto

# Toolchain pin invariant. The check itself lives in blairham/pre-commit-hooks
# and is wired up in .pre-commit-config.yaml — there is no local copy.
check-versions:
	@pre-commit run check-go-version-sync --all-files

# Rewrite .tool-versions' golang pin from go.mod (go.mod is authoritative).
#
# Exit 1 means "drift found and fixed" — that is success here. Only 2+ (or a
# missing toolchain) is a real failure, so don't blanket-swallow with `|| true`.
#
# Chicken-and-egg: if .tool-versions pins a Go that asdf has not installed, the
# `go` shim refuses to run and this cannot self-heal. The fallback rewrites the
# pin with awk/sed, which needs no Go at all.
sync:
	@go run github.com/blairham/pre-commit-hooks/cmd/check-go-version-sync@v0.1.0 -fix; \
	status=$$?; \
	if [ $$status -le 1 ]; then exit 0; fi; \
	echo "note: falling back (the pinned toolchain is unavailable to run the checker)"; \
	GO_VERSION=$$(awk '/^go [0-9]/{print $$2; exit}' go.mod); \
	[ -n "$$GO_VERSION" ] || { echo "error: no go directive in go.mod" >&2; exit 1; }; \
	sed -i.bak "s/^golang .*/golang $$GO_VERSION/" .tool-versions && rm -f .tool-versions.bak
