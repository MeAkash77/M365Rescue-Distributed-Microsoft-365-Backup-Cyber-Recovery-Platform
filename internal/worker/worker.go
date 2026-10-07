// Package worker consumes backup jobs from the queue with DB-backed leases for exactly-one-active-runner semantics.
package worker

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"m365rescue/internal/app"
	"m365rescue/internal/metrics"
)

type W struct{ d *app.Deps }

func Run(ctx context.Context, d *app.Deps) error {
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
		log.Println("worker metrics on :9100")
		_ = http.ListenAndServe(":9100", mux)
	}()
	host, _ := os.Hostname()
	log.Printf("worker %s started (concurrency=%d)", host, d.Cfg.WorkerConcurrency)
	w := &W{d}
	d.Queue.Run(ctx, host, d.Cfg.WorkerConcurrency, w.handle)
	return nil
}

func (w *W) handle(ctx context.Context, jobID string) (bool, error) {
	bg := context.Background()
	var attempts int
	// Claim + lock: only one worker can hold a live lease on a job.
	err := w.d.DB.QueryRow(ctx, `UPDATE backup_jobs SET status='running', attempts=attempts+1, lease_until=now()+interval '30 seconds', started_at=coalesce(started_at, now())
		WHERE id=$1::uuid AND (status IN ('queued','retry') OR (status='running' AND lease_until < now()))
		RETURNING attempts`, jobID).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		var st string
		if e := w.d.DB.QueryRow(ctx, `SELECT status FROM backup_jobs WHERE id=$1::uuid`, jobID).Scan(&st); e != nil {
			return true, nil // unknown job: drop
		}
		if st == "success" || st == "failed" {
			return true, nil // duplicate delivery of a finished job
		}
		return false, nil // another worker holds a live lease: leave pending, retry later
	}
	if err != nil {
		return false, err
	}

	metrics.WorkerActive.Inc()
	defer metrics.WorkerActive.Dec()
	hbCtx, stopHB := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(8 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				w.d.DB.Exec(bg, `UPDATE backup_jobs SET lease_until=now()+interval '30 seconds' WHERE id=$1::uuid`, jobID)
			}
		}
	}()

	log.Printf("job %s attempt %d started", jobID, attempts)
	res, runErr := w.d.Backup.RunJob(ctx, jobID)
	stopHB()

	if runErr == nil {
		w.d.DB.Exec(bg, `UPDATE backup_jobs SET status='success', error='', items_changed=$2, bytes=$3, finished_at=now(), lease_until=NULL WHERE id=$1::uuid`, jobID, res.Changed, res.Bytes)
		metrics.JobsTotal.WithLabelValues("success").Inc()
		log.Printf("job %s done: %d changed items, %d bytes", jobID, res.Changed, res.Bytes)
		return true, nil
	}
	if attempts >= w.d.Cfg.MaxAttempts {
		w.d.DB.Exec(bg, `UPDATE backup_jobs SET status='failed', error=$2, finished_at=now(), lease_until=NULL WHERE id=$1::uuid`, jobID, runErr.Error())
		metrics.JobsTotal.WithLabelValues("failed").Inc()
		return true, runErr
	}
	w.d.DB.Exec(bg, `UPDATE backup_jobs SET status='retry', error=$2, lease_until=now() WHERE id=$1::uuid`, jobID, runErr.Error())
	metrics.JobsTotal.WithLabelValues("retry").Inc()
	return false, runErr // not acked -> redelivered by XAUTOCLAIM
}
