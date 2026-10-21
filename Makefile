.PHONY: help install build lint test fmt compose-up compose-down compose-logs compose-ps seed seed-reset reset gen-proto cover test-integration

help: ## Show available targets
	@echo "Available targets:"
	@echo "  install          Install JS workspace dependencies (pnpm install)"
	@echo "  build            Build Go services and JS packages"
	@echo "  lint             Run Go vet and JS lint placeholders"
	@echo "  test             Run Go and JS test placeholders"
	@echo "  fmt              Format Go code (gofmt)"
	@echo "  compose-up       Start local stack and wait for healthy services"
	@echo "  compose-down     Stop local stack (keeps volumes)"
	@echo "  compose-logs     Follow local stack logs"
	@echo "  compose-ps       Show service health"
	@echo "  seed             Reset demo data and generate the dataset"
	@echo "  seed-reset       Wipe demo data (PostgreSQL + MongoDB)"
	@echo "  reset            Drop volumes and recreate the local stack"
	@echo "  gen-proto        Regenerate gen/go from proto/ (needs protoc; see docs/onboarding.md)"
	@echo "  cover            Identity-domain coverage, enforced threshold (stack must be up)"
	@echo "  test-integration Full go test ./... against the compose database"

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
	gofmt -w services internal scripts

compose-up: ## Start local stack and wait for healthy services
	docker compose up -d --wait

compose-down: ## Stop local stack (keeps volumes)
	docker compose down

compose-logs: ## Follow local stack logs
	docker compose logs -f --tail=100

compose-ps: ## Show service health
	docker compose ps

seed: ## Reset demo data and generate the deterministic dataset
	go run ./scripts/seed

seed-reset: ## Wipe demo data (PostgreSQL + MongoDB)
	go run ./scripts/seed --reset

reset: ## Drop volumes and recreate the local stack
	docker compose down -v --remove-orphans
	docker compose up -d --wait

PROTOC := $(shell command -v .tools/protoc/bin/protoc 2>/dev/null || command -v protoc 2>/dev/null)

# Reads DATABASE_URL from .env (the compose stack) when TEST_DATABASE_URL is
# not set in the environment. Same pattern for the documents test stores.
TEST_DATABASE_URL ?= $(shell sed -n 's/^DATABASE_URL=//p' .env 2>/dev/null)
TEST_MONGO_URI ?= $(shell sed -n 's/^MONGO_URI=//p' .env 2>/dev/null)
TEST_S3_ENDPOINT ?= $(shell sed -n 's/^S3_ENDPOINT=//p' .env 2>/dev/null)
IDENTITY_COVERAGE_THRESHOLD ?= 80

gen-proto: ## Regenerate gen/go from proto/
	@test -n "$(PROTOC)" || (echo "protoc not found: install it into .tools/protoc (see docs/onboarding.md)" && exit 1)
	PATH="$$PATH:$$(go env GOPATH)/bin" $(PROTOC) \
		--proto_path=proto \
		--go_out=. --go_opt=module=github.com/luksluki/eprocurement \
		--go-grpc_out=. --go-grpc_opt=module=github.com/luksluki/eprocurement \
		$$(find proto -name '*.proto')

cover: ## Identity-domain coverage with enforced threshold (compose stack must be up)
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test ./services/identity/... -count=1 \
		-coverprofile=coverage.out \
		-coverpkg=./services/identity/auth,./services/identity/server,./services/identity/config
	go tool cover -func=coverage.out | tail -1
	@go tool cover -func=coverage.out | awk -v t=$(IDENTITY_COVERAGE_THRESHOLD) \
		'/^total:/ {gsub("%","",$$3); if ($$3+0 < t) {print "FAIL: identity coverage " $$3 "% is below the " t "% threshold"; exit 1} print "OK: identity coverage " $$3 "% meets the " t "% threshold"}'

test-integration: ## Full go test ./... against the compose database and object store
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" TEST_MONGO_URI="$(TEST_MONGO_URI)" \
	TEST_S3_ENDPOINT="$(TEST_S3_ENDPOINT)" go test ./... -count=1
