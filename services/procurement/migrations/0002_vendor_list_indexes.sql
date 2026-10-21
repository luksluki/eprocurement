-- 0002_vendor_list_indexes: search/filter indexes for the vendor list (#27).
--
-- Query shapes (always tenant-scoped by RLS):
--   status filter + ORDER BY code  -> (tenant_id, status, code) btree walk
--   code prefix (LIKE 'X%')        -> vendors_tenant_code_uidx range scan
--   name substring (ILIKE '%x%')   -> pg_trgm GIN on name
-- The (tenant_id, status, code) index supersedes 0001's (tenant_id, status).

CREATE EXTENSION IF NOT EXISTS pg_trgm;

DROP INDEX IF EXISTS procurement.vendors_tenant_status_idx;
CREATE INDEX vendors_tenant_status_code_idx ON procurement.vendors (tenant_id, status, code);
CREATE INDEX vendors_name_trgm_idx ON procurement.vendors USING gin (name gin_trgm_ops);
