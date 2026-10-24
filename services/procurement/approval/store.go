// Store persists the approval engine's data — the tenant's configurable
// rules and the per-requisition materialized steps — always scoped to one
// tenant through the RLS helpers. Submit-time materialization runs inside
// the requisition store's transaction (MaterializeTx); Decide opens its own
// transaction and locks the parent requisition row FOR UPDATE, the same
// serialization point submit and edit use. Machine-readable IO failure codes
// live in approval.go next to the domain's validation sentinels.
package approval

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/procurement/requisition"
)

// Store persists rules and steps, always scoped to one tenant.
type Store struct {
	db *pgxpool.Pool
}

// NewStore returns a Store on top of the pool.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// ListRules returns the tenant's whole rule set ordered by currency then
// step_no. Another tenant's rules are simply not visible (RLS).
func (s *Store) ListRules(ctx context.Context, tenantID string) ([]Rule, error) {
	var rules []Rule
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		var err error
		rules, err = loadRulesTx(ctx, tx)
		return err
	})
	if err != nil {
		return nil, wrap("list rules", err)
	}
	return rules, nil
}

// ReplaceRules replaces the tenant's whole rule set in one transaction:
// validation (domain ValidateRuleSet) runs before any write, then the old
// rows are deleted and the new set inserted. Steps already materialized on
// submitted requisitions are snapshots and are never rewritten.
func (s *Store) ReplaceRules(ctx context.Context, tenantID string, rules []Rule) ([]Rule, error) {
	if err := ValidateRuleSet(rules); err != nil {
		return nil, err
	}
	// Echo the stored set in the same order ListRules uses (currency, then
	// step_no) so a PUT response and the next GET agree.
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].Currency != rules[j].Currency {
			return rules[i].Currency < rules[j].Currency
		}
		return rules[i].StepNo < rules[j].StepNo
	})
	stored := []Rule{}
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM procurement.approval_rules`); err != nil {
			return err
		}
		for _, r := range rules {
			row := tx.QueryRow(ctx, `
				INSERT INTO procurement.approval_rules (tenant_id, currency, step_no, role_required, threshold_cents)
				VALUES ($1, $2, $3, $4, $5)
				RETURNING id, currency, step_no, role_required, threshold_cents`,
				tenantID, r.Currency, r.StepNo, r.RoleRequired, r.ThresholdCents)
			var out Rule
			if err := row.Scan(&out.ID, &out.Currency, &out.StepNo, &out.RoleRequired, &out.ThresholdCents); err != nil {
				return err
			}
			stored = append(stored, out)
		}
		return nil
	})
	if err != nil {
		return nil, wrap("replace rules", err)
	}
	return stored, nil
}

// MaterializeTx evaluates the tenant's rules against a freshly submitted
// requisition inside the submit transaction (the requisition store passes
// this as its materialize callback) and inserts the resulting steps. It is
// the tx-local half of Submit: no applicable rule fails with ErrRulesMissing
// and thereby rolls the whole submit back.
func (s *Store) MaterializeTx(ctx context.Context, tx pgx.Tx, r requisition.Requisition) ([]Step, error) {
	rules, err := loadRulesTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	steps, err := Materialize(rules, r.ID, r.Currency, r.TotalCents)
	if err != nil {
		return nil, err
	}
	for i := range steps {
		if err := tx.QueryRow(ctx, `
			INSERT INTO procurement.approval_steps
				(requisition_id, step_no, role_required, threshold_cents)
			VALUES ($1, $2, $3, $4)
			RETURNING id`,
			r.ID, steps[i].StepNo, steps[i].RoleRequired, steps[i].ThresholdCents,
		).Scan(&steps[i].ID); err != nil {
			return nil, err
		}
	}
	return steps, nil
}

func loadRulesTx(ctx context.Context, tx pgx.Tx) ([]Rule, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, currency, step_no, role_required, threshold_cents
		FROM procurement.approval_rules
		ORDER BY currency, step_no`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rules := []Rule{}
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.ID, &r.Currency, &r.StepNo, &r.RoleRequired, &r.ThresholdCents); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// ListSteps returns one requisition's steps ordered by step_no; the parent
// RLS policy makes another tenant's steps invisible (an empty list is
// indistinguishable from a missing requisition).
func (s *Store) ListSteps(ctx context.Context, tenantID, requisitionID string) ([]Step, error) {
	if _, err := uuid.Parse(requisitionID); err != nil {
		return nil, requisition.ErrIDInvalid
	}
	steps := []Step{}
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, stepsQuery+` WHERE requisition_id = $1 ORDER BY step_no`, requisitionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var st Step
			if err := scanStep(rows, &st); err != nil {
				return err
			}
			steps = append(steps, st)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrap("list steps", err)
	}
	return steps, nil
}

const stepColumns = `id, requisition_id, step_no, role_required, threshold_cents, status,
	decided_by, decided_at, reason`

const stepsQuery = `SELECT ` + stepColumns + ` FROM procurement.approval_steps`

func scanStep(row pgx.Row, st *Step) error {
	var decidedBy, reason *string
	if err := row.Scan(&st.ID, &st.RequisitionID, &st.StepNo, &st.RoleRequired,
		&st.ThresholdCents, &st.Status, &decidedBy, &st.DecidedAt, &reason); err != nil {
		return err
	}
	if decidedBy != nil {
		st.DecidedBy = *decidedBy
	}
	if reason != nil {
		st.Reason = *reason
	}
	return nil
}

// DecideInput is the store-level decide payload; Decision/Reason are already
// normalized (trim, lowercase decision) by the API layer.
type DecideInput struct {
	StepID   string
	Decision string
	Reason   string
	ActorID  string
	Roles    []string
}

// Decide applies one approve/reject decision. The whole flow runs in one
// tenant transaction: the step is read, its parent requisition row is locked
// FOR UPDATE (the serialization point shared with submit/edit — a concurrent
// decide on the same requisition blocks here, and a future cancel endpoint
// will serialize on the same lock), the steps are re-loaded after the lock,
// the domain re-checks status and currency, and the guarded writes land.
// Cross-tenant step ids are indistinguishable from missing ones.
func (s *Store) Decide(ctx context.Context, tenantID string, in DecideInput) (requisition.Requisition, []Step, error) {
	if _, err := uuid.Parse(in.StepID); err != nil {
		return requisition.Requisition{}, nil, ErrStepIDInvalid
	}
	var outReq requisition.Requisition
	var outSteps []Step
	err := tenant.WithTenant(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		// Unlocked read: ids are immutable, this only resolves the parent.
		var step Step
		if err := scanStep(tx.QueryRow(ctx, stepsQuery+` WHERE id = $1`, in.StepID), &step); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrStepNotFound
			}
			return err
		}

		// Lock the parent requisition — the shared serialization point.
		req, err := requisition.LoadForUpdate(ctx, tx, step.RequisitionID)
		if err != nil {
			return err
		}

		// Re-load the steps after the lock; the domain re-checks the
		// current-step guard on this post-lock view.
		steps, err := loadStepsTx(ctx, tx, step.RequisitionID)
		if err != nil {
			return err
		}

		updated, err := Decide(&req, steps, DecisionInput{
			StepID: in.StepID, Decision: in.Decision, Reason: in.Reason,
			ActorID: in.ActorID, ActorRoles: in.Roles, Now: time.Now().UTC(),
		})
		if err != nil {
			return err
		}

		var decided *Step
		for i := range updated {
			if updated[i].ID == in.StepID {
				decided = &updated[i]
			}
		}
		// Guarded write of the decision: status='pending' re-check loses
		// only to a race the lock already prevents (belt and braces).
		tag, err := tx.Exec(ctx, `
			UPDATE procurement.approval_steps
			SET status = $2, decided_by = NULLIF($3, '')::uuid, decided_at = $4, reason = $5
			WHERE id = $1 AND status = 'pending'`,
			in.StepID, decided.Status, in.ActorID, *decided.DecidedAt, decided.Reason)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrStepNotCurrent
		}
		if decided.Status == StepRejected {
			// A reject skips every later pending step.
			if _, err := tx.Exec(ctx, `
				UPDATE procurement.approval_steps
				SET status = 'skipped'
				WHERE requisition_id = $1 AND status = 'pending' AND step_no > $2`,
				step.RequisitionID, decided.StepNo); err != nil {
				return err
			}
		}

		// The requisition transition (guarded on 'submitted'); mid-flow
		// approves leave the requisition row untouched.
		if req.Status != requisition.StatusSubmitted {
			tag, err := tx.Exec(ctx, `
				UPDATE procurement.requisitions
				SET status = $2, updated_at = $3, updated_by = NULLIF($4, '')::uuid
				WHERE id = $1 AND status = 'submitted'`,
				req.ID, req.Status, *decided.DecidedAt, in.ActorID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrStepNotCurrent
			}
		}

		outReq = req
		outSteps = updated
		return nil
	})
	if err != nil {
		return requisition.Requisition{}, nil, wrap("decide", err)
	}
	return outReq, outSteps, nil
}

func loadStepsTx(ctx context.Context, tx pgx.Tx, requisitionID string) ([]Step, error) {
	rows, err := tx.Query(ctx, stepsQuery+` WHERE requisition_id = $1 ORDER BY step_no`, requisitionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	steps := []Step{}
	for rows.Next() {
		var st Step
		if err := scanStep(rows, &st); err != nil {
			return nil, err
		}
		steps = append(steps, st)
	}
	return steps, rows.Err()
}

// wrap adds context to unexpected failures but passes the machine-code
// sentinels through bare (same contract as the requisition store).
func wrap(what string, err error) error {
	for _, sentinel := range []error{
		ErrRulesMissing, ErrRuleCurrencyInvalid, ErrRuleRoleInvalid,
		ErrRuleThresholdInvalid, ErrRuleStepNoInvalid, ErrRuleStepNoDuplicate,
		ErrDecisionInvalid, ErrReasonRequired, ErrSelfForbidden,
		ErrRoleForbidden, ErrStepNotCurrent, ErrStepNotFound, ErrStepIDInvalid,
		requisition.ErrNotFound, requisition.ErrIDInvalid,
	} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("approval: %s: %w", what, err)
}
