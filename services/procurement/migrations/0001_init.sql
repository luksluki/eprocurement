-- 0001_init: procurement schema — vendors.
-- Owned by the procurement service (ADR-0001); no other service writes here.
-- Soft-delete only: deactivation sets status; rows are never deleted.

CREATE TABLE procurement.vendors (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL,
    code          text NOT NULL,
    name          text NOT NULL,
    contact_email text NOT NULL DEFAULT '',
    tax_id        text NOT NULL DEFAULT '',
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'deactivated')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    updated_by    uuid
);

-- Codes are unique per tenant only: two tenants may share a vendor code.
CREATE UNIQUE INDEX vendors_tenant_code_uidx ON procurement.vendors (tenant_id, code);
CREATE INDEX vendors_tenant_idx ON procurement.vendors (tenant_id);
CREATE INDEX vendors_tenant_status_idx ON procurement.vendors (tenant_id, status);

-- Same RLS pattern as identity (0002/0003 there): tenant-scoped policies,
-- forced, and the non-superuser app role so the policies actually bind.

ALTER TABLE procurement.vendors ENABLE ROW LEVEL SECURITY;
ALTER TABLE procurement.vendors FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON procurement.vendors
    USING (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    )
    WITH CHECK (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    );

DO $$
BEGIN
    CREATE ROLE eproc_app NOLOGIN NOSUPERUSER;
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

GRANT USAGE ON SCHEMA procurement TO eproc_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA procurement TO eproc_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA procurement
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO eproc_app;
GRANT eproc_app TO eprocurement;
