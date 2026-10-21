package attachment

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/luksluki/eprocurement/services/documents/internal/testenv"
)

// fakeStorage records Remove calls; the reaper only removes objects.
type fakeStorage struct {
	removed map[string]int
}

func (f *fakeStorage) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, time.Time, error) {
	return "", time.Time{}, nil
}
func (f *fakeStorage) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, time.Time, error) {
	return "", time.Time{}, nil
}
func (f *fakeStorage) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	return ObjectInfo{}, errors.New("not implemented")
}
func (f *fakeStorage) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeStorage) Remove(ctx context.Context, key string) error {
	if f.removed == nil {
		f.removed = map[string]int{}
	}
	f.removed[key]++
	return nil
}

func TestReaperRunOnce(t *testing.T) {
	db := testenv.Mongo(t)
	store := NewStore(db)
	storage := &fakeStorage{}
	reaper := NewReaper(store, storage, 24*time.Hour, nil)
	reaper.now = func() time.Time {
		return time.Now().UTC().Add(0) // real clock; fixtures below pin ages
	}

	now := time.Now().UTC()
	stale := newAttachment("eeeeeeee-0000-0000-0000-000000000007", "t-a", "v-1", StatusPending, now.Add(-48*time.Hour))
	fresh := newAttachment("eeeeeeee-0000-0000-0000-000000000008", "t-a", "v-1", StatusPending, now.Add(-time.Minute))
	ready := newAttachment("eeeeeeee-0000-0000-0000-000000000009", "t-a", "v-1", StatusReady, now.Add(-48*time.Hour))
	for _, att := range []Attachment{stale, fresh, ready} {
		if err := store.Insert(t.Context(), att); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	removed, err := reaper.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (only the stale pending)", removed)
	}
	if storage.removed[stale.ObjectKey] != 1 {
		t.Fatalf("stale object not removed: %v", storage.removed)
	}
	if _, err := store.Get(t.Context(), "t-a", stale.ID); err != ErrNotFound {
		t.Fatalf("stale meta after sweep = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(t.Context(), "t-a", fresh.ID); err != nil {
		t.Fatalf("fresh pending should survive: %v", err)
	}
	if _, err := store.Get(t.Context(), "t-a", ready.ID); err != nil {
		t.Fatalf("ready should survive: %v", err)
	}
}
