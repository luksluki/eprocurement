# ADR-0002 — CI on GitHub Actions (GitLab CI concepts preserved)

- **Status:** Accepted
- **Date:** 2026-10-20
- **Related:** PRD §20, JD technology stack (GitLab CI)

## Context

The job description lists **GitLab CI**, but the showcase repository is hosted on **GitHub** (`luksluki/eprocurement`). We need a CI pipeline that proves the same competencies: linting, unit tests, integration tests, image builds, e2e, and deployment gating.

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

**Negative:** JD keyword "GitLab CI" is not literally present in CI config files; mitigated by documenting the mapping in README and keeping pipeline structure equivalent (`docs/ci-mapping.md`, created with M0).

**Follow-up:** optionally add `.gitlab-ci.yml` mirror + GitLab remote in Phase 2 if a specific employer values it.
