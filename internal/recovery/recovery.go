// Package recovery restores snapshot items to the source and verifies integrity.
package recovery

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"m365rescue/internal/metrics"
	"m365rescue/internal/source"
	"m365rescue/internal/vault"
)

type Engine struct {
	DB    *pgxpool.Pool
	Vault *vault.Vault
	Reg   source.Registry
}

// Run executes a restore job: for every manifest entry -> fetch + decrypt + checksum-verify -> write to source
// -> (when the source supports it) read back and compare against the manifest hash.
func (e *Engine) Run(ctx context.Context, restoreID string) {
	start := time.Now()
	var tid, snap, workload, resource string
	if err := e.DB.QueryRow(ctx, `SELECT tenant_id::text, snapshot_id::text, workload, resource_id FROM restore_jobs WHERE id=$1::uuid`, restoreID).Scan(&tid, &snap, &workload, &resource); err != nil {
		e.finish(restoreID, "failed", 0, 0, 0, start, err.Error())
		return
	}
	t, err := source.LoadTenant(ctx, e.DB, tid)
	if err != nil {
		e.finish(restoreID, "failed", 0, 0, 0, start, err.Error())
		return
	}
	src := e.Reg.For(t)

	rows, err := e.DB.Query(ctx, `SELECT resource_id, name, sha256, meta FROM snapshot_items WHERE snapshot_id=$1::uuid AND ($2='' OR resource_id=$2)`, snap, resource)
	if err != nil {
		e.finish(restoreID, "failed", 0, 0, 0, start, err.Error())
		return
	}
	type row struct {
		it  source.Item
		sha string
	}
	var items []row
	for rows.Next() {
		var r row
		var mj []byte
		if err := rows.Scan(&r.it.ID, &r.it.Name, &r.sha, &mj); err != nil {
			rows.Close()
			e.finish(restoreID, "failed", 0, 0, 0, start, err.Error())
			return
		}
		_ = json.Unmarshal(mj, &r.it.Meta)
		items = append(items, r)
	}
	rows.Close()

	var restored, verified, failures int
	lastErr := ""
	ver, canVerify := src.(source.Verifier)
	for _, r := range items {
		data, err := e.Vault.Get(ctx, tid, r.sha) // decrypts + verifies SHA-256 against the manifest
		if err != nil {
			failures++
			lastErr = err.Error()
			metrics.RestoreObjects.WithLabelValues("failed").Inc()
			continue
		}
		if err := src.Restore(ctx, t, workload, r.it, data); err != nil {
			failures++
			lastErr = err.Error()
			metrics.RestoreObjects.WithLabelValues("failed").Inc()
			continue
		}
		restored++
		if canVerify {
			ok, err := ver.Verify(ctx, t, workload, r.it, r.sha)
			if err != nil || !ok {
				failures++
				metrics.RestoreObjects.WithLabelValues("failed").Inc()
				continue
			}
		}
		verified++
		metrics.RestoreObjects.WithLabelValues("ok").Inc()
	}
	status := "complete"
	if failures > 0 {
		status = "failed"
	}
	e.finish(restoreID, status, restored, verified, failures, start, lastErr)
}

func (e *Engine) finish(id, status string, restored, verified, failures int, start time.Time, errMsg string) {
	d := time.Since(start)
	metrics.RestoreDuration.Observe(d.Seconds())
	e.DB.Exec(context.Background(), `UPDATE restore_jobs SET status=$2, objects_restored=$3, objects_verified=$4, checksum_failures=$5, duration_ms=$6, error=$7 WHERE id=$1::uuid`,
		id, status, restored, verified, failures, d.Milliseconds(), errMsg)
}
