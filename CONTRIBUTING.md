# Contributing — eProcurement (ELCODE)

This repository follows **Git Flow** and issue-driven development. Every change starts from an issue and lands through a pull request.

## Branching model

| Branch | Purpose | Rules |
|---|---|---|
| `main` | Release / demo | Protected. Only merges from `release/*` or `hotfix/*`. Tagged releases. |
| `develop` | Integration | Protected. Feature PRs land here. |
| `feature/<issue>-<slug>` | Feature work | Branch from `develop`; PR back to `develop`; squash merge. |
| `release/vX.Y.Z` | Release hardening | Branch from `develop`; merge to `main` + tag; back-merge to `develop`. |
| `hotfix/<slug>` | Production fix | Branch from `main`; merge to `main` + `develop`. |

Branch names must include the issue number, e.g. `feature/42-po-from-pr`.

## Commit messages

Use **Conventional Commits**:

```text
feat(procurement): create PO from approved requisition (#42)
fix(worker): dedupe history consumer by event_id (#57)
docs(adr): record architecture decision (#7)
chore(ci): cache pnpm and Go modules (#3)
```

Types: `feat`, `fix`, `docs`, `chore`, `refactor`, `test`, `perf`, `build`, `ci`.
Reference the issue number in the subject (trailer `(#N)`).

## Pull requests

Every PR must:

- [ ] Link the issue (`Closes #N` or `Part of #N`)
- [ ] Have a clear summary and screenshots/recordings for UI changes
- [ ] Include tests (unit / integration / e2e as applicable)
- [ ] Update docs/ADR when behaviour or architecture changes
- [ ] Pass CI (lint, unit tests, build; integration/e2e where relevant)
- [ ] Contain no secrets (use `.env.example` for samples)
- [ ] Be reviewed by a CODEOWNER before merge

Squash-merge feature PRs into `develop`. Release merges keep a merge commit.

## Definition of Done

- Acceptance criteria in the issue are met and demonstrated.
- Tests cover the new behaviour (happy path + at least one failure path).
- `README`/docs updated if setup or behaviour changed.
- Telemetry added for new endpoints/jobs (traces + metrics when applicable).
- No new TODO/FIXME without a linked issue.

## Labels & milestones

- `type:*` — epic, feature, bug, chore, adr
- `area:*` — platform, identity, procurement, documents, async, web, observability, testing, demo
- `priority:*` — P0 (must), P1 (should), P2 (nice)
- `size:*` — S (≤1 day), M (1–2 days), L (3+ days, split before starting)
- Milestones **M0–M5** map to the PRD delivery plan.

## Local checks (target, once M0 lands)

```bash
make lint        # go vet + eslint
make test        # go test ./... + vitest
make compose-up  # local stack
make seed        # demo data
```
