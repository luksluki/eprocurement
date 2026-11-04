-- 0003_requisitions: purchase requisitions and line items (issue #28).
-- Owned by the procurement service (ADR-0001); no other service writes here.
-- The requisition domain package owns the state machine; the status CHECK
-- mirrors the six statuses as defense in depth. Currency allowlist mirrors
-- the domain constants {IDR, USD} (FR-03, maintainer decision on #28).

CREATE TABLE procurement.requisitions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid NOT NULL,
    number       text NOT NULL,
    requester_id uuid NOT NULL,
    status       text NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'submitted', 'approved', 'rejected', 'cancelled', 'closed')),
    currency     text NOT NULL CHECK (currency IN ('IDR', 'USD')),
    total_cents  bigint NOT NULL DEFAULT 0,
    submitted_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid
);

-- Numbers are unique per tenant only (same rule as vendor codes).
CREATE UNIQUE INDEX requisitions_tenant_number_uidx ON procurement.requisitions (tenant_id, number);
CREATE INDEX requisitions_tenant_idx ON procurement.requisitions (tenant_id);
CREATE INDEX requisitions_tenant_status_idx ON procurement.requisitions (tenant_id, status);

CREATE TABLE procurement.requisition_items (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    requisition_id   uuid NOT NULL REFERENCES procurement.requisitions (id) ON DELETE CASCADE,
    description      text NOT NULL,
    qty              integer NOT NULL CHECK (qty > 0),
    unit_price_cents bigint NOT NULL CHECK (unit_price_cents >= 0),
    category         text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX requisition_items_requisition_idx ON procurement.requisition_items (requisition_id);

-- Same RLS pattern as 0001: tenant-scoped policies, forced, and grants for
-- the non-superuser app role so the policies actually bind. Items carry no
-- tenant_id (PRD §13); their policy derives tenancy from the parent
-- requisition, so isolation stays transitive.

ALTER TABLE procurement.requisitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE procurement.requisitions FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON procurement.requisitions
    USING (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    )
    WITH CHECK (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    );

ALTER TABLE procurement.requisition_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE procurement.requisition_items FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON procurement.requisition_items
    USING (
        EXISTS (
            SELECT 1 FROM procurement.requisitions r
            WHERE r.id = requisition_items.requisition_id
              AND (
                r.tenant_id::text = current_setting('app.tenant_id', true)
                OR current_setting('app.rls_bypass', true) = 'on'
              )
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM procurement.requisitions r
            WHERE r.id = requisition_items.requisition_id
              AND (
                r.tenant_id::text = current_setting('app.tenant_id', true)
                OR current_setting('app.rls_bypass', true) = 'on'
              )
        )
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON procurement.requisitions TO eproc_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON procurement.requisition_items TO eproc_app;
