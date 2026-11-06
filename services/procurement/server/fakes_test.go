// In-memory, tenant-keyed fakes for every store interface in stores.go (#118)
// — the unit-test half of the API layer. One shared memState plays the
// database: every collection is keyed by tenant first, mirroring the RLS
// scoping the concrete stores get from PostgreSQL, so a cross-tenant id reads
// as NotFound exactly like the real thing. The fakes call the same pure
// domain functions the concrete stores call (requisition.New, approval.Decide,
// purchaseorder transitions, the match engine), keeping the sentinels that
// reach the error mappers identical; only persistence is faked. No clocks, no
// sleeps: memState.now is fixed, so the whole suite is deterministic.
package server

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/procurement/approval"
	"github.com/luksluki/eprocurement/services/procurement/dashboard"
	"github.com/luksluki/eprocurement/services/procurement/goodsreceipt"
	"github.com/luksluki/eprocurement/services/procurement/invoice"
	"github.com/luksluki/eprocurement/services/procurement/match"
	"github.com/luksluki/eprocurement/services/procurement/purchaseorder"
	"github.com/luksluki/eprocurement/services/procurement/requisition"
	"github.com/luksluki/eprocurement/services/procurement/vendor"
)

// memState is the shared fake database: tenant-keyed collections plus the
// per-tenant counters and idempotency-key ledgers the real schema carries.
type memState struct {
	now time.Time // fixed clock handed to every domain call
	seq int       // deterministic id generator

	vendors map[string]map[string]*vendor.Vendor
	reqs    map[string]map[string]*requisition.Requisition
	steps   map[string]map[string]*approval.Step
	rules   map[string][]approval.Rule
	pos     map[string]map[string]*purchaseorder.PurchaseOrder
	grns    map[string]map[string]*goodsreceipt.GoodsReceipt
	invs    map[string]map[string]*invoice.Invoice
	// settings per tenant; absence is match.DefaultSettings.
	settings map[string]match.Settings

	submitKeys map[string]map[string]string // tenant -> key -> requisition id
	decideKeys map[string]map[string]string // tenant -> key -> step id
	reqNums    map[string]int64             // tenant -> last PR number
	poNums     map[string]int64             // tenant -> last PO number
	grnNums    map[string]int64             // tenant -> last GRN number

	// exportRequests records every TriggerExport call (tenant -> event ids).
	exportRequests map[string][]string
}

func newMemState() *memState {
	return &memState{
		now:            time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
		vendors:        map[string]map[string]*vendor.Vendor{},
		reqs:           map[string]map[string]*requisition.Requisition{},
		steps:          map[string]map[string]*approval.Step{},
		rules:          map[string][]approval.Rule{},
		pos:            map[string]map[string]*purchaseorder.PurchaseOrder{},
		grns:           map[string]map[string]*goodsreceipt.GoodsReceipt{},
		invs:           map[string]map[string]*invoice.Invoice{},
		settings:       map[string]match.Settings{},
		submitKeys:     map[string]map[string]string{},
		decideKeys:     map[string]map[string]string{},
		reqNums:        map[string]int64{},
		poNums:         map[string]int64{},
		grnNums:        map[string]int64{},
		exportRequests: map[string][]string{},
	}
}

func (m *memState) newID() string {
	m.seq++
	return fmt.Sprintf("dddddddd-0000-0000-0000-%012d", m.seq)
}

// nextReqNum/nextPONum/nextGRNNum allocate the per-tenant %06d sequences.
func (m *memState) nextReqNum(tenantID string) string {
	m.reqNums[tenantID]++
	return fmt.Sprintf("PR-%06d", m.reqNums[tenantID])
}

func (m *memState) nextPONum(tenantID string) string {
	m.poNums[tenantID]++
	return fmt.Sprintf("PO-%06d", m.poNums[tenantID])
}

func (m *memState) nextGRNNum(tenantID string) string {
	m.grnNums[tenantID]++
	return fmt.Sprintf("GRN-%06d", m.grnNums[tenantID])
}

// stepsOfReq returns the requisition's stored steps ordered by step_no.
func (m *memState) stepsOfReq(tenantID, requisitionID string) []*approval.Step {
	var out []*approval.Step
	for _, st := range m.steps[tenantID] {
		if st.RequisitionID == requisitionID {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StepNo < out[j].StepNo })
	return out
}

// reqValues snapshots the stored steps as domain values (approval.Decide's
// input contract: it never mutates the caller's slice).
func stepValues(steps []*approval.Step) []approval.Step {
	out := make([]approval.Step, 0, len(steps))
	for _, st := range steps {
		out = append(out, *st)
	}
	return out
}

// ---------------------------------------------------------------------------
// Clone helpers: work on a copy, commit on success — the fake's rollback.
// ---------------------------------------------------------------------------

func cloneReq(r requisition.Requisition) *requisition.Requisition {
	out := r
	out.Items = append([]requisition.Item(nil), r.Items...)
	if r.SubmittedAt != nil {
		at := *r.SubmittedAt
		out.SubmittedAt = &at
	}
	return &out
}

func clonePO(po purchaseorder.PurchaseOrder) *purchaseorder.PurchaseOrder {
	out := po
	out.Items = append([]purchaseorder.Item(nil), po.Items...)
	if po.IssuedAt != nil {
		at := *po.IssuedAt
		out.IssuedAt = &at
	}
	return &out
}

func cloneStep(st approval.Step) *approval.Step {
	out := st
	if st.DecidedAt != nil {
		at := *st.DecidedAt
		out.DecidedAt = &at
	}
	return &out
}

func cloneInvoice(inv invoice.Invoice) *invoice.Invoice {
	out := inv
	out.Items = append([]invoice.Item(nil), inv.Items...)
	return &out
}

// ---------------------------------------------------------------------------
// The fakes. Each records the tenant ids it was called with (asserting the
// handler forwards auth.TenantID) and supports an injected IO error.
// ---------------------------------------------------------------------------

type fakeVendorStore struct {
	*memState
	err       error
	tenants   []string
	listCalls int
}

func (f *fakeVendorStore) record(tenantID string) { f.tenants = append(f.tenants, tenantID) }

func (f *fakeVendorStore) Create(_ context.Context, tenantID string, in vendor.CreateInput) (vendor.Vendor, error) {
	f.record(tenantID)
	if f.err != nil {
		return vendor.Vendor{}, f.err
	}
	in.Code = vendor.Normalize(in.Code)
	for _, v := range f.vendors[tenantID] {
		if v.Code == in.Code {
			return vendor.Vendor{}, vendor.ErrDuplicateCode
		}
	}
	created := vendor.Vendor{
		ID: f.newID(), TenantID: tenantID, Code: in.Code, Name: in.Name,
		ContactEmail: in.ContactEmail, TaxID: in.TaxID, Status: vendor.StatusActive,
		CreatedAt: f.now, CreatedBy: in.UserID, UpdatedAt: f.now, UpdatedBy: in.UserID,
	}
	if f.vendors[tenantID] == nil {
		f.vendors[tenantID] = map[string]*vendor.Vendor{}
	}
	f.vendors[tenantID][created.ID] = &created
	return created, nil
}

func (f *fakeVendorStore) Get(_ context.Context, tenantID, id string) (vendor.Vendor, error) {
	f.record(tenantID)
	if f.err != nil {
		return vendor.Vendor{}, f.err
	}
	v, ok := f.vendors[tenantID][id]
	if !ok {
		return vendor.Vendor{}, vendor.ErrNotFound
	}
	return *v, nil
}

func (f *fakeVendorStore) Update(_ context.Context, tenantID string, in vendor.UpdateInput) (vendor.Vendor, error) {
	f.record(tenantID)
	if f.err != nil {
		return vendor.Vendor{}, f.err
	}
	v, ok := f.vendors[tenantID][in.ID]
	if !ok {
		return vendor.Vendor{}, vendor.ErrNotFound
	}
	if v.Status != vendor.StatusActive {
		return vendor.Vendor{}, vendor.ErrDeactivated
	}
	if in.Name != "" {
		v.Name = in.Name
	}
	if in.ContactEmail != "" {
		v.ContactEmail = in.ContactEmail
	}
	if in.TaxID != "" {
		v.TaxID = in.TaxID
	}
	v.UpdatedAt = f.now
	v.UpdatedBy = in.UserID
	return *v, nil
}

func (f *fakeVendorStore) Deactivate(_ context.Context, tenantID, id, userID string) (vendor.Vendor, error) {
	f.record(tenantID)
	if f.err != nil {
		return vendor.Vendor{}, f.err
	}
	v, ok := f.vendors[tenantID][id]
	if !ok {
		return vendor.Vendor{}, vendor.ErrNotFound
	}
	if v.Status != vendor.StatusActive {
		return vendor.Vendor{}, vendor.ErrDeactivated
	}
	v.Status = vendor.StatusDeactivated
	v.UpdatedAt = f.now
	v.UpdatedBy = userID
	return *v, nil
}

// List mirrors the real predicates: status equality, a code-prefix or
// name-substring query (case-insensitive, as ILIKE), code-ascending order,
// window total with the past-the-end COUNT fallback.
func (f *fakeVendorStore) List(_ context.Context, tenantID string, flt vendor.ListFilter) ([]vendor.Vendor, int, error) {
	f.record(tenantID)
	f.listCalls++
	if f.err != nil {
		return nil, 0, f.err
	}
	var matched []vendor.Vendor
	for _, v := range f.vendors[tenantID] {
		if flt.Status != "" && v.Status != flt.Status {
			continue
		}
		if flt.Query != "" &&
			!strings.HasPrefix(v.Code, flt.Query) &&
			!strings.Contains(strings.ToUpper(v.Name), flt.Query) {
			continue
		}
		matched = append(matched, *v)
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Code < matched[j].Code })
	total := len(matched)
	offset := (int(flt.Page) - 1) * int(flt.PageSize)
	if offset > total {
		offset = total
	}
	end := offset + int(flt.PageSize)
	if end > total {
		end = total
	}
	return matched[offset:end], total, nil
}

type fakeRequisitionStore struct {
	*memState
	err     error
	tenants []string
}

func (f *fakeRequisitionStore) record(tenantID string) { f.tenants = append(f.tenants, tenantID) }

func (f *fakeRequisitionStore) Create(_ context.Context, tenantID string, in requisition.CreateInput) (requisition.Requisition, error) {
	f.record(tenantID)
	if f.err != nil {
		return requisition.Requisition{}, f.err
	}
	// Validate through the domain constructor first, allocate the number and
	// id only on success — the real store's transaction rolls both back on a
	// validation failure, so a rejected create burns neither.
	agg, err := requisition.New(requisition.NewInput{
		TenantID: tenantID, RequesterID: in.RequesterID,
		Currency: in.Currency, Items: in.Items, Now: f.now, CreatedBy: in.RequesterID,
	})
	if err != nil {
		return requisition.Requisition{}, err // bare sentinel, the store's wrap contract
	}
	agg.Number = f.nextReqNum(tenantID)
	agg.ID = f.newID()
	if f.reqs[tenantID] == nil {
		f.reqs[tenantID] = map[string]*requisition.Requisition{}
	}
	f.reqs[tenantID][agg.ID] = agg
	return *cloneReq(*agg), nil
}

func (f *fakeRequisitionStore) Get(_ context.Context, tenantID, id string) (requisition.Requisition, error) {
	f.record(tenantID)
	if f.err != nil {
		return requisition.Requisition{}, f.err
	}
	if _, err := uuid.Parse(id); err != nil {
		return requisition.Requisition{}, requisition.ErrIDInvalid
	}
	r, ok := f.reqs[tenantID][id]
	if !ok {
		return requisition.Requisition{}, requisition.ErrNotFound
	}
	return *cloneReq(*r), nil
}

func (f *fakeRequisitionStore) UpdateItems(_ context.Context, tenantID, id string, items []requisition.Item, actor string) (requisition.Requisition, error) {
	f.record(tenantID)
	if f.err != nil {
		return requisition.Requisition{}, f.err
	}
	if _, err := uuid.Parse(id); err != nil {
		return requisition.Requisition{}, requisition.ErrIDInvalid
	}
	stored, ok := f.reqs[tenantID][id]
	if !ok {
		return requisition.Requisition{}, requisition.ErrNotFound
	}
	agg := cloneReq(*stored)
	if err := agg.UpdateItems(items, f.now, actor); err != nil {
		return requisition.Requisition{}, err
	}
	f.reqs[tenantID][id] = agg
	return *agg, nil
}

// Submit mirrors the real store's contract: replay returns the current state,
// a key naming another requisition conflicts, the domain transition guards
// the edge, and the materialize callback runs inside the "transaction" — its
// failure (approval.ErrRulesMissing) rolls everything back, key included.
// The tx argument is nil: both fakes own all the state.
func (f *fakeRequisitionStore) Submit(_ context.Context, tenantID, id, actor, idempotencyKey string, materialize func(tx pgx.Tx, r requisition.Requisition) error) (requisition.Requisition, error) {
	f.record(tenantID)
	if f.err != nil {
		return requisition.Requisition{}, f.err
	}
	if _, err := uuid.Parse(id); err != nil {
		return requisition.Requisition{}, requisition.ErrIDInvalid
	}
	stored, ok := f.reqs[tenantID][id]
	if !ok {
		return requisition.Requisition{}, requisition.ErrNotFound
	}
	if idempotencyKey != "" {
		if named, used := f.submitKeys[tenantID][idempotencyKey]; used {
			if named != id {
				return requisition.Requisition{}, requisition.ErrIdempotencyKeyConflict
			}
			return *cloneReq(*stored), nil // replay: current state, no transition
		}
	}
	agg := cloneReq(*stored)
	if err := agg.Submit(f.now, actor); err != nil {
		return requisition.Requisition{}, err
	}
	if materialize != nil {
		if err := materialize(nil, *agg); err != nil {
			return requisition.Requisition{}, err // rolls the submit back
		}
	}
	f.reqs[tenantID][id] = agg
	if idempotencyKey != "" {
		if f.submitKeys[tenantID] == nil {
			f.submitKeys[tenantID] = map[string]string{}
		}
		f.submitKeys[tenantID][idempotencyKey] = id
	}
	return *agg, nil
}

func (f *fakeRequisitionStore) List(_ context.Context, tenantID string, flt requisition.ListFilter) ([]requisition.Requisition, int, error) {
	f.record(tenantID)
	if f.err != nil {
		return nil, 0, f.err
	}
	var matched []requisition.Requisition
	for _, r := range f.reqs[tenantID] {
		if flt.Status != "" && r.Status != flt.Status {
			continue
		}
		if flt.Query != "" && !strings.HasPrefix(r.Number, flt.Query) {
			continue
		}
		header := cloneReq(*r)
		header.Items = nil // rows are header-only; the detail read owns items
		matched = append(matched, *header)
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Number > matched[j].Number })
	total := len(matched)
	offset := (int(flt.Page) - 1) * int(flt.PageSize)
	if offset > total {
		offset = total
	}
	end := offset + int(flt.PageSize)
	if end > total {
		end = total
	}
	return matched[offset:end], total, nil
}

type fakeApprovalStore struct {
	*memState
	err     error
	tenants []string
}

func (f *fakeApprovalStore) record(tenantID string) { f.tenants = append(f.tenants, tenantID) }

func (f *fakeApprovalStore) ListRules(_ context.Context, tenantID string) ([]approval.Rule, error) {
	f.record(tenantID)
	if f.err != nil {
		return nil, f.err
	}
	return append([]approval.Rule(nil), f.rules[tenantID]...), nil
}

func (f *fakeApprovalStore) ReplaceRules(_ context.Context, tenantID string, rules []approval.Rule) ([]approval.Rule, error) {
	f.record(tenantID)
	if f.err != nil {
		return nil, f.err
	}
	if err := approval.ValidateRuleSet(rules); err != nil {
		return nil, err
	}
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].Currency != rules[j].Currency {
			return rules[i].Currency < rules[j].Currency
		}
		return rules[i].StepNo < rules[j].StepNo
	})
	stored := make([]approval.Rule, 0, len(rules))
	for _, r := range rules {
		r.ID = f.newID()
		stored = append(stored, r)
	}
	f.rules[tenantID] = stored
	return append([]approval.Rule(nil), stored...), nil
}

// MaterializeTx ignores the nil tx the fake submit passes and works straight
// on the shared state — same rules, same Materialize, same ErrRulesMissing.
func (f *fakeApprovalStore) MaterializeTx(_ context.Context, _ pgx.Tx, r requisition.Requisition) ([]approval.Step, error) {
	if f.err != nil {
		return nil, f.err
	}
	steps, err := approval.Materialize(f.rules[r.TenantID], r.ID, r.Currency, r.TotalCents)
	if err != nil {
		return nil, err
	}
	if f.steps[r.TenantID] == nil {
		f.steps[r.TenantID] = map[string]*approval.Step{}
	}
	out := make([]approval.Step, 0, len(steps))
	for i := range steps {
		steps[i].ID = f.newID()
		stored := steps[i]
		f.steps[r.TenantID][stored.ID] = &stored
		out = append(out, stored)
	}
	return out, nil
}

func (f *fakeApprovalStore) ListSteps(_ context.Context, tenantID, requisitionID string) ([]approval.Step, error) {
	f.record(tenantID)
	if f.err != nil {
		return nil, f.err
	}
	if _, err := uuid.Parse(requisitionID); err != nil {
		return nil, requisition.ErrIDInvalid
	}
	return stepValues(f.stepsOfReq(tenantID, requisitionID)), nil
}

// Decide mirrors the store's transaction: replay/conflict on the decide key,
// the parent requisition as the domain's locked aggregate, and the pure
// approval.Decide for every guard (self-approval, role, currency, sequence).
func (f *fakeApprovalStore) Decide(_ context.Context, tenantID string, in approval.DecideInput) (requisition.Requisition, []approval.Step, error) {
	f.record(tenantID)
	if f.err != nil {
		return requisition.Requisition{}, nil, f.err
	}
	if _, err := uuid.Parse(in.StepID); err != nil {
		return requisition.Requisition{}, nil, approval.ErrStepIDInvalid
	}
	step, ok := f.steps[tenantID][in.StepID]
	if !ok {
		return requisition.Requisition{}, nil, approval.ErrStepNotFound
	}
	stored, ok := f.reqs[tenantID][step.RequisitionID]
	if !ok {
		return requisition.Requisition{}, nil, requisition.ErrNotFound
	}
	req := cloneReq(*stored)
	if in.IdempotencyKey != "" {
		if named, used := f.decideKeys[tenantID][in.IdempotencyKey]; used {
			if named != in.StepID {
				return requisition.Requisition{}, nil, approval.ErrIdempotencyKeyConflict
			}
			return *req, stepValues(f.stepsOfReq(tenantID, req.ID)), nil // replay
		}
	}
	steps := stepValues(f.stepsOfReq(tenantID, req.ID))
	updated, err := approval.Decide(req, steps, approval.DecisionInput{
		StepID: in.StepID, Decision: in.Decision, Reason: in.Reason,
		ActorID: in.ActorID, ActorRoles: in.Roles, Now: f.now,
	})
	if err != nil {
		return requisition.Requisition{}, nil, err
	}
	f.reqs[tenantID][req.ID] = req
	for i := range updated {
		if existing, ok := f.steps[tenantID][updated[i].ID]; ok {
			*existing = updated[i]
		}
	}
	if in.IdempotencyKey != "" {
		if f.decideKeys[tenantID] == nil {
			f.decideKeys[tenantID] = map[string]string{}
		}
		f.decideKeys[tenantID][in.IdempotencyKey] = in.StepID
	}
	return *req, updated, nil
}

type fakePurchaseOrderStore struct {
	*memState
	err     error
	tenants []string
}

func (f *fakePurchaseOrderStore) record(tenantID string) { f.tenants = append(f.tenants, tenantID) }

// CreateFromRequisition follows the real store's pinned order: id gates, the
// replay lookup, the vendor gate, the status gate, number allocation, the
// domain close, and the copy with server-side total.
func (f *fakePurchaseOrderStore) CreateFromRequisition(_ context.Context, tenantID, requisitionID, vendorID, actor string) (purchaseorder.PurchaseOrder, bool, error) {
	f.record(tenantID)
	if f.err != nil {
		return purchaseorder.PurchaseOrder{}, false, f.err
	}
	if _, err := uuid.Parse(requisitionID); err != nil {
		return purchaseorder.PurchaseOrder{}, false, requisition.ErrIDInvalid
	}
	vendorID = strings.TrimSpace(vendorID)
	if _, err := uuid.Parse(vendorID); err != nil {
		return purchaseorder.PurchaseOrder{}, false, purchaseorder.ErrVendorIDInvalid
	}
	req, ok := f.reqs[tenantID][requisitionID]
	if !ok {
		return purchaseorder.PurchaseOrder{}, false, requisition.ErrNotFound
	}
	for _, po := range f.pos[tenantID] { // replay: existing PO wins, vendor ignored
		if po.RequisitionID == requisitionID {
			return *clonePO(*po), false, nil
		}
	}
	v, ok := f.vendors[tenantID][vendorID]
	if !ok {
		return purchaseorder.PurchaseOrder{}, false, vendor.ErrNotFound
	}
	if v.Status != vendor.StatusActive {
		return purchaseorder.PurchaseOrder{}, false, vendor.ErrDeactivated
	}
	if req.Status != requisition.StatusApproved {
		return purchaseorder.PurchaseOrder{}, false, purchaseorder.ErrRequisitionNotApproved
	}
	items := make([]purchaseorder.Item, 0, len(req.Items))
	for _, it := range req.Items {
		items = append(items, purchaseorder.Item{
			ID: f.newID(), Description: it.Description,
			Qty: it.Qty, UnitPriceCents: it.UnitPriceCents,
		})
	}
	agg, err := purchaseorder.New(purchaseorder.NewInput{
		TenantID: tenantID, RequisitionID: req.ID,
		VendorID: vendorID, Currency: req.Currency, Items: items,
		Now: f.now, CreatedBy: actor,
	})
	if err != nil {
		return purchaseorder.PurchaseOrder{}, false, err
	}
	agg.Number = f.nextPONum(tenantID)
	agg.ID = f.newID()
	req = cloneReq(*req)
	if err := req.Close(f.now, actor); err != nil {
		return purchaseorder.PurchaseOrder{}, false, err
	}
	f.reqs[tenantID][req.ID] = req
	if f.pos[tenantID] == nil {
		f.pos[tenantID] = map[string]*purchaseorder.PurchaseOrder{}
	}
	f.pos[tenantID][agg.ID] = agg
	return *clonePO(*agg), true, nil
}

// transition runs one lifecycle action through the domain method, committing
// only on success (the guarded UPDATE's copy).
func (f *fakePurchaseOrderStore) transition(tenantID, id, actor string, apply func(*purchaseorder.PurchaseOrder, time.Time, string) error) (purchaseorder.PurchaseOrder, error) {
	f.record(tenantID)
	if f.err != nil {
		return purchaseorder.PurchaseOrder{}, f.err
	}
	if _, err := uuid.Parse(id); err != nil {
		return purchaseorder.PurchaseOrder{}, purchaseorder.ErrIDInvalid
	}
	stored, ok := f.pos[tenantID][id]
	if !ok {
		return purchaseorder.PurchaseOrder{}, purchaseorder.ErrNotFound
	}
	agg := clonePO(*stored)
	if err := apply(agg, f.now, actor); err != nil {
		return purchaseorder.PurchaseOrder{}, err
	}
	f.pos[tenantID][id] = agg
	return *clonePO(*agg), nil
}

func (f *fakePurchaseOrderStore) Issue(_ context.Context, tenantID, id, actor string) (purchaseorder.PurchaseOrder, error) {
	return f.transition(tenantID, id, actor, (*purchaseorder.PurchaseOrder).Issue)
}

func (f *fakePurchaseOrderStore) Acknowledge(_ context.Context, tenantID, id, actor string) (purchaseorder.PurchaseOrder, error) {
	return f.transition(tenantID, id, actor, (*purchaseorder.PurchaseOrder).Acknowledge)
}

func (f *fakePurchaseOrderStore) Cancel(_ context.Context, tenantID, id, actor string) (purchaseorder.PurchaseOrder, error) {
	return f.transition(tenantID, id, actor, (*purchaseorder.PurchaseOrder).Cancel)
}

func (f *fakePurchaseOrderStore) Close(_ context.Context, tenantID, id, actor string) (purchaseorder.PurchaseOrder, error) {
	return f.transition(tenantID, id, actor, (*purchaseorder.PurchaseOrder).Close)
}

func (f *fakePurchaseOrderStore) Get(_ context.Context, tenantID, id string) (purchaseorder.PurchaseOrder, error) {
	f.record(tenantID)
	if f.err != nil {
		return purchaseorder.PurchaseOrder{}, f.err
	}
	if _, err := uuid.Parse(id); err != nil {
		return purchaseorder.PurchaseOrder{}, purchaseorder.ErrIDInvalid
	}
	po, ok := f.pos[tenantID][id]
	if !ok {
		return purchaseorder.PurchaseOrder{}, purchaseorder.ErrNotFound
	}
	return *clonePO(*po), nil
}

func (f *fakePurchaseOrderStore) List(_ context.Context, tenantID string, flt purchaseorder.ListFilter) ([]purchaseorder.PurchaseOrder, int, error) {
	f.record(tenantID)
	if f.err != nil {
		return nil, 0, f.err
	}
	var matched []purchaseorder.PurchaseOrder
	for _, po := range f.pos[tenantID] {
		if flt.Status != "" && po.Status != flt.Status {
			continue
		}
		if flt.Query != "" && !strings.HasPrefix(po.Number, flt.Query) {
			continue
		}
		header := clonePO(*po)
		header.Items = nil // rows are header-only; the detail read owns items
		matched = append(matched, *header)
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Number > matched[j].Number })
	total := len(matched)
	offset := (int(flt.Page) - 1) * int(flt.PageSize)
	if offset > total {
		offset = total
	}
	end := offset + int(flt.PageSize)
	if end > total {
		end = total
	}
	return matched[offset:end], total, nil
}

type fakeGoodsReceiptStore struct {
	*memState
	err     error
	tenants []string
}

func (f *fakeGoodsReceiptStore) record(tenantID string) { f.tenants = append(f.tenants, tenantID) }

// Post mirrors the real store's pinned order: shape validation, the PO load
// (tenant-scoped), purchaseorder.ApplyReceipt for every guard and the status
// branch, then the GRN with its per-tenant number.
func (f *fakeGoodsReceiptStore) Post(_ context.Context, tenantID string, in goodsreceipt.PostInput) (goodsreceipt.GoodsReceipt, purchaseorder.PurchaseOrder, error) {
	f.record(tenantID)
	if f.err != nil {
		return goodsreceipt.GoodsReceipt{}, purchaseorder.PurchaseOrder{}, f.err
	}
	if err := goodsreceipt.Validate(in.Items, in.Notes); err != nil {
		return goodsreceipt.GoodsReceipt{}, purchaseorder.PurchaseOrder{}, err
	}
	if _, err := uuid.Parse(in.POID); err != nil {
		return goodsreceipt.GoodsReceipt{}, purchaseorder.PurchaseOrder{}, purchaseorder.ErrIDInvalid
	}
	stored, ok := f.pos[tenantID][in.POID]
	if !ok {
		return goodsreceipt.GoodsReceipt{}, purchaseorder.PurchaseOrder{}, purchaseorder.ErrNotFound
	}
	po := clonePO(*stored)
	lines := make([]purchaseorder.ReceiptLine, 0, len(in.Items))
	for _, it := range in.Items {
		lines = append(lines, purchaseorder.ReceiptLine{POItemID: it.POItemID, Qty: it.QtyReceived})
	}
	if err := po.ApplyReceipt(f.now, in.Actor, lines); err != nil {
		return goodsreceipt.GoodsReceipt{}, purchaseorder.PurchaseOrder{}, err
	}
	grn := goodsreceipt.GoodsReceipt{
		ID: f.newID(), TenantID: tenantID, POID: po.ID,
		Number: f.nextGRNNum(tenantID), Notes: in.Notes, ReceivedBy: in.Actor,
		ReceivedAt: f.now, CreatedAt: f.now, Items: append([]goodsreceipt.Item(nil), in.Items...),
	}
	if f.grns[tenantID] == nil {
		f.grns[tenantID] = map[string]*goodsreceipt.GoodsReceipt{}
	}
	f.grns[tenantID][grn.ID] = &grn
	f.pos[tenantID][po.ID] = po
	return grn, *clonePO(*po), nil
}

type fakeInvoiceStore struct {
	*memState
	err     error
	tenants []string
}

func (f *fakeInvoiceStore) record(tenantID string) { f.tenants = append(f.tenants, tenantID) }

// Create mirrors the real store's pinned order: shape gates, the locked-PO
// load (tenant-scoped), line membership, the cumulative over-billing guard,
// the status gate, and the per-(tenant, vendor, number) duplicate rule.
func (f *fakeInvoiceStore) Create(_ context.Context, tenantID string, in invoice.CreateInput) (invoice.Invoice, error) {
	f.record(tenantID)
	if f.err != nil {
		return invoice.Invoice{}, f.err
	}
	in.Number = strings.TrimSpace(in.Number)
	if err := invoice.ValidateNumber(in.Number); err != nil {
		return invoice.Invoice{}, err
	}
	if err := invoice.ValidateItems(in.Items); err != nil {
		return invoice.Invoice{}, err
	}
	if _, err := uuid.Parse(in.POID); err != nil {
		return invoice.Invoice{}, purchaseorder.ErrIDInvalid
	}
	po, ok := f.pos[tenantID][in.POID]
	if !ok {
		return invoice.Invoice{}, purchaseorder.ErrNotFound
	}
	members := make(map[string]bool, len(po.Items))
	for _, it := range po.Items {
		members[it.ID] = true
	}
	for _, it := range in.Items {
		if !members[it.POItemID] {
			return invoice.Invoice{}, invoice.ErrPOItemUnknown
		}
	}
	total := invoice.TotalCents(in.Items)
	var invoiced int64
	for _, inv := range f.invs[tenantID] {
		if inv.POID == po.ID {
			invoiced += inv.TotalCents
		}
	}
	if invoiced+total > po.TotalCents {
		return invoice.Invoice{}, &invoice.OverBillError{
			PONumber: po.Number, Invoiced: invoiced, Requested: total, Total: po.TotalCents,
		}
	}
	switch po.Status {
	case purchaseorder.StatusAcknowledged,
		purchaseorder.StatusPartiallyReceived,
		purchaseorder.StatusReceived:
		// invoicable
	default:
		return invoice.Invoice{}, purchaseorder.ErrIllegalTransition
	}
	for _, inv := range f.invs[tenantID] {
		if inv.VendorID == po.VendorID && inv.Number == in.Number {
			return invoice.Invoice{}, invoice.ErrNumberDuplicate
		}
	}
	created := invoice.Invoice{
		ID: f.newID(), TenantID: tenantID, POID: po.ID, VendorID: po.VendorID,
		Number: in.Number, Currency: po.Currency, TotalCents: total,
		Status: invoice.StatusPending, Items: append([]invoice.Item(nil), in.Items...),
		CreatedBy: in.Actor, CreatedAt: f.now,
	}
	if f.invs[tenantID] == nil {
		f.invs[tenantID] = map[string]*invoice.Invoice{}
	}
	f.invs[tenantID][created.ID] = &created
	return *cloneInvoice(created), nil
}

func (f *fakeInvoiceStore) Get(_ context.Context, tenantID, id string) (invoice.Invoice, error) {
	f.record(tenantID)
	if f.err != nil {
		return invoice.Invoice{}, f.err
	}
	if _, err := uuid.Parse(id); err != nil {
		return invoice.Invoice{}, invoice.ErrIDInvalid
	}
	inv, ok := f.invs[tenantID][id]
	if !ok {
		return invoice.Invoice{}, invoice.ErrNotFound
	}
	return *cloneInvoice(*inv), nil
}

func (f *fakeInvoiceStore) List(_ context.Context, tenantID string, flt invoice.ListFilter) ([]invoice.Invoice, int, error) {
	f.record(tenantID)
	if f.err != nil {
		return nil, 0, f.err
	}
	var matched []invoice.Invoice
	for _, inv := range f.invs[tenantID] {
		if flt.Status != "" && inv.Status != flt.Status {
			continue
		}
		if flt.Query != "" && !strings.HasPrefix(strings.ToUpper(inv.Number), flt.Query) {
			continue
		}
		header := cloneInvoice(*inv)
		header.Items = nil      // rows are header-only; the detail read owns lines
		header.MatchResult = "" // ...and never carries the stored verdict
		matched = append(matched, *header)
	}
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		}
		return matched[i].ID > matched[j].ID
	})
	total := len(matched)
	offset := (int(flt.Page) - 1) * int(flt.PageSize)
	if offset > total {
		offset = total
	}
	end := offset + int(flt.PageSize)
	if end > total {
		end = total
	}
	return matched[offset:end], total, nil
}

type fakeMatchStore struct {
	*memState
	err     error
	tenants []string
}

func (f *fakeMatchStore) record(tenantID string) { f.tenants = append(f.tenants, tenantID) }

// Match mirrors the store: invoice resolve (tenant-scoped), PO load, the
// re-run guard, the tenant's tolerances, the cumulative invoiced basis, and
// the pure engine's verdict.
func (f *fakeMatchStore) Match(_ context.Context, tenantID, invoiceID, actor string) (invoice.Invoice, match.Result, error) {
	f.record(tenantID)
	if f.err != nil {
		return invoice.Invoice{}, match.Result{}, f.err
	}
	if _, err := uuid.Parse(invoiceID); err != nil {
		return invoice.Invoice{}, match.Result{}, invoice.ErrIDInvalid
	}
	inv, ok := f.invs[tenantID][invoiceID]
	if !ok {
		return invoice.Invoice{}, match.Result{}, invoice.ErrNotFound
	}
	po, ok := f.pos[tenantID][inv.POID]
	if !ok {
		return invoice.Invoice{}, match.Result{}, purchaseorder.ErrNotFound
	}
	if err := invoice.MatchGuard(inv.Status); err != nil {
		return invoice.Invoice{}, match.Result{}, err
	}
	settings, ok := f.settings[tenantID]
	if !ok {
		settings = match.DefaultSettings()
	}
	invoicedQty := map[string]int32{}
	for _, other := range f.invs[tenantID] {
		if other.POID != po.ID {
			continue
		}
		for _, it := range other.Items {
			invoicedQty[it.POItemID] += it.Qty
		}
	}
	lines := make([]match.Line, 0, len(inv.Items))
	for _, it := range inv.Items {
		lines = append(lines, match.Line{
			POItemID: it.POItemID, Qty: it.Qty, UnitPriceCents: it.UnitPriceCents,
			QtyInvoicedCum: invoicedQty[it.POItemID],
		})
	}
	poItems := make([]match.POItem, 0, len(po.Items))
	for _, it := range po.Items {
		poItems = append(poItems, match.POItem{
			ID: it.ID, QtyOrdered: it.Qty, QtyReceived: it.QtyReceived,
			UnitPriceCents: it.UnitPriceCents,
		})
	}
	result, err := match.Run(match.Input{POItems: poItems, Lines: lines, Settings: settings})
	if err != nil {
		return invoice.Invoice{}, match.Result{}, err
	}
	inv = cloneInvoice(*inv)
	inv.Status = invoice.StatusDiscrepancy
	if result.Matched {
		inv.Status = invoice.StatusMatched
	}
	inv.MatchResult = result.ResultJSON()
	f.invs[tenantID][inv.ID] = inv
	return *cloneInvoice(*inv), result, nil
}

func (f *fakeMatchStore) GetSettings(_ context.Context, tenantID string) (match.Settings, error) {
	f.record(tenantID)
	if f.err != nil {
		return match.Settings{}, f.err
	}
	if s, ok := f.settings[tenantID]; ok {
		return s, nil
	}
	return match.DefaultSettings(), nil
}

func (f *fakeMatchStore) ReplaceSettings(_ context.Context, tenantID string, in match.Settings, _ string) (match.Settings, error) {
	f.record(tenantID)
	if f.err != nil {
		return match.Settings{}, f.err
	}
	if err := match.ValidateSettings(in); err != nil {
		return match.Settings{}, err
	}
	f.settings[tenantID] = in
	return in, nil
}

// fakeDashboardStore computes the three widgets straight from the shared
// state — same shapes the SQL store aggregates, trivial at fake scale.
type fakeDashboardStore struct {
	*memState
	err     error
	tenants []string
}

func (f *fakeDashboardStore) record(tenantID string) { f.tenants = append(f.tenants, tenantID) }

func (f *fakeDashboardStore) Summary(_ context.Context, tenantID string, now time.Time) (dashboard.Summary, error) {
	f.record(tenantID)
	if f.err != nil {
		return dashboard.Summary{}, f.err
	}
	// Pending: every submitted requisition's current (smallest pending) step,
	// oldest-submitted first.
	var pending []dashboard.PendingApproval
	for _, r := range f.reqs[tenantID] {
		if r.Status != requisition.StatusSubmitted || r.SubmittedAt == nil {
			continue
		}
		for _, st := range f.stepsOfReq(tenantID, r.ID) {
			if st.Status != approval.StepPending {
				continue
			}
			pending = append(pending, dashboard.PendingApproval{
				RequisitionID: r.ID, Number: r.Number, Currency: r.Currency,
				TotalCents: r.TotalCents, SubmittedAt: *r.SubmittedAt,
				StepID: st.ID, StepNo: st.StepNo, RoleRequired: st.RoleRequired,
			})
			break
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if !pending[i].SubmittedAt.Equal(pending[j].SubmittedAt) {
			return pending[i].SubmittedAt.Before(pending[j].SubmittedAt)
		}
		return pending[i].Number < pending[j].Number
	})
	total := len(pending)
	if len(pending) > dashboard.PendingPageSize {
		pending = pending[:dashboard.PendingPageSize]
	}
	// Spend: issued POs per vendor+currency inside the windows.
	type key struct{ vendor, currency string }
	spend := map[key]*dashboard.VendorSpend{}
	totals := map[string]*dashboard.CurrencySpend{}
	for _, po := range f.pos[tenantID] {
		if po.Status == purchaseorder.StatusDraft || po.Status == purchaseorder.StatusCancelled ||
			po.IssuedAt == nil || po.IssuedAt.Before(now.Add(-dashboard.SpendWindow90d)) {
			continue
		}
		k := key{po.VendorID, po.Currency}
		row := spend[k]
		if row == nil {
			name := ""
			if v, ok := f.vendors[tenantID][po.VendorID]; ok {
				name = v.Name
			}
			row = &dashboard.VendorSpend{VendorID: po.VendorID, VendorName: name, Currency: po.Currency}
			spend[k] = row
			totals[po.Currency] = &dashboard.CurrencySpend{Currency: po.Currency}
		}
		row.SpendCents90d += po.TotalCents
		totals[po.Currency].NinetyDayCents += po.TotalCents
		if !po.IssuedAt.Before(now.Add(-dashboard.SpendWindow30d)) {
			row.SpendCents30d += po.TotalCents
			totals[po.Currency].ThirtyDayCents += po.TotalCents
		}
	}
	rows := make([]dashboard.VendorSpend, 0, len(spend))
	for _, row := range spend {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].SpendCents90d != rows[j].SpendCents90d {
			return rows[i].SpendCents90d > rows[j].SpendCents90d
		}
		return rows[i].VendorID < rows[j].VendorID
	})
	if len(rows) > dashboard.SpendVendorLimit {
		rows = rows[:dashboard.SpendVendorLimit]
	}
	currencyTotals := make([]dashboard.CurrencySpend, 0, len(totals))
	for _, t := range totals {
		currencyTotals = append(currencyTotals, *t)
	}
	sort.Slice(currencyTotals, func(i, j int) bool {
		return currencyTotals[i].NinetyDayCents > currencyTotals[j].NinetyDayCents
	})
	// Distribution: all seven statuses, zeros included.
	counts := map[string]int64{}
	for _, po := range f.pos[tenantID] {
		counts[po.Status]++
	}
	dist := make([]dashboard.StatusCount, 0, len(dashboard.POStatusOrder))
	for _, status := range dashboard.POStatusOrder {
		dist = append(dist, dashboard.StatusCount{Status: status, Count: counts[status]})
	}
	return dashboard.Summary{
		Pending:              dashboard.PendingApprovals{Total: total, Items: pending},
		SpendByVendor:        rows,
		SpendTotals:          currencyTotals,
		POStatusDistribution: dist,
	}, nil
}

type fakeExportStore struct {
	*memState
	err     error
	tenants []string
}

func (f *fakeExportStore) record(tenantID string) { f.tenants = append(f.tenants, tenantID) }

func (f *fakeExportStore) Request(_ context.Context, tenantID, actorID, _, _ string, _ time.Time) (string, error) {
	f.record(tenantID)
	if f.err != nil {
		return "", f.err
	}
	eventID := f.newID()
	f.exportRequests[tenantID] = append(f.exportRequests[tenantID], eventID+" "+actorID)
	return eventID, nil
}

// fakePinger stands in for the pgx pool's Ping in the constructor test.
type fakePinger struct{ err error }

func (p fakePinger) Ping(context.Context) error { return p.err }

// fakes bundles the nine fakes over one shared state, wired the way the
// handler wires the concrete stores (the submit callback crosses
// requisition → approval through the API's own closure).
type fakes struct {
	state      *memState
	vendors    *fakeVendorStore
	reqs       *fakeRequisitionStore
	approvals  *fakeApprovalStore
	pos        *fakePurchaseOrderStore
	grns       *fakeGoodsReceiptStore
	invoices   *fakeInvoiceStore
	matches    *fakeMatchStore
	dashboards *fakeDashboardStore
	exports    *fakeExportStore
}

func newFakes() *fakes {
	state := newMemState()
	return &fakes{
		state:      state,
		vendors:    &fakeVendorStore{memState: state},
		reqs:       &fakeRequisitionStore{memState: state},
		approvals:  &fakeApprovalStore{memState: state},
		pos:        &fakePurchaseOrderStore{memState: state},
		grns:       &fakeGoodsReceiptStore{memState: state},
		invoices:   &fakeInvoiceStore{memState: state},
		matches:    &fakeMatchStore{memState: state},
		dashboards: &fakeDashboardStore{memState: state},
		exports:    &fakeExportStore{memState: state},
	}
}

// unitCtx builds the handler-side context directly: the gateway-propagated
// identity as INCOMING metadata, the way the transport delivers it (the
// integration tests' callerCtx builds the client-side outgoing half).
func unitCtx(userID, tenantID string, roles ...string) context.Context {
	md := metadata.Pairs(rbac.MDUserID, userID, rbac.MDTenantID, tenantID)
	for _, r := range roles {
		md.Append(rbac.MDRole, r)
	}
	return metadata.NewIncomingContext(context.Background(), md)
}

// lastTenant returns the most recently recorded tenant id of the fake.
func lastTenant(recorded []string) string {
	if len(recorded) == 0 {
		return ""
	}
	return recorded[len(recorded)-1]
}

// poItemByQty picks the PO line with the given ordered quantity (item ids are
// fake-generated; tests address lines by their stable business fields).
func poItemByQty(t *testing.T, po *purchaseorder.PurchaseOrder, qty int32) purchaseorder.Item {
	t.Helper()
	for _, it := range po.Items {
		if it.Qty == qty {
			return it
		}
	}
	t.Fatalf("no PO line with qty %d in %+v", qty, po.Items)
	return purchaseorder.Item{}
}

// seedApprovedReq inserts a requisition straight into the fake state at
// status approved (the submit flow's end state), returning its domain value.
func (m *memState) seedApprovedReq(t *testing.T, tenantID, requesterID string, items []requisition.Item) requisition.Requisition {
	t.Helper()
	agg, err := requisition.New(requisition.NewInput{
		TenantID: tenantID, RequesterID: requesterID,
		Currency: requisition.CurrencyIDR, Items: items, Now: m.now, CreatedBy: requesterID,
	})
	if err != nil {
		t.Fatalf("seed req: %v", err)
	}
	agg.Number = m.nextReqNum(tenantID)
	agg.ID = m.newID()
	if err := agg.Submit(m.now, requesterID); err != nil {
		t.Fatalf("seed submit: %v", err)
	}
	if err := agg.Approve(m.now, "dddddddd-0000-0000-0000-0000000000cc"); err != nil {
		t.Fatalf("seed approve: %v", err)
	}
	if m.reqs[tenantID] == nil {
		m.reqs[tenantID] = map[string]*requisition.Requisition{}
	}
	m.reqs[tenantID][agg.ID] = agg
	return *agg
}

// seedAcknowledgedPO converts a seeded approved requisition into an
// acknowledged PO — the state every receipt and invoice test starts from.
func (m *memState) seedAcknowledgedPO(t *testing.T, tenantID, requesterID, vendorCode string, items []requisition.Item) purchaseorder.PurchaseOrder {
	t.Helper()
	if _, err := m.seedVendorRaw(tenantID, vendorCode); err != nil {
		t.Fatalf("seed vendor: %v", err)
	}
	req := m.seedApprovedReq(t, tenantID, requesterID, items)
	var vendorID string
	for _, v := range m.vendors[tenantID] {
		if v.Code == vendor.Normalize(vendorCode) {
			vendorID = v.ID
		}
	}
	poItems := make([]purchaseorder.Item, 0, len(req.Items))
	for _, it := range req.Items {
		poItems = append(poItems, purchaseorder.Item{
			ID: m.newID(), Description: it.Description, Qty: it.Qty, UnitPriceCents: it.UnitPriceCents,
		})
	}
	agg, err := purchaseorder.New(purchaseorder.NewInput{
		TenantID: tenantID, RequisitionID: req.ID,
		VendorID: vendorID, Currency: req.Currency, Items: poItems,
		Now: m.now, CreatedBy: requesterID,
	})
	if err != nil {
		t.Fatalf("seed po: %v", err)
	}
	agg.Number = m.nextPONum(tenantID)
	agg.ID = m.newID()
	if err := agg.Issue(m.now, requesterID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	if err := agg.Acknowledge(m.now, requesterID); err != nil {
		t.Fatalf("seed acknowledge: %v", err)
	}
	if m.pos[tenantID] == nil {
		m.pos[tenantID] = map[string]*purchaseorder.PurchaseOrder{}
	}
	m.pos[tenantID][agg.ID] = agg
	return *clonePO(*agg)
}

// seedVendorRaw inserts an active vendor without going through the API.
func (m *memState) seedVendorRaw(tenantID, code string) (vendor.Vendor, error) {
	created := vendor.Vendor{
		ID: m.newID(), TenantID: tenantID, Code: vendor.Normalize(code),
		Name: "PT " + code, Status: vendor.StatusActive,
		CreatedAt: m.now, UpdatedAt: m.now,
	}
	if m.vendors[tenantID] == nil {
		m.vendors[tenantID] = map[string]*vendor.Vendor{}
	}
	m.vendors[tenantID][created.ID] = &created
	return created, nil
}

// seedInvoiceRaw inserts a pending invoice straight into the state.
func (m *memState) seedInvoiceRaw(tenantID string, po purchaseorder.PurchaseOrder, number string, items []invoice.Item) invoice.Invoice {
	inv := invoice.Invoice{
		ID: m.newID(), TenantID: tenantID, POID: po.ID, VendorID: po.VendorID,
		Number: number, Currency: po.Currency, TotalCents: invoice.TotalCents(items),
		Status: invoice.StatusPending, Items: append([]invoice.Item(nil), items...),
		CreatedAt: m.now,
	}
	if m.invs[tenantID] == nil {
		m.invs[tenantID] = map[string]*invoice.Invoice{}
	}
	m.invs[tenantID][inv.ID] = &inv
	return inv
}

// keyOfLen renders a deterministic idempotency key of the given length.
func keyOfLen(n int) string { return strings.Repeat("k", n) }
