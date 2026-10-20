# ADR-0002 — CI on GitHub Actions (GitLab CI concepts preserved)

- **Status:** Accepted
- **Date:** 2026-10-20
- **Related:** PRD §20, toolchain requirements (GitLab CI)

## Context

GitLab CI is part of the intended toolchain, but this repository is hosted on **GitHub** (`luksluki/eprocurement`). We need a CI pipeline that covers the same ground: linting, unit tests, integration tests, image builds, e2e, and deployment gating.

## Decision

Use **GitHub Actions** as the CI runner, with a pipeline shaped like a GitLab CI one:

1. `lint` — Go vet/staticcheck, ESLint, type checks.
2. `unit` — `go test ./...`, Vitest.
3. `integration` — dockerized Postgres/Mongo/Redis/RabbitMQ services; run integration tests.
4. `build` — container images (multi-stage, cached).
5. `e2e` — Playwright against the compose stack.
6. `manifests` — kubeconform validation of `deploy/k8s`.

Caching: Go modules/build cache, pnpm store, Docker layer cache. Concurrency cancels superseded runs.

## Rationale

- The repository is on GitHub; adding a GitLab remote only to satisfy the stack list adds operational overhead without learning value.
- GitHub Actions supports the same concepts (stages/jobs/services, artifacts, caches, protected branches) used in GitLab CI.
- A GitLab CI mirror can be added later; the YAML would map 1:1 (stages → jobs).

## Consequences

**Positive:** free for the repo; tight GitHub integration; PR checks and branch protection work natively.

**Negative:** the GitLab CI keyword is not literally present in the CI configuration; mitigated by documenting equivalent stages in the README and keeping the pipeline structure equivalent.

**Follow-up:** optionally add `.gitlab-ci.yml` mirror + GitLab remote in Phase 2 if a specific employer values it.
