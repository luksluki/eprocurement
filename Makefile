.PHONY: help install build lint test fmt compose-up compose-down seed reset

help: ## Show available targets
	@echo "Available targets:"
	@echo "  install       Install JS workspace dependencies (pnpm install)"
	@echo "  build         Build Go services and JS packages"
	@echo "  lint          Run Go vet and JS lint placeholders"
	@echo "  test          Run Go and JS test placeholders"
	@echo "  fmt           Format Go code (gofmt)"
	@echo "  compose-up    Start local stack (arrives in #14)"
	@echo "  compose-down  Stop local stack (arrives in #14)"
	@echo "  seed          Seed demo data (arrives in #17)"
	@echo "  reset         Reset local data (arrives in #17)"

install: ## Install JS workspace dependencies
	pnpm install

build: ## Build Go services and JS packages
	go build ./...
	pnpm build

lint: ## Run Go vet and JS lint placeholders
	go vet ./...
	pnpm lint

test: ## Run Go and JS test placeholders
	go test ./...
	pnpm test

fmt: ## Format Go code
	gofmt -w services internal

compose-up: ## Start local stack (arrives in #14)
	@echo "docker compose stack arrives in issue #14"

compose-down: ## Stop local stack (arrives in #14)
	@echo "docker compose stack arrives in issue #14"

seed: ## Seed demo data (arrives in #17)
	@echo "seed script arrives in issue #17"

reset: ## Reset local data (arrives in #17)
	@echo "reset script arrives in issue #17"
