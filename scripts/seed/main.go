// Command seed generates the deterministic demo dataset for the local stack.
//
// Usage:
//
//	go run ./scripts/seed            # wipe demo data, then seed (deterministic)
//	go run ./scripts/seed --reset    # wipe demo data only
package main

import (
	"context"
	"embed"
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
)

//go:embed schema.sql
var schemaFS embed.FS

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

type tenantSeed struct {
	id   string
	name string
	slug string
}

type userSeed struct {
	id       string
	tenantID string
	email    string
	role     string
}

type vendorSeed struct {
	id       string
	tenantID string
	code     string
	name     string
	status   string
}

type requisitionSeed struct {
	id          string
	tenantID    string
	number      string
	requesterID string
	status      string
	totalCents  int64
	submittedAt time.Time
}

type poSeed struct {
	id            string
	tenantID      string
	number        string
	requisitionID string
	vendorID      string
	status        string
	totalCents    int64
	issuedAt      time.Time
}

type grnSeed struct {
	id         string
	tenantID   string
	poID       string
	number     string
	receivedAt time.Time
}

type invoiceSeed struct {
	id          string
	tenantID    string
	number      string
	poID        string
	amountCents int64
	status      string
	createdAt   time.Time
}

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

	tenants, users, vendors := seedReferenceData(ctx, db, rng, cfg)
	reqs, pos, grns, invoices := seedProcurementData(ctx, db, rng, cfg, users, vendors)
	seededEvents := seedEvents(ctx, cfg, rng, reqs)

	fmt.Printf("seeded tenants=%d users=%d vendors=%d requisitions=%d purchase_orders=%d goods_receipts=%d invoices=%d events=%d in %s\n",
		len(tenants), len(users), len(vendors), len(reqs), len(pos), len(grns), len(invoices), seededEvents,
		time.Since(start).Round(time.Millisecond))
}

func seedReferenceData(ctx context.Context, db *pgxpool.Pool, rng *rand.Rand, cfg config) ([]tenantSeed, []userSeed, []vendorSeed) {
	tenantNames := []string{"Nusantara Retail", "Kirana Logistics", "Sagara Manufacturing", "Merapi Foods", "Bumi Health"}
	slugs := []string{"nusantara-retail", "kirana-logistics", "sagara-manufacturing", "merapi-foods", "bumi-health"}
	roles := []string{"admin", "requester", "approver", "buyer", "finance"}
	vendorPrefixes := []string{"PT", "CV", "UD"}
	vendorWords := []string{"Sinar", "Maju", "Sentosa", "Abadi", "Prima", "Karya", "Global", "Cahaya", "Mitra", "Jaya"}
	vendorGoods := []string{"Supply", "Teknik", "Logistik", "Pangan", "Medika", "Energi"}

	tenants := make([]tenantSeed, 0, cfg.tenants)
	users := make([]userSeed, 0, cfg.tenants*len(roles))
	vendors := make([]vendorSeed, 0, cfg.tenants*cfg.vendorsPer)

	for i := 0; i < cfg.tenants; i++ {
		name := tenantNames[i%len(tenantNames)]
		slug := slugs[i%len(slugs)]
		t := tenantSeed{id: newUUID(rng), name: name, slug: slug}
		tenants = append(tenants, t)

		for _, role := range roles {
			users = append(users, userSeed{
				id:       newUUID(rng),
				tenantID: t.id,
				email:    fmt.Sprintf("%s@%s.example", role, slug),
				role:     role,
			})
		}

		for j := 0; j < cfg.vendorsPer; j++ {
			vendors = append(vendors, vendorSeed{
				id:       newUUID(rng),
				tenantID: t.id,
				code:     fmt.Sprintf("V-%d-%04d", i+1, j+1),
				name:     fmt.Sprintf("%s %s %s %s", vendorPrefixes[j%len(vendorPrefixes)], vendorWords[rng.Intn(len(vendorWords))], vendorGoods[rng.Intn(len(vendorGoods))], fmt.Sprintf("%d", j+1)),
				status:   "active",
			})
		}
	}

	if _, err := copyRows(ctx, db, "tenants", []string{"id", "name", "slug"}, func(yield func([]any) error) error {
		for _, t := range tenants {
			if err := yield([]any{t.id, t.name, t.slug}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		fatalf("copy tenants: %v", err)
	}

	if _, err := copyRows(ctx, db, "users", []string{"id", "tenant_id", "email", "role"}, func(yield func([]any) error) error {
		for _, u := range users {
			if err := yield([]any{u.id, u.tenantID, u.email, u.role}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		fatalf("copy users: %v", err)
	}

	if _, err := copyRows(ctx, db, "vendors", []string{"id", "tenant_id", "code", "name", "status"}, func(yield func([]any) error) error {
		for _, v := range vendors {
			if err := yield([]any{v.id, v.tenantID, v.code, v.name, v.status}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		fatalf("copy vendors: %v", err)
	}

	return tenants, users, vendors
}

func seedProcurementData(ctx context.Context, db *pgxpool.Pool, rng *rand.Rand, cfg config, users []userSeed, vendors []vendorSeed) ([]requisitionSeed, []poSeed, []grnSeed, []invoiceSeed) {
	statuses := []string{"approved", "approved", "approved", "approved", "approved", "submitted", "submitted", "rejected", "closed"}
	poStatuses := []string{"issued", "acknowledged", "partially_received", "received", "closed"}
	base := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)

	requisitions := make([]requisitionSeed, 0, cfg.requisitions)
	pos := make([]poSeed, 0, cfg.requisitions)
	grns := make([]grnSeed, 0, cfg.requisitions)
	invoices := make([]invoiceSeed, 0, cfg.requisitions)

	for i := 0; i < cfg.requisitions; i++ {
		u := users[rng.Intn(len(users))]
		v := vendors[rng.Intn(len(vendors))]
		status := statuses[rng.Intn(len(statuses))]
		total := int64(250000 + rng.Intn(50000000))
		submitted := base.Add(time.Duration(i) * time.Minute)

		r := requisitionSeed{
			id:          newUUID(rng),
			tenantID:    u.tenantID,
			number:      fmt.Sprintf("PR-%06d", i+1),
			requesterID: u.id,
			status:      status,
			totalCents:  total,
			submittedAt: submitted,
		}
		requisitions = append(requisitions, r)

		if status == "approved" || status == "closed" {
			po := poSeed{
				id:            newUUID(rng),
				tenantID:      r.tenantID,
				number:        fmt.Sprintf("PO-%06d", i+1),
				requisitionID: r.id,
				vendorID:      v.id,
				status:        poStatuses[rng.Intn(len(poStatuses))],
				totalCents:    total,
				issuedAt:      submitted.Add(2 * time.Hour),
			}
			pos = append(pos, po)

			if rng.Intn(100) < 60 {
				grns = append(grns, grnSeed{
					id:         newUUID(rng),
					tenantID:   r.tenantID,
					poID:       po.id,
					number:     fmt.Sprintf("GRN-%06d", len(grns)+1),
					receivedAt: po.issuedAt.Add(24 * time.Hour),
				})
			}
			if rng.Intn(100) < 50 {
				invoices = append(invoices, invoiceSeed{
					id:          newUUID(rng),
					tenantID:    r.tenantID,
					number:      fmt.Sprintf("INV-%06d", len(invoices)+1),
					poID:        po.id,
					amountCents: total,
					status:      "matched",
					createdAt:   po.issuedAt.Add(48 * time.Hour),
				})
			}
		}
	}

	if _, err := copyRows(ctx, db, "requisitions", []string{"id", "tenant_id", "number", "requester_id", "status", "currency", "total_cents", "submitted_at"}, func(yield func([]any) error) error {
		for _, r := range requisitions {
			if err := yield([]any{r.id, r.tenantID, r.number, r.requesterID, r.status, "IDR", r.totalCents, r.submittedAt}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		fatalf("copy requisitions: %v", err)
	}

	if _, err := copyRows(ctx, db, "requisition_items", []string{"id", "requisition_id", "description", "qty", "unit_price_cents"}, func(yield func([]any) error) error {
		for _, r := range requisitions {
			for k := 0; k < 2; k++ {
				qty := 1 + rng.Intn(10)
				unit := r.totalCents / 2 / int64(qty)
				if err := yield([]any{newUUID(rng), r.id, fmt.Sprintf("Item %d for %s", k+1, r.number), qty, unit}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		fatalf("copy requisition_items: %v", err)
	}

	if _, err := copyRows(ctx, db, "approval_steps", []string{"id", "requisition_id", "step_no", "role_required", "status", "decided_at"}, func(yield func([]any) error) error {
		for _, r := range requisitions {
			decided := any(nil)
			status := "pending"
			if r.status == "approved" || r.status == "closed" {
				status = "approved"
				decided = r.submittedAt.Add(1 * time.Hour)
			}
			for step := 1; step <= 2; step++ {
				if err := yield([]any{newUUID(rng), r.id, step, "approver", status, decided}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		fatalf("copy approval_steps: %v", err)
	}

	if _, err := copyRows(ctx, db, "purchase_orders", []string{"id", "tenant_id", "number", "requisition_id", "vendor_id", "status", "total_cents", "issued_at"}, func(yield func([]any) error) error {
		for _, p := range pos {
			if err := yield([]any{p.id, p.tenantID, p.number, p.requisitionID, p.vendorID, p.status, p.totalCents, p.issuedAt}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		fatalf("copy purchase_orders: %v", err)
	}

	if _, err := copyRows(ctx, db, "po_items", []string{"id", "po_id", "description", "qty_ordered", "qty_received", "unit_price_cents"}, func(yield func([]any) error) error {
		for _, p := range pos {
			for k := 0; k < 2; k++ {
				qty := 1 + rng.Intn(10)
				received := 0
				if p.status == "received" || p.status == "closed" {
					received = qty
				} else if p.status == "partially_received" {
					received = qty / 2
				}
				if err := yield([]any{newUUID(rng), p.id, fmt.Sprintf("Item %d for %s", k+1, p.number), qty, received, p.totalCents / 2 / int64(qty)}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		fatalf("copy po_items: %v", err)
	}

	if _, err := copyRows(ctx, db, "goods_receipts", []string{"id", "tenant_id", "po_id", "number", "received_at"}, func(yield func([]any) error) error {
		for _, g := range grns {
			if err := yield([]any{g.id, g.tenantID, g.poID, g.number, g.receivedAt}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		fatalf("copy goods_receipts: %v", err)
	}

	if _, err := copyRows(ctx, db, "invoices", []string{"id", "tenant_id", "number", "po_id", "amount_cents", "status", "created_at"}, func(yield func([]any) error) error {
		for _, inv := range invoices {
			if err := yield([]any{inv.id, inv.tenantID, inv.number, inv.poID, inv.amountCents, inv.status, inv.createdAt}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		fatalf("copy invoices: %v", err)
	}

	return requisitions, pos, grns, invoices
}

func seedEvents(ctx context.Context, cfg config, rng *rand.Rand, reqs []requisitionSeed) int {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(cfg.mongoURI))
	if err != nil {
		fatalf("mongo connect: %v", err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()

	collection := client.Database(cfg.mongoDB).Collection("events")
	types := []string{"pr.created", "pr.submitted", "pr.approved", "po.issued", "po.acknowledged", "grn.posted", "invoice.matched", "pr.rejected", "po.closed", "notification.sent"}
	base := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)

	const batchSize = 5000
	seeded := 0
	batch := make([]any, 0, batchSize)
	for i := 0; i < cfg.events; i++ {
		r := reqs[rng.Intn(len(reqs))]
		event := bson.M{
			"event_id":    newUUID(rng),
			"tenant_id":   r.tenantID,
			"type":        types[rng.Intn(len(types))],
			"entity_type": "requisition",
			"entity_id":   r.id,
			"occurred_at": base.Add(time.Duration(i) * time.Second),
			"payload":     bson.M{"number": r.number},
		}
		batch = append(batch, event)
		if len(batch) == batchSize {
			if _, err := collection.InsertMany(ctx, batch); err != nil {
				fatalf("insert events: %v", err)
			}
			seeded += len(batch)
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if _, err := collection.InsertMany(ctx, batch); err != nil {
			fatalf("insert events: %v", err)
		}
		seeded += len(batch)
	}

	if _, err := collection.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "event_id", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "occurred_at", Value: -1}}},
	}); err != nil {
		fatalf("create event indexes: %v", err)
	}
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

func resetData(ctx context.Context, db *pgxpool.Pool, mongoURI, mongoDB string) error {
	_, err := db.Exec(ctx, `TRUNCATE demo.invoices, demo.goods_receipts, demo.po_items, demo.purchase_orders, demo.approval_steps, demo.requisition_items, demo.requisitions, demo.vendors, demo.users, demo.tenants RESTART IDENTITY CASCADE`)
	if err != nil {
		return err
	}
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		return err
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	err = client.Database(mongoDB).Collection("events").Drop(ctx)
	if err != nil && !strings.Contains(err.Error(), "ns not found") {
		return err
	}
	return nil
}

func copyRows(ctx context.Context, db *pgxpool.Pool, table string, columns []string, gen func(yield func([]any) error) error) (int64, error) {
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
	return db.CopyFrom(ctx, pgx.Identifier{"demo", table}, columns, src)
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
