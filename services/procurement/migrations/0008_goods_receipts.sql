-- 0008_goods_receipts: goods receipts with partial receipt rules (issue
-- #37, FR-07). Owned by the procurement service (ADR-0001); no other service
-- writes here. The goodsreceipt domain package owns validation and the
-- cumulative over-receipt guard; this schema is the storage mirror plus the
-- database backstops (CHECK qty > 0, UNIQUE (grn_id, po_item_id)).
--
-- Column deltas vs the PRD §13 summary rows (goods_receipts: id, tenant_id,
-- po_id, number, received_by, received_at, notes; goods_receipt_items: id,
-- grn_id, po_item_id, qty_received), documented in
-- docs/schema/procurement.md:
--   created_at              audit timestamp, same shape as the other
--                           procurement tables
--
-- Numbers follow the GRN-%06d counter-table pattern of 0004/0007: one row
-- per tenant in grn_counters, incremented with UPDATE ... RETURNING inside
-- the posting transaction so a failed post rolls the increment back and the
-- per-tenant sequence stays gap-free.

CREATE TABLE procurement.goods_receipts (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid NOT NULL,
    po_id        uuid NOT NULL REFERENCES procurement.purchase_orders (id),
    number       text NOT NULL,
    received_by  uuid NOT NULL,
    received_at  timestamptz NOT NULL,
    notes        text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Numbers are unique per tenant only (same rule as PR/PO numbers). No ON
-- DELETE CASCADE on po_id: goods receipts are financial records (the 3-way
-- match of #38 reads them); deleting a PO that has receipts must fail, not
-- cascade.
CREATE UNIQUE INDEX goods_receipts_tenant_number_uidx
    ON procurement.goods_receipts (tenant_id, number);
CREATE INDEX goods_receipts_tenant_idx ON procurement.goods_receipts (tenant_id);
CREATE INDEX goods_receipts_po_idx ON procurement.goods_receipts (po_id);

-- goods_receipt_items mirrors the other item tables (no tenant_id — PRD §13;
-- tenancy derives from the parent goods receipt, transitively from the PO).
-- UNIQUE (grn_id, po_item_id): one GRN records each PO line at most once;
-- further deliveries arrive as further GRNs. The qty CHECK is the database
-- twin of the domain's qty > 0 validation.
CREATE TABLE procurement.goods_receipt_items (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    grn_id       uuid NOT NULL REFERENCES procurement.goods_receipts (id) ON DELETE CASCADE,
    po_item_id   uuid NOT NULL REFERENCES procurement.po_items (id),
    qty_received integer NOT NULL CHECK (qty_received > 0),
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (grn_id, po_item_id)
);

CREATE INDEX goods_receipt_items_grn_idx ON procurement.goods_receipt_items (grn_id);
CREATE INDEX goods_receipt_items_po_item_idx ON procurement.goods_receipt_items (po_item_id);

-- Same counter pattern as 0004/0007: one row per tenant, the serialization
-- point for GRN number generation inside the posting transaction (a counter
-- table, not a native SEQUENCE — nextval() never rolls back, so a failed
-- post would burn a number and leave a gap; UPDATE ... RETURNING under the
-- row lock rolls the increment back with the transaction).
CREATE TABLE procurement.grn_counters (
    tenant_id   uuid PRIMARY KEY,
    last_number bigint NOT NULL DEFAULT 0
);

-- Same RLS pattern as 0001/0003/0007: tenant-scoped policies, forced, and
-- grants for the non-superuser app role. Items derive tenancy from the
-- parent goods receipt (transitively from the PO), so isolation stays
-- transitive.

ALTER TABLE procurement.goods_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE procurement.goods_receipts FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON procurement.goods_receipts
    USING (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    )
    WITH CHECK (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    );

ALTER TABLE procurement.goods_receipt_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE procurement.goods_receipt_items FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON procurement.goods_receipt_items
    USING (
        EXISTS (
            SELECT 1 FROM procurement.goods_receipts gr
            WHERE gr.id = goods_receipt_items.grn_id
              AND (
                gr.tenant_id::text = current_setting('app.tenant_id', true)
                OR current_setting('app.rls_bypass', true) = 'on'
              )
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM procurement.goods_receipts gr
            WHERE gr.id = goods_receipt_items.grn_id
              AND (
                gr.tenant_id::text = current_setting('app.tenant_id', true)
                OR current_setting('app.rls_bypass', true) = 'on'
              )
        )
    );

ALTER TABLE procurement.grn_counters ENABLE ROW LEVEL SECURITY;
ALTER TABLE procurement.grn_counters FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON procurement.grn_counters
    USING (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    )
    WITH CHECK (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    );

GRANT SELECT, INSERT, UPDATE ON procurement.goods_receipts TO eproc_app;
GRANT SELECT, INSERT, UPDATE ON procurement.goods_receipt_items TO eproc_app;
GRANT SELECT, INSERT, UPDATE ON procurement.grn_counters TO eproc_app;
