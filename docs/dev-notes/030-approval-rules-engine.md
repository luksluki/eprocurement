# 030 — Approval rules and engine

- **Issue:** #30
- **Branch:** feature/30-approval-rules-engine
- **Date:** 2026-10-24

> **Provenance:** the maintainer was unavailable while #30 was prepared, so
> the four open questions in the issue comments were decided by the agent on
> 2026-10-24 ("Decisions applied by agent 2026-10-24, maintainer unavailable
> — override welcome in PR review"). The applied decisions — 412
> `approval_rules_missing` with full submit rollback, the universal
> self-approval ban (no admin exception), the inclusive threshold lower
> bound, and reason-required-on-reject — plus the judgment calls listed
> below are all open to override in PR review.

## What

- **`services/procurement/approval`** (new) — the approval engine:
  - `approval.go` (pure domain): `Rule` / `Step` types with machine-code sentinels, `ValidateRuleSet` (currency ∈ {IDR, USD}, role in the six-role catalog, threshold ≥ 0, step_no > 0 and unique per currency), `Materialize` (matrix evaluation), and `Decide` (the sequential decide flow with self/role/state checks). Step aggregates record `approval_step_approved` / `approval_step_rejected` events (`Events()`/`PullEvents()`); terminal transitions call the existing requisition `Approve`/`Reject`.
  - `store.go`: `ListRules` / `ReplaceRules` (whole-set replace in one tx), `MaterializeTx` (runs inside the requisition store's submit transaction), `ListSteps`, and `Decide` (one `tenant.WithTenant` tx: read step → lock parent requisition `FOR UPDATE` via the newly exported `requisition.LoadForUpdate` → reload steps → domain re-check → guarded writes). Cross-tenant step ids are invisible (transitive RLS) → `approval_step_not_found`.
- **Migration `0005_approvals.sql`** — `procurement.approval_rules` (tenant_id + currency/step_no unique) and `procurement.approval_steps` (snapshot columns, `pending|approved|rejected|skipped`, unique `(requisition_id, step_no)`); RLS on both — rules keyed on `tenant_id`, steps deriving tenancy from the parent requisition (the `requisition_items` pattern).
- **Submit integration** — `requisition.Store.Submit` gained a `materialize func(tx, Requisition) error` callback invoked inside the submit transaction after the successful draft → submitted UPDATE (never on a replay). The API wires `approvals.MaterializeTx` into it: no applicable rule → `approval_rules_missing` (gRPC `FailedPrecondition` → HTTP 412) and the **whole submit rolls back** — draft status, no steps, and the idempotency key is not recorded (it is only written by the successful UPDATE), so the same key later names a fresh submit. The pinned replay contract for successful submits is unchanged.
- **proto `procurement.v1`** — `ApprovalStep` / `ApprovalRule` messages; additive `repeated ApprovalStep approval_steps = 14` on `Requisition`; `DecideApproval`, `GetApprovalRules`, `ReplaceApprovalRules` RPCs; `gen/go` regenerated (`make gen-proto`) and committed.
- **procurement server** — the three gRPC methods; `methodPermissions` maps decide → `approval:decide` and the rules pair → `approval:configure`; `mapApprovalError` plus a `FailedPrecondition` arm for `approval_rules_missing` in `mapRequisitionError`; submit/decide responses embed the steps (ids in-band). `main.go` wires the approval store.
- **rbac** — new `approval:configure` permission, admin-only by matrix (`{ApprovalConfigure, nil}`), following the documented 3-step process (matrix + `AllPermissions`, `rbac_test.go` `wantPermission`, route/method registration).
- **gateway** — `services/gateway/server/approvals.go`: `POST /api/v1/approvals/:stepId/decide` (PRD §15 path) behind `approval:decide` with edge validation (decision ∈ {approve, reject} after trim+lowercase; reject requires a non-blank reason; reason ≤ 1000 chars — re-checked in the service layer), and `GET`/`PUT /api/v1/approval-rules` behind `approval:configure` with edge normalization (currency upper, role lower). `ProcurementClient` widened to the three new methods; `requisitionJSON` renders `approval_steps`.
- **Docs** — `docs/schema/procurement.md` (0005 tables, approval lifecycle, error-code tables, submit section) and `docs/rbac.md` (matrix row, landed-rules paragraph).

## Decisions

- **The four issue decisions were applied as commented** (provenance above): 412 + rollback on missing rules; self-approval forbidden for everyone including admins; threshold lower bound inclusive (total == threshold materializes the step); reason required on reject only.
- **Matrix evaluation, not highest-threshold** — every matching-currency rule at or below the total becomes a step, ordered by `step_no`. The domain test table distinguishes the two readings explicitly (total == threshold → in; total == threshold−1 → out; above two thresholds → two ordered steps).
- **Admin exemption from `role_required` follows precedent** (`rbac.Check` grants everything to admin; `requireOwner` exempts admins) and fulfills the docs/rbac.md promise that approver-scope rules arrive with the approval service. Consequence, now documented in docs/rbac.md and docs/schema/procurement.md: since `approval:decide` is approver-only in the matrix, a rule with role R is decided by a holder of R **and** approver, or by an admin. The self-approval ban is deliberately NOT admin-exempt (agent decision #2).
- **Materialization runs inside the submit transaction via a callback** (`func(tx pgx.Tx, requisition.Requisition) error`) — dependency inversion that keeps the requisition and approval packages decoupled (approval imports requisition for the decide orchestration; requisition never imports approval). A materializer error aborts the submit tx before the key is durably recorded.
- **Steps ride in-band** (`approval_steps` on the submit/decide responses): the plan's answer to "step ids without a read endpoint". Demo flow: POST requisitions → submit (response carries step ids) → POST approvals/{stepId}/decide with the id from the previous response. Read endpoints/timeline/queue stay with #31/#32-34.
- **Decisions serialize on the requisition row lock** — the decide tx reuses `FOR UPDATE` on the parent requisition (the submit/edit serialization point), re-checks status/current-step after the lock, and guards the writes (`WHERE status = 'pending'` / `WHERE status = 'submitted'`). A parallel store test proves two concurrent decides on one step yield exactly one winner + one `approval_step_not_current`. **Decide-vs-cancel is not tested**: the cancel endpoint does not exist yet (domain-only), so the API race is impossible today; when the endpoint arrives it must take the same lock.
- **Judgment calls open to override**: admin exemption from `role_required`; the rules-config API shape (GET/PUT whole-set replace, admin-only); the `currency` column on rules (matrix per currency rather than a single chain); `skipped` as the downstream status on reject; reason length bound of 1000.
- **Dev-note filename is `030-…`** (zero-padded), matching the repo's convention and the plan's artifact list.
- Out of scope, per #31/#45: audit log, notifications (outbox), timeline, idempotent decide, step read/queue endpoints. Events are recorded on the aggregates and not persisted yet.

## Evidence

All against a scratch PostgreSQL 17 on the local instance (database `eproc_test_30`, throwaway `eprocurement` role with CI-parity privileges — the CI postgres superuser; migrations 0001–0005 pre-applied via `go run ./services/procurement/cmd/migrate` because `dbmigrate` has no advisory lock and CI applies migrations before `go test`):

- `make gen-proto PROTOC=.tools/protoc/bin/protoc.exe` — regenerated `gen/go/procurement/v1/{procurement.pb.go,procurement_grpc.pb.go}` cleanly; only procurement files changed.
- `go vet ./... && go build ./...` — clean.
- `go test ./internal/platform/rbac/ -count=1` — green, including the extended `wantPermission` matrix (`approval:configure` admin-only).
- `TEST_DATABASE_URL=… go test ./services/procurement/approval/ -count=1` — green: domain suite (matrix table with the inclusive/exclusive boundary cases, rule validation, approve flow, reject-skips flow, role matrix, universal self-approval ban, decision/reason validation, lookup/state guards, event pull semantics) plus store integration (rules lifecycle + validation + cross-tenant invisibility, **412-rollback sequence: submit+key → rules missing → still draft/no steps → configure → same key submits fresh → replay returns current state**, snapshot semantics after rule replace, sequential approve/reject flows with persisted statuses and decision fields, authorization suite incl. cross-tenant 404, **2-goroutine concurrent decide → exactly one winner**).
- `TEST_DATABASE_URL=… go test ./services/procurement/requisition/ -count=1` — green: the #28/#29 suites (updated for the `Submit` callback) plus the new rollback test (materializer failure → draft, no steps, key not recorded, same key then names the fresh submit, replay idempotent).
- `TEST_DATABASE_URL=… go test ./services/procurement/server/ -count=1 -run 'TestRequisition|TestApproval|TestMap'` — green: rules gRPC (permissions, normalization, validation codes, tenant isolation), the end-to-end flow (submit without rules → `FailedPrecondition approval_rules_missing` → configure → same key → submitted with two in-band step ids → replay unchanged → viewer 403 → self-approval 403 for approver/admin/requester+approver → out-of-order 409 → pure-approver-on-finance-step 403 → decision/reason 400s → dual-role approve → requisition approved → terminal 409 → cross-tenant 404), and the reject flow (admin rejects, downstream skipped, dead-flow 409s). `TestVendorGRPCList` failed once mid-session when runs repeated against the dirty database — the pre-existing cleanup leak documented in dev-note 029 (not touched here); after a manual clean the **full** suite passed three consecutive runs, with only the tolerated `G-01` leftover the lifecycle test pre-cleans itself.
- `go test ./services/gateway/... -count=1` — green: decide routing through `server.New` (steps in-band, decision/reason normalization, claims propagation), edge-validation table (400s without a procurement call), rules permissions (approver/requester/finance 403 before any call; admin GET/PUT with normalization + echo), and the error-mapping table (412/403/409/404/400 with machine codes preserved).
- `gofmt -l` clean on every touched file.
- pnpm build/lint/test not run by me (no TypeScript touched); the workflow script re-runs the full gate set.
- Full gate set (`go vet/build/test ./...`, `pnpm build/lint/test`, compose) re-run by the workflow script after this dev note.

## Follow-ups

- #31: approval audit log, notifications, idempotent decide; #32-34: read endpoints (steps/timeline/approval queue) — the in-band steps are the interim answer for demos.
- #45 outbox: step and requisition events are in-memory only (`Events()`/`PullEvents()` unpersisted).
- When the cancel endpoint arrives, it must take the same requisition row lock as submit/edit/decide (already documented in the lifecycle section).
- Pre-existing (from dev-note 029, unchanged): the procurement server test cleanup leak that makes `TestVendorGRPCList` fail on repeat runs against one database; `go test -race` still unrunnable locally (no cgo).
- `dbmigrate` has no advisory lock — concurrent first-boot migrators can race (worked around in tests by pre-applying; CI applies migrations before tests). A `pg_advisory_lock` in `Apply` would be a small platform fix.
