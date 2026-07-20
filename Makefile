BINARY_NAME=makewand
VERSION=0.2.0-dev
BUILD_DIR=build
MAIN_PKG=./cmd/makewand

# Build info package path for ldflags
BUILDINFO_PKG=github.com/makewand/makewand/internal/buildinfo

# Detect git commit and dirty status for development builds
GIT_COMMIT=$(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
GIT_DIRTY=$(shell git status --porcelain 2>/dev/null | grep -q . && echo "dirty" || echo "")

# ldflags for build injection
LDFLAGS=-s -w
LDFLAGS+=-X $(BUILDINFO_PKG).Version=$(VERSION)
LDFLAGS+=-X $(BUILDINFO_PKG).Commit=$(GIT_COMMIT)
LDFLAGS+=-X $(BUILDINFO_PKG).Dirty=$(GIT_DIRTY)

.PHONY: all build run clean test test-race test-gate vulncheck install prelaunch

all: build

build:
	@mkdir -p $(BUILD_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN_PKG)

run: build
	./$(BUILD_DIR)/$(BINARY_NAME)

run-new: build
	./$(BUILD_DIR)/$(BINARY_NAME) new

run-chat: build
	./$(BUILD_DIR)/$(BINARY_NAME) chat

clean:
	rm -rf $(BUILD_DIR)

test:
	bash ./scripts/test_gate.sh

test-gate:
	bash ./scripts/test_gate.sh

test-race:
	bash ./scripts/test_race.sh

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

install: build
	cp $(BUILD_DIR)/$(BINARY_NAME) $(HOME)/.local/bin/$(BINARY_NAME)

# Cross-compilation
build-all: build-linux build-darwin build-windows

build-linux:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 $(MAIN_PKG)

build-darwin:
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 $(MAIN_PKG)

build-windows:
	@mkdir -p $(BUILD_DIR)
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe $(MAIN_PKG)

fmt:
	go fmt ./...

vet:
	go vet ./...

lint: fmt vet

prelaunch:
	./scripts/prelaunch_gate.sh

version-info:
	@echo "Build Info:"
	@echo "  VERSION: $(VERSION)"
	@echo "  COMMIT:  $(GIT_COMMIT)"
	@echo "  DIRTY:   $(GIT_DIRTY)"
	@echo "  LDFLAGS: $(LDFLAGS)"
