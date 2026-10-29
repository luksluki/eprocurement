package objectstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/luksluki/eprocurement/services/documents/attachment"
	"github.com/luksluki/eprocurement/services/documents/internal/testenv"
)

// putViaPresignedURL performs the client side of the upload flow: a plain
// HTTP PUT with Content-Type, exactly what a browser would do against the
// presigned URL. This is the AC "upload completes directly to MinIO".
func putViaPresignedURL(t *testing.T, url, contentType string, body []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build PUT: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d", resp.StatusCode)
	}
}

func getViaPresignedURL(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.DefaultClient.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read GET body: %v", err)
	}
	return body
}

func TestMinIORoundtrip(t *testing.T) {
	endpoint, accessKey, secretKey, bucket := testenv.MinIOEnv(t)
	storage, err := New(endpoint, "", accessKey, secretKey, bucket, false)
	if err != nil {
		t.Fatalf("connect object store: %v", err)
	}
	if err := storage.EnsureBucket(t.Context()); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	ctx := t.Context()
	key := attachment.ObjectKey("t-rw", attachment.EntityTypeVendor, "test-roundtrip-1")
	content := []byte("attachment roundtrip bytes")
	contentType := "application/pdf"

	putURL, _, err := storage.PresignPut(ctx, key, 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if !strings.Contains(putURL, key) {
		t.Fatalf("presigned URL %q missing key %q", putURL, key)
	}
	putViaPresignedURL(t, putURL, contentType, content)

	info, err := storage.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(content)) || info.ContentType != contentType {
		t.Fatalf("Stat info = %+v", info)
	}

	reader, err := storage.Read(ctx, key)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, reader); err != nil {
		t.Fatalf("hash: %v", err)
	}
	reader.Close()
	if hex.EncodeToString(hash.Sum(nil)) != sha256Hex(content) {
		t.Fatal("streamed bytes differ from uploaded bytes")
	}

	getURL, _, err := storage.PresignGet(ctx, key, 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	if got := getViaPresignedURL(t, getURL); !bytes.Equal(got, content) {
		t.Fatalf("GET body = %q, want %q", got, content)
	}

	// Presign of a missing object must not fail (S3 signs blindly); Stat is
	// the existence check.
	if err := storage.Remove(ctx, key); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := storage.Stat(ctx, key); err != attachment.ErrObjectMissing {
		t.Fatalf("Stat after Remove = %v, want ErrObjectMissing", err)
	}
	if err := storage.Remove(ctx, key); err != nil {
		t.Fatalf("Remove is not idempotent: %v", err)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestMinIOPutAndOverwrite pins the export writer's contract (#49): Put
// stores an object with its content type under the exports/ key family, and
// a second Put to the same key overwrites it — the per-date idempotency the
// nightly export relies on. The presigned GET then serves the latest bytes.
func TestMinIOPutAndOverwrite(t *testing.T) {
	endpoint, accessKey, secretKey, bucket := testenv.MinIOEnv(t)
	storage, err := New(endpoint, "", accessKey, secretKey, bucket, false)
	if err != nil {
		t.Fatalf("connect object store: %v", err)
	}
	if err := storage.EnsureBucket(t.Context()); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	ctx := t.Context()
	key := "exports/t-put/2026-10-27/purchase-orders.csv"

	first := []byte("po_number,vendor_code\nPO-1,V-1\n")
	if err := storage.Put(ctx, key, bytes.NewReader(first), int64(len(first)), "text/csv; charset=utf-8"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	info, err := storage.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(first)) || info.ContentType != "text/csv; charset=utf-8" {
		t.Fatalf("Stat info = %+v", info)
	}

	second := []byte("po_number,vendor_code\nPO-1,V-1\nPO-2,V-2\n")
	if err := storage.Put(ctx, key, bytes.NewReader(second), int64(len(second)), "text/csv; charset=utf-8"); err != nil {
		t.Fatalf("overwrite Put: %v", err)
	}
	getURL, _, err := storage.PresignGet(ctx, key, 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	if got := getViaPresignedURL(t, getURL); !bytes.Equal(got, second) {
		t.Fatalf("GET after overwrite = %q, want %q", got, second)
	}

	if err := storage.Remove(ctx, key); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}
