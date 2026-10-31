// Store serves the FR-09 dashboard read model, always scoped to one tenant
// through the RLS helpers. Summary runs its queries in ONE tenant
// transaction, so the widgets describe one consistent snapshot and the
// whole dashboard is a single round trip for callers. The query texts are
// package constants so the scale test can EXPLAIN the exact statements the
// store runs.
package dashboard

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/tenant"
)

// Store reads the dashboard aggregates, always scoped to one tenant.
type Store struct {
	db *pgxpool.Pool
}

// NewStore returns a Store on top of the pool.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// Summary returns the tenant's dashboard snapshot as of now: the pending
// approvals (total + first page, oldest-submitted first), the spend per
// vendor+currency over the trailing 30- and 90-day windows plus the
// per-currency KPI totals, and the PO status distribution over all seven
// FR-06 statuses (zeros included).
//
// Reconciliation contract (issue #52): pending.total counts current pending
// steps — one per submitted requisition that still has a pending step, the
// smallest step_no — so it equals len(items) only when the queue fits one
// page; the distribution totals the tenant's whole purchase_orders table, so
// its sum equals the unfiltered ListPurchaseOrders total; the spend rows sum
// purchase_orders.total_cents per vendor+currency exactly as the PO list
// would when filtered to the window (issued_at in window, status not
// draft/cancelled); the per-currency totals are the same sums unbounded by
// the vendor-row limit.
func (s *Store) Summary(ctx context.Context, tenantID string, now time.Time) (Summary, error) {
	var out Summary
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		pending, err := pendingApprovals(ctx, tx)
		if err != nil {
			return err
		}
		spend, totals, err := vendorSpend(ctx, tx, now)
		if err != nil {
			return err
		}
		dist, err := statusDistribution(ctx, tx)
		if err != nil {
			return err
		}
		out = Summary{Pending: pending, SpendByVendor: spend,
			POStatusDistribution: dist, SpendTotals: totals}
		return nil
	})
	if err != nil {
		return Summary{}, fmt.Errorf("dashboard: summary: %w", err)
	}
	return out, nil
}

// pendingCountSQL counts the current pending steps: one per requisition in
// submitted that still has a pending step (the EXISTS guard — a submitted
// requisition whose steps were all decided, a shape the domain never leaves
// behind, does not count).
const pendingCountSQL = `
		SELECT count(*)
		FROM procurement.requisitions r
		WHERE r.status = 'submitted'
		  AND EXISTS (SELECT 1 FROM procurement.approval_steps st
		              WHERE st.requisition_id = r.id AND st.status = 'pending')`

// pendingPageSQL reads the first page of the queue oldest-submitted first.
// DISTINCT ON picks each requisition's smallest pending step_no — the
// current step, exactly the one the decide API accepts. approval_steps
// carries no tenant_id by design (0005); tenancy derives from the parent
// requisition through the join, which RLS scopes.
const pendingPageSQL = `
		SELECT r.id, r.number, r.currency, r.total_cents, r.submitted_at,
		       cur.id, cur.step_no, cur.role_required
		FROM procurement.requisitions r
		JOIN (
			SELECT DISTINCT ON (st.requisition_id)
			       st.requisition_id, st.id, st.step_no, st.role_required
			FROM procurement.approval_steps st
			WHERE st.status = 'pending'
			ORDER BY st.requisition_id, st.step_no
		) cur ON cur.requisition_id = r.id
		WHERE r.status = 'submitted'
		ORDER BY r.submitted_at, r.number
		LIMIT $1`

// spendTotalsSQL is the KPI arm of the spend widget: the unbounded window
// sums per currency (money never mixes currencies — a tenant may hold IDR
// and USD side by side). $1 = now-30d, $2 = now-90d.
const spendTotalsSQL = `
		SELECT po.currency,
		       coalesce(sum(po.total_cents) FILTER (WHERE po.issued_at >= $1), 0) AS spend_30d,
		       coalesce(sum(po.total_cents), 0) AS spend_90d
		FROM procurement.purchase_orders po
		WHERE po.status NOT IN ('draft', 'cancelled')
		  AND po.issued_at >= $2
		GROUP BY po.currency
		ORDER BY spend_90d DESC`

// spendByVendorSQL is the table arm: one row per vendor+currency with issued
// spend in the 90-day window (the FILTER arms carry both windows — the
// 30-day one is a subset), ordered by 90-day spend descending and bounded to
// SpendVendorLimit rows. $1 = now-30d, $2 = now-90d, $3 = row limit.
const spendByVendorSQL = `
		SELECT po.vendor_id, v.name, po.currency,
		       coalesce(sum(po.total_cents) FILTER (WHERE po.issued_at >= $1), 0) AS spend_30d,
		       coalesce(sum(po.total_cents) FILTER (WHERE po.issued_at >= $2), 0) AS spend_90d
		FROM procurement.purchase_orders po
		JOIN procurement.vendors v ON v.id = po.vendor_id
		WHERE po.status NOT IN ('draft', 'cancelled')
		  AND po.issued_at >= $2
		GROUP BY po.vendor_id, v.name, po.currency
		ORDER BY spend_90d DESC, po.vendor_id
		LIMIT $3`

// distributionSQL counts the tenant's purchase orders per status; the
// zero-filling of the missing statuses happens in Go (POStatusOrder).
const distributionSQL = `
		SELECT status, count(*) FROM procurement.purchase_orders GROUP BY status`

// pendingApprovals runs the approvals widget's two queries.
func pendingApprovals(ctx context.Context, tx pgx.Tx) (PendingApprovals, error) {
	var out PendingApprovals
	if err := tx.QueryRow(ctx, pendingCountSQL).Scan(&out.Total); err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, pendingPageSQL, PendingPageSize)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var p PendingApproval
		if err := rows.Scan(&p.RequisitionID, &p.Number, &p.Currency, &p.TotalCents,
			&p.SubmittedAt, &p.StepID, &p.StepNo, &p.RoleRequired); err != nil {
			return out, err
		}
		out.Items = append(out.Items, p)
	}
	return out, rows.Err()
}

// vendorSpend runs the spend widget's two queries (KPI totals + the bounded
// vendor table). Basis across both: POs with issued_at inside the window and
// status neither draft nor cancelled — the #49 nightly register basis
// (drafts never carry issued_at; cancelled POs that were issued drop out).
func vendorSpend(ctx context.Context, tx pgx.Tx, now time.Time) ([]VendorSpend, []CurrencySpend, error) {
	totalsRows, err := tx.Query(ctx, spendTotalsSQL, now.Add(-SpendWindow30d), now.Add(-SpendWindow90d))
	if err != nil {
		return nil, nil, err
	}
	defer totalsRows.Close()
	var totals []CurrencySpend
	for totalsRows.Next() {
		var c CurrencySpend
		if err := totalsRows.Scan(&c.Currency, &c.ThirtyDayCents, &c.NinetyDayCents); err != nil {
			return nil, nil, err
		}
		totals = append(totals, c)
	}
	if err := totalsRows.Err(); err != nil {
		return nil, nil, err
	}

	rows, err := tx.Query(ctx, spendByVendorSQL, now.Add(-SpendWindow30d), now.Add(-SpendWindow90d), SpendVendorLimit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []VendorSpend
	for rows.Next() {
		var vs VendorSpend
		if err := rows.Scan(&vs.VendorID, &vs.VendorName, &vs.Currency,
			&vs.SpendCents30d, &vs.SpendCents90d); err != nil {
			return nil, nil, err
		}
		out = append(out, vs)
	}
	return out, totals, rows.Err()
}

// statusDistribution runs the distribution query and fills every FR-06
// status the table does not mention with zero, in the canonical lifecycle
// order — the widget's bars stay stable across tenants.
func statusDistribution(ctx context.Context, tx pgx.Tx) ([]StatusCount, error) {
	rows, err := tx.Query(ctx, distributionSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]int64, len(POStatusOrder))
	for rows.Next() {
		var (
			status string
			n      int64
		)
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		counts[status] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]StatusCount, 0, len(POStatusOrder))
	for _, status := range POStatusOrder {
		out = append(out, StatusCount{Status: status, Count: counts[status]})
	}
	return out, nil
}
