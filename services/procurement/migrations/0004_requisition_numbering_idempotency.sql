-- 0004_requisition_numbering_idempotency: per-tenant PR number counter and
-- the submit idempotency key (issue #29). Owned by the procurement service
-- (ADR-0001); no other service writes here.

-- The counter row is the serialization point for number generation: the
-- store's UPDATE ... RETURNING inside the requisition INSERT transaction
-- takes the row lock, so concurrent creates in one tenant receive distinct
-- sequential numbers (PR-%06d). The row is initialized lazily from the
-- tenant's highest existing number, so a fresh counter never collides with
-- pre-counter data.
CREATE TABLE procurement.requisition_counters (
    tenant_id   uuid PRIMARY KEY,
    last_number bigint NOT NULL DEFAULT 0
);

-- Same RLS pattern as 0001/0003: tenant-scoped policy, forced, granted to the
-- non-superuser app role. 0001's ALTER DEFAULT PRIVILEGES already covers
-- tables created later by the migrating role; the explicit GRANT follows the
-- 0003 belt-and-braces pattern so the counter keeps working even if default
-- privileges were rebuilt by hand.

ALTER TABLE procurement.requisition_counters ENABLE ROW LEVEL SECURITY;
ALTER TABLE procurement.requisition_counters FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON procurement.requisition_counters
    USING (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    )
    WITH CHECK (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    );

GRANT SELECT, INSERT, UPDATE ON procurement.requisition_counters TO eproc_app;

-- Submit idempotency (Idempotency-Key header, optional): one key names one
-- submit per tenant. The partial unique index enforces that — reusing a key
-- for a different requisition's submit fails with 23505, which the store maps
-- to requisition_idempotency_key_conflict (HTTP 409). NULL keys (submits
-- without the header) never collide.
ALTER TABLE procurement.requisitions ADD COLUMN submit_idempotency_key text;

CREATE UNIQUE INDEX requisitions_tenant_submit_key_uidx
    ON procurement.requisitions (tenant_id, submit_idempotency_key)
    WHERE submit_idempotency_key IS NOT NULL;
