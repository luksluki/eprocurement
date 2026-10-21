package attachment

import (
	"testing"
	"time"

	"github.com/luksluki/eprocurement/services/documents/internal/testenv"
)

func newAttachment(id, tenant, entityID, status string, createdAt time.Time) Attachment {
	return Attachment{
		ID: id, TenantID: tenant, EntityType: EntityTypeVendor, EntityID: entityID,
		Filename: "contract.pdf", MimeType: "application/pdf", SizeBytes: 10,
		ChecksumSHA256: validChecksum, Status: status,
		ObjectKey: ObjectKey(tenant, EntityTypeVendor, id),
		CreatedBy: "user-1", CreatedAt: createdAt,
	}
}

func TestStoreLifecycle(t *testing.T) {
	db := testenv.Mongo(t)
	store := NewStore(db)
	if err := store.EnsureIndexes(t.Context()); err != nil {
		t.Fatalf("EnsureIndexes: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	att := newAttachment("aaaaaaaa-0000-0000-0000-000000000001", "t-a", "v-1", StatusPending, now)

	if err := store.Insert(t.Context(), att); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := store.Get(t.Context(), "t-a", att.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusPending || got.ObjectKey != att.ObjectKey || !got.UploadedAt.IsZero() {
		t.Fatalf("Get returned %+v", got)
	}

	// Cross-tenant denial: tenant B cannot see tenant A's attachment.
	if _, err := store.Get(t.Context(), "t-b", att.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant Get error = %v, want ErrNotFound", err)
	}

	if _, err := store.Get(t.Context(), "t-a", "not a uuid"); err != ErrIDInvalid {
		t.Fatalf("malformed id error = %v, want ErrIDInvalid", err)
	}

	uploadedAt := now.Add(time.Minute)
	if err := store.MarkReady(t.Context(), "t-a", att.ID, 10, uploadedAt); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	got, err = store.Get(t.Context(), "t-a", att.ID)
	if err != nil {
		t.Fatalf("Get after ready: %v", err)
	}
	if got.Status != StatusReady || got.UploadedAt.IsZero() {
		t.Fatalf("MarkReady did not stick: %+v", got)
	}

	// MarkReady on unknown id surfaces NotFound.
	if err := store.MarkReady(t.Context(), "t-a", "bbbbbbbb-0000-0000-0000-000000000002", 10, uploadedAt); err != ErrNotFound {
		t.Fatalf("MarkReady unknown error = %v, want ErrNotFound", err)
	}
}

func TestStoreList(t *testing.T) {
	db := testenv.Mongo(t)
	store := NewStore(db)
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i, status := range []string{StatusPending, StatusReady} {
		id := string(rune('a'+i)) + "bbbbbbb-0000-0000-0000-00000000000" + string(rune('1'+i))
		att := newAttachment(id, "t-a", "v-9", status, now.Add(time.Duration(i)*time.Second))
		if err := store.Insert(t.Context(), att); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	// Same tenant, other vendor: must not leak into the v-9 listing.
	other := newAttachment("cccccccc-0000-0000-0000-000000000003", "t-a", "v-8", StatusReady, now)
	if err := store.Insert(t.Context(), other); err != nil {
		t.Fatalf("Insert other: %v", err)
	}

	list, err := store.List(t.Context(), "t-a", EntityTypeVendor, "v-9")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].Status != StatusPending || list[1].Status != StatusReady {
		t.Fatalf("List = %+v", list)
	}

	// Cross-tenant listing sees nothing.
	list, err = store.List(t.Context(), "t-b", EntityTypeVendor, "v-9")
	if err != nil || len(list) != 0 {
		t.Fatalf("cross-tenant List = %+v, %v", list, err)
	}

	// Unknown entity kind is rejected before any query.
	if _, err := store.List(t.Context(), "t-a", "requisition", "v-9"); err != ErrEntityInvalid {
		t.Fatalf("List entity error = %v, want ErrEntityInvalid", err)
	}
}

func TestStoreListPendingCreatedBefore(t *testing.T) {
	db := testenv.Mongo(t)
	store := NewStore(db)
	now := time.Now().UTC().Truncate(time.Millisecond)
	stale := newAttachment("dddddddd-0000-0000-0000-000000000004", "t-a", "v-1", StatusPending, now.Add(-48*time.Hour))
	fresh := newAttachment("dddddddd-0000-0000-0000-000000000005", "t-a", "v-1", StatusPending, now.Add(-time.Minute))
	ready := newAttachment("dddddddd-0000-0000-0000-000000000006", "t-a", "v-1", StatusReady, now.Add(-48*time.Hour))
	for _, att := range []Attachment{stale, fresh, ready} {
		if err := store.Insert(t.Context(), att); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	pending, err := store.ListPendingCreatedBefore(t.Context(), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ListPendingCreatedBefore: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != stale.ID {
		t.Fatalf("pending sweep = %+v, want only the stale one", pending)
	}

	if err := store.Delete(t.Context(), "t-a", stale.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(t.Context(), "t-a", stale.ID); err != ErrNotFound {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
}
