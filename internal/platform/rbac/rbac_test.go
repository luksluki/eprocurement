package rbac

import "testing"

// wantPermission is the documented matrix (docs/rbac.md) restated for the
// test: permission -> roles that must pass (admin always passes implicitly).
var wantPermission = map[string][]string{
	RequisitionRead:   {RoleRequester, RoleApprover, RoleBuyer, RoleFinance, RoleViewer},
	RequisitionWrite:  {RoleRequester},
	ApprovalDecide:    {RoleApprover},
	ApprovalConfigure: {},
	VendorRead:        {RoleRequester, RoleApprover, RoleBuyer, RoleFinance, RoleViewer},
	VendorWrite:       {RoleBuyer},
	PORead:            {RoleRequester, RoleApprover, RoleBuyer, RoleFinance, RoleViewer},
	POWrite:           {RoleBuyer},
	GRNPost:           {RoleBuyer},
	InvoiceRead:       {RoleRequester, RoleApprover, RoleBuyer, RoleFinance, RoleViewer},
	InvoiceWrite:      {RoleFinance},
	MatchConfigure:    {},
	ExportRun:         {RoleFinance},
	AuditRead:         {RoleViewer},
	UsersManage:       {},
	DLQManage:         {},
}

// TestMatrixCoversEveryPermission guards against a permission that exists in
// the code but not in the documented expectations (or vice versa).
func TestMatrixCoversEveryPermission(t *testing.T) {
	all := AllPermissions()
	if len(all) != len(wantPermission) {
		t.Fatalf("matrix has %d permissions, expectations cover %d", len(all), len(wantPermission))
	}
	for _, p := range all {
		if _, ok := wantPermission[p]; !ok {
			t.Errorf("permission %q missing from test expectations", p)
		}
	}
}

// AC: each role tested against a route matrix — every role × every permission.
func TestEveryRoleAgainstMatrix(t *testing.T) {
	for _, role := range allRoles {
		for _, p := range AllPermissions() {
			want := role == RoleAdmin || contains(wantPermission[p], role)
			if got := Check([]string{role}, p); got != want {
				t.Errorf("Check(%s, %s) = %v, want %v", role, p, got, want)
			}
		}
	}
}

func TestCheckSemantics(t *testing.T) {
	if Check(nil, VendorRead) {
		t.Fatal("empty roles must never pass")
	}
	if Check([]string{"", "  "}, VendorRead) {
		t.Fatal("blank roles must never pass")
	}
	if Check([]string{"superuser"}, UsersManage) {
		t.Fatal("unknown roles must never pass")
	}
	// Multiple roles: any granting role suffices.
	if !Check([]string{RoleViewer, RoleBuyer}, VendorWrite) {
		t.Fatal("one granting role among several must pass")
	}
	if Check([]string{RoleViewer, RoleApprover}, VendorWrite) {
		t.Fatal("no granting role must fail")
	}
	// Unknown permission passes for nobody, not even admin-listed roles.
	if Check([]string{RoleBuyer}, "no:such:permission") {
		t.Fatal("unknown permission must fail")
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
