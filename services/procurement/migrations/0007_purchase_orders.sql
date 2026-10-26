-- 0007_purchase_orders: purchase orders created from approved requisitions
-- (issue #35). Owned by the procurement service (ADR-0001); no other service
-- writes here. The purchaseorder domain package owns creation; the PO
-- lifecycle transitions (issue → acknowledged → … → closed/cancelled, FR-06)
-- belong to #36 and only move `status` / `issued_at` from here.
--
-- Column deltas vs the PRD §13 summary row (id, tenant_id, number,
-- requisition_id, vendor_id, status, total, issued_at), documented in
-- docs/schema/procurement.md:
--   currency                the requisition's currency (IDR | USD) — §13 omits
--                           it, but every money field needs its unit; copied
--                           verbatim on conversion
--   total_cents             the §13 "total" as integer cents (same convention
--                           as requisitions.total_cents)
--   created_at/by, updated_at/by   audit fields, same shape as vendors and
--                           requisitions
--
-- The status CHECK carries all seven FR-06 lifecycle statuses from day one
-- ('draft' lands with #35; the transitions are #36's), so #36 never needs an
-- ALTER CHECK. Seed's demo POs (scripts/seed/main.go poStatuses) have no
-- 'draft' — they are demo.* rows that never pass through this table.

CREATE TABLE procurement.purchase_orders (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid NOT NULL,
    number         text NOT NULL,
    requisition_id uuid NOT NULL REFERENCES procurement.requisitions (id),
    vendor_id      uuid NOT NULL REFERENCES procurement.vendors (id),
    status         text NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'issued', 'acknowledged', 'partially_received',
                          'received', 'closed', 'cancelled')),
    currency       text NOT NULL CHECK (currency IN ('IDR', 'USD')),
    total_cents    bigint NOT NULL DEFAULT 0,
    issued_at      timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    created_by     uuid,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    updated_by     uuid
);

-- Numbers are unique per tenant only (same rule as vendor codes and PR
-- numbers). The tenant+requisition unique index is the "cannot convert
-- twice" rule (FR-05) as a database guarantee: concurrent conversions of one
-- requisition serialize on the requisition row lock inside the conversion
-- transaction; this index is the belt-and-braces backstop that keeps the
-- invariant true even under a hypothetical lock-less writer.
--
-- No ON DELETE CASCADE on requisition_id: purchase orders are financial
-- records; deleting a requisition that was converted must fail, not cascade.
CREATE UNIQUE INDEX purchase_orders_tenant_number_uidx
    ON procurement.purchase_orders (tenant_id, number);
CREATE UNIQUE INDEX purchase_orders_tenant_requisition_uidx
    ON procurement.purchase_orders (tenant_id, requisition_id);
CREATE INDEX purchase_orders_tenant_idx ON procurement.purchase_orders (tenant_id);

-- po_items mirrors requisition_items (no tenant_id — PRD §13; tenancy derives
-- from the parent purchase order, exactly like requisition_items and
-- approval_steps). Lines are copied verbatim from requisition_items on
-- conversion; qty_received starts at 0 and is advanced by goods receipts
-- (#37). §13's unit_price lands as unit_price_cents (integer cents).
CREATE TABLE procurement.po_items (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    po_id            uuid NOT NULL REFERENCES procurement.purchase_orders (id) ON DELETE CASCADE,
    description      text NOT NULL,
    qty_ordered      integer NOT NULL CHECK (qty_ordered > 0),
    qty_received     integer NOT NULL DEFAULT 0 CHECK (qty_received >= 0),
    unit_price_cents bigint NOT NULL CHECK (unit_price_cents >= 0),
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX po_items_po_idx ON procurement.po_items (po_id);

-- Same counter pattern as 0004: one row per tenant, the serialization point
-- for PO number generation inside the conversion transaction. A counter
-- table, not a native PostgreSQL SEQUENCE: nextval() is not transactional
-- (it never rolls back), so a conversion that fails after allocation (vendor
-- deactivated, requisition not approved) would burn a number and leave a gap
-- in the per-tenant sequence (FR-05). UPDATE ... RETURNING under the row lock
-- rolls the increment back with the transaction, keeping numbers gap-free.
CREATE TABLE procurement.po_counters (
    tenant_id   uuid PRIMARY KEY,
    last_number bigint NOT NULL DEFAULT 0
);

-- Same RLS pattern as 0001/0003/0004: tenant-scoped policies, forced, and
-- grants for the non-superuser app role. Items derive tenancy from the parent
-- purchase order, so isolation stays transitive.

ALTER TABLE procurement.purchase_orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE procurement.purchase_orders FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON procurement.purchase_orders
    USING (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    )
    WITH CHECK (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    );

ALTER TABLE procurement.po_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE procurement.po_items FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON procurement.po_items
    USING (
        EXISTS (
            SELECT 1 FROM procurement.purchase_orders po
            WHERE po.id = po_items.po_id
              AND (
                po.tenant_id::text = current_setting('app.tenant_id', true)
                OR current_setting('app.rls_bypass', true) = 'on'
              )
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM procurement.purchase_orders po
            WHERE po.id = po_items.po_id
              AND (
                po.tenant_id::text = current_setting('app.tenant_id', true)
                OR current_setting('app.rls_bypass', true) = 'on'
              )
        )
    );

ALTER TABLE procurement.po_counters ENABLE ROW LEVEL SECURITY;
ALTER TABLE procurement.po_counters FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON procurement.po_counters
    USING (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    )
    WITH CHECK (
        tenant_id::text = current_setting('app.tenant_id', true)
        OR current_setting('app.rls_bypass', true) = 'on'
    );

GRANT SELECT, INSERT, UPDATE ON procurement.purchase_orders TO eproc_app;
GRANT SELECT, INSERT, UPDATE ON procurement.po_items TO eproc_app;
GRANT SELECT, INSERT, UPDATE ON procurement.po_counters TO eproc_app;
