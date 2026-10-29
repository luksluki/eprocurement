// Package objectstore adapts the attachment domain's Storage port to MinIO
// (S3-compatible API, SigV4 presigned URLs).
package objectstore

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/luksluki/eprocurement/services/documents/attachment"
)

// MinIO implements attachment.Storage against one bucket. API calls
// (stat/read/remove) use the internal endpoint via minio-go; presigned URLs
// are minted locally (sigv4.go) for presignHost, which may differ — e.g.
// compose services reach MinIO at "minio:9000" while clients outside the
// network need "localhost:9000". Empty presignEndpoint falls back to
// endpoint.
type MinIO struct {
	client      *minio.Client
	presignHost string
	accessKey   string
	secretKey   string
	useSSL      bool
	bucket      string
}

// New connects to the S3-compatible endpoint (host:port, no scheme).
func New(endpoint, presignEndpoint, accessKey, secretKey, bucket string, useSSL bool) (*MinIO, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, err
	}
	if presignEndpoint == "" {
		presignEndpoint = endpoint
	}
	return &MinIO{
		client:      client,
		presignHost: presignEndpoint,
		accessKey:   accessKey,
		secretKey:   secretKey,
		useSSL:      useSSL,
		bucket:      bucket,
	}, nil
}

// EnsureBucket creates the bucket when missing (idempotent, safe on boot).
func (m *MinIO) EnsureBucket(ctx context.Context) error {
	exists, err := m.client.BucketExists(ctx, m.bucket)
	if err != nil {
		return err
	}
	if !exists {
		return m.client.MakeBucket(ctx, m.bucket, minio.MakeBucketOptions{})
	}
	return nil
}

// PresignPut returns a presigned PUT URL for direct browser/service upload.
func (m *MinIO) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, time.Time, error) {
	return m.presign("PUT", key, ttl)
}

// PresignGet returns a presigned GET URL for direct download.
func (m *MinIO) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, time.Time, error) {
	return m.presign("GET", key, ttl)
}

// Stat reports the stored object's size and content type.
func (m *MinIO) Stat(ctx context.Context, key string) (attachment.ObjectInfo, error) {
	info, err := m.client.StatObject(ctx, m.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isNoSuchKey(err) {
			return attachment.ObjectInfo{}, attachment.ErrObjectMissing
		}
		return attachment.ObjectInfo{}, err
	}
	return attachment.ObjectInfo{Size: info.Size, ContentType: info.ContentType}, nil
}

// Read streams the object bytes.
func (m *MinIO) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := m.client.GetObject(ctx, m.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		if isNoSuchKey(err) {
			return nil, attachment.ErrObjectMissing
		}
		return nil, err
	}
	return obj, nil
}

// Remove deletes the object; S3 removal of a missing key is a success.
func (m *MinIO) Remove(ctx context.Context, key string) error {
	return m.client.RemoveObject(ctx, m.bucket, key, minio.RemoveObjectOptions{})
}

// Put stores an object with the given size and content type. The nightly
// export writer (#49) uses it for the CSV artifacts under exports/; a
// same-key put overwrites, which is the export's per-date idempotency.
func (m *MinIO) Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error {
	_, err := m.client.PutObject(ctx, m.bucket, key, reader, size, minio.PutObjectOptions{
		ContentType: contentType,
	})
	return err
}

// isNoSuchKey maps MinIO's "NoSuchKey" error response to the domain
// sentinel's condition.
func isNoSuchKey(err error) bool {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.Code == "NoSuchKey" || resp.Code == "NoSuchObject"
	}
	_, ok := err.(*url.Error)
	return ok && strings.Contains(err.Error(), "NoSuchKey")
}
