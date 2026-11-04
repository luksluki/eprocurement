# AI-assisted development workflow

This repository is built with AI coding agents (Claude Code, Cursor, Copilot, or similar) as part of the standard engineering workflow. The rules below keep AI output reviewable and production-safe.

> **Operational workflow:** coding agents follow [`AGENTS.md`](../AGENTS.md) at the repository root. This document defines the AI usage policy and review standards; `AGENTS.md` defines how work is executed (branching, gates, PRs, reporting).

## Principles

1. **Context before code.** Every task starts from an issue with acceptance criteria, plus the PRD and relevant ADRs. Prompts must include the issue number.
2. **Small diffs.** One issue → one branch → one PR. No opportunistic refactors in feature PRs.
3. **Tests are the referee.** AI-generated code is accepted only when tests (unit + at least one failure path) pass and the diff is reviewed.
4. **Human approval for risk.** Schema migrations, security-sensitive changes, CI/CD changes, and dependency upgrades require explicit owner review.
5. **No secrets, ever.** Prompts and code contain `.env.example` placeholders only. Never paste credentials into AI tools.

## Review checklist for AI-generated code

- [ ] Correctness: matches acceptance criteria; edge cases handled (empty, duplicate, concurrent).
- [ ] Security: input validation, authZ, tenant scoping, no injection, no secret leakage.
- [ ] Data: migrations reversible; indexes justified; no N+1; idempotency for async paths.
- [ ] Maintainability: fits existing architecture; naming consistent; no dead code or TODOs.
- [ ] Observability: spans/metrics for new endpoints/jobs; errors logged with `trace_id`.
- [ ] Tests: meaningful assertions; not snapshot-only; covers a failure mode.

## Traceability

- PR description records: issue link, what AI produced vs. what was changed manually, and how it was verified.
- `docs/dev-notes/` (created with M0) stores short notes for non-trivial issues: decision, trade-offs, evidence (test output, trace screenshots).
