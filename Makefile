.PHONY: help install build lint test fmt compose-up compose-down compose-logs compose-ps seed seed-reset reset gen-proto cover test-integration test-p2p e2e perf-budget

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
	@echo "  test-p2p         Procure-to-pay cross-service suite (postgres+mongo+rabbitmq up)"
	@echo "  e2e              Playwright happy path against the compose stack (stack must be up)"
	@echo "  perf-budget      Run the 100k scale fixtures and report p95 vs budget (stack must be up)"

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
TEST_RABBITMQ_URL ?= $(shell sed -n 's/^RABBITMQ_URL=//p' .env 2>/dev/null)
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

# The #58 procure-to-pay cross-service suite (ADR-0002 CI stage 3): the real
# gRPC procurement API in-process → transactional outbox → publisher loop →
# RabbitMQ → consumer runner → MongoDB projection, against the compose
# infrastructure (postgres + mongo + rabbitmq must be healthy: make
# compose-up). Scoped to ./tests/integration ONLY on purpose: with
# TEST_RABBITMQ_URL exported for ./... and the full stack running, the older
# broker tests that share the compose database (e.g. the publisher's
# scratch-database loop test) would race the live worker — the new suite
# avoids exactly that with its own scratch database.
test-p2p: ## Run the procure-to-pay integration suite (stack must be up)
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" TEST_MONGO_URI="$(TEST_MONGO_URI)" \
	TEST_RABBITMQ_URL="$(TEST_RABBITMQ_URL)" go test ./tests/integration/... -count=1 -v

# Playwright happy path (#59) — the same sequence the CI e2e job runs:
# fixtures are installed by the suite's globalSetup (cmd/useradmin needs
# DATABASE_URL, hence the .env passthrough), the web app is rebuilt and
# served with `next start`, and the spec drives the compose stack. Identity's
# login limiter can 429 rapid local re-runs; raise it per docs/onboarding.md
# (IDENTITY_LOGIN_RATE_PER_MIN in .env, then make compose-up) if retrying.
e2e: ## Run the Playwright happy path (compose stack must be up)
	pnpm --filter @eprocurement/web build
	DATABASE_URL="$(TEST_DATABASE_URL)" pnpm --filter @eprocurement/e2e e2e

# The #56 p95 budget re-run path: the five ~100k scale fixtures against the
# compose database, with a per-shape p95-vs-budget report parsed from the
# tests' own logs (the budgets live in the tests, not here). Serialized
# bulk loads; a full run is roughly 8 minutes.
perf-budget: ## Run the 100k scale fixtures and report p95 vs budget (stack must be up)
	bash scripts/dev/perf-budget.sh
