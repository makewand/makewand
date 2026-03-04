BINARY_NAME=makewand
VERSION=0.1.0
BUILD_DIR=build
MAIN_PKG=./cmd/makewand

.PHONY: all build run clean test install prelaunch

all: build

build:
	@mkdir -p $(BUILD_DIR)
	go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN_PKG)

run: build
	./$(BUILD_DIR)/$(BINARY_NAME)

run-new: build
	./$(BUILD_DIR)/$(BINARY_NAME) new

run-chat: build
	./$(BUILD_DIR)/$(BINARY_NAME) chat

clean:
	rm -rf $(BUILD_DIR)

test:
	go test ./...

install: build
	cp $(BUILD_DIR)/$(BINARY_NAME) $(HOME)/.local/bin/$(BINARY_NAME)

# Cross-compilation
build-all: build-linux build-darwin build-windows

build-linux:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 $(MAIN_PKG)

build-darwin:
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 $(MAIN_PKG)

build-windows:
	@mkdir -p $(BUILD_DIR)
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe $(MAIN_PKG)

fmt:
	go fmt ./...

vet:
	go vet ./...

lint: fmt vet

prelaunch:
	./scripts/prelaunch_gate.sh
