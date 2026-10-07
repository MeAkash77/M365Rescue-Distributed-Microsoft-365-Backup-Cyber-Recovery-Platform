// Package app wires every dependency together (used by both the api and worker processes).
package app

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"m365rescue/internal/backup"
	"m365rescue/internal/config"
	"m365rescue/internal/db"
	"m365rescue/internal/graph"
	"m365rescue/internal/queue"
	"m365rescue/internal/recovery"
	"m365rescue/internal/source"
	"m365rescue/internal/storage"
	"m365rescue/internal/vault"
)

type Deps struct {
	Cfg      config.Config
	DB       *pgxpool.Pool
	Redis    *redis.Client
	Queue    *queue.Queue
	Vault    *vault.Vault
	Reg      source.Registry
	Backup   *backup.Engine
	Recovery *recovery.Engine
}

func Build(ctx context.Context, cfg config.Config) (*Deps, error) {
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(ctx, pool); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	opt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, err
	}
	rdb := redis.NewClient(opt)
	for i := 0; ; i++ {
		if err = rdb.Ping(ctx).Err(); err == nil {
			break
		}
		if i >= 30 {
			return nil, fmt.Errorf("redis unavailable: %w", err)
		}
		time.Sleep(2 * time.Second)
	}
	q := &queue.Queue{R: rdb}
	if err := q.Init(ctx); err != nil {
		return nil, err
	}
	blobs, err := storage.New(ctx, cfg.S3Endpoint, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3UseSSL, cfg.S3Bucket, cfg.ObjectLock, cfg.RetentionMode)
	if err != nil {
		return nil, fmt.Errorf("object store: %w", err)
	}
	retention := time.Duration(cfg.RetentionDays) * 24 * time.Hour
	v := &vault.Vault{DB: pool, Blobs: blobs, Master: cfg.MasterKey, Retention: retention}
	reg := source.Registry{Demo: &source.Demo{DB: pool}, Graph: &source.Graph{C: graph.New(cfg.AzureClientID, cfg.AzureClientSecret)}}
	return &Deps{
		Cfg: cfg, DB: pool, Redis: rdb, Queue: q, Vault: v, Reg: reg,
		Backup:   &backup.Engine{DB: pool, Vault: v, Reg: reg, Retention: retention},
		Recovery: &recovery.Engine{DB: pool, Vault: v, Reg: reg},
	}, nil
}

// EnqueueBackup creates one job per workload and pushes it onto the queue.
func (d *Deps) EnqueueBackup(ctx context.Context, tenantID string, workloads []string) ([]string, error) {
	t, err := source.LoadTenant(ctx, d.DB, tenantID)
	if err != nil {
		return nil, fmt.Errorf("tenant not found")
	}
	supported := map[string]bool{}
	for _, w := range d.Reg.For(t).Workloads() {
		supported[w] = true
	}
	if len(workloads) == 0 {
		workloads = d.Reg.For(t).Workloads()
	}
	var ids []string
	for _, w := range workloads {
		if !supported[w] {
			return nil, fmt.Errorf("workload %q is not supported for this tenant", w)
		}
		var id string
		if err := d.DB.QueryRow(ctx, `INSERT INTO backup_jobs(tenant_id,workload) VALUES($1,$2) RETURNING id::text`, tenantID, w).Scan(&id); err != nil {
			return nil, err
		}
		if err := d.Queue.Enqueue(ctx, id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}
