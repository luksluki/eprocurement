-- 0012_invoice_list_indexes: the status-filter index for the invoice list
-- (issue #41). Direct parity with requisitions_tenant_status_idx (0003) and
-- purchase_orders_tenant_status_idx (0011): the list's status arm runs as an
-- Index Only Scan over (tenant_id, status) once the tenant's invoices reach
-- list scale; the number-prefix arm keeps the tenant-leading scans (the #32
-- finding: the RLS policy's bypass arm keeps tenant-leading index quals
-- unavailable, so prefix shapes scan anyway — see the query-plan section in
-- docs/schema/procurement.md).
--
-- No DROP needed: 0009 created no (tenant_id, status) index to supersede.

CREATE INDEX invoices_tenant_status_idx
    ON procurement.invoices (tenant_id, status);
