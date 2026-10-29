-- 0011_purchase_order_list_indexes: the status-filter index for the PO list
-- (issue #38). Direct parity with requisitions_tenant_status_idx (0003): the
-- list's status arm runs as an Index Only Scan over (tenant_id, status) once
-- the tenant's POs reach list scale; the number-prefix arm keeps using the
-- existing purchase_orders_tenant_number_uidx / heap scan (the #32 finding:
-- the RLS policy's bypass arm keeps tenant-leading index quals unavailable,
-- so prefix shapes scan anyway — see the query-plan section in
-- docs/schema/procurement.md).
--
-- No DROP needed: 0007 created no (tenant_id, status) index to supersede.

CREATE INDEX purchase_orders_tenant_status_idx
    ON procurement.purchase_orders (tenant_id, status);
