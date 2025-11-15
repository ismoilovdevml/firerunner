.PHONY: build test clean install docker-build run help

# Build variables
BINARY_NAME=firerunner
VERSION?=dev
COMMIT=$(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS=-ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)"

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

docker-build: ## Build Docker image for rootfs
	@echo "Building rootfs Docker image..."
	docker build -t firerunner/gitlab-runner:latest images/rootfs/
	@echo "Docker image built"

docker-push: docker-build ## Push Docker image to registry
	@echo "Pushing Docker image..."
	docker push firerunner/gitlab-runner:latest
	@echo "Docker image pushed"

run: build ## Build and run locally
	@echo "Starting FireRunner..."
	$(BUILD_DIR)/$(BINARY_NAME) --config config.example.yaml

dev: ## Run in development mode
	@echo "Starting FireRunner (development)..."
	$(GOCMD) run ./$(CMD_DIR) --config config.example.yaml

fmt: ## Format Go code
	@echo "Formatting code..."
	$(GOCMD) fmt ./...
	@echo "Format complete"

lint: ## Run linter
	@echo "Running linter..."
	@which golangci-lint > /dev/null || (echo "Installing golangci-lint..." && go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)
	golangci-lint run ./...
	@echo "Lint complete"

vet: ## Run go vet
	@echo "Running go vet..."
	$(GOCMD) vet ./...
	@echo "Vet complete"

check: fmt vet lint test ## Run all checks (fmt, vet, lint, test)

release: clean check build-linux ## Create release build
	@echo "Creating release..."
	cd $(BUILD_DIR) && tar -czf $(BINARY_NAME)-$(VERSION)-linux-amd64.tar.gz $(BINARY_NAME)-linux-amd64
	@echo "Release created: $(BUILD_DIR)/$(BINARY_NAME)-$(VERSION)-linux-amd64.tar.gz"

.PHONY: all
all: clean deps check build ## Run clean, deps, check, and build
