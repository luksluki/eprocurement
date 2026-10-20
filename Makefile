.PHONY: help install build lint test fmt compose-up compose-down compose-logs compose-ps reset

help: ## Show available targets
	@echo "Available targets:"
	@echo "  install       Install JS workspace dependencies (pnpm install)"
	@echo "  build         Build Go services and JS packages"
	@echo "  lint          Run Go vet and JS lint placeholders"
	@echo "  test          Run Go and JS test placeholders"
	@echo "  fmt           Format Go code (gofmt)"
	@echo "  compose-up    Start local stack and wait for healthy services"
	@echo "  compose-down  Stop local stack (keeps volumes)"
	@echo "  compose-logs  Follow local stack logs"
	@echo "  compose-ps    Show service health"
	@echo "  reset         Drop volumes and recreate the local stack"

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

compose-up: ## Start local stack and wait for healthy services
	docker compose up -d --wait

compose-down: ## Stop local stack (keeps volumes)
	docker compose down

compose-logs: ## Follow local stack logs
	docker compose logs -f --tail=100

compose-ps: ## Show service health
	docker compose ps

reset: ## Drop volumes and recreate the local stack
	docker compose down -v --remove-orphans
	docker compose up -d --wait
