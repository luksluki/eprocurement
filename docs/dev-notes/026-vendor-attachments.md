# Dev note — #26 Vendor attachments via presigned uploads

## What changed

Bootstrapped the **documents** service (gRPC :8083) and the vendor
attachment flow end-to-end. Attachment bytes never stream through the API:
clients upload to and download from MinIO presigned URLs (15 min, SigV4),
while metadata lives in MongoDB `document_meta`.

- `proto/documents/v1` + `gen/go/documents/v1`: `CreateUpload`,
  `CompleteUpload`, `ListAttachments`, `GetDownloadUrl` (regenerated with
  `make gen-proto`).
- `services/documents`: `attachment` domain (declaration validation: mime
  allowlist pdf/png/jpeg/webp/docx/xlsx, size ≤ `ATTACHMENT_MAX_SIZE_MB`,
  filename 1–255 without separators, sha256 hex checksum; pending→ready
  lifecycle), Mongo store (tenant-scoped queries, `owner_lookup` +
  `reaper_pending` indexes), MinIO adapter implementing the `Storage` port,
  gRPC server on the shared `grpcserver` scaffolding with the RBAC
  re-check, and an in-service orphan **reaper** (pending > TTL → object +
  metadata removed).
- `services/gateway`: `POST /api/v1/vendors/:id/attachments` (vendor
  existence checked against procurement first), `.../complete`,
  `.../download`, and vendor detail now embeds `attachments`. Machine error
  codes pass through (`attachment_too_large` → 400, `attachment_not_found`
  → 404, `attachment_checksum_mismatch` → 412, …); `object_key` stays out
  of REST responses.
- Compose: `documents` container (depends mongo + minio healthy,
  grpc-health-probe) and `DOCUMENTS_ADDR` on the gateway. CI go job gained
  MongoDB and MinIO service containers. New dependency: `minio-go v7.0.83`
  (SigV4 presigning; pinned so `go 1.24.0` holds).

## Decisions

- **Verification at complete-time, not presign-time.** Presigned PUT cannot
  bind content-length/content-type server-side, so `CompleteUpload` is the
  enforcement point: StatObject (size == declared, content type allowed),
  then one streaming pass to sha256 the object. On mismatch the object is
  deleted but metadata stays pending — the client can re-upload correct
  bytes and complete again instead of re-declaring.
- **Presign host ≠ API host.** Presigned URLs embed the endpoint host, and
  the compose-internal `minio:9000` is unreachable from a browser on the
  host. `S3_PRESIGN_ENDPOINT` (default: same as `S3_ENDPOINT`) lets the
  service dial MinIO internally while minting URLs for the client-visible
  address (compose sets `localhost:9000`). SigV4 covers the Host header, so
  each URL is valid exactly for the host it was minted for.
- **Reaper inside documents, not worker.** PRD assigns cron to the worker
  (Redis locks, RabbitMQ consumers — later issues). A periodic sweep in the
  singleton documents container satisfies the AC now; moving it to the
  worker later touches only the loop's home, not the sweep logic.
- **Entity model generic, vendor wired.** Attachments are owned by
  `(entity_type, entity_id)`; `"vendor"` is the only accepted type today,
  so the service RBAC map uses `vendor:read`/`vendor:write` directly. New
  owners (requisitions, invoices) add an entity case + per-entity
  permission.
- **Object-key safety.** Entity types and ids are validated to
  `[A-Za-z0-9][A-Za-z0-9-]{0,63}` before becoming key segments
  (`attachments/{tenant}/{entity}/{uuid}`), blocking path injection from
  URL parameters.

## Evidence

- `go vet ./... && go build ./...` — clean; `pnpm build && pnpm lint && pnpm test` — clean.
- Integration (real MongoDB + MinIO, gated on `TEST_MONGO_URI` /
  `TEST_S3_ENDPOINT`): full flow test does a real HTTP PUT against the
  presigned URL (AC "upload completes directly to MinIO"), verifies
  complete→ready, list, and a presigned GET returning identical bytes;
  verification failures (missing object / size / mime / checksum) each map
  to their machine code; cross-tenant download → NotFound; viewer →
  PermissionDenied; anonymous → Unauthenticated; reaper removes only stale
  pending uploads. Gateway route tests cover upload URL + Content-Type
  header, unknown vendor 404, complete/download, vendor-detail embedding
  (and `object_key` absence), 403 without `vendor:write`.
- `docker compose config` OK; `docker compose up -d --wait --build` — all
  services healthy; live smoke through gateway → documents → MinIO
  documented in the PR body.

## Follow-ups

- Move the reaper loop to the worker when cron/Redis locks arrive.
- Per-entity permission map when requisition/invoice attachments land.
- Attachment delete endpoint (currently uploads only; deactivation follows
  vendor lifecycle semantics if needed by the UI).
