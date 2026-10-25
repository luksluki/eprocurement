package server

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jackc/pgx/v5"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/procurement/approval"
	"github.com/luksluki/eprocurement/services/procurement/requisition"
	"github.com/luksluki/eprocurement/services/procurement/vendor"
)

// api adapts the vendor, requisition, and approval domains to the
// procurement.v1 gRPC contract. Every method reads the caller identity the
// gateway propagated (tenant scoping, audit ids); the RBAC interceptor has
// already re-checked the permission.
type api struct {
	procurementv1.UnimplementedProcurementServer
	store        *vendor.Store
	requisitions *requisition.Store
	approvals    *approval.Store
	log          *slog.Logger
}

// authTenant returns the caller's auth context, rejecting calls that reached
// this far without one.
func authTenant(ctx context.Context) (*rbac.AuthContext, error) {
	auth := rbac.FromMetadata(ctx)
	if auth == nil || auth.TenantID == "" {
		return nil, status.Error(codes.Unauthenticated, "auth context missing: no caller tenant")
	}
	return auth, nil
}

func (a *api) CreateVendor(ctx context.Context, req *procurementv1.CreateVendorRequest) (*procurementv1.Vendor, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	if err := vendor.ValidateCreate(vendor.Normalize(req.GetCode()), req.GetName(), req.GetContactEmail()); err != nil {
		return nil, mapVendorError(err)
	}
	v, err := a.store.Create(ctx, auth.TenantID, vendor.CreateInput{
		Code: req.GetCode(), Name: req.GetName(),
		ContactEmail: req.GetContactEmail(), TaxID: req.GetTaxId(),
		UserID: auth.UserID,
	})
	if err != nil {
		return nil, mapVendorError(err)
	}
	return vendorPB(v), nil
}

func (a *api) GetVendor(ctx context.Context, req *procurementv1.GetVendorRequest) (*procurementv1.Vendor, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	v, err := a.store.Get(ctx, auth.TenantID, req.GetId())
	if err != nil {
		return nil, mapVendorError(err)
	}
	return vendorPB(v), nil
}

func (a *api) UpdateVendor(ctx context.Context, req *procurementv1.UpdateVendorRequest) (*procurementv1.Vendor, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	if err := vendor.ValidateUpdate(req.GetName(), req.GetContactEmail()); err != nil {
		return nil, mapVendorError(err)
	}
	v, err := a.store.Update(ctx, auth.TenantID, vendor.UpdateInput{
		ID: req.GetId(), Name: req.GetName(),
		ContactEmail: req.GetContactEmail(), TaxID: req.GetTaxId(),
		UserID: auth.UserID,
	})
	if err != nil {
		return nil, mapVendorError(err)
	}
	return vendorPB(v), nil
}

func (a *api) DeactivateVendor(ctx context.Context, req *procurementv1.DeactivateVendorRequest) (*procurementv1.Vendor, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	v, err := a.store.Deactivate(ctx, auth.TenantID, req.GetId(), auth.UserID)
	if err != nil {
		return nil, mapVendorError(err)
	}
	return vendorPB(v), nil
}

func mapVendorError(err error) error {
	switch {
	case errors.Is(err, vendor.ErrCodeInvalid),
		errors.Is(err, vendor.ErrNameInvalid),
		errors.Is(err, vendor.ErrEmailInvalid),
		errors.Is(err, vendor.ErrIDInvalid),
		errors.Is(err, vendor.ErrStatusInvalid),
		errors.Is(err, vendor.ErrPageInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, vendor.ErrDuplicateCode):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, vendor.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, vendor.ErrDeactivated):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.Internal, "internal error")
	}
}

// mapRequisitionError maps requisition domain and store errors to gRPC codes.
// Validation sentinels → InvalidArgument, the illegal-transition sentinel →
// Aborted (the gateway renders Aborted as HTTP 409), IO sentinels per their
// semantics. Domain and store errors are bare sentinels, so err.Error()
// carries the machine code verbatim (same contract as mapVendorError).
func mapRequisitionError(err error) error {
	switch {
	case errors.Is(err, requisition.ErrCurrencyInvalid),
		errors.Is(err, requisition.ErrDescriptionInvalid),
		errors.Is(err, requisition.ErrQtyInvalid),
		errors.Is(err, requisition.ErrUnitPriceInvalid),
		errors.Is(err, requisition.ErrItemsEmpty),
		errors.Is(err, requisition.ErrIDInvalid),
		errors.Is(err, requisition.ErrIdempotencyKeyInvalid),
		errors.Is(err, requisition.ErrStatusInvalid),
		errors.Is(err, requisition.ErrPageInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, requisition.ErrIllegalTransition):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, requisition.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, requisition.ErrIdempotencyKeyConflict):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, requisition.ErrForbidden):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, approval.ErrRulesMissing):
		// Wrapped by the requisition store's submit context — the machine
		// code is re-created here so it rides through verbatim.
		return status.Error(codes.FailedPrecondition, approval.ErrRulesMissing.Error())
	default:
		return status.Error(codes.Internal, "internal error")
	}
}

// mapApprovalError maps approval domain and store errors to gRPC codes.
// Same contract as the vendor/requisition mappers: sentinels carry their
// machine code verbatim in the gRPC message.
func mapApprovalError(err error) error {
	switch {
	case errors.Is(err, approval.ErrRuleCurrencyInvalid),
		errors.Is(err, approval.ErrRuleRoleInvalid),
		errors.Is(err, approval.ErrRuleThresholdInvalid),
		errors.Is(err, approval.ErrRuleStepNoInvalid),
		errors.Is(err, approval.ErrRuleStepNoDuplicate),
		errors.Is(err, approval.ErrDecisionInvalid),
		errors.Is(err, approval.ErrReasonRequired),
		errors.Is(err, approval.ErrReasonTooLong),
		errors.Is(err, approval.ErrStepIDInvalid),
		errors.Is(err, requisition.ErrIDInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, approval.ErrStepNotCurrent):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, approval.ErrIdempotencyKeyInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, approval.ErrIdempotencyKeyConflict):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, approval.ErrStepNotFound), errors.Is(err, requisition.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, approval.ErrSelfForbidden), errors.Is(err, approval.ErrRoleForbidden):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, approval.ErrRulesMissing):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.Internal, "internal error")
	}
}

// requireOwner enforces the ownership rule promised in docs/rbac.md ("a
// requester edits their own requisition"): the caller must be the
// requisition's requester. Admins may act on any requisition of the tenant
// (maintainer decision, 2026-10-23). requester_id is immutable, so the check
// cannot race the mutation that follows it.
func requireOwner(r requisition.Requisition, auth *rbac.AuthContext) error {
	if slices.Contains(auth.Roles, rbac.RoleAdmin) || r.RequesterID == auth.UserID {
		return nil
	}
	return requisition.ErrForbidden
}

// maxIdempotencyKeyLen bounds the submit and decide Idempotency-Key (the
// gateway re-checks the same limit at the edge; this is the service-layer
// defense in depth).
const maxIdempotencyKeyLen = 64

// normalizeCurrency uppercases and trims the client currency at the API
// boundary — the domain matches {IDR, USD} exactly (decision on #28: case
// normalization is the API layer's job in #29).
func normalizeCurrency(c string) string { return strings.ToUpper(strings.TrimSpace(c)) }

// itemDomain converts wire items to domain items.
func itemDomain(items []*procurementv1.RequisitionItem) []requisition.Item {
	out := make([]requisition.Item, 0, len(items))
	for _, it := range items {
		out = append(out, requisition.Item{
			Description: it.GetDescription(), Qty: it.GetQty(),
			UnitPriceCents: it.GetUnitPriceCents(), Category: it.GetCategory(),
		})
	}
	return out
}

// requisitionPB renders the aggregate for the wire. steps (nil for the
// create/edit responses, which never have materialized steps) rides along
// so submit and decide callers receive the step ids in-band — the decide
// API's input — without a read endpoint (#31/#32-34 own those).
func requisitionPB(r requisition.Requisition, steps []approval.Step) *procurementv1.Requisition {
	items := make([]*procurementv1.RequisitionItem, 0, len(r.Items))
	for _, it := range r.Items {
		items = append(items, &procurementv1.RequisitionItem{
			Description: it.Description, Qty: it.Qty,
			UnitPriceCents: it.UnitPriceCents, Category: it.Category,
		})
	}
	var stepsPB []*procurementv1.ApprovalStep
	for _, st := range steps {
		decidedAt := ""
		if st.DecidedAt != nil {
			decidedAt = requisition.RFC3339(*st.DecidedAt)
		}
		stepsPB = append(stepsPB, &procurementv1.ApprovalStep{
			Id: st.ID, RequisitionId: st.RequisitionID, StepNo: st.StepNo,
			RoleRequired: st.RoleRequired, ThresholdCents: st.ThresholdCents,
			Status: st.Status, DecidedBy: st.DecidedBy, DecidedAt: decidedAt, Reason: st.Reason,
		})
	}
	submittedAt := ""
	if r.SubmittedAt != nil {
		submittedAt = requisition.RFC3339(*r.SubmittedAt)
	}
	return &procurementv1.Requisition{
		Id: r.ID, TenantId: r.TenantID, Number: r.Number, RequesterId: r.RequesterID,
		Status: r.Status, Currency: r.Currency, TotalCents: r.TotalCents, Items: items,
		SubmittedAt: submittedAt, ApprovalSteps: stepsPB,
		CreatedAt: requisition.RFC3339(r.CreatedAt), CreatedBy: r.CreatedBy,
		UpdatedAt: requisition.RFC3339(r.UpdatedAt), UpdatedBy: r.UpdatedBy,
	}
}

// CreateRequisition creates a draft for the caller: the caller is the
// requester, the number is generated per tenant, and the total is computed
// server-side. Items may be empty; currency is required.
func (a *api) CreateRequisition(ctx context.Context, req *procurementv1.CreateRequisitionRequest) (*procurementv1.Requisition, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	r, err := a.requisitions.Create(ctx, auth.TenantID, requisition.CreateInput{
		RequesterID: auth.UserID,
		Currency:    normalizeCurrency(req.GetCurrency()),
		Items:       itemDomain(req.GetItems()),
	})
	if err != nil {
		return nil, mapRequisitionError(err)
	}
	return requisitionPB(r, nil), nil
}

// UpdateRequisition replaces the basket of a draft the caller owns (admins
// excepted) and recomputes the total.
func (a *api) UpdateRequisition(ctx context.Context, req *procurementv1.UpdateRequisitionRequest) (*procurementv1.Requisition, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	current, err := a.requisitions.Get(ctx, auth.TenantID, req.GetId())
	if err != nil {
		return nil, mapRequisitionError(err)
	}
	if err := requireOwner(current, auth); err != nil {
		return nil, mapRequisitionError(err)
	}
	r, err := a.requisitions.UpdateItems(ctx, auth.TenantID, req.GetId(), itemDomain(req.GetItems()), auth.UserID)
	if err != nil {
		return nil, mapRequisitionError(err)
	}
	return requisitionPB(r, nil), nil
}

// SubmitRequisition moves a draft the caller owns (admins excepted) to
// submitted. The approval engine materializes the tenant's matching rules
// into approval steps inside the same transaction — no applicable rule is a
// FailedPrecondition (approval_rules_missing) and the whole submit rolls
// back. A replayed idempotency key returns the requisition's current state
// without a second transition. The response embeds the requisition's
// approval steps (their ids are the decide API's input).
func (a *api) SubmitRequisition(ctx context.Context, req *procurementv1.SubmitRequisitionRequest) (*procurementv1.Requisition, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(req.GetIdempotencyKey())
	if key != "" && len(key) > maxIdempotencyKeyLen {
		return nil, mapRequisitionError(requisition.ErrIdempotencyKeyInvalid)
	}
	current, err := a.requisitions.Get(ctx, auth.TenantID, req.GetId())
	if err != nil {
		return nil, mapRequisitionError(err)
	}
	if err := requireOwner(current, auth); err != nil {
		return nil, mapRequisitionError(err)
	}
	r, err := a.requisitions.Submit(ctx, auth.TenantID, req.GetId(), auth.UserID, key,
		func(tx pgx.Tx, r requisition.Requisition) error {
			_, err := a.approvals.MaterializeTx(ctx, tx, r)
			return err
		})
	if err != nil {
		return nil, mapRequisitionError(err)
	}
	steps, err := a.approvals.ListSteps(ctx, auth.TenantID, r.ID)
	if err != nil {
		return nil, mapApprovalError(err)
	}
	return requisitionPB(r, steps), nil
}

// ListRequisitions serves the requisition list (requisition:read): filters
// validated in the domain, one header-only page (items and approval steps
// are not loaded — the detail reads own them), newest number first.
func (a *api) ListRequisitions(ctx context.Context, req *procurementv1.ListRequisitionsRequest) (*procurementv1.ListRequisitionsResponse, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	filter, err := requisition.ValidateList(requisition.ListFilter{
		Query: req.GetQuery(), Status: req.GetStatus(),
		Page: req.GetPage(), PageSize: req.GetPageSize(),
	})
	if err != nil {
		return nil, mapRequisitionError(err)
	}
	headers, total, err := a.requisitions.List(ctx, auth.TenantID, filter)
	if err != nil {
		return nil, mapRequisitionError(err)
	}
	out := &procurementv1.ListRequisitionsResponse{
		Total: int32(total), Page: filter.Page, PageSize: filter.PageSize,
		Requisitions: make([]*procurementv1.Requisition, 0, len(headers)),
	}
	for _, r := range headers {
		out.Requisitions = append(out.Requisitions, requisitionPB(r, nil))
	}
	return out, nil
}

// GetRequisition serves the detail read (requisition:read, tenant-wide per
// docs/rbac.md — no ownership check, unlike edit/submit): header plus items
// and the materialized approval steps, the bodies the list rows never carry.
// The wizard's draft restore (#33) reads drafts through it; other tenants'
// requisitions are indistinguishable from missing.
func (a *api) GetRequisition(ctx context.Context, req *procurementv1.GetRequisitionRequest) (*procurementv1.Requisition, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	r, err := a.requisitions.Get(ctx, auth.TenantID, req.GetId())
	if err != nil {
		return nil, mapRequisitionError(err)
	}
	steps, err := a.approvals.ListSteps(ctx, auth.TenantID, r.ID)
	if err != nil {
		return nil, mapApprovalError(err)
	}
	return requisitionPB(r, steps), nil
}

// DecideApproval applies one approve/reject decision to the current approval
// step (approval:decide at both layers). Authorization beyond the
// permission: the decider must not be the requester (self-approval is
// forbidden with no admin exception) and must hold the step's role_required
// (admins excepted) — enforced in the approval domain inside the locked
// transaction. The optional idempotency key follows the submit convention:
// 1–64 chars after trimming, replay returns the current state, another
// step's key conflicts. The decision's outbox rows (step + terminal event)
// commit with the decision; the response carries the requisition with its
// latest steps.
func (a *api) DecideApproval(ctx context.Context, req *procurementv1.DecideApprovalRequest) (*procurementv1.DecideApprovalResponse, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(req.GetIdempotencyKey())
	if key != "" && len(key) > maxIdempotencyKeyLen {
		return nil, mapApprovalError(approval.ErrIdempotencyKeyInvalid)
	}
	reason := strings.TrimSpace(req.GetReason())
	if len(reason) > approval.MaxReasonLen {
		return nil, mapApprovalError(approval.ErrReasonTooLong)
	}
	r, steps, err := a.approvals.Decide(ctx, auth.TenantID, approval.DecideInput{
		StepID:         strings.TrimSpace(req.GetStepId()),
		Decision:       normalizeDecision(req.GetDecision()),
		Reason:         reason,
		ActorID:        auth.UserID,
		Roles:          auth.Roles,
		IdempotencyKey: key,
	})
	if err != nil {
		return nil, mapApprovalError(err)
	}
	return &procurementv1.DecideApprovalResponse{Requisition: requisitionPB(r, steps)}, nil
}

// GetApprovalRules returns the caller tenant's whole rule set
// (approval:configure, admin-only by matrix).
func (a *api) GetApprovalRules(ctx context.Context, _ *procurementv1.GetApprovalRulesRequest) (*procurementv1.ApprovalRulesResponse, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	rules, err := a.approvals.ListRules(ctx, auth.TenantID)
	if err != nil {
		return nil, mapApprovalError(err)
	}
	return rulesPB(rules), nil
}

// ReplaceApprovalRules replaces the caller tenant's whole rule set in one
// transaction (approval:configure, admin-only by matrix). The set may be
// empty; steps already materialized on submitted requisitions are snapshots
// and stay untouched.
func (a *api) ReplaceApprovalRules(ctx context.Context, req *procurementv1.ReplaceApprovalRulesRequest) (*procurementv1.ApprovalRulesResponse, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	rules := make([]approval.Rule, 0, len(req.GetRules()))
	for _, r := range req.GetRules() {
		rules = append(rules, approval.Rule{
			Currency:       normalizeCurrency(r.GetCurrency()),
			StepNo:         r.GetStepNo(),
			RoleRequired:   normalizeRole(r.GetRoleRequired()),
			ThresholdCents: r.GetThresholdCents(),
		})
	}
	stored, err := a.approvals.ReplaceRules(ctx, auth.TenantID, rules)
	if err != nil {
		return nil, mapApprovalError(err)
	}
	return rulesPB(stored), nil
}

// normalizeDecision lowercases and trims the wire decision — the domain
// matches {approve, reject} exactly.
func normalizeDecision(d string) string { return strings.ToLower(strings.TrimSpace(d)) }

// normalizeRole lowercases and trims a role code at the boundary — the
// catalog matches the fixed lowercase codes exactly.
func normalizeRole(r string) string { return strings.ToLower(strings.TrimSpace(r)) }

func rulesPB(rules []approval.Rule) *procurementv1.ApprovalRulesResponse {
	out := &procurementv1.ApprovalRulesResponse{Rules: make([]*procurementv1.ApprovalRule, 0, len(rules))}
	for _, r := range rules {
		out.Rules = append(out.Rules, &procurementv1.ApprovalRule{
			Id: r.ID, Currency: r.Currency, StepNo: r.StepNo,
			RoleRequired: r.RoleRequired, ThresholdCents: r.ThresholdCents,
		})
	}
	return out
}

func vendorPB(v vendor.Vendor) *procurementv1.Vendor {
	return &procurementv1.Vendor{
		Id: v.ID, TenantId: v.TenantID, Code: v.Code, Name: v.Name,
		ContactEmail: v.ContactEmail, TaxId: v.TaxID, Status: v.Status,
		CreatedAt: vendor.RFC3339(v.CreatedAt), CreatedBy: v.CreatedBy,
		UpdatedAt: vendor.RFC3339(v.UpdatedAt), UpdatedBy: v.UpdatedBy,
	}
}

func (a *api) ListVendors(ctx context.Context, req *procurementv1.ListVendorsRequest) (*procurementv1.ListVendorsResponse, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	filter, err := vendor.ValidateList(vendor.ListFilter{
		Query: req.GetQuery(), Status: req.GetStatus(),
		Page: req.GetPage(), PageSize: req.GetPageSize(),
	})
	if err != nil {
		return nil, mapVendorError(err)
	}
	vendors, total, err := a.store.List(ctx, auth.TenantID, filter)
	if err != nil {
		return nil, mapVendorError(err)
	}
	out := &procurementv1.ListVendorsResponse{
		Total: int32(total), Page: filter.Page, PageSize: filter.PageSize,
		Vendors: make([]*procurementv1.Vendor, 0, len(vendors)),
	}
	for _, v := range vendors {
		out.Vendors = append(out.Vendors, vendorPB(v))
	}
	return out, nil
}
