package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/luksluki/eprocurement/services/identity/auth"
)

func TestNewUUIDDeterministic(t *testing.T) {
	a := newUUID(rand.New(rand.NewSource(7)))
	b := newUUID(rand.New(rand.NewSource(7)))
	if a != b {
		t.Fatalf("expected deterministic UUID, got %q and %q", a, b)
	}
	if len(a) != 36 {
		t.Fatalf("expected canonical UUID length 36, got %d", len(a))
	}
}

func TestLoadDotEnv(t *testing.T) {
	key := "SEED_TEST_KEY_FOR_TESTS"
	_ = os.Unsetenv(key)

	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("# comment\n"+key+"=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	loadDotEnv(path)
	if got := os.Getenv(key); got != "from-file" {
		t.Fatalf("expected from-file, got %q", got)
	}

	if err := os.Setenv(key, "from-env"); err != nil {
		t.Fatal(err)
	}
	loadDotEnv(path)
	if got := os.Getenv(key); got != "from-env" {
		t.Fatalf("environment must win, got %q", got)
	}
	_ = os.Unsetenv(key)
}

// smallCfg is a compact-but-representative dataset: two tenants, enough
// requisitions that every status and both approval-chain shapes appear.
func smallCfg() config {
	return config{tenants: 2, vendorsPer: 6, requisitions: 120, events: 0, seed: 42}
}

func fixedBase() time.Time {
	return time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
}

// TestBuildDatasetDeterministic pins the issue #110 determinism contract at
// the generator level: same cfg + RNG seed + anchor ⇒ identical datasets
// (every column the writers persist comes from this struct, so identical
// struct ⇒ identical rows; timestamps hang off the shared base, the same
// property seedBase's hour truncation gives two runs inside one hour).
func TestBuildDatasetDeterministic(t *testing.T) {
	cfg := smallCfg()
	a := buildDataset(cfg, rand.New(rand.NewSource(cfg.seed)), fixedBase())
	b := buildDataset(cfg, rand.New(rand.NewSource(cfg.seed)), fixedBase())
	if !reflect.DeepEqual(a, b) {
		t.Fatal("expected identical datasets for the same seed and anchor")
	}

	// Spot-check the AC's sample row: PR-000001 exists once per tenant and
	// is fully identical across builds.
	findPR := func(ds dataset) map[string]requisitionSeed {
		out := map[string]requisitionSeed{}
		for _, r := range ds.requisitions {
			if r.number == "PR-000001" {
				out[r.tenantID] = r
			}
		}
		return out
	}
	pa, pb := findPR(a), findPR(b)
	if len(pa) == 0 {
		t.Fatal("PR-000001 missing from dataset")
	}
	for tenant, ra := range pa {
		rb, ok := pb[tenant]
		if !ok {
			t.Fatalf("PR-000001 of tenant %s missing in second build", tenant)
		}
		if !reflect.DeepEqual(ra, rb) {
			t.Fatalf("PR-000001 of tenant %s differs between builds", tenant)
		}
	}
}

// TestDatasetInvariants checks the schema and domain invariants the issue
// demands of the generated live rows — the same checks the dev note runs
// against the database after `make seed`, asserted here on the pure dataset.
func TestDatasetInvariants(t *testing.T) {
	cfg := smallCfg()
	ds := buildDataset(cfg, rand.New(rand.NewSource(cfg.seed)), fixedBase())

	tenants := map[string]bool{}
	for _, t2 := range ds.tenants {
		tenants[t2.id] = true
	}
	if len(tenants) != cfg.tenants {
		t.Fatalf("expected %d tenants, got %d", cfg.tenants, len(tenants))
	}

	// Users: one per catalog role per tenant, password hash present.
	rolesByTenant := map[string]map[string]string{} // tenant -> role -> userID
	for _, u := range ds.users {
		if !tenants[u.tenantID] {
			t.Fatalf("user %s references unknown tenant %s", u.email, u.tenantID)
		}
		if u.passwordPHC == "" {
			t.Fatalf("user %s has no password hash", u.email)
		}
		set := rolesByTenant[u.tenantID]
		if set == nil {
			set = map[string]string{}
			rolesByTenant[u.tenantID] = set
		}
		if _, dup := set[u.role]; dup {
			t.Fatalf("duplicate role %s in tenant %s", u.role, u.tenantID)
		}
		set[u.role] = u.id
	}
	for tenant, set := range rolesByTenant {
		for _, role := range seedRoles {
			if _, ok := set[role]; !ok {
				t.Fatalf("tenant %s missing a %s user", tenant, role)
			}
		}
	}

	// Approval rules: the seeded two-step IDR chain, valid per the engine's
	// own validator.
	for _, tenant := range ds.tenants {
		rules := ruleDomainSet(ds.rules, tenant.id)
		if len(rules) != 2 {
			t.Fatalf("tenant %s: expected 2 approval rules, got %d", tenant.id, len(rules))
		}
	}

	prByTenant := map[string]map[string]bool{}
	poByTenant := map[string]map[string]bool{}
	grnByTenant := map[string]map[string]bool{}
	invSeen := map[string]bool{} // (tenant, vendor, number) uniqueness
	for i := range ds.requisitions {
		r := &ds.requisitions[i]
		if !tenants[r.tenantID] {
			t.Fatalf("requisition %s references unknown tenant", r.number)
		}
		// (a) total = Σ(qty × unit price) of the lines.
		var sum int64
		for _, it := range r.items {
			if it.requisitionID != r.id {
				t.Fatalf("item of %s references another requisition", r.number)
			}
			sum += int64(it.qty) * it.unitPriceCents
		}
		if sum != r.totalCents {
			t.Fatalf("%s: total %d ≠ Σ lines %d", r.number, r.totalCents, sum)
		}
		// Per-tenant number uniqueness (unique index tenant_id+number).
		set := prByTenant[r.tenantID]
		if set == nil {
			set = map[string]bool{}
			prByTenant[r.tenantID] = set
		}
		if set[r.number] {
			t.Fatalf("duplicate requisition number %s in one tenant", r.number)
		}
		set[r.number] = true

		// (b) approval steps consistent with the state machine, mirroring
		// the tenant rules; (c) actor invariants on decided steps.
		if len(r.steps) == 0 {
			t.Fatalf("%s has no approval steps", r.number)
		}
		sawRejected := false
		for _, st := range r.steps {
			if st.requisitionID != r.id {
				t.Fatalf("step of %s references another requisition", r.number)
			}
			switch r.status {
			case "approved", "closed":
				if st.status != "approved" {
					t.Fatalf("%s (%s): step %d is %s, want approved", r.number, r.status, st.stepNo, st.status)
				}
			case "submitted":
				if st.status != "pending" {
					t.Fatalf("%s (submitted): step %d is %s, want pending", r.number, st.stepNo, st.status)
				}
			case "rejected":
				if st.stepNo == 1 && st.status != "rejected" {
					t.Fatalf("%s (rejected): step 1 is %s, want rejected", r.number, st.status)
				}
				if st.stepNo > 1 && st.status != "skipped" {
					t.Fatalf("%s (rejected): step %d is %s, want skipped", r.number, st.stepNo, st.status)
				}
			}
			if st.status == "approved" || st.status == "rejected" {
				if st.decidedBy == "" || st.decidedAt == nil {
					t.Fatalf("%s: decided step %d missing actor or time", r.number, st.stepNo)
				}
				if st.decidedBy == r.requesterID {
					t.Fatalf("%s: step %d decided by the requester (self-approval)", r.number, st.stepNo)
				}
				holder := rolesByTenant[r.tenantID][st.roleRequired]
				if holder == "" || holder != st.decidedBy {
					t.Fatalf("%s: step %d decider does not hold role %s", r.number, st.stepNo, st.roleRequired)
				}
				if st.status == "rejected" && st.reason == "" {
					t.Fatalf("%s: rejected step carries no reason", r.number)
				}
				sawRejected = sawRejected || st.status == "rejected"
			} else if st.decidedBy != "" || st.decidedAt != nil {
				t.Fatalf("%s: undecided step %d carries decision fields", r.number, st.stepNo)
			}
		}
		if r.status == "rejected" && !sawRejected {
			t.Fatalf("%s (rejected) has no rejected step", r.number)
		}
	}

	grnByPO := map[string][]grnSeed{}
	for i := range ds.grns {
		g := &ds.grns[i]
		grnByPO[g.poID] = append(grnByPO[g.poID], *g)
		if !tenants[g.tenantID] {
			t.Fatalf("GRN %s references unknown tenant", g.number)
		}
		set := grnByTenant[g.tenantID]
		if set == nil {
			set = map[string]bool{}
			grnByTenant[g.tenantID] = set
		}
		if set[g.number] {
			t.Fatalf("duplicate GRN number %s in one tenant", g.number)
		}
		set[g.number] = true
	}

	poByID := map[string]*poSeed{}
	for i := range ds.purchaseOrders {
		p := &ds.purchaseOrders[i]
		poByID[p.id] = p
		if !tenants[p.tenantID] {
			t.Fatalf("PO %s references unknown tenant", p.number)
		}
		set := poByTenant[p.tenantID]
		if set == nil {
			set = map[string]bool{}
			poByTenant[p.tenantID] = set
		}
		if set[p.number] {
			t.Fatalf("duplicate PO number %s in one tenant", p.number)
		}
		set[p.number] = true
		// (a) PO total = Σ lines (copied verbatim from the requisition).
		var sum int64
		for _, it := range p.items {
			sum += int64(it.qtyOrdered) * it.unitPriceCents
		}
		if sum != p.totalCents {
			t.Fatalf("%s: total %d ≠ Σ lines %d", p.number, p.totalCents, sum)
		}
		// (d) qty_received = Σ goods_receipt_items per PO line.
		for _, it := range p.items {
			var received int
			for _, g := range grnByPO[p.id] {
				for _, gi := range g.items {
					if gi.poItemID == it.id {
						received += gi.qtyReceived
					}
				}
			}
			if received != it.qtyReceived {
				t.Fatalf("%s item %s: qty_received %d ≠ Σ GRN items %d", p.number, it.id, it.qtyReceived, received)
			}
		}
	}

	for i := range ds.invoices {
		inv := &ds.invoices[i]
		if !tenants[inv.tenantID] {
			t.Fatalf("invoice %s references unknown tenant", inv.number)
		}
		key := fmt.Sprintf("%s/%s/%s", inv.tenantID, inv.vendorID, inv.number)
		if invSeen[key] {
			t.Fatalf("duplicate invoice number %s for one tenant+vendor", inv.number)
		}
		invSeen[key] = true
		var sum int64
		for _, l := range inv.lines {
			sum += int64(l.qty) * l.unitPriceCents
		}
		if sum != inv.totalCents {
			t.Fatalf("%s: total %d ≠ Σ lines %d", inv.number, inv.totalCents, sum)
		}
		// (e) matched only on POs with a GRN and an invoicable status.
		if inv.status == "matched" {
			po := poByID[inv.poID]
			if po == nil {
				t.Fatalf("matched invoice %s references unknown PO", inv.number)
			}
			if len(grnByPO[po.id]) == 0 {
				t.Fatalf("matched invoice %s on PO %s without a GRN", inv.number, po.number)
			}
			if po.status != "partially_received" && po.status != "received" && po.status != "acknowledged" {
				t.Fatalf("matched invoice %s on non-invoicable PO status %s", inv.number, po.status)
			}
			if inv.matchResult == "" || inv.matchResult[0] != '{' {
				t.Fatalf("matched invoice %s carries no match_result JSON", inv.number)
			}
		}
	}

	// The dataset must exercise every shape the dev note reports.
	var submitted, rejected, closed, matched int
	for _, r := range ds.requisitions {
		switch r.status {
		case "submitted":
			submitted++
		case "rejected":
			rejected++
		case "closed":
			closed++
		}
	}
	for _, inv := range ds.invoices {
		if inv.status == "matched" {
			matched++
		}
	}
	if submitted == 0 || rejected == 0 || closed == 0 || matched == 0 {
		t.Fatalf("dataset misses shapes: submitted=%d rejected=%d closed=%d matched=%d", submitted, rejected, closed, matched)
	}
}

// TestPHCHashVerifies pins the credential contract: the seed-built PHC string
// is deterministic and verifies against the identity service's own
// VerifyPassword (the demo login path).
func TestPHCHashVerifies(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	h1 := phcHash(rng, demoPassword)
	h2 := phcHash(rand.New(rand.NewSource(99)), demoPassword)
	if h1 != h2 {
		t.Fatal("expected deterministic PHC string for the same RNG seed")
	}
	if len(h1) < 10 || h1[:9] != "$argon2id" {
		t.Fatalf("not an argon2id PHC string: %q", h1[:9])
	}

	ok, err := auth.VerifyPassword(demoPassword, h1)
	if err != nil || !ok {
		t.Fatalf("VerifyPassword(demo-password) = %v, %v; want true, nil", ok, err)
	}
	ok, err = auth.VerifyPassword("wrong-password", h1)
	if err != nil || ok {
		t.Fatalf("VerifyPassword(wrong-password) = %v, %v; want false, nil", ok, err)
	}

	// Distinct salts per draw: two hashes from one stream differ.
	h3 := phcHash(rng, demoPassword)
	if h1 == h3 {
		t.Fatal("expected per-user salt variation across the RNG stream")
	}
}

// TestSeedBaseTruncatedToHour guards the anchor rule the determinism
// protocol relies on: two calls inside one hour return the same base.
func TestSeedBaseTruncatedToHour(t *testing.T) {
	a := seedBase()
	b := seedBase()
	if !a.Equal(b) {
		t.Fatalf("seedBase moved inside the same hour: %v vs %v", a, b)
	}
	if a.Location() != time.UTC {
		t.Fatalf("seedBase must be UTC, got %v", a.Location())
	}
}
