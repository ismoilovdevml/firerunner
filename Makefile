.PHONY: build test clean install docker-build run dev help

# Build variables
BINARY_NAME=firerunner
VERSION?=dev
# main.version is the only build-time variable cmd/firerunner declares.
LDFLAGS=-ldflags "-X main.version=$(VERSION)"

# Pinned to the versions CI runs (.github/workflows/ci.yml).
GOLANGCI_LINT_VERSION=v2.13.2
GOVULNCHECK_VERSION=v1.8.0

# Local tag for the microVM rootfs image; CI publishes to ghcr.io (images.yml).
ROOTFS_IMAGE?=firerunner-rootfs:local

# Arguments for `make run` / `make dev`, e.g. make run ARGS="vm list".
ARGS?=status

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod
GOCLEAN=$(GOCMD) clean

# Directories
BUILD_DIR=build
CMD_DIR=cmd/firerunner

.DEFAULT_GOAL := help

help: ## Show this help message
	@echo "FireRunner - Makefile Commands"
	@echo "==============================="
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

build: ## Build the binary
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./$(CMD_DIR)
	@echo "Build complete: $(BUILD_DIR)/$(BINARY_NAME)"

build-linux: ## Build for Linux
	@echo "Building for Linux..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./$(CMD_DIR)
	@echo "Build complete: $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64"

test: ## Run tests
	@echo "Running tests..."
	$(GOTEST) -v -race -coverprofile=coverage.out ./...
	@echo "Tests complete"

test-coverage: test ## Run tests with coverage report
	$(GOCMD) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

clean: ## Clean build artifacts
	@echo "Cleaning..."
	$(GOCLEAN)
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html
	@echo "Clean complete"

install: build ## Install binary to /usr/local/bin
	@echo "Installing $(BINARY_NAME)..."
	sudo cp $(BUILD_DIR)/$(BINARY_NAME) /usr/local/bin/
	@echo "Installed to /usr/local/bin/$(BINARY_NAME)"

deps: ## Download dependencies
	@echo "Downloading dependencies..."
	$(GOMOD) download
	$(GOMOD) tidy
	@echo "Dependencies updated"

docker-build: ## Build the microVM rootfs image locally (tag: ROOTFS_IMAGE)
	@echo "Building rootfs image $(ROOTFS_IMAGE)..."
	docker build -t $(ROOTFS_IMAGE) images/rootfs/
	@echo "Docker image built: $(ROOTFS_IMAGE)"

run: build ## Build and run a command (ARGS="status"; config from FIRERUNNER_CONFIG)
	$(BUILD_DIR)/$(BINARY_NAME) $(ARGS)

dev: ## go run a command (ARGS="status"; config from FIRERUNNER_CONFIG)
	$(GOCMD) run ./$(CMD_DIR) $(ARGS)

fmt: ## Format Go code
	@echo "Formatting code..."
	$(GOCMD) fmt ./...
	@echo "Format complete"

lint: ## Run golangci-lint (pinned version)
	@echo "Running golangci-lint $(GOLANGCI_LINT_VERSION)..."
	$(GOCMD) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...
	@echo "Lint complete"

vulncheck: ## Run govulncheck (pinned version)
	$(GOCMD) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./cmd/...

vet: ## Run go vet
	@echo "Running go vet..."
	$(GOCMD) vet ./...
	@echo "Vet complete"

check: fmt vet lint vulncheck test ## Run all checks (fmt, vet, lint, vulncheck, test)

release: clean check build-linux ## Create release build
	@echo "Creating release..."
	cd $(BUILD_DIR) && tar -czf $(BINARY_NAME)-$(VERSION)-linux-amd64.tar.gz $(BINARY_NAME)-linux-amd64
	@echo "Release created: $(BUILD_DIR)/$(BINARY_NAME)-$(VERSION)-linux-amd64.tar.gz"

.PHONY: all build-linux test-coverage deps fmt lint vulncheck vet check release
all: clean deps check build ## Run clean, deps, check, and build
