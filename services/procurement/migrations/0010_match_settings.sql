-- 0010_match_settings: per-tenant three-way match tolerances (issue #40,
-- FR-08). Owned by the procurement service (ADR-0001); no other service
-- writes here. One row per tenant — the live configuration the #40 match
-- engine reads inside its transaction; absence of a row means the effective
-- defaults 0%/0% (strict), so a fresh tenant matches exactly without any
-- seeding step. The table follows the 0005 approval_rules pattern: RLS
-- tenant isolation forced, granted to the non-superuser app role.

CREATE TABLE procurement.match_settings (
    tenant_id  uuid PRIMARY KEY,
    qty_pct    integer NOT NULL CHECK (qty_pct BETWEEN 0 AND 100),
    price_pct  integer NOT NULL CHECK (price_pct BETWEEN 0 AND 100),
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by uuid
);

-- Same RLS pattern as 0005 approval_rules: tenant-scoped policy, forced,
-- granted to the non-superuser app role.

ALTER TABLE procurement.match_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE procurement.match_settings FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON procurement.match_settings
    USING (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    )
    WITH CHECK (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    );

GRANT SELECT, INSERT, UPDATE ON procurement.match_settings TO eproc_app;
