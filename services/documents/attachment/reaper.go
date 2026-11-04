package attachment

import (
	"context"
	"io"
	"log/slog"
	"time"
)

// Storage is the object-store port: presigning, stat, streaming, and
// removal. The MinIO adapter implements it; tests may substitute a fake.
type Storage interface {
	// PresignPut returns a presigned PUT URL for direct upload.
	PresignPut(ctx context.Context, key string, ttl time.Duration) (url string, expiresAt time.Time, err error)
	// PresignGet returns a presigned GET URL for direct download.
	PresignGet(ctx context.Context, key string, ttl time.Duration) (url string, expiresAt time.Time, err error)
	// Stat reports the stored object's size and content type;
	// ErrObjectMissing when it was never uploaded (or already removed).
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	// Read streams the object bytes (checksum verification).
	Read(ctx context.Context, key string) (io.ReadCloser, error)
	// Remove deletes the object; it is idempotent (removing a missing
	// object succeeds).
	Remove(ctx context.Context, key string) error
}

// ObjectInfo is the verified shape of a stored object.
type ObjectInfo struct {
	Size        int64
	ContentType string
}

// Reaper deletes orphan uploads: pending attachments older than the TTL
// (never completed, or completed with bytes that failed verification) have
// their object removed best-effort and their metadata dropped. It runs as a
// periodic job inside the documents service — a singleton there until the
// worker takes over cron with Redis locks in a later issue.
type Reaper struct {
	store   *Store
	storage Storage
	ttl     time.Duration
	log     *slog.Logger
	now     func() time.Time
}

// NewReaper builds a reaper with the given pending TTL.
func NewReaper(store *Store, storage Storage, ttl time.Duration, log *slog.Logger) *Reaper {
	return &Reaper{store: store, storage: storage, ttl: ttl, log: log, now: time.Now}
}

// RunOnce sweeps once and returns the number of orphans removed.
func (r *Reaper) RunOnce(ctx context.Context) (int, error) {
	stale, err := r.store.ListPendingCreatedBefore(ctx, r.now().Add(-r.ttl))
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, a := range stale {
		if err := r.storage.Remove(ctx, a.ObjectKey); err != nil && r.log != nil {
			// Best-effort: the object may already be gone; metadata still
			// goes so the orphan is no longer addressable.
			r.log.Warn("reaper: remove object failed", "object_key", a.ObjectKey, "error", err)
		}
		if err := r.store.Delete(ctx, a.TenantID, a.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// Loop runs RunOnce immediately and then on every tick until ctx is done.
func (r *Reaper) Loop(ctx context.Context, interval time.Duration) {
	if r.log != nil {
		r.log.Info("attachment reaper started", "interval", interval.String(), "pending_ttl", r.ttl.String())
	}
	r.sweep(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sweep(ctx)
		}
	}
}

func (r *Reaper) sweep(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	removed, err := r.RunOnce(ctx)
	if err != nil {
		if r.log != nil {
			r.log.Error("attachment reaper sweep failed", "error", err)
		}
		return
	}
	if removed > 0 && r.log != nil {
		r.log.Info("attachment reaper removed orphans", "count", removed)
	}
}
