// Package backup runs one backup job: delta-sync a workload and materialise a new snapshot.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"m365rescue/internal/chaos"
	"m365rescue/internal/metrics"
	"m365rescue/internal/retry"
	"m365rescue/internal/source"
	"m365rescue/internal/vault"
)

type Engine struct {
	DB        *pgxpool.Pool
	Vault     *vault.Vault
	Reg       source.Registry
	Retention time.Duration
}

type Result struct {
	Changed int
	Bytes   int64
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ensureSnapshot creates the job's snapshot once (carrying the parent manifest forward). Retries reuse it,
// which is what makes the job resumable.
func (e *Engine) ensureSnapshot(ctx context.Context, jobID, tenantID, workload, existing string) (string, error) {
	if existing != "" {
		return existing, nil
	}
	tx, err := e.DB.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var parent string
	err = tx.QueryRow(ctx, `SELECT id::text FROM snapshots WHERE tenant_id=$1 AND workload=$2 AND status='complete' ORDER BY created_at DESC LIMIT 1`, tenantID, workload).Scan(&parent)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	var snap string
	if err = tx.QueryRow(ctx, `INSERT INTO snapshots(tenant_id,workload,parent_id,status) VALUES($1,$2,$3,'running') RETURNING id::text`, tenantID, workload, nullable(parent)).Scan(&snap); err != nil {
		return "", err
	}
	if parent != "" {
		if _, err = tx.Exec(ctx, `INSERT INTO snapshot_items(snapshot_id,resource_id,name,sha256,size,meta)
			SELECT $1::uuid, resource_id,name,sha256,size,meta FROM snapshot_items WHERE snapshot_id=$2::uuid`, snap, parent); err != nil {
			return "", err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE backup_jobs SET snapshot_id=$1::uuid WHERE id=$2::uuid`, snap, jobID); err != nil {
		return "", err
	}
	return snap, tx.Commit(ctx)
}

func (e *Engine) RunJob(ctx context.Context, jobID string) (Result, error) {
	start := time.Now()
	var res Result
	var tid, workload, existing string
	if err := e.DB.QueryRow(ctx, `SELECT tenant_id::text, workload, coalesce(snapshot_id::text,'') FROM backup_jobs WHERE id=$1::uuid`, jobID).Scan(&tid, &workload, &existing); err != nil {
		return res, err
	}
	t, err := source.LoadTenant(ctx, e.DB, tid)
	if err != nil {
		return res, err
	}
	src := e.Reg.For(t)
	snap, err := e.ensureSnapshot(ctx, jobID, tid, workload, existing)
	if err != nil {
		return res, err
	}

	token := ""
	if err := e.DB.QueryRow(ctx, `SELECT token FROM delta_state WHERE tenant_id=$1 AND workload=$2`, tid, workload).Scan(&token); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return res, err
	}

	newToken, err := src.Delta(ctx, t, workload, token, func(it source.Item) error {
		if it.Deleted {
			_, err := e.DB.Exec(ctx, `DELETE FROM snapshot_items WHERE snapshot_id=$1::uuid AND resource_id=$2`, snap, it.ID)
			return err
		}
		// Checkpoint: already captured at this exact version by a previous attempt -> skip.
		var have string
		if err := e.DB.QueryRow(ctx, `SELECT coalesce(meta->>'mod','') FROM snapshot_items WHERE snapshot_id=$1::uuid AND resource_id=$2`, snap, it.ID).Scan(&have); err == nil && have != "" && have == it.Mod {
			return nil
		}
		var data []byte
		if err := retry.Do(ctx, 3, func() (e2 error) { data, e2 = src.Fetch(ctx, t, workload, it); return }); err != nil {
			return err
		}
		sha, size, err := e.Vault.Put(ctx, tid, data)
		if err != nil {
			return err
		}
		meta := map[string]string{}
		for k, v := range it.Meta {
			meta[k] = v
		}
		meta["mod"] = it.Mod
		mj, _ := json.Marshal(meta)
		if _, err := e.DB.Exec(ctx, `INSERT INTO snapshot_items(snapshot_id,resource_id,name,sha256,size,meta) VALUES($1::uuid,$2,$3,$4,$5,$6)
			ON CONFLICT (snapshot_id,resource_id) DO UPDATE SET name=EXCLUDED.name, sha256=EXCLUDED.sha256, size=EXCLUDED.size, meta=EXCLUDED.meta`,
			snap, it.ID, it.Name, sha, size, mj); err != nil {
			return err
		}
		res.Changed++
		res.Bytes += size
		metrics.BackupBytes.Add(float64(size))
		if chaos.Hit("worker_crash") {
			log.Printf("chaos: simulating hard worker crash mid-job %s", jobID)
			os.Exit(137)
		}
		return nil
	})
	if err != nil {
		return res, err
	}

	// Seal the snapshot: manifest hash over (resource_id:sha256) in order.
	rows, err := e.DB.Query(ctx, `SELECT resource_id || ':' || sha256, size FROM snapshot_items WHERE snapshot_id=$1::uuid ORDER BY resource_id`, snap)
	if err != nil {
		return res, err
	}
	h := sha256.New()
	var count int
	var total int64
	for rows.Next() {
		var line string
		var sz int64
		if err := rows.Scan(&line, &sz); err != nil {
			rows.Close()
			return res, err
		}
		h.Write([]byte(line + "\n"))
		count++
		total += sz
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	tx, err := e.DB.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE snapshots SET status='complete', object_count=$2, total_bytes=$3, manifest_hash=$4, retention_until=$5 WHERE id=$1::uuid`,
		snap, count, total, hex.EncodeToString(h.Sum(nil)), time.Now().Add(e.Retention)); err != nil {
		return res, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO delta_state(tenant_id,workload,token) VALUES($1,$2,$3)
		ON CONFLICT (tenant_id,workload) DO UPDATE SET token=EXCLUDED.token, updated_at=now()`, tid, workload, newToken); err != nil {
		return res, err
	}
	if _, err = tx.Exec(ctx, `UPDATE tenants SET last_sync=now() WHERE id=$1`, tid); err != nil {
		return res, err
	}
	if err = tx.Commit(ctx); err != nil {
		return res, err
	}
	metrics.BackupDuration.Observe(time.Since(start).Seconds())
	return res, nil
}

// IsUnsupported lets callers distinguish "will never work" errors from transient ones.
func IsUnsupported(err error) bool { return err != nil && strings.Contains(err.Error(), source.ErrUnsupported.Error()) }
