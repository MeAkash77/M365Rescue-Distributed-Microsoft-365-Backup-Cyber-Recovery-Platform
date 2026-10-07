// Package vault is the backup data plane: hash -> dedup -> compress -> encrypt -> immutable object store.
package vault

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"m365rescue/internal/chaos"
	"m365rescue/internal/crypto"
	"m365rescue/internal/metrics"
	"m365rescue/internal/retry"
)

type Blobs interface {
	Put(ctx context.Context, key string, data []byte, retain *time.Time) error
	Get(ctx context.Context, key string) ([]byte, error)
}

type Vault struct {
	DB        *pgxpool.Pool
	Blobs     Blobs
	Master    []byte
	Retention time.Duration
}

func aad(tenantID, sha string) []byte { return []byte(tenantID + ":" + sha) }

// Put stores data idempotently. Identical content within a tenant is stored exactly once; replaying the
// same call after a crash (S3 write done, DB row missing) simply rewrites the same key and converges.
func (v *Vault) Put(ctx context.Context, tenantID string, data []byte) (sha string, size int64, err error) {
	sum := sha256.Sum256(data)
	sha = hex.EncodeToString(sum[:])
	size = int64(len(data))

	var exists bool
	if err = v.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM objects WHERE tenant_id=$1 AND sha256=$2)`, tenantID, sha).Scan(&exists); err != nil {
		return
	}
	if exists {
		metrics.ObjectsStored.WithLabelValues("dedup").Inc()
		return
	}

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err = zw.Write(data); err != nil {
		return
	}
	if err = zw.Close(); err != nil {
		return
	}
	blob, err := crypto.Seal(crypto.TenantKey(v.Master, tenantID), gz.Bytes(), aad(tenantID, sha))
	if err != nil {
		return
	}
	key := fmt.Sprintf("tenants/%s/objects/%s/%s", tenantID, sha[:2], sha)
	err = retry.Do(ctx, 3, func() error {
		if chaos.Hit("storage_error") {
			return errors.New("chaos: injected object-store failure")
		}
		var retain *time.Time
		if v.Retention > 0 {
			t := time.Now().Add(v.Retention)
			retain = &t
		}
		return v.Blobs.Put(ctx, key, blob, retain)
	})
	if err != nil {
		return
	}
	_, err = v.DB.Exec(ctx, `INSERT INTO objects(tenant_id,sha256,size,stored_size,storage_key) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
		tenantID, sha, size, int64(len(blob)), key)
	if err == nil {
		metrics.ObjectsStored.WithLabelValues("new").Inc()
	}
	return
}

// Get fetches, decrypts, decompresses and verifies the content hash.
func (v *Vault) Get(ctx context.Context, tenantID, sha string) ([]byte, error) {
	var key string
	if err := v.DB.QueryRow(ctx, `SELECT storage_key FROM objects WHERE tenant_id=$1 AND sha256=$2`, tenantID, sha).Scan(&key); err != nil {
		return nil, fmt.Errorf("object %s not indexed: %w", sha, err)
	}
	blob, err := v.Blobs.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	gz, err := crypto.Open(crypto.TenantKey(v.Master, tenantID), blob, aad(tenantID, sha))
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != sha {
		return nil, errors.New("checksum mismatch after decrypt")
	}
	return data, nil
}
