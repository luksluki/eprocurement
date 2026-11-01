# 057 — Go unit test baseline and mocks

- **Issue:** #57
- **Branch:** feature/57-go-unit-test-baseline
- **Date:** 2026-11-01

> **Provenance:** both maintainer comment rounds on issue #57 were read this
> session (`gh issue view 57 --comments`) and are applied as recorded:
>
> Round 1: **(1)** threshold = 80%, matching the identity precedent
> (Makefile `IDENTITY_COVERAGE_THRESHOLD`, ci.yml identity guard, dev-note
> 024:25); **(2)** procurement gated at 80% now, documents measured first
> (report-only); **(3)** the 2-minute gate becomes a tunable knob (default
> 120s) with the measured duration logged; **(4)** the api-layer interface
> extraction (8+ concrete stores) is OUT of scope — separate issue.
>
> Round 2: **(5)** the 120s knob applies to BOTH duration gates (make seed
> and the #57 unit suite) as one convention, both defaulting to 120s;
> **(6)** procurement threshold scope = the identity parity set (domain +
> server + config) pinned via an explicit coverpkg list, with the first CI
> number and the domain-only reading recorded in this note; **(7)** documents
> stays report-only with its coverpkg set pinned in writing (the 23.7%
> figure from round 1 was a different set — the 7-package set reads 16.7%);
> **(8)** proceed with `timeout-minutes: 15`; bump to 20 only if CI round 1
> nears the ceiling, flagged in the PR body.

## What

- **New domain unit tests (table-driven, no DB), closing the verified gaps:**
  - `services/procurement/vendor/vendor_test.go` (new): `Normalize` (trim +
    uppercase), `ValidateCreate` (code pattern with 2..32-char boundaries,
    name 2..200 after trim, optional email shape, first-error-wins),
    `ValidateUpdate` (empty-keeps-stored semantics, boundary lengths,
    precedence), `ValidateList` (status allowlist after trim, page clamp
    into [1, MaxPage], page_size default/cap, query uppercased + trimmed +
    LIKE-escaped), and `escapeLike` (metacharacter table). The DB-backed
    store suite (`store_test.go`, `store_list_test.go`) still covers the
    same rules end-to-end and skips without `TEST_DATABASE_URL`.
  - `services/procurement/config/config_test.go` (new): `Load` defaults,
    overrides, and `envOr`'s empty-value fallback — mirrors the
    `services/identity/config` precedent.
  - `services/procurement/approval/approval_test.go` (+16 lines):
    `TestCurrencyAllowed` — the rule-currency allowlist mirrors the
    requisition domain's {IDR, USD}, exact case-sensitive membership.
  - `services/procurement/approval/store_error_test.go` (new):
    `TestMapDecideKeyError` — pure error mapping tested with synthetic
    `*pgconn.PgError` values, no DB: the `decision_idempotency_pkey` 23505
    violation (also when wrapped) becomes `ErrIdempotencyKeyConflict`; other
    constraints, other codes, plain and nil errors pass through unchanged.
- **Mocks convention (unchanged, now documented):** fakes over deep mocks —
  the stdlib-only suite keeps using real structs and error values; no
  mocking dependency was added, and per maintainer decision 4 no interfaces
  were extracted for the store concrete types.
- **`scripts/dev/test-unit.sh` (new) + `make test-unit`:** the unit-suite
  duration gate — `go test ./...` with every `TEST_*` variable unset, wall
  time printed, exit 1 past `UNIT_SUITE_MAX_SECONDS` (default 120).
- **Seed duration gate:** `make seed` now measures its wall time and fails
  past `SEED_MAX_SECONDS` (default 120) — one convention with the unit-suite
  knob, nothing loosened (maintainer decision 5).
- **`make cover-procurement` + `PROCUREMENT_COVERAGE_THRESHOLD ?= 80`:** the
  procurement twin of `make cover`, over the parity set with the pinned
  11-package coverpkg list (approval, config, dashboard, goodsreceipt,
  invoice, match, outbox, purchaseorder, requisition, server, vendor — cmd
  wiring and the migrations FS package excluded). Needs the compose stack:
  the DB-backed store tests carry most of the covered lines.
- **CI (`.github/workflows/ci.yml`, Go job):** three steps added —
  1. *Unit suite (duration gate)*: runs `scripts/dev/test-unit.sh` with the
     five job-level `TEST_*` variables cleared at step level (integration
     helpers treat empty like unset), enforcing the 2-minute AC as a
     measured gate. The existing integration `Test` step is kept unchanged
     below it, so nothing that runs in CI today stops running.
  2. *Coverage (procurement parity set, threshold 80%)*: `go test
     ./services/procurement/... -count=1 -coverpkg=<pinned set>` **with the
     service containers** (job-level `TEST_DATABASE_URL`), same awk guard
     shape as the identity step.
  3. *Coverage (documents, report-only)*: unit-only (step-level cleared env)
     over the pinned 7-package set (attachment, config, export, history,
     notifications, objectstore, server), prints the total, **no guard**.
  `timeout-minutes: 15` unchanged (decision 8).
- `docs/onboarding.md`: §2.6 and the daily-targets table document
  `make test-unit`, `make cover-procurement`, and the CI report-only step.

## Decisions

1. **Maintainer decisions 1–8 applied as recorded on the issue** (see
   Provenance). Nothing was loosened: the seed gate keeps its 2-minute
   contract as the default, and the unit-suite knob starts at the same 120s.
2. **The unit-suite CI step is additive.** Removing the integration `Test`
   step would silently drop documents/worker/gateway/match integration
   coverage from CI — out of scope for #57. Cost: `go test ./...` runs twice
   in the Go job (gated unit run + integration run); the unit run is the
   cheap one (seconds with the warm build cache).
3. **The CI steps hardcode 80** exactly like the identity step they mirror;
   the Makefile carries the `?=` knobs for local overrides
   (`PROCUREMENT_COVERAGE_THRESHOLD`), keeping the two files consistent with
   the existing identity precedent rather than inventing a new wiring.
4. **`escapeLike`'s backslash pair is a pinned no-op, not a fix.** The
   replacer maps `\` → `\`, so a literal backslash in a user query is not
   escaped today (verified by running the replacer standalone and by
   `TestEscapeLike`). The fix (`\` → `\\`) changes LIKE matching semantics
   and belongs in its own issue with a DB-backed test; the unit test pins
   current behavior and its comment names the follow-up.
5. **Dev-note filename** follows the zero-padded convention pinned by
   dev-note 056 (`057-…`), which supersedes the shorter form used in the
   task description.
6. **No sleeps added; existing sleeps reported as-is.** The new tests are
   pure and deterministic. Domain time-dependence keeps the existing
   injected-clock convention (`requisition_test.go`'s `testNow`, the
   approval decide inputs). One planning caveat, corrected by measurement:
   the unit-only suite does contain pre-existing short sleeps (worker
   publisher/dlqmonitor/topology/consumer/export, identity/server,
   procurement/outbox — 2–250 ms async-settlement waits); the plan's claim
   that the only sleep lived in the DB-gated `gateway/dlq` integration test
   was inaccurate. They run in the suite today and the suite still measures
   ~3 s wall, so no action is taken in #57.

## Evidence

All commands run this session on the working tree (Windows, Git Bash, Go
1.24.0; compose stack healthy).

- `go test ./services/procurement/vendor ./services/procurement/config
  ./services/procurement/approval -count=1` — all three packages **ok**
  (unit-only; the DB-backed store tests skip). New tests first caught two
  wrong expectations of mine (`ValidateList` returns the input filter on
  error; a raw-literal miscount in the `escapeLike` table) — both fixed in
  the tests, no product code touched.
- **Documents coverage, unit-only, CI command:** `TEST_*=` (empty)
  `go test ./services/documents/... -count=1 -coverprofile=…
  -coverpkg=<7 packages>` → **total 16.7%** — matches the recorded baseline.
- **Procurement coverage, parity set with DB, CI command:** `go test
  ./services/procurement/... -count=1 -coverprofile=… -coverpkg=<11
  packages>` with `TEST_DATABASE_URL` → **total 88.1%** (pre-change parity
  set read 87.5%; the new tests moved it). Domain-only re-aggregation of the
  same profile (9 packages, server+config excluded, duplicate coverpkg
  blocks deduped) → **88.3%**. Both well above the 80% threshold. The same
  set measured unit-only (TEST_* empty, the new domain tests included)
  reads **23.3%** — the gap is why the gate must run with the DB (the
  CI comment carries the current figure).
- `make cover-procurement` → `total: 88.1%` + `OK: procurement coverage
  88.1% meets the 80% threshold`. Guard failure path proven against the
  same profile with `t=99`: `FAIL: procurement coverage 88.1% is below the
  99% threshold`, exit 1.
- `make test-unit` → `test-unit: unit suite took 3s (budget 120s)` +
  `PASS`, exit 0 (warm build cache; integration tests demonstrably skipped —
  the full DB-backed suite takes minutes, this ran in 3 s, and Go's test
  cache keys on the env the helpers read). Failure path:
  `UNIT_SUITE_MAX_SECONDS=0 bash scripts/dev/test-unit.sh` → `FAIL — unit
  suite took 1s, over the 0s budget`, exit 1 (checked without a pipe so the
  script's own status is read).
- `make seed` → `seeded tenants=3 … events=1000000 in 40.972s` +
  `seed: completed in 42s (budget 120s)` + `OK: seed within the 120s budget`.
  Over-budget path: `make seed SEED_MAX_SECONDS=1` → `FAIL: seed took 37s,
  over the 1s budget`, make exits non-zero.
- `bash -n scripts/dev/test-unit.sh` clean; `make -n seed`,
  `make -n cover-procurement`, `make -n test-unit` expansions inspected
  (`$$`/`$(…)` quoting correct); `.github/workflows/ci.yml` parses as YAML.
- `go vet` on the three touched packages clean; `gofmt -l` clean on the four
  touched Go files. (`gofmt -l` still flags pre-existing files on this CRLF
  checkout — config.go, vendor store/scale tests — untouched by this diff.)
- `git check-ignore coverage*.out` — coverage artifacts ignored.
- **Not run here:** the full `go vet ./...`/`go build ./...`/`go test ./...`
  and pnpm gates (the workflow script runs them after this ask), CI round 1
  itself, and the `docker compose config` gate (no compose file touched).

## Follow-ups

- **`escapeLike` backslash escaping** — a user query containing `\` is not
  matched literally today (and can even shift the match, e.g. `a\b`
  behaving like `ab` under PostgreSQL's default LIKE escape). One-line
  replacer fix + flipping the pinned test + a DB-backed literal-backslash
  list case; deserves its own small issue.
- **Documents coverage threshold** — once CI reports the 16.7% unit-only
  number on the pinned 7-package set for a few runs, open the follow-up
  that gates it at the real figure plus buffer (maintainer decision 2).
- **CI round 1 watch** — the Go job grows by the gated unit run (~seconds)
  and the procurement coverage run (~3 min measured locally, packages in
  parallel). Decision 8: if round 1 approaches the 15-minute ceiling, bump
  `timeout-minutes` to 20 and flag it in the PR body.
- **Interface extraction for the 8+ store concrete types** (out of scope by
  decision 4) — tracked as its own issue; until then the fakes-over-mocks
  convention stands as documented here.
- **Pre-existing short sleeps in the unit-only suite** (worker/identity
  packages, 2–250 ms) — fine at today's 3 s wall time; revisit only if the
  suite's duration knob ever becomes tight.
