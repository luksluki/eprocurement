CREATE SCHEMA IF NOT EXISTS demo;

CREATE TABLE IF NOT EXISTS demo.tenants (
    id text PRIMARY KEY,
    name text NOT NULL,
    slug text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS demo.users (
    id text PRIMARY KEY,
    tenant_id text NOT NULL,
    email text NOT NULL,
    role text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, email)
);

CREATE TABLE IF NOT EXISTS demo.vendors (
    id text PRIMARY KEY,
    tenant_id text NOT NULL,
    code text NOT NULL,
    name text NOT NULL,
    status text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, code)
);

CREATE TABLE IF NOT EXISTS demo.requisitions (
    id text PRIMARY KEY,
    tenant_id text NOT NULL,
    number text NOT NULL,
    requester_id text NOT NULL,
    status text NOT NULL,
    currency text NOT NULL,
    total_cents bigint NOT NULL,
    submitted_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS demo.requisition_items (
    id text PRIMARY KEY,
    requisition_id text NOT NULL,
    description text NOT NULL,
    qty integer NOT NULL,
    unit_price_cents bigint NOT NULL
);

CREATE TABLE IF NOT EXISTS demo.approval_steps (
    id text PRIMARY KEY,
    requisition_id text NOT NULL,
    step_no integer NOT NULL,
    role_required text NOT NULL,
    status text NOT NULL,
    decided_at timestamptz
);

CREATE TABLE IF NOT EXISTS demo.purchase_orders (
    id text PRIMARY KEY,
    tenant_id text NOT NULL,
    number text NOT NULL,
    requisition_id text NOT NULL,
    vendor_id text NOT NULL,
    status text NOT NULL,
    total_cents bigint NOT NULL,
    issued_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS demo.po_items (
    id text PRIMARY KEY,
    po_id text NOT NULL,
    description text NOT NULL,
    qty_ordered integer NOT NULL,
    qty_received integer NOT NULL,
    unit_price_cents bigint NOT NULL
);

CREATE TABLE IF NOT EXISTS demo.goods_receipts (
    id text PRIMARY KEY,
    tenant_id text NOT NULL,
    po_id text NOT NULL,
    number text NOT NULL,
    received_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS demo.invoices (
    id text PRIMARY KEY,
    tenant_id text NOT NULL,
    number text NOT NULL,
    po_id text NOT NULL,
    amount_cents bigint NOT NULL,
    status text NOT NULL,
    created_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_demo_requisitions_tenant_submitted ON demo.requisitions (tenant_id, submitted_at DESC);
CREATE INDEX IF NOT EXISTS idx_demo_requisition_items_req ON demo.requisition_items (requisition_id);
CREATE INDEX IF NOT EXISTS idx_demo_approval_steps_req ON demo.approval_steps (requisition_id);
CREATE INDEX IF NOT EXISTS idx_demo_purchase_orders_tenant_issued ON demo.purchase_orders (tenant_id, issued_at DESC);
CREATE INDEX IF NOT EXISTS idx_demo_po_items_po ON demo.po_items (po_id);
CREATE INDEX IF NOT EXISTS idx_demo_goods_receipts_po ON demo.goods_receipts (po_id);
CREATE INDEX IF NOT EXISTS idx_demo_invoices_tenant_created ON demo.invoices (tenant_id, created_at DESC);
