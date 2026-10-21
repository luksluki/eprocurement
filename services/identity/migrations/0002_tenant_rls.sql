-- 0002_tenant_rls: row-level security on tenant-owned identity tables.
--
-- Every tenant-scoped query runs inside a transaction that sets
-- app.tenant_id (see internal/platform/tenant.WithTenant); policies compare
-- against it. FORCE applies the policies to the table owner as well.
--
-- identity's auth flows (login email lookup, refresh, logout) legitimately
-- span tenants: a caller's email must be searchable across tenants to
-- resolve the exactly-one-match rule. Those flows run inside a transaction
-- that sets app.rls_bypass=on (tenant.WithBypass) — a transaction-scoped,
-- application-controlled flag, auditable and never exposed to clients.

ALTER TABLE identity.users ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.users FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON identity.users
    USING (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    )
    WITH CHECK (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    );

-- Child rows are visible exactly when their parent user is visible under
-- the users policy (RLS applies inside policy subqueries too).

ALTER TABLE identity.user_roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.user_roles FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON identity.user_roles
    USING (
        EXISTS (SELECT 1 FROM identity.users u WHERE u.id = identity.user_roles.user_id)
        OR current_setting('app.rls_bypass', true) = 'on'
    );

ALTER TABLE identity.refresh_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.refresh_tokens FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON identity.refresh_tokens
    USING (
        EXISTS (SELECT 1 FROM identity.users u WHERE u.id = identity.refresh_tokens.user_id)
        OR current_setting('app.rls_bypass', true) = 'on'
    )
    WITH CHECK (
        EXISTS (SELECT 1 FROM identity.users u WHERE u.id = identity.refresh_tokens.user_id)
        OR current_setting('app.rls_bypass', true) = 'on'
    );
