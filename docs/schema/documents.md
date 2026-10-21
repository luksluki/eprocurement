# Documents service — attachments

Owns attachment metadata (MongoDB `document_meta`) and the MinIO object
lifecycle. Attachment bytes never stream through the gateway or this
service: clients upload to and download from presigned URLs (15-minute
validity, SigV4).

## Lifecycle

```text
declare → pending metadata + presigned PUT
   │  client PUTs bytes directly to MinIO (Content-Type: declared mime)
   ▼
complete → verify (size == declared, content type allowed, sha256 matches)
   │  ok → ready (uploaded_at set; presigned GET available)
   │  mismatch → object deleted, metadata stays pending (client may re-upload)
   ▼
reaper → pending older than ATTACHMENT_PENDING_TTL (default 24h):
         object removed best-effort, metadata dropped
```

- Object keys follow PRD §13: `attachments/{tenant_id}/{entity}/{uuid}`.
  Entity types and ids are validated to a path-safe charset before they
  become key segments; `vendor` is the only entity today (requisitions and
  invoices reuse the same shape later).
- Declarations are bounded: mime allowlist (pdf, png, jpeg, webp, docx,
  xlsx), `ATTACHMENT_MAX_SIZE_MB` (default 25), filename 1–255 chars with no
  path separators, checksum must be sha256 hex (lowercased on accept).
- `CompleteUpload` is idempotent (re-completing a ready attachment is a
  no-op). Verification failures keep the metadata pending so a client can
  upload correct bytes and complete again.
- Tenant scoping is by `tenant_id` in every query — cross-tenant reads are
  indistinguishable from missing (`attachment_not_found`). Only the reaper
  query sweeps across tenants.

## gRPC API (`documents.v1`, port 8083)

| Method | Permission | Notes |
|---|---|---|
| `CreateUpload` | `vendor:write` | validates the declaration, returns presigned PUT + pending metadata |
| `CompleteUpload` | `vendor:write` | verifies size/mime/checksum, flips to `ready` |
| `ListAttachments` | `vendor:read` | by owner (entity type + id), oldest first |
| `GetDownloadUrl` | `vendor:read` | ready attachments only, returns presigned GET |

Permissions are re-checked service-side from the propagated gRPC metadata
(`x-user-id` / `x-tenant-id` / `x-role`) — the same defense-in-depth map as
procurement. When new entity types arrive, the map moves to per-entity
permissions.

## REST surface (gateway)

| Route | Permission | Body / result |
|---|---|---|
| `POST /api/v1/vendors/:id/attachments` | `vendor:write` | `{filename, mime_type, size_bytes, checksum_sha256}` → `201` with `upload: {url, method, expires_at, headers}` |
| `POST .../attachments/:attachmentId/complete` | `vendor:write` | → attachment metadata, `status: ready` |
| `GET .../attachments/:attachmentId/download` | `vendor:read` | → `download: {url, expires_at}` |
| `GET /api/v1/vendors/:id` | `vendor:read` | vendor detail embeds `attachments: [...]` |

The gateway verifies the vendor exists (procurement) before any documents
call; `object_key` never appears in REST responses. Machine error codes
(`attachment_too_large`, `attachment_checksum_mismatch`, …) pass through
unchanged; validation → 400, missing → 404, lifecycle violations → 412.

## Storage

- **MongoDB** `document_meta` (`MONGO_URI` / `MONGO_DB`): indexes
  `owner_lookup` `{tenant_id, entity_type, entity_id, created_at}` and
  `reaper_pending` `{status, created_at}` (created on boot).
- **MinIO** (`S3_ENDPOINT` host:port, no scheme; `S3_USE_SSL` for TLS):
  bucket `attachments` (default, created on boot). Credentials:
  `S3_ACCESS_KEY` / `S3_SECRET_KEY` (= `MINIO_ROOT_*` locally).
- **Reaper job**: runs inside the documents service every
  `REAPER_INTERVAL` (default 30m; `0` disables). It is a singleton there —
  when the worker service takes over cron with Redis locks, the sweep moves
  with it.

## Environment

| Variable | Default | Purpose |
|---|---|---|
| `MONGO_URI` | `mongodb://eprocurement:eprocurement@localhost:27017/?authSource=admin` | MongoDB connection |
| `MONGO_DB` | `eprocurement` | database name |
| `S3_ENDPOINT` | `localhost:9000` | MinIO host:port, no scheme |
| `S3_PRESIGN_ENDPOINT` | *(empty = `S3_ENDPOINT`)* | host minted into presigned URLs when clients reach MinIO by a different address (compose: services dial `minio:9000`, clients outside use `localhost:9000`) |
| `S3_ACCESS_KEY` / `S3_SECRET_KEY` | `eprocurement` | object-store credentials |
| `S3_BUCKET` | `attachments` | bucket (created when missing) |
| `S3_USE_SSL` | `false` | TLS to the object store |
| `ATTACHMENT_MAX_SIZE_MB` | `25` | declaration size limit |
| `ATTACHMENT_PENDING_TTL` | `24h` | reaper threshold for pending uploads |
| `REAPER_INTERVAL` | `30m` | sweep cadence; `0` disables |

Integration tests run against real MongoDB + MinIO and are gated on
`TEST_MONGO_URI` + `TEST_S3_ENDPOINT` (set for local runs and in CI;
`go test ./...` skips them otherwise).
