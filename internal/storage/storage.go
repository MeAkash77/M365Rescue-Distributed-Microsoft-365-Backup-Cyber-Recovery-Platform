// Package storage wraps an S3-compatible object store (MinIO or AWS S3) with optional Object Lock.
package storage

import (
	"bytes"
	"context"
	"io"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Store struct {
	c      *minio.Client
	bucket string
	lock   bool
	mode   string
}

func New(ctx context.Context, endpoint, ak, sk string, ssl bool, bucket string, lock bool, mode string) (*Store, error) {
	c, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: ssl})
	if err != nil {
		return nil, err
	}
	var lastErr error
	for i := 0; i < 30; i++ {
		var ok bool
		ok, lastErr = c.BucketExists(ctx, bucket)
		if lastErr == nil {
			if !ok {
				// Object Lock can only be enabled at bucket creation time (it also enables versioning).
				lastErr = c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{ObjectLocking: lock})
			}
			if lastErr == nil {
				return &Store{c: c, bucket: bucket, lock: lock, mode: strings.ToUpper(mode)}, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, lastErr
}

// Put writes an object; when Object Lock is on and retain is set the version is WORM-protected until then.
func (s *Store) Put(ctx context.Context, key string, data []byte, retain *time.Time) error {
	o := minio.PutObjectOptions{ContentType: "application/octet-stream"}
	if s.lock && retain != nil {
		o.Mode = minio.RetentionMode(s.mode)
		o.RetainUntilDate = *retain
	}
	_, err := s.c.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)), o)
	return err
}

func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.c.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	return io.ReadAll(obj)
}
