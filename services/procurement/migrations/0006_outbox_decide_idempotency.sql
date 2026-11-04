-- 0006_outbox_decide_idempotency: transactional outbox + idempotent decide
-- (issue #31). procurement.outbox is the persistent half of the transactional
-- outbox (PRD §14.3, ADR-0001): state writes and their events commit
-- atomically in one transaction; the publisher loop that drains the table to
-- RabbitMQ stays with #45.
--
-- Column deltas vs the PRD §13 summary row (id, tenant_id, event_type,
-- payload JSONB, created_at, published_at), documented in
-- docs/schema/procurement.md:
--   version        the envelope schema version (PRD §14.5) as a column, so
--                  the #45 publisher assembles the envelope without parsing
--                  the payload JSON
--   actor_id       envelope field (PRD §14.5) as a column
--   trace_id       envelope field (PRD §14.5) as a column ('' when the caller
--                  propagated no trace)
--   occurred_at    envelope field (PRD §14.5) as a column
--   entity_type /  filter keys for GET /api/v1/events?entityType=&entityId=
--   entity_id      (PRD §15) and the timeline projection (#43) — a conscious
--                  extension beyond §13/§14.5

CREATE TABLE procurement.outbox (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),  -- = event_id (PRD §14.5)
    tenant_id    uuid NOT NULL,
    event_type   text NOT NULL,
    version      integer NOT NULL DEFAULT 1 CHECK (version > 0),
    payload      jsonb NOT NULL,
    actor_id     uuid,
    trace_id     text NOT NULL DEFAULT '',
    entity_type  text NOT NULL,
    entity_id    uuid NOT NULL,
    occurred_at  timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);

-- The #45 publisher polls unpublished rows oldest-first in batches; rows are
-- never deleted, published_at marks them done.
CREATE INDEX outbox_unpublished_idx ON procurement.outbox (created_at)
    WHERE published_at IS NULL;

-- Same RLS pattern as 0001/0003: tenant-scoped policy, forced, granted to
-- the non-superuser app role. The publisher (#45) reads through the
-- transaction-scoped app.rls_bypass arm like every other cross-tenant flow.

ALTER TABLE procurement.outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE procurement.outbox FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON procurement.outbox
    USING (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    )
    WITH CHECK (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    );

-- Insert for the decide writer; SELECT + UPDATE (published_at) for the #45
-- publisher; no DELETE — the outbox is append-only history.

GRANT SELECT, INSERT, UPDATE ON procurement.outbox TO eproc_app;

-- procurement.decision_idempotency: one row per recorded decide idempotency
-- key, inserted inside the decide transaction only on its success path (the
-- same semantics as requisitions.submit_idempotency_key). A dedicated table
-- rather than a key column on approval_steps: the key names a request, not a
-- step state, and its uniqueness scope is the tenant — approval_steps
-- deliberately carries no tenant_id (0005, PRD §13), so a unique key column
-- there would let a cross-tenant 23505 leak which keys exist in other
-- tenants. The composite PK keeps key existence tenant-scoped.

CREATE TABLE procurement.decision_idempotency (
    tenant_id       uuid NOT NULL,
    idempotency_key text NOT NULL,
    step_id         uuid NOT NULL REFERENCES procurement.approval_steps (id) ON DELETE CASCADE,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, idempotency_key)
);

ALTER TABLE procurement.decision_idempotency ENABLE ROW LEVEL SECURITY;
ALTER TABLE procurement.decision_idempotency FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON procurement.decision_idempotency
    USING (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    )
    WITH CHECK (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    );

GRANT SELECT, INSERT ON procurement.decision_idempotency TO eproc_app;
