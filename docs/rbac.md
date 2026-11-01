# RBAC model — roles, permissions, and enforcement

Single source of truth for the role/permission matrix: [`internal/platform/rbac`](../../internal/platform/rbac/rbac.go). The Go package and this document must stay in sync — the package's test suite (`TestMatrixCoversEveryPermission`, `TestEveryRoleAgainstMatrix`) enforces the code side.

## Roles

Fixed catalog (seeded in `identity.roles`): **admin, requester, approver, buyer, finance, viewer**. A user may hold several roles; any one of them granting the permission is enough.

## Permission matrix

`admin` implicitly holds every permission; the table lists the other roles per permission.

| Permission | requester | approver | buyer | finance | viewer |
|---|---|---|---|---|---|
| `requisition:read` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `requisition:write` | ✓ | | | | |
| `approval:decide` | | ✓ | | | |
| `approval:configure` | | | | | |
| `vendor:read` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `vendor:write` | ✓ | | | | |
| `po:read` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `po:write` | | | ✓ | | |
| `grn:post` | | | ✓ | | |
| `invoice:read` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `invoice:write` | | | | ✓ | |
| `match:configure` | | | | | |
| `export:run` | | | | ✓ | |
| `audit:read` | | | | | ✓ |
| `users:manage` | | | | | |
| `dlq:manage` | | | | | |
| `dashboard:read` | ✓ | ✓ | ✓ | ✓ | ✓ |

Read permissions are broad by design (everyone in the tenant sees the flow they participate in); writes are narrow. Ownership rules are domain checks on top of RBAC, enforced inside the owning service. Reads are tenant-wide: `requisition:read` (#32 list, #33 detail) has no ownership check — another same-tenant user's draft is readable, while its edits/submit are not. Landed so far:

- **requisition edit/submit (#29, maintainer decision 2026-10-23)** — the caller must be the requisition's `requester_id`; admins may act on any requisition of the tenant; violations return `403` with machine code `requisition_forbidden`.
- **approval decisions (#30)** — beyond `approval:decide`, the decider must not be the requisition's `requester_id` (self-approval is forbidden for everyone, admins included — `403 approval_self_forbidden`) and must hold the step's materialized `role_required` (`403 approval_role_forbidden`; admins excepted, mirroring `rbac.Check`). Because `approval:decide` is approver-only above, **a rule with role R is decided by a holder of R who also holds `approval:decide` (R + approver), or by an admin**. `approval:configure` (the rules API) is admin-only by design — PRD §5 puts approval-rule management on the tenant admin.
- **PO conversion (#35)** — `POST /api/v1/purchase-orders/from-requisition/{id}` is `po:write` (buyer/admin; no ownership rule — any buyer of the tenant converts). Beyond the permission, procurement enforces two domain gates inside the conversion transaction: the named `vendor_id` must be an **active** vendor of the tenant (`412 vendor_deactivated` / `404 vendor_not_found`), and the requisition must be `approved` (`409 po_requisition_not_approved`). Replaying a conversion returns the existing PO — the gates run on the creation path only.
- **PO lifecycle (#36)** — `POST /api/v1/purchase-orders/{id}/{issue|acknowledge|cancel|close}` and their gRPC twins are `po:write` (buyer/admin at both layers; no ownership rule — POs belong to the tenant, not a user). Beyond the permission, the domain enforces the FR-06 state machine: only `draft` issues (`409 po_illegal_transition` otherwise), only `issued` acknowledges, cancel works from draft/issued/acknowledged **only while nothing was received** (`409 po_has_receipts` once any `qty_received` > 0), and only `partially_received`/`received` close.
- **Goods receipts (#37)** — `POST /api/v1/purchase-orders/{id}/goods-receipts` and its gRPC twin `PostGoodsReceipt` are `grn:post` (buyer/admin at both layers; no ownership rule). Beyond the permission, the domain enforces FR-07 inside the posting transaction under the PO row lock: only `acknowledged`/`partially_received` POs receive (`409 po_illegal_transition` otherwise), the cumulative per-line guard rejects over-receipts (`409 grn_over_receipt: po_item=… received=… requested=… ordered=…`), and the shape guards answer 400 (`grn_items_empty`, `grn_qty_invalid`, `grn_po_item_duplicate`, `grn_notes_invalid`, `grn_po_item_unknown`).
- **PO reads (#38)** — `GET /api/v1/purchase-orders` and `GET /api/v1/purchase-orders/{id}` (plus the gRPC twins `ListPurchaseOrders`/`GetPurchaseOrder`) are the broad `po:read` — every role, tenant-wide, no ownership rule. The `/app/purchase-orders` screens hide the issue/acknowledge buttons and the goods-receipt form for non-buyers/non-admins purely client-side (`apps/web/src/lib/po-eligibility.ts` mirrors `po:write`/`grn:post` and the status edges); the gateway and the interceptor still enforce every gate on the actual calls, and the BFF proxies keep the token server-side.
- **Invoices (#39)** — `POST /api/v1/invoices`, its gRPC twin `CreateInvoice`, and the invoice attachment declare/complete routes are `invoice:write` (**finance/admin only** — the one money-records write no other role holds); `GET /api/v1/invoices/{id}`, `GetInvoice`, and attachment download are the broad `invoice:read`. Invoice attachments re-check the same area per entity in documents (`entityPermissions`). Beyond the permission, the domain enforces inside the creation transaction under the PO row lock: only `acknowledged`/`partially_received`/`received` POs are invoicable (`409 po_illegal_transition` otherwise — maintainer decision 2026-10-27), the cumulative strict guard rejects over-billing (`409 inv_over_bill: po_number=… invoiced=… requested=… total=…`), duplicates per `(tenant, vendor, number)` answer `409 inv_number_duplicate`, and the shape guards answer 400 (`inv_number_invalid`, `inv_items_empty`, `inv_qty_invalid`, `inv_unit_price_invalid`, `inv_po_item_duplicate`, `inv_po_item_unknown`). No ownership rule — invoices belong to the tenant.
- **Export run history (#49)** — `GET /api/v1/exports/runs` and `GET /api/v1/exports/runs/{id}/download-url` are `export:run` at the gateway, and their gRPC twins `ListExportRuns` / `GetExportDownloadUrl` sit in documents' `MethodPermissions` map — the **first real consumer of `export:run`** (finance/admin at both layers, defense in depth). No ownership rule — a tenant's run history belongs to the tenant; the store scopes every query by `tenant_id`, so another tenant's run id is indistinguishable from missing (`404 export_run_not_found`). The nightly export itself runs in the worker under a per-tenant Redis lock and needs no REST permission (no user acts); `POST /api/v1/exports` (manual run) stays with #53.

- **Three-way match (#40)** — `POST /api/v1/invoices/{id}/match` and its gRPC twin `MatchInvoice` reuse **`invoice:write`** (finance/admin at both layers) — the same money-records gate as creating the invoice (the finance reviewer persona, PRD §5); there is deliberately **no separate permission for the act of matching**. Beyond the permission, the domain enforces inside the match transaction under the PO row lock: a re-run on `matched` answers `409 inv_already_matched` (pending/discrepancy may be re-matched — a late GRN resolving a discrepancy), unknown invoices answer `404 inv_not_found` (also the cross-tenant answer, via RLS), and non-UUID ids `400 inv_id_invalid`. The tolerance surface `GET|PUT /api/v1/match-settings` and its gRPC twins `GetMatchSettings`/`ReplaceMatchSettings` are **`match:configure` — admin-only by design** (the `approval:configure` twin; maintainer decision 2026-10-28: tolerances live on the tenant admin's `/admin/settings` surface, PRD §11) — finance is denied too, and out-of-bound values answer `400 match_qty_pct_invalid` / `match_price_pct_invalid`.

- **DLQ admin (#48)** — `GET /api/v1/dlq` and `POST /api/v1/dlq/replay` are **`dlq:manage` — admin-only by design** (the `approval:configure`/`match:configure` pattern; poison messages are an operational surface, not a business role — maintainer decision 2026-10-28, issue #48 comments). There is no service-layer re-check: the endpoint is the gateway's own small AMQP client (option A), not a proxied gRPC call. Tenant scoping: decoded DLQ entries are filtered to the caller's tenant; **undecodable entries appear cross-tenant as metadata only** (`decode_error: true` — no payload, no tenant: the body does not decode, so the tenant cannot be known; visibility of poison messages outweighs the secrecy of their existence, and the leak is existence-only — documented in `docs/ops/dlq-monitor.md`). Replay of another tenant's (or an unknown) message answers `404 dlq_message_not_found` (cross-tenant opacity); replay of an undecodable message answers `409 dlq_message_not_replayable` (it can never succeed — removal goes through broker tooling per the runbook).

- **Invoice reads + UI (#41)** — `GET /api/v1/invoices` and its gRPC twin `ListInvoices` join the broad `invoice:read` (every role, tenant-wide; header-only rows — no lines, no match_result). The `/app/invoices` screens gate purely client-side through `apps/web/src/lib/invoice-eligibility.ts` (the #38 po-eligibility pattern): the Match button renders for `invoice:write` holders on a pending/discrepancy invoice and — per the maintainer decision 2026-10-29 — stays **disabled until an attachment reaches `ready` state** (a hard client-side gate; the server deliberately never checks, #40's decision), and the Create-invoice link from the PO detail renders for `invoice:write` holders on an invoicable PO. The gateway, the interceptor, and the domain still enforce every gate on the actual calls; the BFF proxies keep the token server-side.

- **Audit log (#44)** — `GET /api/v1/audit` and `GET /api/v1/audit/export` are **`audit:read` at both layers** (gateway `RequirePermission` + documents' `MethodPermissions` on `ListAudit`/`ExportAudit`) — **viewer + admin only**, the permission's first real consumer, per the maintainer decision on issue #44. No ownership rule — a tenant's decision log belongs to the tenant; the store scopes every query by `tenant_id`, so cross-tenant reads are invisible (empty result). The audit surface is read-only over an append-only collection (PRD §17). The web nav entry renders only for `audit:read` holders (`layout.tsx`), and the page itself answers the gateway's 403 with a dedicated sentence for anyone reaching it by URL.

- **Dashboard (#52)** — `GET /api/v1/dashboard/summary` and its gRPC twin `GetDashboard` are the broad **`dashboard:read`** — every role, tenant-wide. The widget aggregates only reads the roles already hold (pending approval steps over submitted requisitions, the PO register behind the spend windows, the PO status distribution — all behind `requisition:read`/`po:read` individually), so the landing page (PRD §11) is as broad as its ingredients; no ownership rule applies. The store scopes every query by tenant (RLS via `WithTenant`), so a cross-tenant dashboard is an empty snapshot, not a leak.

## Enforcement layers (defense in depth)

1. **Gateway middleware** (`services/gateway/server`): after Bearer-token verification, `RequirePermission("<permission>")` checks the token's roles. Failure → `403` with a machine-readable body:

   ```json
   {"error": "forbidden", "code": "permission_denied", "required": "vendor:write"}
   ```

2. **Service-layer gRPC interceptor** (`internal/platform/rbac.UnaryInterceptor`): each service maps its user-facing methods to required permissions (`MethodPermissions`); the interceptor re-checks the roles the gateway propagated in gRPC metadata (`x-role`, with `x-user-id` / `x-tenant-id` alongside). Methods absent from the map are internal or pre-auth. Failures:
   - protected method called without caller metadata → `Unauthenticated` (`auth context missing`)
   - roles insufficient → `PermissionDenied` (`permission_denied: requires <permission>`)

   One documented exception (#33): the documents attachment methods are absent from that service's map **by design** — their permission depends on the attachment's entity type (`vendor:read`/`write` vs `requisition:read`/`write`), which the interceptor cannot see. Each handler re-checks the per-entity permission itself (`services/documents/server/api.go`, `entityPermissions` table) as soon as the entity type is known; the gateway routes enforce the same permission at the edge, so both layers still check.

3. **PostgreSQL RLS** — tenant tables run under `FORCE ROW LEVEL SECURITY`; policies key on the transaction-local `app.tenant_id` set by `internal/platform/tenant.WithTenant` (#23). The gateway injects the token's `tid` into gRPC metadata (`rbac.OutgoingContext`), services read it back (`rbac.FromMetadata`), and every tenant-scoped query runs through `WithTenant` as the non-superuser `eproc_app` role.

The gateway never sees credentials and the services never see the JWT — only the verified claims, so neither layer can be bypassed by the other trusting too much.

## Adding or changing a permission

1. Add/change the entry in the `matrix` slice in `internal/platform/rbac/rbac.go` and in `AllPermissions` (constants).
2. Mirror the change in the table above and in `rbac_test.go`'s `wantPermission`.
3. Register the middleware on the gateway route and the method in the owning service's `MethodPermissions`.

`TestMatrixCoversEveryPermission` fails if the code matrix and the test's documented expectations diverge.
