package approval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luksluki/eprocurement/internal/platform/dbmigrate"
	"github.com/luksluki/eprocurement/internal/platform/logger"
	"github.com/luksluki/eprocurement/internal/platform/tenant"
	"github.com/luksluki/eprocurement/services/procurement/migrations"
	"github.com/luksluki/eprocurement/services/procurement/requisition"
)

const (
	tenantA = "ffffffff-0000-0000-0000-000000000001"
	tenantB = "ffffffff-0000-0000-0000-000000000002"
	userReq = "ffffffff-0000-0000-0000-0000000000aa" // requester
	userApp = "ffffffff-0000-0000-0000-0000000000bb" // approver
	userFin = "ffffffff-0000-0000-0000-0000000000cc" // finance
	userAdm = "ffffffff-0000-0000-0000-0000000000dd" // admin
)

// openTestDB mirrors the requisition harness: apply migrations (0005 rides
// the embedded FS), open a pool, skip without TEST_DATABASE_URL.
func openTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	migCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	migCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	migPool, err := pgxpool.NewWithConfig(context.Background(), migCfg)
	if err != nil {
		t.Fatalf("open migration pool: %v", err)
	}
	if _, err := dbmigrate.Apply(t.Context(), migPool, "procurement", migrations.FS()); err != nil {
		migPool.Close()
		t.Fatalf("apply migrations: %v", err)
	}
	migPool.Close()

	db, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func cleanupApprovals(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.outbox WHERE tenant_id IN ($1, $2)`, tenantA, tenantB); err != nil {
			return err
		}
		// decision_idempotency rides the approval_steps → requisitions
		// cascades, so deleting the requisitions clears it too.
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.requisitions WHERE tenant_id IN ($1, $2) AND number LIKE 'PR-%'`,
			tenantA, tenantB); err != nil {
			return err
		}
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.requisition_counters WHERE tenant_id IN ($1, $2)`, tenantA, tenantB); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(),
			`DELETE FROM procurement.approval_rules WHERE tenant_id IN ($1, $2)`, tenantA, tenantB)
		return err
	})
}

// submit submits r through the requisition store with the approval
// materializer wired in, exactly like the procurement API does.
func submit(ctx context.Context, s *Store, reqs *requisition.Store, tenantID, id, actor, key string) (requisition.Requisition, []Step, error) {
	r, err := reqs.Submit(ctx, tenantID, id, actor, key, func(tx pgx.Tx, r requisition.Requisition) error {
		_, err := s.MaterializeTx(ctx, tx, r)
		return err
	})
	if err != nil {
		return requisition.Requisition{}, nil, err
	}
	steps, err := s.ListSteps(ctx, tenantID, r.ID)
	if err != nil {
		return requisition.Requisition{}, nil, err
	}
	return r, steps, nil
}

// submittedPR creates and submits a 15,450,000 IDR requisition under rulesA
// (approver@5M step 1, finance@10M step 2) and returns it with its steps.
func submittedPR(t *testing.T, ctx context.Context, s *Store, reqs *requisition.Store, requester, key string) (requisition.Requisition, []Step) {
	t.Helper()
	if _, err := s.ReplaceRules(ctx, tenantA, rulesA); err != nil {
		t.Fatalf("replace rules: %v", err)
	}
	r, err := reqs.Create(ctx, tenantA, requisition.CreateInput{
		RequesterID: requester, Currency: "IDR",
		Items: []requisition.Item{{Description: "Laptop", Qty: 2, UnitPriceCents: 7_725_000}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	out, steps, err := submit(ctx, s, reqs, tenantA, r.ID, requester, key)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return out, steps
}

// rulesA: the IDR chain used by most tests, plus a USD rule that must never
// match an IDR requisition.
var rulesA = []Rule{
	{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: 5_000_000},
	{Currency: "IDR", StepNo: 2, RoleRequired: "finance", ThresholdCents: 10_000_000},
	{Currency: "USD", StepNo: 1, RoleRequired: "approver", ThresholdCents: 1},
}

// AC (configurable rules, one table, replace-set in one tx): replace and
// list round-trip; an invalid replacement fails before any write and leaves
// the previous set intact; the empty set is legal.
func TestStoreRulesLifecycle(t *testing.T) {
	s := NewStore(openTestDB(t))
	ctx := context.Background()

	got, err := s.ListRules(ctx, tenantA)
	if err != nil || len(got) != 0 {
		t.Fatalf("initial list = (%v, %v), want empty", got, err)
	}

	stored, err := s.ReplaceRules(ctx, tenantA, rulesA)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if len(stored) != 3 {
		t.Fatalf("stored %d rules, want 3", len(stored))
	}
	for _, r := range stored {
		if r.ID == "" {
			t.Fatal("stored rule has no id")
		}
	}
	got, err = s.ListRules(ctx, tenantA)
	if err != nil || len(got) != 3 {
		t.Fatalf("list = (%v, %v), want 3 rules", got, err)
	}

	// Invalid replacement: duplicate step_no per currency — sentinel error,
	// previous set untouched.
	dup := append(append([]Rule(nil), rulesA...), Rule{Currency: "IDR", StepNo: 1, RoleRequired: "buyer", ThresholdCents: 1})
	if _, err := s.ReplaceRules(ctx, tenantA, dup); !errors.Is(err, ErrRuleStepNoDuplicate) {
		t.Fatalf("dup step_no err = %v, want ErrRuleStepNoDuplicate", err)
	}
	got, err = s.ListRules(ctx, tenantA)
	if err != nil || len(got) != 3 {
		t.Fatalf("list after failed replace = (%v, %v), want the previous 3", got, err)
	}

	// Validation sentinels reach the caller bare.
	for name, tc := range map[string]struct {
		rule    Rule
		wantErr error
	}{
		"bad currency":       {Rule{Currency: "EUR", StepNo: 1, RoleRequired: "approver"}, ErrRuleCurrencyInvalid},
		"bad role":           {Rule{Currency: "IDR", StepNo: 1, RoleRequired: "cto"}, ErrRuleRoleInvalid},
		"negative threshold": {Rule{Currency: "IDR", StepNo: 1, RoleRequired: "approver", ThresholdCents: -1}, ErrRuleThresholdInvalid},
		"zero step_no":       {Rule{Currency: "IDR", StepNo: 0, RoleRequired: "approver"}, ErrRuleStepNoInvalid},
	} {
		if _, err := s.ReplaceRules(ctx, tenantA, []Rule{tc.rule}); !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.wantErr)
		}
	}

	// The empty set is a legal configuration (all submits then 412).
	if _, err := s.ReplaceRules(ctx, tenantA, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, err := s.ListRules(ctx, tenantA); err != nil || len(got) != 0 {
		t.Fatalf("cleared list = (%v, %v), want empty", got, err)
	}
}

// AC (rules missing → submit rolled back): a submit without applicable rules
// fails with ErrRulesMissing and leaves the requisition a draft with no
// steps and no recorded idempotency key — the same key then names the fresh
// submit once rules exist, and its replay returns the current state without
// a second transition.
func TestStoreSubmitRulesMissingRollback(t *testing.T) {
	db := openTestDB(t)
	s, reqs := NewStore(db), requisition.NewStore(db)
	t.Cleanup(func() { cleanupApprovals(t, db) })
	cleanupApprovals(t, db)
	ctx := context.Background()

	r, err := reqs.Create(ctx, tenantA, requisition.CreateInput{
		RequesterID: userReq, Currency: "IDR",
		Items: []requisition.Item{{Description: "Laptop", Qty: 2, UnitPriceCents: 7_725_000}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// No rules configured: the submit fails and rolls back entirely.
	if _, _, err := submit(ctx, s, reqs, tenantA, r.ID, userReq, "key-30-a"); !errors.Is(err, ErrRulesMissing) {
		t.Fatalf("submit without rules err = %v, want ErrRulesMissing", err)
	}
	stored, err := reqs.Get(ctx, tenantA, r.ID)
	if err != nil || stored.Status != requisition.StatusDraft || stored.SubmittedAt != nil {
		t.Fatalf("after failed submit = (%+v, %v), want untouched draft", stored, err)
	}
	steps, err := s.ListSteps(ctx, tenantA, r.ID)
	if err != nil || len(steps) != 0 {
		t.Fatalf("steps after failed submit = (%v, %v), want none", steps, err)
	}

	// Rules appear: the same key behaves as a fresh submit (it was never
	// recorded), materializing the steps.
	if _, err := s.ReplaceRules(ctx, tenantA, rulesA); err != nil {
		t.Fatalf("replace rules: %v", err)
	}
	submitted, steps, err := submit(ctx, s, reqs, tenantA, r.ID, userReq, "key-30-a")
	if err != nil || submitted.Status != requisition.StatusSubmitted {
		t.Fatalf("submit after configuring = (%+v, %v), want submitted", submitted, err)
	}
	if len(steps) != 2 || steps[0].StepNo != 1 || steps[1].StepNo != 2 {
		t.Fatalf("materialized steps = %+v, want step 1 then 2", steps)
	}
	for _, st := range steps {
		if st.ID == "" || st.Status != StepPending {
			t.Fatalf("step = %+v, want pending with id", st)
		}
	}

	// Replaying the key returns the current state without a second
	// transition (steps not duplicated, submitted_at unchanged).
	replayed, steps, err := submit(ctx, s, reqs, tenantA, r.ID, userReq, "key-30-a")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed.Status != requisition.StatusSubmitted || !replayed.SubmittedAt.Equal(*submitted.SubmittedAt) {
		t.Fatalf("replay = (%s, %v), want submitted at the original time", replayed.Status, replayed.SubmittedAt)
	}
	if len(steps) != 2 {
		t.Fatalf("replay steps = %d, want 2 (no duplicates)", len(steps))
	}
}

// AC (matrix evaluation at submit): every matching-currency rule at or
// below the total is materialized ordered by step_no; the lower bound is
// inclusive; other currencies never match; steps are a snapshot of the
// rules at submit time.
func TestStoreSubmitMaterialization(t *testing.T) {
	db := openTestDB(t)
	s, reqs := NewStore(db), requisition.NewStore(db)
	t.Cleanup(func() { cleanupApprovals(t, db) })
	cleanupApprovals(t, db)
	ctx := context.Background()

	create := func(total int64) requisition.Requisition {
		t.Helper()
		r, err := reqs.Create(ctx, tenantA, requisition.CreateInput{
			RequesterID: userReq, Currency: "IDR",
			Items: []requisition.Item{{Description: "Item", Qty: 1, UnitPriceCents: total}},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		return r
	}

	// Threshold exactly equal: the step is materialized (inclusive).
	if _, err := s.ReplaceRules(ctx, tenantA, rulesA[:1]); err != nil {
		t.Fatalf("replace rules: %v", err)
	}
	atThreshold := create(5_000_000)
	_, steps, err := submit(ctx, s, reqs, tenantA, atThreshold.ID, userReq, "")
	if err != nil || len(steps) != 1 || steps[0].RoleRequired != "approver" {
		t.Fatalf("submit at threshold = (%v, %v)", steps, err)
	}

	// One cent below every rule: no step, submit rolled back.
	below := create(4_999_999)
	if _, _, err := submit(ctx, s, reqs, tenantA, below.ID, userReq, ""); !errors.Is(err, ErrRulesMissing) {
		t.Fatalf("submit below threshold err = %v, want ErrRulesMissing", err)
	}

	// Above both thresholds: both steps, ordered; the USD rule never leaks
	// into an IDR requisition.
	if _, err := s.ReplaceRules(ctx, tenantA, rulesA); err != nil {
		t.Fatalf("replace rules: %v", err)
	}
	above := create(15_450_000)
	submitted, steps, err := submit(ctx, s, reqs, tenantA, above.ID, userReq, "")
	if err != nil {
		t.Fatalf("submit above: %v", err)
	}
	if submitted.Status != requisition.StatusSubmitted || len(steps) != 2 ||
		steps[0].StepNo != 1 || steps[0].RoleRequired != "approver" ||
		steps[1].StepNo != 2 || steps[1].RoleRequired != "finance" {
		t.Fatalf("steps = %+v", steps)
	}

	// Snapshot: replacing the rules never rewrites materialized steps.
	if _, err := s.ReplaceRules(ctx, tenantA, []Rule{
		{Currency: "IDR", StepNo: 1, RoleRequired: "buyer", ThresholdCents: 0},
	}); err != nil {
		t.Fatalf("replace rules (snapshot): %v", err)
	}
	after, err := s.ListSteps(ctx, tenantA, above.ID)
	if err != nil || len(after) != 2 || after[0].RoleRequired != "approver" || after[1].RoleRequired != "finance" {
		t.Fatalf("steps after rule replace = (%v, %v), want the original snapshot", after, err)
	}
}

// AC (sequential decide + reject stops the flow): the happy path approves
// through both steps to an approved requisition; a reject skips downstream
// steps and rejects the requisition; every later decision is
// ErrStepNotCurrent.
func TestStoreDecideFlows(t *testing.T) {
	db := openTestDB(t)
	s, reqs := NewStore(db), requisition.NewStore(db)
	t.Cleanup(func() { cleanupApprovals(t, db) })
	cleanupApprovals(t, db)
	ctx := context.Background()

	// Happy path: approve step 1 (approver), then step 2 (finance) → approved.
	submitted, steps := submittedPR(t, ctx, s, reqs, userReq, "key-30-flow")
	after1, updated, err := s.Decide(ctx, tenantA, DecideInput{
		StepID: steps[0].ID, Decision: DecisionApprove, ActorID: userApp, Roles: []string{"approver"},
	})
	if err != nil {
		t.Fatalf("approve step 1: %v", err)
	}
	if after1.Status != requisition.StatusSubmitted || updated[0].Status != StepApproved || updated[1].Status != StepPending {
		t.Fatalf("after step 1: status=%s steps=%+v", after1.Status, updated)
	}
	// The decided step carries its domain event in-memory (outbox #45).
	if evs := updated[0].Events(); len(evs) != 1 || evs[0].Type != EventStepApproved || evs[0].Actor != userApp {
		t.Fatalf("step events = %+v", evs)
	}

	after2, updated, err := s.Decide(ctx, tenantA, DecideInput{
		StepID: steps[1].ID, Decision: DecisionApprove, Reason: "within budget", ActorID: userFin, Roles: []string{"finance"},
	})
	if err != nil {
		t.Fatalf("approve step 2: %v", err)
	}
	if after2.Status != requisition.StatusApproved {
		t.Fatalf("final status = %s, want approved", after2.Status)
	}
	stored, err := reqs.Get(ctx, tenantA, submitted.ID)
	if err != nil || stored.Status != requisition.StatusApproved {
		t.Fatalf("stored status = (%s, %v), want approved", stored.Status, err)
	}
	persisted, err := s.ListSteps(ctx, tenantA, submitted.ID)
	if err != nil || len(persisted) != 2 || persisted[0].Status != StepApproved || persisted[1].Status != StepApproved {
		t.Fatalf("persisted steps = (%v, %v)", persisted, err)
	}
	if persisted[1].DecidedBy != userFin || persisted[1].Reason != "within budget" || persisted[1].DecidedAt == nil {
		t.Fatalf("decision fields = %+v", persisted[1])
	}
	// Terminal: every further decision is a 409.
	if _, _, err := s.Decide(ctx, tenantA, DecideInput{StepID: steps[1].ID, Decision: DecisionApprove, ActorID: userFin, Roles: []string{"finance"}}); !errors.Is(err, ErrStepNotCurrent) {
		t.Fatalf("decide after terminal err = %v, want ErrStepNotCurrent", err)
	}

	// Reject path: step 1 rejected with a reason, step 2 skipped,
	// requisition rejected.
	rejReq, rejSteps := submittedPR(t, ctx, s, reqs, userReq, "key-30-reject")
	_, updated, err = s.Decide(ctx, tenantA, DecideInput{
		StepID: rejSteps[0].ID, Decision: DecisionReject, Reason: "over budget", ActorID: userApp, Roles: []string{"approver"},
	})
	if err != nil {
		t.Fatalf("reject step 1: %v", err)
	}
	if updated[0].Status != StepRejected || updated[1].Status != StepSkipped {
		t.Fatalf("after reject: %+v", updated)
	}
	stored, err = reqs.Get(ctx, tenantA, rejReq.ID)
	if err != nil || stored.Status != requisition.StatusRejected {
		t.Fatalf("stored status = (%s, %v), want rejected", stored.Status, err)
	}
	persisted, err = s.ListSteps(ctx, tenantA, rejReq.ID)
	if err != nil || persisted[1].Status != StepSkipped || persisted[1].DecidedBy != "" {
		t.Fatalf("persisted after reject = (%v, %v)", persisted, err)
	}
	// Both the decided and the skipped step refuse further decisions.
	for _, st := range persisted {
		_, _, err := s.Decide(ctx, tenantA, DecideInput{StepID: st.ID, Decision: DecisionApprove, ActorID: userAdm, Roles: []string{"admin"}})
		if !errors.Is(err, ErrStepNotCurrent) {
			t.Fatalf("decide %s after reject err = %v, want ErrStepNotCurrent", st.ID, err)
		}
	}
}

// AC (authorization at the store level): self-approval universally
// forbidden (admins included), role_required enforced (admins excepted),
// cross-tenant steps indistinguishable from missing, and bad/unknown step
// ids carry their own sentinels.
func TestStoreDecideAuthorization(t *testing.T) {
	db := openTestDB(t)
	s, reqs := NewStore(db), requisition.NewStore(db)
	t.Cleanup(func() { cleanupApprovals(t, db) })
	cleanupApprovals(t, db)
	ctx := context.Background()

	_, steps := submittedPR(t, ctx, s, reqs, userReq, "key-30-auth")

	// The requester holds approver: still forbidden.
	if _, _, err := s.Decide(ctx, tenantA, DecideInput{StepID: steps[0].ID, Decision: DecisionApprove, ActorID: userReq, Roles: []string{"approver"}}); !errors.Is(err, ErrSelfForbidden) {
		t.Fatalf("self approve err = %v, want ErrSelfForbidden", err)
	}
	// The requester as admin: the ban has no admin exception.
	if _, _, err := s.Decide(ctx, tenantA, DecideInput{StepID: steps[0].ID, Decision: DecisionReject, Reason: "x", ActorID: userReq, Roles: []string{"admin"}}); !errors.Is(err, ErrSelfForbidden) {
		t.Fatalf("self admin reject err = %v, want ErrSelfForbidden", err)
	}
	// A pure approver cannot decide the finance step.
	if _, _, err := s.Decide(ctx, tenantA, DecideInput{StepID: steps[1].ID, Decision: DecisionApprove, ActorID: userApp, Roles: []string{"approver"}}); !errors.Is(err, ErrRoleForbidden) {
		t.Fatalf("approver on finance step err = %v, want ErrRoleForbidden", err)
	}
	// An admin may decide any step of someone else's requisition.
	if _, _, err := s.Decide(ctx, tenantA, DecideInput{StepID: steps[0].ID, Decision: DecisionApprove, ActorID: userAdm, Roles: []string{"admin"}}); err != nil {
		t.Fatalf("admin decide err = %v, err", err)
	}

	// Cross-tenant: tenant B cannot see tenant A's step (404 sentinel).
	if _, _, err := s.Decide(ctx, tenantB, DecideInput{StepID: steps[1].ID, Decision: DecisionApprove, ActorID: userFin, Roles: []string{"finance"}}); !errors.Is(err, ErrStepNotFound) {
		t.Fatalf("cross-tenant decide err = %v, want ErrStepNotFound", err)
	}
	if got, err := s.ListSteps(ctx, tenantB, steps[0].RequisitionID); err != nil || len(got) != 0 {
		t.Fatalf("cross-tenant list steps = (%v, %v), want empty", got, err)
	}

	// Bad and unknown step ids.
	if _, _, err := s.Decide(ctx, tenantA, DecideInput{StepID: "not-a-uuid", Decision: DecisionApprove, ActorID: userApp, Roles: []string{"approver"}}); !errors.Is(err, ErrStepIDInvalid) {
		t.Fatalf("bad id err = %v, want ErrStepIDInvalid", err)
	}
	if _, _, err := s.Decide(ctx, tenantA, DecideInput{StepID: "ffffffff-0000-0000-0000-0000000000ff", Decision: DecisionApprove, ActorID: userApp, Roles: []string{"approver"}}); !errors.Is(err, ErrStepNotFound) {
		t.Fatalf("unknown id err = %v, want ErrStepNotFound", err)
	}

	// Decision shape and reject-reason sentinels reach the caller bare.
	fresh, freshSteps := submittedPR(t, ctx, s, reqs, userReq, "key-30-shape")
	_ = fresh
	if _, _, err := s.Decide(ctx, tenantA, DecideInput{StepID: freshSteps[0].ID, Decision: "maybe", ActorID: userApp, Roles: []string{"approver"}}); !errors.Is(err, ErrDecisionInvalid) {
		t.Fatalf("bad decision err = %v, want ErrDecisionInvalid", err)
	}
	if _, _, err := s.Decide(ctx, tenantA, DecideInput{StepID: freshSteps[0].ID, Decision: DecisionReject, Reason: "  ", ActorID: userApp, Roles: []string{"approver"}}); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("blank reason err = %v, want ErrReasonRequired", err)
	}
}

// AC (concurrency): two parallel decides on the same step — exactly one
// wins, the loser gets ErrStepNotCurrent, and the persisted state shows a
// single decision (the pattern of the parallel store test in dev-note 029).
func TestStoreDecideConcurrent(t *testing.T) {
	db := openTestDB(t)
	s, reqs := NewStore(db), requisition.NewStore(db)
	t.Cleanup(func() { cleanupApprovals(t, db) })
	cleanupApprovals(t, db)
	ctx := context.Background()

	// One-step flow (only the approver rule) so the first decision is also
	// the final one.
	if _, err := s.ReplaceRules(ctx, tenantA, rulesA[:1]); err != nil {
		t.Fatalf("replace rules: %v", err)
	}
	r, err := reqs.Create(ctx, tenantA, requisition.CreateInput{
		RequesterID: userReq, Currency: "IDR",
		Items: []requisition.Item{{Description: "Item", Qty: 1, UnitPriceCents: 6_000_000}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	submitted, steps, err := submit(ctx, s, reqs, tenantA, r.ID, userReq, "")
	if err != nil || len(steps) != 1 {
		t.Fatalf("submit = (%+v, %v, steps=%d)", submitted, err, len(steps))
	}

	const n = 2
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = s.Decide(ctx, tenantA, DecideInput{
				StepID: steps[0].ID, Decision: DecisionApprove,
				ActorID: userApp, Roles: []string{"approver"},
			})
		}(i)
	}
	wg.Wait()

	wins, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrStepNotCurrent):
			conflicts++
		default:
			t.Fatalf("unexpected decide error: %v", err)
		}
	}
	if wins != 1 || conflicts != n-1 {
		t.Fatalf("wins=%d conflicts=%d, want exactly 1 win and %d conflicts (errs=%v)", wins, conflicts, n-1, errs)
	}
	stored, err := reqs.Get(ctx, tenantA, submitted.ID)
	if err != nil || stored.Status != requisition.StatusApproved {
		t.Fatalf("stored status = (%s, %v), want approved exactly once", stored.Status, err)
	}
	persisted, err := s.ListSteps(ctx, tenantA, submitted.ID)
	if err != nil || len(persisted) != 1 || persisted[0].Status != StepApproved || persisted[0].DecidedBy != userApp {
		t.Fatalf("persisted step = (%v, %v)", persisted, err)
	}
}

// outboxRow is one procurement.outbox row as the tests read it back.
type outboxRow struct {
	ID         string
	EventType  string
	Version    int32
	Payload    []byte
	ActorID    *string
	TraceID    string
	EntityType string
	EntityID   string
	Published  *struct{}
}

// outboxRows reads the decide events of one requisition straight from the
// table (bypassing RLS on purpose — the count is the exactly-once evidence).
// Filtered to the decide event types: since #45 the same requisition also
// carries its pr.submitted row, which the requisition store suite owns.
func outboxRows(t *testing.T, db *pgxpool.Pool, requisitionID string) []outboxRow {
	t.Helper()
	rows := []outboxRow{}
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		rs, err := tx.Query(context.Background(), `
			SELECT id::text, event_type, version, payload, actor_id::text,
			       trace_id, entity_type, entity_id::text, published_at
			FROM procurement.outbox
			WHERE entity_id = $1
			  AND event_type IN ($2, $3, $4, $5)
			ORDER BY occurred_at, event_type`,
			requisitionID, OutboxStepApproved, OutboxStepRejected, OutboxApproved, OutboxRejected)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var r outboxRow
			var published any
			if err := rs.Scan(&r.ID, &r.EventType, &r.Version, &r.Payload,
				&r.ActorID, &r.TraceID, &r.EntityType, &r.EntityID, &published); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return rs.Err()
	})
	return rows
}

func decideKeyRows(t *testing.T, db *pgxpool.Pool, tenantID, key string) int {
	t.Helper()
	var n int
	_ = tenant.WithBypass(context.Background(), db, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM procurement.decision_idempotency WHERE tenant_id = $1 AND idempotency_key = $2`,
			tenantID, key).Scan(&n)
	})
	return n
}

// AC (idempotent decide + exactly-once outbox): a keyed decide records its
// key once and writes one step event (plus one terminal event only when the
// flow finishes); replaying the key returns the current state and writes
// nothing; the same key on another step of the tenant is a conflict; a
// duplicate decide without a key on a decided step is still
// ErrStepNotCurrent; the rows carry actor and the propagated trace id.
func TestStoreDecideIdempotencyAndOutbox(t *testing.T) {
	db := openTestDB(t)
	s, reqs := NewStore(db), requisition.NewStore(db)
	t.Cleanup(func() { cleanupApprovals(t, db) })
	cleanupApprovals(t, db)
	// The trace id rides the context exactly as the TraceInterceptor puts
	// it there for gRPC calls.
	ctx := logger.WithTraceID(context.Background(), "trace-31-store")

	submitted, steps := submittedPR(t, ctx, s, reqs, userReq, "key-31-sub-a")

	// Mid-flow keyed approve: one step event, no terminal event.
	after1, updated, err := s.Decide(ctx, tenantA, DecideInput{
		StepID: steps[0].ID, Decision: DecisionApprove, ActorID: userApp,
		Roles: []string{"approver"}, IdempotencyKey: "decide-31-a",
	})
	if err != nil {
		t.Fatalf("keyed approve step 1: %v", err)
	}
	if after1.Status != requisition.StatusSubmitted || updated[0].Status != StepApproved {
		t.Fatalf("after keyed approve = %s %+v", after1.Status, updated)
	}
	rows := outboxRows(t, db, submitted.ID)
	if len(rows) != 1 || rows[0].EventType != OutboxStepApproved ||
		rows[0].ActorID == nil || *rows[0].ActorID != userApp ||
		rows[0].TraceID != "trace-31-store" || rows[0].Version != 1 ||
		rows[0].EntityType != "requisition" || rows[0].EntityID != submitted.ID ||
		rows[0].Published != nil {
		t.Fatalf("mid-flow outbox rows = %+v", rows)
	}
	var payload DecisionPayload
	if err := json.Unmarshal(rows[0].Payload, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload.RequisitionID != submitted.ID || payload.Number != submitted.Number ||
		payload.RequesterID != userReq || payload.StepID != steps[0].ID ||
		payload.StepNo != 1 || payload.Decision != DecisionApprove ||
		payload.Reason != "" || payload.DecidedBy != userApp || payload.DecidedAt.IsZero() {
		t.Fatalf("step payload = %+v", payload)
	}
	if n := decideKeyRows(t, db, tenantA, "decide-31-a"); n != 1 {
		t.Fatalf("decide key rows = %d, want 1", n)
	}

	// Replay: current requisition + steps, nothing written. (DecidedAt is
	// truncated to microseconds: the replay reads the value back from
	// timestamptz, the winner's copy is nanosecond in-memory.)
	replayed, replaySteps, err := s.Decide(ctx, tenantA, DecideInput{
		StepID: steps[0].ID, Decision: DecisionApprove, ActorID: userApp,
		Roles: []string{"approver"}, IdempotencyKey: "decide-31-a",
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed.Status != requisition.StatusSubmitted || len(replaySteps) != 2 ||
		replaySteps[0].Status != StepApproved || replaySteps[0].DecidedBy != userApp ||
		!replaySteps[0].DecidedAt.Equal(updated[0].DecidedAt.Truncate(time.Microsecond)) {
		t.Fatalf("replay = %s %+v, want the winner's current state", replayed.Status, replaySteps)
	}
	if got := len(outboxRows(t, db, submitted.ID)); got != 1 {
		t.Fatalf("outbox rows after replay = %d, want 1", got)
	}
	if n := decideKeyRows(t, db, tenantA, "decide-31-a"); n != 1 {
		t.Fatalf("decide key rows after replay = %d, want 1", n)
	}

	// The same key cannot name another step of the tenant (even of the same
	// requisition) — conflict, and still nothing written.
	if _, _, err := s.Decide(ctx, tenantA, DecideInput{
		StepID: steps[1].ID, Decision: DecisionApprove, ActorID: userFin,
		Roles: []string{"finance", "approver"}, IdempotencyKey: "decide-31-a",
	}); !errors.Is(err, ErrIdempotencyKeyConflict) {
		t.Fatalf("key on other step err = %v, want ErrIdempotencyKeyConflict", err)
	}
	if got := len(outboxRows(t, db, submitted.ID)); got != 1 {
		t.Fatalf("outbox rows after key conflict = %d, want 1", got)
	}

	// Terminal keyed approve of the last step: step event + pr.approved.
	after2, _, err := s.Decide(ctx, tenantA, DecideInput{
		StepID: steps[1].ID, Decision: DecisionApprove, Reason: "within budget",
		ActorID: userFin, Roles: []string{"finance", "approver"}, IdempotencyKey: "decide-31-b",
	})
	if err != nil || after2.Status != requisition.StatusApproved {
		t.Fatalf("terminal approve = (%s, %v), want approved", after2.Status, err)
	}
	rows = outboxRows(t, db, submitted.ID)
	if len(rows) != 3 {
		t.Fatalf("terminal outbox rows = %d, want 3 (step 1, step 2, pr.approved)", len(rows))
	}
	types := map[string]bool{}
	for _, r := range rows {
		types[r.EventType] = true
	}
	if !types[OutboxStepApproved] || !types[OutboxApproved] || len(types) != 2 {
		t.Fatalf("terminal event types = %v", types)
	}

	// A duplicate decide WITHOUT a key on the decided step keeps its #30
	// answer: ErrStepNotCurrent, no outbox growth.
	if _, _, err := s.Decide(ctx, tenantA, DecideInput{
		StepID: steps[1].ID, Decision: DecisionApprove, ActorID: userFin,
		Roles: []string{"finance", "approver"},
	}); !errors.Is(err, ErrStepNotCurrent) {
		t.Fatalf("no-key duplicate err = %v, want ErrStepNotCurrent", err)
	}
	if got := len(outboxRows(t, db, submitted.ID)); got != 3 {
		t.Fatalf("outbox rows after no-key duplicate = %d, want 3", got)
	}

	// Cross-tenant: tenant B cannot even see the step, key or not.
	if _, _, err := s.Decide(ctx, tenantB, DecideInput{
		StepID: steps[1].ID, Decision: DecisionApprove, ActorID: userFin,
		Roles: []string{"finance", "approver"}, IdempotencyKey: "decide-31-a",
	}); !errors.Is(err, ErrStepNotFound) {
		t.Fatalf("cross-tenant keyed decide err = %v, want ErrStepNotFound", err)
	}

	// A reject is always terminal: step event + pr.rejected in one decide.
	rejReq, rejSteps := submittedPR(t, ctx, s, reqs, userReq, "key-31-sub-b")
	if _, _, err := s.Decide(ctx, tenantA, DecideInput{
		StepID: rejSteps[0].ID, Decision: DecisionReject, Reason: "over budget",
		ActorID: userApp, Roles: []string{"approver"}, IdempotencyKey: "decide-31-rej",
	}); err != nil {
		t.Fatalf("keyed reject: %v", err)
	}
	rows = outboxRows(t, db, rejReq.ID)
	if len(rows) != 2 {
		t.Fatalf("reject outbox rows = %d, want 2", len(rows))
	}
	rejTypes := map[string]bool{}
	for _, r := range rows {
		rejTypes[r.EventType] = true
	}
	if !rejTypes[OutboxStepRejected] || !rejTypes[OutboxRejected] {
		t.Fatalf("reject event types = %v", rejTypes)
	}
}

// AC (concurrent keyed decides): two goroutines decide the same step with
// the same key — the parent lock serializes them into one winner and one
// idempotent replay (never a conflict), one key row, and exactly the one
// decision's outbox rows.
func TestStoreDecideConcurrentSameKey(t *testing.T) {
	db := openTestDB(t)
	s, reqs := NewStore(db), requisition.NewStore(db)
	t.Cleanup(func() { cleanupApprovals(t, db) })
	cleanupApprovals(t, db)
	ctx := context.Background()

	// One-step flow so the first decision is also terminal.
	if _, err := s.ReplaceRules(ctx, tenantA, rulesA[:1]); err != nil {
		t.Fatalf("replace rules: %v", err)
	}
	r, err := reqs.Create(ctx, tenantA, requisition.CreateInput{
		RequesterID: userReq, Currency: "IDR",
		Items: []requisition.Item{{Description: "Item", Qty: 1, UnitPriceCents: 6_000_000}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	submitted, steps, err := submit(ctx, s, reqs, tenantA, r.ID, userReq, "")
	if err != nil || len(steps) != 1 {
		t.Fatalf("submit = (%+v, %v, steps=%d)", submitted, err, len(steps))
	}

	const n = 2
	type result struct {
		req  requisition.Requisition
		step Step
	}
	results := make([]result, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, updated, err := s.Decide(ctx, tenantA, DecideInput{
				StepID: steps[0].ID, Decision: DecisionApprove,
				ActorID: userApp, Roles: []string{"approver"},
				IdempotencyKey: "decide-31-race",
			})
			if err == nil {
				results[i] = result{req, updated[0]}
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d failed: %v (want one winner + one replay, never a conflict)", i, err)
		}
		if results[i].req.Status != requisition.StatusApproved || results[i].step.Status != StepApproved {
			t.Fatalf("goroutine %d = (%s, %s), want approved", i, results[i].req.Status, results[i].step.Status)
		}
	}
	if results[0].step.DecidedBy != results[1].step.DecidedBy ||
		!results[0].step.DecidedAt.Truncate(time.Microsecond).Equal(results[1].step.DecidedAt.Truncate(time.Microsecond)) {
		t.Fatalf("decisions differ: %+v vs %+v", results[0].step, results[1].step)
	}
	if got := len(outboxRows(t, db, submitted.ID)); got != 2 {
		t.Fatalf("outbox rows after race = %d, want 2 (one step event + one terminal)", got)
	}
	if n := decideKeyRows(t, db, tenantA, "decide-31-race"); n != 1 {
		t.Fatalf("decide key rows after race = %d, want 1", n)
	}
}
