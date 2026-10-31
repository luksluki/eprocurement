-- 0014_dashboard_read_indexes: the read indexes for the dashboard summary
-- (issue #52, FR-09). Three partial indexes carry the windowed shapes the
-- summary runs in one transaction:
--
--   * requisitions_dashboard_pending_idx — the approvals widget walks
--     submitted requisitions oldest-first (partial: status = 'submitted',
--     leading (tenant_id, submitted_at)). The unfiltered submitted count and
--     the first page both read it.
--   * approval_steps_pending_idx — the widget's current-step probe: the
--     page query picks each submitted requisition's smallest pending step_no
--     (DISTINCT ON over pending steps), and without a status-leading access
--     path that meant a full approval_steps scan + sort at scale. The
--     partial index keeps only the pending rows, already in
--     (requisition_id, step_no) order, so the DISTINCT ON reads it straight;
--     the count's EXISTS probe rides it too. In production pending steps are
--     transient (decide clears them), so the index stays small.
--   * purchase_orders_dashboard_spend_idx — the spend widget aggregates
--     issued, not-cancelled POs inside a trailing window (partial:
--     issued_at IS NOT NULL AND status NOT IN ('draft','cancelled'), leading
--     (tenant_id, issued_at)) — both FILTER arms cut on the issued_at range.
--
-- The status distribution is a whole-table GROUP BY (every row is visited by
-- design, zeros included) and keeps its seq scan; the #27 finding about the
-- RLS policy's bypass arm keeping tenant-leading index quals unavailable
-- applies to all of these — the partial predicate shrinks the scanned set
-- even when tenant scoping executes as a filter.
--
-- No DROP needed: no earlier migration indexes these shapes.

CREATE INDEX requisitions_dashboard_pending_idx
    ON procurement.requisitions (tenant_id, submitted_at)
    WHERE status = 'submitted';

CREATE INDEX approval_steps_pending_idx
    ON procurement.approval_steps (requisition_id, step_no)
    WHERE status = 'pending';

CREATE INDEX purchase_orders_dashboard_spend_idx
    ON procurement.purchase_orders (tenant_id, issued_at)
    WHERE issued_at IS NOT NULL AND status NOT IN ('draft', 'cancelled');
