// Command seed generates the deterministic demo dataset for the local stack
// and loads it into BOTH the reconciliation export (demo.* schema) and the
// LIVE service schemas (procurement.*, identity.*) — issue #110 — so the
// running gateway, web app, and #52 dashboard read seeded data for every
// demo tenant right after `make seed`.
//
// The dataset is anchored on a trailing window: every timestamp hangs from
// now - 120 days (seedBase) and the requisitions are spread evenly across
// that window, so trailing-window analytics (the #52 dashboard's 30/90-day
// spend) hold data on every reseed. The RNG stream and the document ordering
// are deterministic; only the anchor follows the clock. EVERY column the
// writers touch is pinned by the generator (timestamps included — no
// reliance on table DEFAULT now()), so two runs with the same --seed inside
// the same anchor hour produce identical values in every column.
//
// Domain consistency is built in, not hoped for: requisition/PO/invoice
// totals are Σ(qty × unit price) of their lines, approval steps come from
// the real approval.Materialize over the seeded tenant rules (decided steps
// carry a decided_by user who holds the step's role and is never the
// requester — self-approval is forbidden for everyone, docs/rbac.md), PO
// qty_received is Σ goods_receipt_items, and matched invoices are produced
// by the real match.Run engine on lines identical to the PO. Per-tenant
// number counters are deliberately NOT written: seeded numbers are a random
// per-tenant subset of the global PR/PO/GRN streams, and the stores' lazy
// counter init (max(number)) continues the sequence.
//
// Reset (issue #110) is authoritative, not replayed: the tenant list is read
// from demo.tenants BEFORE the truncate (so a `--reset` with default flags
// still removes tenants of earlier non-default runs), live rows are deleted
// per tenant in FK order (PostgreSQL) and via deleteMany({tenant_id: {$in}})
// on the two history collections (MongoDB) — other tenants' events/audit
// rows survive, unlike the old collection Drop.
//
// Usage:
//
//	go run ./scripts/seed            # wipe demo data, then seed (deterministic)
//	go run ./scripts/seed --reset    # wipe demo data only
package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/crypto/argon2"

	"github.com/luksluki/eprocurement/services/documents/history"
	"github.com/luksluki/eprocurement/services/procurement/approval"
	"github.com/luksluki/eprocurement/services/procurement/match"
)

//go:embed schema.sql
var schemaFS embed.FS

// demoPassword is the documented fixture password of every seeded demo user
// (the onboarding/demo-story convention, scripts/dev/demo-story.sh).
const demoPassword = "demo-password"

// argon2id parameters mirror services/identity/auth (password.go): the seed
// builds the PHC string itself because auth.phcEncode is unexported and
// auth.HashPassword draws its salt from crypto/rand (not reproducible). The
// salt comes from the seed RNG instead, so the hash is deterministic and
// still verifies through auth.VerifyPassword.
const (
	argonTime    uint32 = 2
	argonMemory  uint32 = 64 * 1024 // KiB
	argonThreads uint8  = 4
	argonSaltLen        = 16
	argonKeyLen         = 32
)

// The seeded per-tenant approval chain (IDR): every requisition needs an
// approver (step 1, threshold 0), and requisitions totalling at least
// stepTwoThresholdCents also need finance (step 2) — the matrix semantics of
// approval.Materialize decide which requisitions get the second step.
const (
	ruleStepOneRole             = "approver"
	ruleStepTwoRole             = "finance"
	stepTwoThresholdCents int64 = 10_000_000
)

// seedRoles is the fixed per-tenant user catalog: one user per role, in this
// order. Requesters are drawn from {admin, requester, buyer}; step deciders
// hold the step's own role ({approver, finance}), so decided_by can never be
// the requester and always satisfies role_required (docs/rbac.md:37).
var seedRoles = []string{"admin", "requester", "approver", "buyer", "finance"}

// requesterRoleIdx lists the indices into seedRoles a requester is drawn
// from — disjoint from the decider roles by construction.
var requesterRoleIdx = []int{0, 1, 3}

func main() {
	loadDotEnv(".env")

	cfg := config{
		dsn:      env("DATABASE_URL", "postgres://eprocurement:eprocurement@localhost:5432/eprocurement?sslmode=disable"),
		mongoURI: env("MONGO_URI", "mongodb://eprocurement:eprocurement@localhost:27017/?authSource=admin"),
		mongoDB:  env("MONGO_DB", "eprocurement"),
		seed:     42,
	}

	flag.StringVar(&cfg.dsn, "dsn", cfg.dsn, "PostgreSQL connection string")
	flag.StringVar(&cfg.mongoURI, "mongo-uri", cfg.mongoURI, "MongoDB connection string")
	flag.StringVar(&cfg.mongoDB, "mongo-db", cfg.mongoDB, "MongoDB database name")
	flag.IntVar(&cfg.tenants, "tenants", 3, "number of demo tenants")
	flag.IntVar(&cfg.vendorsPer, "vendors-per-tenant", 100, "vendors per tenant")
	flag.IntVar(&cfg.requisitions, "requisitions", 100000, "total requisitions to generate")
	flag.IntVar(&cfg.events, "events", 1000000, "total domain events to generate")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "deterministic RNG seed")
	flag.BoolVar(&cfg.resetOnly, "reset", false, "wipe demo data and exit")
	flag.Parse()

	if cfg.tenants < 1 || cfg.vendorsPer < 1 || cfg.requisitions < 1 || cfg.events < 1 {
		fatalf("counts must be positive")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	db, err := pgxpool.New(ctx, cfg.dsn)
	if err != nil {
		fatalf("postgres connect: %v", err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		fatalf("postgres ping: %v", err)
	}

	if err := applySchema(ctx, db); err != nil {
		fatalf("apply schema: %v", err)
	}
	if err := resetData(ctx, db, cfg.mongoURI, cfg.mongoDB); err != nil {
		fatalf("reset: %v", err)
	}
	if cfg.resetOnly {
		fmt.Println("demo data reset")
		return
	}

	start := time.Now()
	rng := rand.New(rand.NewSource(cfg.seed))
	base := seedBase()
	ds := buildDataset(cfg, rng, base)

	writeDemoPostgres(ctx, db, ds)
	roleIDs, err := loadRoleIDs(ctx, db)
	if err != nil {
		fatalf("load identity roles (is the compose stack migrated?): %v", err)
	}
	writeLivePostgres(ctx, db, ds, roleIDs)
	seededEvents := seedEvents(ctx, cfg, rng, ds.requisitions, base)

	fmt.Printf("seeded tenants=%d users=%d vendors=%d requisitions=%d purchase_orders=%d goods_receipts=%d invoices=%d events=%d in %s\n",
		len(ds.tenants), len(ds.users), len(ds.vendors), len(ds.requisitions), len(ds.purchaseOrders), len(ds.grns), len(ds.invoices), seededEvents,
		time.Since(start).Round(time.Millisecond))
}

type config struct {
	dsn          string
	mongoURI     string
	mongoDB      string
	tenants      int
	vendorsPer   int
	requisitions int
	events       int
	seed         int64
	resetOnly    bool
}

// --- The generated dataset (pure data; writers adapt it below) ------------

type tenantSeed struct {
	id        string
	name      string
	slug      string
	createdAt time.Time
}

type userSeed struct {
	id          string
	tenantID    string
	email       string
	displayName string
	role        string
	passwordPHC string
	grantedAt   time.Time
	createdAt   time.Time
	updatedAt   time.Time
}

type vendorSeed struct {
	id           string
	tenantID     string
	code         string
	name         string
	contactEmail string
	taxID        string
	status       string
	createdBy    string
	updatedBy    string
	createdAt    time.Time
	updatedAt    time.Time
}

type approvalRuleSeed struct {
	id             string
	tenantID       string
	currency       string
	stepNo         int
	roleRequired   string
	thresholdCents int64
	createdAt      time.Time
	updatedAt      time.Time
}

type requisitionItemSeed struct {
	id             string
	requisitionID  string
	description    string
	qty            int
	unitPriceCents int64
	category       string
	createdAt      time.Time
}

type approvalStepSeed struct {
	id             string
	requisitionID  string
	stepNo         int
	roleRequired   string
	thresholdCents int64
	status         string
	decidedBy      string // "" for pending/skipped steps (no decision recorded)
	decidedAt      *time.Time
	reason         string
	createdAt      time.Time
}

type requisitionSeed struct {
	id          string
	tenantID    string
	number      string
	requesterID string
	status      string
	currency    string
	totalCents  int64
	createdBy   string
	updatedBy   string
	submittedAt time.Time
	createdAt   time.Time
	updatedAt   time.Time
	items       []requisitionItemSeed
	steps       []approvalStepSeed
}

type poItemSeed struct {
	id             string
	poID           string
	description    string
	qtyOrdered     int
	qtyReceived    int
	unitPriceCents int64
	createdAt      time.Time
}

type poSeed struct {
	id            string
	tenantID      string
	number        string
	requisitionID string
	vendorID      string
	status        string
	currency      string
	totalCents    int64
	createdBy     string
	updatedBy     string
	issuedAt      time.Time
	createdAt     time.Time
	updatedAt     time.Time
	items         []poItemSeed
}

type grnItemSeed struct {
	id          string
	grnID       string
	poItemID    string
	qtyReceived int
	createdAt   time.Time
}

type grnSeed struct {
	id         string
	tenantID   string
	poID       string
	number     string
	receivedBy string
	receivedAt time.Time
	createdAt  time.Time
	items      []grnItemSeed
}

type invoiceLineSeed struct {
	id             string
	invoiceID      string
	poItemID       string
	qty            int
	unitPriceCents int64
	createdAt      time.Time
}

type invoiceSeed struct {
	id          string
	tenantID    string
	number      string
	poID        string
	vendorID    string
	currency    string
	status      string
	totalCents  int64
	matchResult string
	createdBy   string
	createdAt   time.Time
	lines       []invoiceLineSeed
}

type dataset struct {
	tenants        []tenantSeed
	users          []userSeed
	vendors        []vendorSeed
	rules          []approvalRuleSeed
	requisitions   []requisitionSeed
	purchaseOrders []poSeed
	grns           []grnSeed
	invoices       []invoiceSeed
}

// seedWindowDays is the trailing window the demo dataset spans: the base is
// now - 120 days and the N requisitions are spread evenly across it (the
// maintainer decision on issue #52, option (a) as amended), so the 30/90-day
// trailing windows of the dashboard's spend widget (~25% / ~75% of the data)
// fill on every reseed. The RNG stream and the document ordering stay
// deterministic; only the anchor follows the clock.
const seedWindowDays = 120

// seedBase is the anchor every generated timestamp hangs from: now minus the
// 120-day window, truncated to the hour so consecutive runs inside one hour
// produce identical timestamps (the determinism spot-check protocol runs
// both passes inside one anchor hour).
func seedBase() time.Time {
	return time.Now().UTC().Add(-seedWindowDays * 24 * time.Hour).Truncate(time.Hour)
}

// buildDataset is the pure, deterministic generator: same cfg + rng seed +
// base ⇒ identical dataset, every column pinned. No IO happens here.
func buildDataset(cfg config, rng *rand.Rand, base time.Time) dataset {
	tenantNames := []string{"Nusantara Retail", "Kirana Logistics", "Sagara Manufacturing", "Merapi Foods", "Bumi Health"}
	slugs := []string{"nusantara-retail", "kirana-logistics", "sagara-manufacturing", "merapi-foods", "bumi-health"}
	vendorPrefixes := []string{"PT", "CV", "UD"}
	vendorWords := []string{"Sinar", "Maju", "Sentosa", "Abadi", "Prima", "Karya", "Global", "Cahaya", "Mitra", "Jaya"}
	vendorGoods := []string{"Supply", "Teknik", "Logistik", "Pangan", "Medika", "Energi"}

	ds := dataset{
		tenants: make([]tenantSeed, 0, cfg.tenants),
		users:   make([]userSeed, 0, cfg.tenants*len(seedRoles)),
		vendors: make([]vendorSeed, 0, cfg.tenants*cfg.vendorsPer),
		rules:   make([]approvalRuleSeed, 0, cfg.tenants*2),
	}

	tenantStart := base.Add(-24 * time.Hour) // tenants predate their documents
	for i := 0; i < cfg.tenants; i++ {
		slug := slugs[i%len(slugs)]
		t := tenantSeed{id: newUUID(rng), name: tenantNames[i%len(tenantNames)], slug: slug, createdAt: tenantStart}
		ds.tenants = append(ds.tenants, t)

		for ri, role := range seedRoles {
			created := tenantStart.Add(time.Duration(ri) * time.Minute)
			ds.users = append(ds.users, userSeed{
				id:          newUUID(rng),
				tenantID:    t.id,
				email:       fmt.Sprintf("%s@%s.example", role, slug),
				displayName: strings.ToUpper(role[:1]) + role[1:] + " " + strings.ToUpper(slug[:1]) + slug[1:],
				role:        role,
				passwordPHC: phcHash(rng, demoPassword),
				grantedAt:   created,
				createdAt:   created,
				updatedAt:   created,
			})
		}

		for j := 0; j < cfg.vendorsPer; j++ {
			code := fmt.Sprintf("V-%d-%04d", i+1, j+1)
			ds.vendors = append(ds.vendors, vendorSeed{
				id:           newUUID(rng),
				tenantID:     t.id,
				code:         code,
				name:         fmt.Sprintf("%s %s %s %s", vendorPrefixes[j%len(vendorPrefixes)], vendorWords[rng.Intn(len(vendorWords))], vendorGoods[rng.Intn(len(vendorGoods))], fmt.Sprintf("%d", j+1)),
				contactEmail: fmt.Sprintf("sales@%s.example", strings.ToLower(code)),
				taxID:        fmt.Sprintf("10.%03d.%03d.%d-%05d", (j+1)%1000, (i*37+j)%1000, 4, (j+1)%100000),
				status:       "active",
				createdBy:    ds.users[i*len(seedRoles)].id, // the tenant's admin
				updatedBy:    ds.users[i*len(seedRoles)].id,
				createdAt:    tenantStart.Add(time.Duration(j) * time.Minute),
				updatedAt:    tenantStart.Add(time.Duration(j) * time.Minute),
			})
		}

		// The two-step IDR approval chain every tenant's submits mirror.
		for _, r := range []approvalRuleSeed{
			{currency: "IDR", stepNo: 1, roleRequired: ruleStepOneRole, thresholdCents: 0},
			{currency: "IDR", stepNo: 2, roleRequired: ruleStepTwoRole, thresholdCents: stepTwoThresholdCents},
		} {
			r.id = newUUID(rng)
			r.tenantID = t.id
			r.createdAt = tenantStart
			r.updatedAt = tenantStart
			ds.rules = append(ds.rules, r)
		}
	}

	ds.requisitions = make([]requisitionSeed, 0, cfg.requisitions)
	ds.purchaseOrders = make([]poSeed, 0, cfg.requisitions)
	ds.grns = make([]grnSeed, 0, cfg.requisitions)
	ds.invoices = make([]invoiceSeed, 0, cfg.requisitions)

	statuses := []string{"approved", "approved", "approved", "approved", "approved", "submitted", "submitted", "rejected", "closed"}
	poStatuses := []string{"issued", "acknowledged", "partially_received", "received", "closed"}
	// Even spread over the window: each requisition sits at base + i*spacing
	// (at the 100k default ≈ 1.73 minutes apart), so the last lands at the
	// window's end and the trailing 30/90-day windows both hold data (the
	// amended option-(a) arithmetic: a literal 1-minute spacing would end the
	// data ~50 days back and leave the 30-day window empty).
	spacing := (seedWindowDays * 24 * time.Hour) / time.Duration(cfg.requisitions)

	for i := 0; i < cfg.requisitions; i++ {
		tenantIdx := rng.Intn(cfg.tenants)
		tenant := ds.tenants[tenantIdx]
		userBase := tenantIdx * len(seedRoles)
		requester := ds.users[userBase+requesterRoleIdx[rng.Intn(len(requesterRoleIdx))]]
		approverUser := ds.users[userBase+2] // seedRoles[2] = approver
		financeUser := ds.users[userBase+4]  // seedRoles[4] = finance
		buyerUser := ds.users[userBase+3]    // seedRoles[3] = buyer (PO/GRN actor)
		status := statuses[rng.Intn(len(statuses))]

		// Lines first, total = Σ(qty × unit price) — the AC's invariant (a).
		items := make([]requisitionItemSeed, 0, 2)
		var linesTotal int64
		for k := 0; k < 2; k++ {
			qty := 2 + rng.Intn(9) // 2..10: partial receipts stay ≥ 1 per line
			unit := int64(125000 + rng.Intn(25000000))
			linesTotal += int64(qty) * unit
			items = append(items, requisitionItemSeed{
				description:    fmt.Sprintf("Item %d for PR-%06d", k+1, i+1),
				qty:            qty,
				unitPriceCents: unit,
				category:       "general",
			})
		}

		submitted := base.Add(time.Duration(i) * spacing)
		created := submitted.Add(-24 * time.Hour)

		// Steps mirror the tenant's rules via the real materializer
		// (approval.Materialize — same semantics as a live submit).
		materialized, err := approval.Materialize(ruleDomainSet(ds.rules, tenant.id), "", "IDR", linesTotal)
		if err != nil {
			panic(fmt.Sprintf("seed: materialize rules for %s: %v", tenant.id, err))
		}
		steps := make([]approvalStepSeed, 0, len(materialized))
		lastDecidedAt := submitted
		lastActor := requester.id
		for si, st := range materialized {
			step := approvalStepSeed{
				stepNo:         int(st.StepNo),
				roleRequired:   st.RoleRequired,
				thresholdCents: st.ThresholdCents,
				status:         approval.StepPending,
				createdAt:      submitted,
			}
			decidedAt := submitted.Add(time.Duration(si+1) * time.Hour)
			switch {
			case status == "approved" || status == "closed":
				// Every step approved by a holder of its role_required
				// (approver for step 1, finance for step 2) — never the
				// requester (self-approval ban, docs/rbac.md:37).
				step.status = approval.StepApproved
				step.decidedBy = roleHolder(st.RoleRequired, approverUser, financeUser)
				step.decidedAt = &decidedAt
				lastDecidedAt, lastActor = decidedAt, step.decidedBy
			case status == "rejected" && si == 0:
				step.status = approval.StepRejected
				step.decidedBy = roleHolder(st.RoleRequired, approverUser, financeUser)
				step.decidedAt = &decidedAt
				step.reason = "Rejected: seeded demo rejection"
				lastDecidedAt, lastActor = decidedAt, step.decidedBy
			case status == "rejected":
				// A reject short-circuits the flow: later steps are skipped
				// with no decision recorded (approval.go Decide semantics).
				step.status = approval.StepSkipped
			}
			steps = append(steps, step)
		}

		r := requisitionSeed{
			tenantID:    tenant.id,
			number:      fmt.Sprintf("PR-%06d", i+1),
			requesterID: requester.id,
			status:      status,
			currency:    "IDR",
			totalCents:  linesTotal,
			createdBy:   requester.id,
			updatedBy:   lastActor,
			submittedAt: submitted,
			createdAt:   created,
			updatedAt:   lastDecidedAt,
		}
		r.id = newUUID(rng)
		for k := range items {
			items[k].id = newUUID(rng)
			items[k].requisitionID = r.id
			items[k].createdAt = created
		}
		for k := range steps {
			steps[k].id = newUUID(rng)
			steps[k].requisitionID = r.id
		}
		r.items = items
		r.steps = steps
		ds.requisitions = append(ds.requisitions, r)

		if status != "approved" && status != "closed" {
			continue
		}

		// PO for converted requisitions; vendor from the SAME tenant (the
		// dashboard's spend widget joins po → vendors under tenant RLS, so a
		// cross-tenant vendor would silently drop the PO from spend).
		poStatus := poStatuses[rng.Intn(len(poStatuses))]
		vendor := ds.vendors[tenantIdx*cfg.vendorsPer+rng.Intn(cfg.vendorsPer)]
		poCreated := lastDecidedAt.Add(1 * time.Hour)
		issued := poCreated.Add(1 * time.Hour)

		po := poSeed{
			tenantID:      tenant.id,
			number:        fmt.Sprintf("PO-%06d", i+1),
			requisitionID: r.id,
			vendorID:      vendor.id,
			status:        poStatus,
			currency:      "IDR",
			totalCents:    linesTotal, // lines are copied verbatim on conversion
			createdBy:     buyerUser.id,
			updatedBy:     buyerUser.id,
			issuedAt:      issued,
			createdAt:     poCreated,
			updatedAt:     issued,
		}
		po.id = newUUID(rng)
		po.items = make([]poItemSeed, 0, len(r.items))
		for k, it := range r.items {
			pi := poItemSeed{
				description:    it.description,
				qtyOrdered:     it.qty,
				unitPriceCents: it.unitPriceCents,
				createdAt:      poCreated.Add(time.Duration(k) * time.Second),
			}
			pi.id = newUUID(rng)
			pi.poID = po.id
			po.items = append(po.items, pi)
		}

		var lastReceipt time.Time
		if poStatus == "partially_received" || poStatus == "received" || poStatus == "closed" {
			// One GRN carrying every PO line: 2/3 of the ordered qty for a
			// partial receipt (≥ 1 because qty ≥ 2), the full qty otherwise.
			partial := poStatus == "partially_received"
			grn := grnSeed{
				tenantID:   tenant.id,
				poID:       po.id,
				number:     fmt.Sprintf("GRN-%06d", len(ds.grns)+1),
				receivedBy: buyerUser.id,
				receivedAt: issued.Add(24 * time.Hour),
			}
			grn.id = newUUID(rng)
			grn.createdAt = grn.receivedAt
			grn.items = make([]grnItemSeed, 0, len(po.items))
			for k := range po.items {
				qty := po.items[k].qtyOrdered
				if partial {
					qty = qty * 2 / 3
				}
				po.items[k].qtyReceived = qty // = Σ goods_receipt_items (one GRN)
				gi := grnItemSeed{poItemID: po.items[k].id, qtyReceived: qty, createdAt: grn.createdAt}
				gi.id = newUUID(rng)
				gi.grnID = grn.id
				grn.items = append(grn.items, gi)
			}
			ds.grns = append(ds.grns, grn)
			lastReceipt = grn.receivedAt
			po.updatedAt = lastReceipt
		}

		if poStatus == "closed" {
			po.updatedAt = lastReceipt.Add(24 * time.Hour)
		}
		ds.purchaseOrders = append(ds.purchaseOrders, po)

		// Matched invoices only on POs that have a GRN and are invoicable
		// (partially_received / received; acknowledged is invoicable too but
		// carries no receipt, so a strict 0%/0% match is impossible there).
		if (poStatus == "partially_received" || poStatus == "received") && rng.Intn(100) < 50 {
			inv := invoiceSeed{
				tenantID:  tenant.id,
				number:    fmt.Sprintf("INV-%06d", len(ds.invoices)+1),
				poID:      po.id,
				vendorID:  vendor.id,
				currency:  "IDR",
				status:    "matched",
				createdBy: financeUser.id,
				createdAt: lastReceipt.Add(24 * time.Hour),
			}
			inv.id = newUUID(rng)
			inv.lines = make([]invoiceLineSeed, 0, len(po.items))
			engineItems := make([]match.POItem, 0, len(po.items))
			engineLines := make([]match.Line, 0, len(po.items))
			var invTotal int64
			for k := range po.items {
				it := &po.items[k]
				li := invoiceLineSeed{poItemID: it.id, qty: it.qtyReceived, unitPriceCents: it.unitPriceCents, createdAt: inv.createdAt}
				li.id = newUUID(rng)
				li.invoiceID = inv.id
				inv.lines = append(inv.lines, li)
				invTotal += int64(li.qty) * li.unitPriceCents
				engineItems = append(engineItems, match.POItem{
					ID: it.id, QtyOrdered: int32(it.qtyOrdered),
					QtyReceived: int32(it.qtyReceived), UnitPriceCents: it.unitPriceCents,
				})
				engineLines = append(engineLines, match.Line{
					POItemID: it.id, Qty: int32(li.qty),
					UnitPriceCents: li.unitPriceCents, QtyInvoicedCum: int32(li.qty),
				})
			}
			inv.totalCents = invTotal
			// The verdict comes from the real engine (strict 0%/0% — no
			// match_settings row is seeded), so the stored match_result JSON
			// is byte-identical to what a live match run would persist.
			result, err := match.Run(match.Input{POItems: engineItems, Lines: engineLines, Settings: match.DefaultSettings()})
			if err != nil || !result.Matched {
				panic(fmt.Sprintf("seed: seeded invoice %s did not match (err=%v matched=%v)", inv.number, err, result.Matched))
			}
			inv.matchResult = result.ResultJSON()
			ds.invoices = append(ds.invoices, inv)
		}
	}

	return ds
}

// ruleDomainSet adapts the tenant's seeded rules to the approval engine's
// Rule shape (same order as stored).
func ruleDomainSet(rules []approvalRuleSeed, tenantID string) []approval.Rule {
	out := make([]approval.Rule, 0, 2)
	for _, r := range rules {
		if r.tenantID != tenantID {
			continue
		}
		out = append(out, approval.Rule{
			ID: r.id, Currency: r.currency, StepNo: int32(r.stepNo),
			RoleRequired: r.roleRequired, ThresholdCents: r.thresholdCents,
		})
	}
	return out
}

// roleHolder returns the seeded user holding roleRequired — the approver for
// step 1, finance for step 2 of the seeded chain (disjoint from the
// requester candidates, so decided_by ≠ requester_id holds by construction).
func roleHolder(roleRequired string, approverUser, financeUser userSeed) string {
	switch roleRequired {
	case ruleStepTwoRole:
		return financeUser.id
	default:
		return approverUser.id
	}
}

// phcHash builds the deterministic argon2id PHC string for password with a
// salt drawn from the seed RNG — the same shape and parameters
// auth.HashPassword produces, so auth.VerifyPassword accepts it.
func phcHash(rng *rand.Rand, password string) string {
	salt := make([]byte, argonSaltLen)
	for i := range salt {
		salt[i] = byte(rng.Intn(256))
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

// --- Writers ---------------------------------------------------------------

// writeDemoPostgres loads the reconciliation export (the demo.* schema the
// earlier issues seeded; kept so demo.* and the live schema stay comparable
// table by table).
func writeDemoPostgres(ctx context.Context, db *pgxpool.Pool, ds dataset) {
	copyOrFatal(ctx, db, pgx.Identifier{"demo", "tenants"}, []string{"id", "name", "slug", "created_at"}, func(yield func([]any) error) error {
		for _, t := range ds.tenants {
			if err := yield([]any{t.id, t.name, t.slug, t.createdAt}); err != nil {
				return err
			}
		}
		return nil
	}, "tenants")

	copyOrFatal(ctx, db, pgx.Identifier{"demo", "users"}, []string{"id", "tenant_id", "email", "role", "created_at"}, func(yield func([]any) error) error {
		for _, u := range ds.users {
			if err := yield([]any{u.id, u.tenantID, u.email, u.role, u.createdAt}); err != nil {
				return err
			}
		}
		return nil
	}, "users")

	copyOrFatal(ctx, db, pgx.Identifier{"demo", "vendors"}, []string{"id", "tenant_id", "code", "name", "status", "created_at"}, func(yield func([]any) error) error {
		for _, v := range ds.vendors {
			if err := yield([]any{v.id, v.tenantID, v.code, v.name, v.status, v.createdAt}); err != nil {
				return err
			}
		}
		return nil
	}, "vendors")

	copyOrFatal(ctx, db, pgx.Identifier{"demo", "requisitions"}, []string{"id", "tenant_id", "number", "requester_id", "status", "currency", "total_cents", "submitted_at", "created_at"}, func(yield func([]any) error) error {
		for _, r := range ds.requisitions {
			if err := yield([]any{r.id, r.tenantID, r.number, r.requesterID, r.status, r.currency, r.totalCents, r.submittedAt, r.createdAt}); err != nil {
				return err
			}
		}
		return nil
	}, "requisitions")

	copyOrFatal(ctx, db, pgx.Identifier{"demo", "requisition_items"}, []string{"id", "requisition_id", "description", "qty", "unit_price_cents"}, func(yield func([]any) error) error {
		for _, r := range ds.requisitions {
			for _, it := range r.items {
				if err := yield([]any{it.id, it.requisitionID, it.description, it.qty, it.unitPriceCents}); err != nil {
					return err
				}
			}
		}
		return nil
	}, "requisition_items")

	copyOrFatal(ctx, db, pgx.Identifier{"demo", "approval_steps"}, []string{"id", "requisition_id", "step_no", "role_required", "status", "decided_at"}, func(yield func([]any) error) error {
		for _, r := range ds.requisitions {
			for _, st := range r.steps {
				var decidedAt any
				if st.decidedAt != nil {
					decidedAt = *st.decidedAt
				}
				if err := yield([]any{st.id, st.requisitionID, st.stepNo, st.roleRequired, st.status, decidedAt}); err != nil {
					return err
				}
			}
		}
		return nil
	}, "approval_steps")

	copyOrFatal(ctx, db, pgx.Identifier{"demo", "purchase_orders"}, []string{"id", "tenant_id", "number", "requisition_id", "vendor_id", "status", "total_cents", "issued_at"}, func(yield func([]any) error) error {
		for _, p := range ds.purchaseOrders {
			if err := yield([]any{p.id, p.tenantID, p.number, p.requisitionID, p.vendorID, p.status, p.totalCents, p.issuedAt}); err != nil {
				return err
			}
		}
		return nil
	}, "purchase_orders")

	copyOrFatal(ctx, db, pgx.Identifier{"demo", "po_items"}, []string{"id", "po_id", "description", "qty_ordered", "qty_received", "unit_price_cents"}, func(yield func([]any) error) error {
		for _, p := range ds.purchaseOrders {
			for _, it := range p.items {
				if err := yield([]any{it.id, it.poID, it.description, it.qtyOrdered, it.qtyReceived, it.unitPriceCents}); err != nil {
					return err
				}
			}
		}
		return nil
	}, "po_items")

	copyOrFatal(ctx, db, pgx.Identifier{"demo", "goods_receipts"}, []string{"id", "tenant_id", "po_id", "number", "received_at"}, func(yield func([]any) error) error {
		for _, g := range ds.grns {
			if err := yield([]any{g.id, g.tenantID, g.poID, g.number, g.receivedAt}); err != nil {
				return err
			}
		}
		return nil
	}, "goods_receipts")

	copyOrFatal(ctx, db, pgx.Identifier{"demo", "invoices"}, []string{"id", "tenant_id", "number", "po_id", "amount_cents", "status", "created_at"}, func(yield func([]any) error) error {
		for _, inv := range ds.invoices {
			if err := yield([]any{inv.id, inv.tenantID, inv.number, inv.poID, inv.totalCents, inv.status, inv.createdAt}); err != nil {
				return err
			}
		}
		return nil
	}, "invoices")
}

// loadRoleIDs reads the fixed RBAC catalog (identity.roles, seeded by the
// identity migrations) so user_roles rows can reference it.
func loadRoleIDs(ctx context.Context, db *pgxpool.Pool) (map[string]string, error) {
	rows, err := db.Query(ctx, `SELECT code, id FROM identity.roles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var code, id string
		if err := rows.Scan(&code, &id); err != nil {
			return nil, err
		}
		out[code] = id
	}
	return out, rows.Err()
}

// writeLivePostgres loads the dataset into the live service schemas:
// identity.users + identity.user_roles (a user with no role row gets an
// empty role list in the JWT and every permission check fails, so the roles
// are load-bearing) and procurement.* (vendors, approval_rules,
// requisitions + items + steps, purchase_orders + items, goods_receipts +
// items, invoices + lines). Every column the writers own is pinned by the
// generator; the per-tenant number counters are deliberately NOT written
// (the stores lazily initialize them from max(number)).
func writeLivePostgres(ctx context.Context, db *pgxpool.Pool, ds dataset, roleIDs map[string]string) {
	copyOrFatal(ctx, db, pgx.Identifier{"identity", "users"}, []string{"id", "tenant_id", "email", "display_name", "password_hash", "status", "created_at", "updated_at"}, func(yield func([]any) error) error {
		for _, u := range ds.users {
			if err := yield([]any{u.id, u.tenantID, u.email, u.displayName, u.passwordPHC, "active", u.createdAt, u.updatedAt}); err != nil {
				return err
			}
		}
		return nil
	}, "identity.users")

	copyOrFatal(ctx, db, pgx.Identifier{"identity", "user_roles"}, []string{"user_id", "role_id", "granted_at"}, func(yield func([]any) error) error {
		for _, u := range ds.users {
			roleID, ok := roleIDs[u.role]
			if !ok {
				return fmt.Errorf("identity.roles has no code %q (run the identity migrations)", u.role)
			}
			if err := yield([]any{u.id, roleID, u.grantedAt}); err != nil {
				return err
			}
		}
		return nil
	}, "identity.user_roles")

	copyOrFatal(ctx, db, pgx.Identifier{"procurement", "vendors"}, []string{"id", "tenant_id", "code", "name", "contact_email", "tax_id", "status", "created_at", "created_by", "updated_at", "updated_by"}, func(yield func([]any) error) error {
		for _, v := range ds.vendors {
			if err := yield([]any{v.id, v.tenantID, v.code, v.name, v.contactEmail, v.taxID, v.status, v.createdAt, v.createdBy, v.updatedAt, v.updatedBy}); err != nil {
				return err
			}
		}
		return nil
	}, "procurement.vendors")

	copyOrFatal(ctx, db, pgx.Identifier{"procurement", "approval_rules"}, []string{"id", "tenant_id", "currency", "step_no", "role_required", "threshold_cents", "created_at", "updated_at"}, func(yield func([]any) error) error {
		for _, r := range ds.rules {
			if err := yield([]any{r.id, r.tenantID, r.currency, r.stepNo, r.roleRequired, r.thresholdCents, r.createdAt, r.updatedAt}); err != nil {
				return err
			}
		}
		return nil
	}, "procurement.approval_rules")

	copyOrFatal(ctx, db, pgx.Identifier{"procurement", "requisitions"}, []string{"id", "tenant_id", "number", "requester_id", "status", "currency", "total_cents", "submitted_at", "created_at", "created_by", "updated_at", "updated_by"}, func(yield func([]any) error) error {
		for _, r := range ds.requisitions {
			if err := yield([]any{r.id, r.tenantID, r.number, r.requesterID, r.status, r.currency, r.totalCents, r.submittedAt, r.createdAt, r.createdBy, r.updatedAt, r.updatedBy}); err != nil {
				return err
			}
		}
		return nil
	}, "procurement.requisitions")

	copyOrFatal(ctx, db, pgx.Identifier{"procurement", "requisition_items"}, []string{"id", "requisition_id", "description", "qty", "unit_price_cents", "category", "created_at"}, func(yield func([]any) error) error {
		for _, r := range ds.requisitions {
			for _, it := range r.items {
				if err := yield([]any{it.id, it.requisitionID, it.description, it.qty, it.unitPriceCents, it.category, it.createdAt}); err != nil {
					return err
				}
			}
		}
		return nil
	}, "procurement.requisition_items")

	copyOrFatal(ctx, db, pgx.Identifier{"procurement", "approval_steps"}, []string{"id", "requisition_id", "step_no", "role_required", "threshold_cents", "status", "decided_by", "decided_at", "reason", "created_at"}, func(yield func([]any) error) error {
		for _, r := range ds.requisitions {
			for _, st := range r.steps {
				var decidedBy, decidedAt any
				if st.decidedBy != "" {
					decidedBy = st.decidedBy
				}
				if st.decidedAt != nil {
					decidedAt = *st.decidedAt
				}
				if err := yield([]any{st.id, st.requisitionID, st.stepNo, st.roleRequired, st.thresholdCents, st.status, decidedBy, decidedAt, st.reason, st.createdAt}); err != nil {
					return err
				}
			}
		}
		return nil
	}, "procurement.approval_steps")

	copyOrFatal(ctx, db, pgx.Identifier{"procurement", "purchase_orders"}, []string{"id", "tenant_id", "number", "requisition_id", "vendor_id", "status", "currency", "total_cents", "issued_at", "created_at", "created_by", "updated_at", "updated_by"}, func(yield func([]any) error) error {
		for _, p := range ds.purchaseOrders {
			if err := yield([]any{p.id, p.tenantID, p.number, p.requisitionID, p.vendorID, p.status, p.currency, p.totalCents, p.issuedAt, p.createdAt, p.createdBy, p.updatedAt, p.updatedBy}); err != nil {
				return err
			}
		}
		return nil
	}, "procurement.purchase_orders")

	copyOrFatal(ctx, db, pgx.Identifier{"procurement", "po_items"}, []string{"id", "po_id", "description", "qty_ordered", "qty_received", "unit_price_cents", "created_at"}, func(yield func([]any) error) error {
		for _, p := range ds.purchaseOrders {
			for _, it := range p.items {
				if err := yield([]any{it.id, it.poID, it.description, it.qtyOrdered, it.qtyReceived, it.unitPriceCents, it.createdAt}); err != nil {
					return err
				}
			}
		}
		return nil
	}, "procurement.po_items")

	copyOrFatal(ctx, db, pgx.Identifier{"procurement", "goods_receipts"}, []string{"id", "tenant_id", "po_id", "number", "received_by", "received_at", "notes", "created_at"}, func(yield func([]any) error) error {
		for _, g := range ds.grns {
			if err := yield([]any{g.id, g.tenantID, g.poID, g.number, g.receivedBy, g.receivedAt, "", g.createdAt}); err != nil {
				return err
			}
		}
		return nil
	}, "procurement.goods_receipts")

	copyOrFatal(ctx, db, pgx.Identifier{"procurement", "goods_receipt_items"}, []string{"id", "grn_id", "po_item_id", "qty_received", "created_at"}, func(yield func([]any) error) error {
		for _, g := range ds.grns {
			for _, it := range g.items {
				if err := yield([]any{it.id, it.grnID, it.poItemID, it.qtyReceived, it.createdAt}); err != nil {
					return err
				}
			}
		}
		return nil
	}, "procurement.goods_receipt_items")

	copyOrFatal(ctx, db, pgx.Identifier{"procurement", "invoices"}, []string{"id", "tenant_id", "po_id", "vendor_id", "number", "currency", "total_cents", "status", "match_result", "created_at", "created_by"}, func(yield func([]any) error) error {
		for _, inv := range ds.invoices {
			if err := yield([]any{inv.id, inv.tenantID, inv.poID, inv.vendorID, inv.number, inv.currency, inv.totalCents, inv.status, inv.matchResult, inv.createdAt, inv.createdBy}); err != nil {
				return err
			}
		}
		return nil
	}, "procurement.invoices")

	copyOrFatal(ctx, db, pgx.Identifier{"procurement", "invoice_lines"}, []string{"id", "invoice_id", "po_item_id", "qty", "unit_price_cents", "created_at"}, func(yield func([]any) error) error {
		for _, inv := range ds.invoices {
			for _, l := range inv.lines {
				if err := yield([]any{l.id, l.invoiceID, l.poItemID, l.qty, l.unitPriceCents, l.createdAt}); err != nil {
					return err
				}
			}
		}
		return nil
	}, "procurement.invoice_lines")
}

func seedEvents(ctx context.Context, cfg config, rng *rand.Rand, reqs []requisitionSeed, base time.Time) int {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(cfg.mongoURI))
	if err != nil {
		fatalf("mongo connect: %v", err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	db := client.Database(cfg.mongoDB)
	eventsColl := db.Collection(history.EventsCollectionName)
	auditColl := db.Collection(history.AuditCollectionName)

	// The named index sets of the service — one definition, no drift
	// (history.EventIndexes / AuditIndexes are what EnsureIndexes creates).
	// Indexes are created BEFORE the bulk insert. The reset no longer drops
	// the collections (issue #110: deleteMany per tenant keeps other
	// tenants' rows and their indexes), so on every rerun after the first
	// this is a no-op against the existing named indexes; on a fresh
	// collection the build scans nothing. Either way the indexed inserts
	// stay well inside the 2-minute seed budget (dev-note 042).
	for _, spec := range []struct {
		coll   *mongo.Collection
		models []mongo.IndexModel
	}{
		{eventsColl, history.EventIndexes()},
		{auditColl, history.AuditIndexes()},
	} {
		for _, model := range spec.models {
			if _, err := spec.coll.Indexes().CreateOne(ctx, model); err != nil {
				fatalf("create index %s: %v", model.Options.Name, err)
			}
		}
	}

	types := []string{"pr.created", "pr.submitted", "pr.approved", "po.issued", "po.acknowledged", "grn.posted", "invoice.matched", "pr.rejected", "po.closed", "notification.sent"}

	const batchSize = 5000
	seeded := 0
	batch := make([]any, 0, batchSize)
	auditBatch := make([]any, 0, batchSize)
	flush := func() {
		if len(batch) > 0 {
			if _, err := eventsColl.InsertMany(ctx, batch); err != nil {
				fatalf("insert events: %v", err)
			}
			batch = batch[:0]
		}
		if len(auditBatch) > 0 {
			if _, err := auditColl.InsertMany(ctx, auditBatch); err != nil {
				fatalf("insert audit_log: %v", err)
			}
			auditBatch = auditBatch[:0]
		}
	}
	for i := 0; i < cfg.events; i++ {
		r := reqs[rng.Intn(len(reqs))]
		eventType := types[rng.Intn(len(types))]
		occurredAt := base.Add(time.Duration(i) * time.Second)
		payload := map[string]any{"number": r.number}
		// Decision events carry the decision so their audit rows read well.
		if action, ok := history.AuditAction(eventType); ok {
			payload["decision"] = action
		}
		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			fatalf("marshal payload: %v", err)
		}
		// The constructors of the history package pin the document shapes —
		// the seed writes exactly what the service's Apply writes, and the
		// deterministic RNG keeps event_id stable across reruns. The actor
		// IP draws from the same RNG (deterministic private-address ranges),
		// so the audit view shows populated actor_ip columns on demo data.
		ev, err := history.NewEvent(
			newUUID(rng), r.tenantID, eventType, occurredAt,
			r.requesterID, seedActorIP(rng), strings.ReplaceAll(newUUID(rng), "-", ""),
			"requisition", r.id, string(payloadJSON))
		if err != nil {
			fatalf("build event: %v", err)
		}
		batch = append(batch, history.EventBSON(ev))
		if action, ok := history.AuditAction(ev.Type); ok {
			auditBatch = append(auditBatch, history.AuditBSON(
				history.NewAuditEntry(ev, action, ev.OccurredAt)))
		}
		seeded++
		if len(batch) == batchSize {
			flush()
		}
	}
	flush()

	return seeded
}

func applySchema(ctx context.Context, db *pgxpool.Pool) error {
	sql, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, string(sql))
	return err
}

// loadDemoTenantIDs reads the authoritative list of seeded tenants from
// demo.tenants — BEFORE any truncate — so the reset removes every earlier
// seed run's tenants (including runs with different --tenants/--seed flags),
// not just the ones the current flags would regenerate.
func loadDemoTenantIDs(ctx context.Context, db *pgxpool.Pool) ([]string, error) {
	rows, err := db.Query(ctx, `SELECT id FROM demo.tenants`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// deleteLivePostgres removes every live-schema row of the given tenants in
// FK-respecting order (children that carry no tenant_id cascade with their
// parents): invoices → goods receipts → purchase orders → decide
// idempotency → requisitions → the three per-tenant counters (freed so a
// rerun's lazy init restarts from the new max(number)) → approval rules →
// match settings → outbox → vendors, then identity.users (user_roles and
// refresh_tokens cascade). The demo-story.sh reset order, applied per tenant
// batch.
func deleteLivePostgres(ctx context.Context, db *pgxpool.Pool, tenantIDs []string) error {
	if len(tenantIDs) == 0 {
		return nil
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, stmt := range []string{
		`DELETE FROM procurement.invoices WHERE tenant_id = ANY($1::uuid[])`,
		`DELETE FROM procurement.goods_receipts WHERE tenant_id = ANY($1::uuid[])`,
		`DELETE FROM procurement.purchase_orders WHERE tenant_id = ANY($1::uuid[])`,
		`DELETE FROM procurement.decision_idempotency WHERE tenant_id = ANY($1::uuid[])`,
		`DELETE FROM procurement.requisitions WHERE tenant_id = ANY($1::uuid[])`,
		`DELETE FROM procurement.requisition_counters WHERE tenant_id = ANY($1::uuid[])`,
		`DELETE FROM procurement.po_counters WHERE tenant_id = ANY($1::uuid[])`,
		`DELETE FROM procurement.grn_counters WHERE tenant_id = ANY($1::uuid[])`,
		`DELETE FROM procurement.approval_rules WHERE tenant_id = ANY($1::uuid[])`,
		`DELETE FROM procurement.match_settings WHERE tenant_id = ANY($1::uuid[])`,
		`DELETE FROM procurement.outbox WHERE tenant_id = ANY($1::uuid[])`,
		`DELETE FROM procurement.vendors WHERE tenant_id = ANY($1::uuid[])`,
		`DELETE FROM identity.users WHERE tenant_id = ANY($1::uuid[])`,
	} {
		if _, err := tx.Exec(ctx, stmt, tenantIDs); err != nil {
			return fmt.Errorf("reset live rows (%s): %w", firstLineOf(stmt), err)
		}
	}
	return tx.Commit(ctx)
}

// truncateDemo wipes the reconciliation export.
func truncateDemo(ctx context.Context, db *pgxpool.Pool) error {
	_, err := db.Exec(ctx, `TRUNCATE demo.invoices, demo.goods_receipts, demo.po_items, demo.purchase_orders, demo.approval_steps, demo.requisition_items, demo.requisitions, demo.vendors, demo.users, demo.tenants RESTART IDENTITY CASCADE`)
	return err
}

// deleteHistoryDocs removes ONLY the seeded tenants' rows from the two
// history collections — deleteMany per tenant, the demo-story.sh:150-157
// pattern — instead of the old Drop. Dropping destroyed the events/audit
// history of every other tenant sharing the collections (onboarding 1111…,
// e2e 3333…, demo-story 5555…, loadtest 7777…); deleteMany keeps them and
// keeps the collections' indexes (no drop/rebuild cycle).
//
// Deliberate limitation (documented in dev-note 110): document_meta,
// notifications, and export_runs rows of a seeded tenant created by LIVE
// sessions (via outbox → worker) survive this reset as orphans once their
// Postgres parents are gone — the seed never writes those collections, so it
// does not presume to reset them; `make reset` remains the nuclear option.
func deleteHistoryDocs(ctx context.Context, client *mongo.Client, mongoDB string, tenantIDs []string) error {
	if len(tenantIDs) == 0 {
		return nil
	}
	filter := bson.D{{Key: "tenant_id", Value: bson.D{{Key: "$in", Value: tenantIDs}}}}
	db := client.Database(mongoDB)
	for _, name := range []string{history.EventsCollectionName, history.AuditCollectionName} {
		if _, err := db.Collection(name).DeleteMany(ctx, filter); err != nil {
			return fmt.Errorf("delete %s: %w", name, err)
		}
	}
	return nil
}

func resetData(ctx context.Context, db *pgxpool.Pool, mongoURI, mongoDB string) error {
	// Authoritative tenant list first — never a replay of the current flags.
	tenantIDs, err := loadDemoTenantIDs(ctx, db)
	if err != nil {
		return fmt.Errorf("read demo.tenants: %w", err)
	}
	if err := deleteLivePostgres(ctx, db, tenantIDs); err != nil {
		return err
	}
	if err := truncateDemo(ctx, db); err != nil {
		return err
	}
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		return err
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	return deleteHistoryDocs(ctx, client, mongoDB, tenantIDs)
}

func copyOrFatal(ctx context.Context, db *pgxpool.Pool, table pgx.Identifier, columns []string, gen func(yield func([]any) error) error, label string) {
	if _, err := copyRows(ctx, db, table, columns, gen); err != nil {
		fatalf("copy %s: %v", label, err)
	}
}

func copyRows(ctx context.Context, db *pgxpool.Pool, table pgx.Identifier, columns []string, gen func(yield func([]any) error) error) (int64, error) {
	rows := make([][]any, 0, 1024)
	if err := gen(func(values []any) error {
		rows = append(rows, values)
		return nil
	}); err != nil {
		return 0, err
	}
	src := pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
		return rows[i], nil
	})
	return db.CopyFrom(ctx, table, columns, src)
}

func newUUID(rng *rand.Rand) string {
	var b [16]byte
	for i := range b {
		b[i] = byte(rng.Intn(256))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// seedActorIP draws a deterministic documentation-range IPv4 address
// (RFC 5737 TEST-NET style but inside 10.x so it reads as an office VPN):
// the same rng stream, so reseeding reproduces identical values.
func seedActorIP(rng *rand.Rand) string {
	return fmt.Sprintf("10.%d.%d.%d", rng.Intn(4), rng.Intn(256), 1+rng.Intn(254))
}

func firstLineOf(sql string) string {
	if i := strings.IndexByte(sql, '\n'); i >= 0 {
		return sql[:i]
	}
	return sql
}

func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "seed: "+format+"\n", args...)
	os.Exit(1)
}
