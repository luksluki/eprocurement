-- 0003_app_role: a non-superuser runtime role so RLS actually binds.
--
-- Superusers bypass row-level security even with FORCE, and the local/dev
-- stack connects as the bootstrap superuser (POSTGRES_USER). The tenant
-- helpers (internal/platform/tenant) therefore switch every transaction to
-- eproc_app via SET LOCAL ROLE; policies then apply. Migrations keep running
-- as the superuser — DDL like CREATE POLICY needs it.

DO $$
BEGIN
    CREATE ROLE eproc_app NOLOGIN NOSUPERUSER;
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

GRANT USAGE ON SCHEMA identity TO eproc_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA identity TO eproc_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA identity
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO eproc_app;

-- Let the bootstrap superuser become eproc_app inside transactions.
GRANT eproc_app TO eprocurement;
