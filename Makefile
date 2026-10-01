.PHONY: all install test test-py test-go test-dispatch test-install test-benchmark test-scripts \
	check-secrets check-version check-shell fmt-check lint vuln race prelaunch probe status clean \
	test-architecture drill-long

# Go-based analyzers must run under the toolchain CI uses (actions/setup-go reads
# go.mod). A prebuilt golangci-lint/govulncheck built with go.mod's Go panics or
# refuses to run against a newer local GOROOT (e.g. go1.27), so lint/vuln pin
# GOTOOLCHAIN to go.mod: `toolchain` directive if present, else `go`.
GO_MOD_TOOLCHAIN := $(shell awk '$$1 == "toolchain" { print $$2; exit }' go.mod)
GO_MOD_VERSION := $(shell awk '$$1 == "go" { print $$2; exit }' go.mod)
GOTOOLCHAIN_PIN ?= $(if $(GO_MOD_TOOLCHAIN),$(GO_MOD_TOOLCHAIN),go$(GO_MOD_VERSION))
# Keep in sync with the golangci-lint-action `version:` pin in .github/workflows/ci.yml.
GOLANGCI_LINT_VERSION ?= v2.12.2
GOLANGCI_LINT ?= golangci-lint
GOVULNCHECK ?= govulncheck
SHELL_SCRIPTS := scripts/*.sh site/install.sh dispatch/*.sh dispatch/ai-dispatch

all: test

install:
	./scripts/install.sh

test-py:
	python3 -I scripts/test_python.py

test-go:
	go test -count=1 ./...

test-dispatch:
	bash dispatch/test.sh

test-install:
	python3 -I scripts/test_installation.py

test-benchmark:
	python3 -I benchmarks/test_runner.py
	python3 -I benchmarks/test_deep_config_acceptance_v4.py
	python3 -I benchmarks/test_analyze_results.py

test-architecture:
	MAKEWAND_REQUIRE_BWRAP=1 go test ./internal/engine -run 'TrustedAcceptance|^TestApply|^TestPendingApproval' -count=1
	go test ./internal/tui -run '^TestPendingApproval' -count=1
	go test ./internal/processjob -count=1
	python3 -I scripts/test_python.py test_candidate_recovery
	go run ./cmd/server-drill --output benchmarks/results/architecture/server-short.json --requests 400 --concurrency 32 --tenants 4 --router-calls 100000

drill-long:
	go run ./cmd/server-drill --output benchmarks/results/architecture/server-long.json --requests 10000 --concurrency 64 --tenants 10 --router-calls 100000

# Release-tooling self-tests: secret scanner regression corpus + version consistency.
test-scripts:
	bash scripts/test_check_secrets.sh
	bash scripts/check_version.sh
	python3 -I scripts/generate_cli_contract.py --check
	python3 -I scripts/check_execution_contract.py

test: test-py test-go test-dispatch test-install test-benchmark test-scripts

check-secrets:
	./scripts/check_secrets.sh

check-version:
	bash scripts/check_version.sh

check-shell:
	@echo "Checking shell script syntax..."
	@for f in $(SHELL_SCRIPTS); do bash -n "$$f" || exit 1; done

fmt-check:
	@unformatted="$$(gofmt -l $$(git ls-files '*.go'))"; \
	if [ -n "$$unformatted" ]; then echo "The following files need gofmt:"; echo "$$unformatted"; exit 1; fi; \
	echo "gofmt: ok"

lint:
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || { \
		echo "golangci-lint not found. Install the version CI pins:" >&2; \
		echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)" >&2; \
		exit 1; }
	@have="$$($(GOLANGCI_LINT) version --short 2>/dev/null || true)"; \
	if [ "v$${have#v}" != "$(GOLANGCI_LINT_VERSION)" ]; then \
		echo "warning: golangci-lint $${have:-unknown} differs from CI pin $(GOLANGCI_LINT_VERSION); results may differ" >&2; fi
	GOTOOLCHAIN=$(GOTOOLCHAIN_PIN) $(GOLANGCI_LINT) run ./...

vuln:
	@command -v $(GOVULNCHECK) >/dev/null 2>&1 || { \
		echo "govulncheck not found. Install it with:" >&2; \
		echo "  go install golang.org/x/vuln/cmd/govulncheck@latest" >&2; \
		exit 1; }
	GOTOOLCHAIN=$(GOTOOLCHAIN_PIN) $(GOVULNCHECK) ./...

race:
	bash ./scripts/test_race.sh

# Release gate = every CI `verify` check plus the operator-only doctor check.
# See docs/PRELAUNCH.md. MAKEWAND_LIVE_SMOKE=1 / MAKEWAND_DOCTOR_MODES are
# honoured by scripts/prelaunch_gate.sh.
prelaunch: check-secrets test-scripts check-shell fmt-check lint
	bash ./scripts/prelaunch_gate.sh
	$(MAKE) race vuln
	@echo "Prelaunch checks passed successfully!"

probe:
	./bin/makewand probe

status:
	./bin/makewand status

clean:
	find . -type d -name "__pycache__" -exec rm -rf {} + 2>/dev/null || true
	find . -type f -name "*.pyc" -delete 2>/dev/null || true
