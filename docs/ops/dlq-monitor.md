# DLQ monitor and admin runbook (#48)

`procurement.dlq` is the final destination of every message the #47
consumers give up on (PRD §14.5 "after max attempts → DLQ + admin
visible"): deliveries whose retry ladder (5 s → 30 s → 5 min, 3 retries)
was exhausted, and permanently invalid messages (undecodable body,
invalid envelope) that skip the ladder entirely. The topology and the
failure path are documented in
[`docs/schema/procurement.md`](../schema/procurement.md) (RabbitMQ
topology, #46).

Two surfaces make the DLQ visible:

- **Hourly depth summary (worker, #48)** — the worker logs one structured
  line per hour (configurable via `DLQ_SUMMARY_INTERVAL`, default `1h`):
  `{"msg":"dlq depth summary","depth":N}` — `INFO` at 0, `WARN` above 0.
  This is the interim metric surface; the Prometheus/OTel export of the
  same depth lands with the observability stack (#54, PRD §18).
- **Admin endpoint (gateway, #48)** — `GET /api/v1/dlq` and
  `POST /api/v1/dlq/replay`, behind Bearer auth + the admin-only
  `dlq:manage` permission (`docs/rbac.md`).

## Inspecting the DLQ

```bash
curl -H "Authorization: Bearer <admin-token>" \
  "http://localhost:8080/api/v1/dlq?limit=50"     # limit: default 50, max 500
```

```json
{
  "depth": 3,
  "scanned": 3,
  "messages": [
    {
      "message_id": "0198…", "decode_error": false,
      "event_id": "0198…", "tenant_id": "t-acme", "type": "pr.submitted",
      "occurred_at": "2026-10-28T10:00:00Z", "retry_count": 3,
      "original_queue": "documents.history", "reason": "rejected",
      "died_at": "2026-10-28T10:05:12Z"
    },
    {
      "message_id": "0199…", "decode_error": true, "retry_count": 0,
      "original_queue": "notifications", "reason": "rejected",
      "died_at": "2026-10-28T10:06:00Z"
    }
  ]
}
```

- `depth` — the DLQ's ready-message count at the moment of the call
  (point-in-time).
- `scanned` — how many messages the call drained and inspected (see the
  cost note below).
- Decoded entries (`decode_error: false`) are filtered to **the caller's
  tenant**. `retry_count` is the `x-retry-count` at death;
  `original_queue`/`reason`/`died_at` come from the broker's `x-death`
  trail.
- **Undecodable entries (`decode_error: true`) appear to every tenant,
  metadata only** — `message_id`, queues/reasons/times, no payload and no
  tenant. The body does not decode, so the tenant cannot be known
  (the worker's `Decode` is a pure `json.Unmarshal`); the deliberate
  trade-off is that visibility of poison messages outweighs the secrecy
  of their existence. The leak is existence-only.

### Cost and the 1000-message scan cap

Every list (and replay) **drains the whole DLQ**: messages are taken off
the queue unacknowledged, inspected, and requeued at the end — O(depth)
per call. A hard cap of **1000 scanned messages** bounds a pathological
DLQ: `scanned` reports the cap, `depth` still reports the true size, and
messages beyond the cap are simply not in that response. Treat the
endpoint as an operator tool, not a polling loop.

### Cross-replica race (retry on a surprising 404)

Sessions hold their drained messages unacknowledged, and `Basic.Get`
cannot see another session's unacked messages. Within one gateway
process a mutex serializes list/replay; **across replicas there is no
such lock**: if an admin holds a list/replay on instance A, a replay on
instance B may answer `404 dlq_message_not_found` for a message that
exists (and the list on B may under-report). Retry after a moment — the
holding session requeues everything when it finishes.

## Replaying a message

```bash
curl -X POST -H "Authorization: Bearer <admin-token>" \
  -H "Content-Type: application/json" \
  -d '{"message_id":"0198…"}' http://localhost:8080/api/v1/dlq/replay
```

`message_id` is the publisher's `event_id` (it rides the AMQP
`MessageId` property, so it survives even a broken body — take it from
the list response). Replay republishes the message to its original work
queue (`documents.history` / `notifications` / `exports`, from
`x-death`) with:

- **`x-retry-count` reset to 0** — a fresh full retry ladder
  (maintainer decision 2026-10-28),
- the stale `x-death` trail dropped,
- body, `MessageId`, and `ContentType` preserved verbatim.

The DLQ copy is acknowledged only after the republish succeeded, so a
failed publish leaves the DLQ untouched; consumers dedupe by `event_id`,
so a crash between publish and ack at worst replays a duplicate
(at-least-once, PRD §14.8).

Responses: `200` replayed · `404 dlq_message_not_found` (unknown id — or
**another tenant's message**: cross-tenant opacity) · `409
dlq_message_not_replayable` (**undecodable message** — replaying it
would only bounce it straight back to the DLQ; it is a permanent
failure) · `503 dlq_broker_unavailable`.

## Removing messages (purge)

The API deliberately has **no delete**: an undecodable message is
removed with broker tooling, after the underlying cause (usually a
publisher bug) is understood:

```bash
# inspect directly (mind the admin-tool race above)
docker exec eprocurement-rabbitmq-1 rabbitmqctl list_queues name messages messages_ready

# purge everything — the only removal path, all tenants at once
docker exec eprocurement-rabbitmq-1 rabbitmqctl purge_queue procurement.dlq
```

The RabbitMQ management UI (`localhost:15672`) offers the same per-queue
view and purge.

## Alerting thresholds (defaults — override welcome)

Layered on the worker's **hourly** summary cadence (PRD §16):

| Severity | Condition | Rationale |
|---|---|---|
| **WARN** | depth ≥ 1 persisting **> 1 hour** (two consecutive nonzero hourly summaries) | a single poisoned message is worth a look; one hour matches the summary cadence so it does not page at night |
| **CRITICAL** | depth ≥ 50 | the DLQ is accumulating faster than operators act — consumers or a dependency are down |

The `depth` field of `GET /api/v1/dlq` is the live value for ad-hoc
checks between summaries.
