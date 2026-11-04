-- 0005_approvals: approval rules and materialized approval steps (issue #30).
-- Owned by the procurement service (ADR-0001); no other service writes here.
-- Rules are the live per-tenant configuration (role + threshold + currency +
-- step_no); steps are the per-requisition snapshot produced at submit —
-- replacing rules never rewrites existing steps.

CREATE TABLE procurement.approval_rules (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid NOT NULL,
    currency        text NOT NULL CHECK (currency IN ('IDR', 'USD')),
    step_no         integer NOT NULL CHECK (step_no > 0),
    role_required   text NOT NULL CHECK (role_required IN ('admin', 'requester', 'approver', 'buyer', 'finance', 'viewer')),
    threshold_cents bigint NOT NULL CHECK (threshold_cents >= 0),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- One step_no per currency per tenant: the position in that currency's
-- approval chain. The same step_no may exist for IDR and USD.
CREATE UNIQUE INDEX approval_rules_tenant_currency_step_uidx
    ON procurement.approval_rules (tenant_id, currency, step_no);
CREATE INDEX approval_rules_tenant_idx ON procurement.approval_rules (tenant_id);

CREATE TABLE procurement.approval_steps (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    requisition_id  uuid NOT NULL REFERENCES procurement.requisitions (id) ON DELETE CASCADE,
    step_no         integer NOT NULL CHECK (step_no > 0),
    role_required   text NOT NULL,
    threshold_cents bigint NOT NULL,
    status          text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'rejected', 'skipped')),
    decided_by      uuid,
    decided_at      timestamptz,
    reason          text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- Steps of one requisition carry distinct positions (the materialized rule
-- set is unique per currency, and a submit matches exactly one currency).
CREATE UNIQUE INDEX approval_steps_requisition_stepno_uidx
    ON procurement.approval_steps (requisition_id, step_no);
CREATE INDEX approval_steps_requisition_idx ON procurement.approval_steps (requisition_id);

-- Same RLS pattern as 0001/0003: tenant-scoped policy, forced, granted to
-- the non-superuser app role. Rules carry tenant_id directly; steps follow
-- the requisition_items pattern — no tenant_id (PRD §13), tenancy derived
-- from the parent requisition so isolation stays transitive.

ALTER TABLE procurement.approval_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE procurement.approval_rules FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON procurement.approval_rules
    USING (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    )
    WITH CHECK (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    );

ALTER TABLE procurement.approval_steps ENABLE ROW LEVEL SECURITY;
ALTER TABLE procurement.approval_steps FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON procurement.approval_steps
    USING (
        EXISTS (
            SELECT 1 FROM procurement.requisitions r
            WHERE r.id = approval_steps.requisition_id
              AND (
                r.tenant_id::text = current_setting('app.tenant_id', true)
                OR current_setting('app.rls_bypass', true) = 'on'
              )
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM procurement.requisitions r
            WHERE r.id = approval_steps.requisition_id
              AND (
                r.tenant_id::text = current_setting('app.tenant_id', true)
                OR current_setting('app.rls_bypass', true) = 'on'
              )
        )
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON procurement.approval_rules TO eproc_app;
GRANT SELECT, INSERT, UPDATE ON procurement.approval_steps TO eproc_app;
