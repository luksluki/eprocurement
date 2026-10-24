// Package rbac holds the platform-wide role/permission model: one
// declarative matrix shared by the gateway middleware and the gRPC service
// interceptors, so the edge and the service layer enforce the same rules
// (defense in depth, PRD §7). The matrix is documented for humans in
// docs/rbac.md — keep both in sync.
package rbac

import "strings"

// Role codes match the fixed catalog seeded in identity.roles.
const (
	RoleAdmin     = "admin"
	RoleRequester = "requester"
	RoleApprover  = "approver"
	RoleBuyer     = "buyer"
	RoleFinance   = "finance"
	RoleViewer    = "viewer"
)

var allRoles = []string{RoleAdmin, RoleRequester, RoleApprover, RoleBuyer, RoleFinance, RoleViewer}

// Permission names follow "<area>:<action>".
const (
	RequisitionRead   = "requisition:read"
	RequisitionWrite  = "requisition:write"
	ApprovalDecide    = "approval:decide"
	ApprovalConfigure = "approval:configure"
	VendorRead        = "vendor:read"
	VendorWrite       = "vendor:write"
	PORead            = "po:read"
	POWrite           = "po:write"
	GRNPost           = "grn:post"
	InvoiceRead       = "invoice:read"
	InvoiceWrite      = "invoice:write"
	ExportRun         = "export:run"
	AuditRead         = "audit:read"
	UsersManage       = "users:manage"
)

// AllPermissions returns every permission in the matrix (tests and docs).
func AllPermissions() []string {
	ps := make([]string, 0, len(matrix))
	for _, e := range matrix {
		ps = append(ps, e.permission)
	}
	return ps
}

type entry struct {
	permission string
	roles      []string
}

// matrix is the single source of truth. admin implicitly holds everything
// (see Check), so entries list the non-admin roles that grant the permission.
var matrix = []entry{
	{RequisitionRead, []string{RoleRequester, RoleApprover, RoleBuyer, RoleFinance, RoleViewer}},
	{RequisitionWrite, []string{RoleRequester}},
	{ApprovalDecide, []string{RoleApprover}},
	// Admin-only by design (PRD §5: the tenant admin manages approval
	// rules): no non-admin role grants it.
	{ApprovalConfigure, nil},
	{VendorRead, []string{RoleRequester, RoleApprover, RoleBuyer, RoleFinance, RoleViewer}},
	{VendorWrite, []string{RoleBuyer}},
	{PORead, []string{RoleRequester, RoleApprover, RoleBuyer, RoleFinance, RoleViewer}},
	{POWrite, []string{RoleBuyer}},
	{GRNPost, []string{RoleBuyer}},
	{InvoiceRead, []string{RoleRequester, RoleApprover, RoleBuyer, RoleFinance, RoleViewer}},
	{InvoiceWrite, []string{RoleFinance}},
	{ExportRun, []string{RoleFinance}},
	{AuditRead, []string{RoleViewer}},
	{UsersManage, nil},
}

var granted map[string]map[string]bool

func init() {
	granted = make(map[string]map[string]bool, len(matrix))
	for _, e := range matrix {
		set := map[string]bool{}
		for _, r := range e.roles {
			set[r] = true
		}
		granted[e.permission] = set
	}
}

// Check reports whether any of the caller's roles grants the permission.
// Unknown roles grant nothing; admin grants everything; an empty role list
// never passes.
func Check(roles []string, permission string) bool {
	for _, r := range roles {
		if r == RoleAdmin {
			return true
		}
		if granted[permission][strings.TrimSpace(r)] {
			return true
		}
	}
	return false
}
