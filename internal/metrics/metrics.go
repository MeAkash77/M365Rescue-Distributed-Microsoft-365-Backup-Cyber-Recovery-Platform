// Package metrics defines the Prometheus metrics exported by every process.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	JobsTotal       = promauto.NewCounterVec(prometheus.CounterOpts{Name: "backup_jobs_total", Help: "Backup jobs by outcome"}, []string{"status"})
	BackupBytes     = promauto.NewCounter(prometheus.CounterOpts{Name: "backup_bytes_total", Help: "Plaintext bytes ingested"})
	BackupDuration  = promauto.NewHistogram(prometheus.HistogramOpts{Name: "backup_duration_seconds", Help: "Backup job duration", Buckets: prometheus.ExponentialBuckets(0.05, 2, 14)})
	RestoreDuration = promauto.NewHistogram(prometheus.HistogramOpts{Name: "restore_duration_seconds", Help: "Restore job duration", Buckets: prometheus.ExponentialBuckets(0.05, 2, 14)})
	GraphRequests   = promauto.NewCounterVec(prometheus.CounterOpts{Name: "graph_requests_total", Help: "Graph HTTP responses by status"}, []string{"code"})
	GraphThrottled  = promauto.NewCounter(prometheus.CounterOpts{Name: "graph_throttled_total", Help: "Throttled/transient Graph responses (429/503/504, real or injected)"})
	WorkerActive    = promauto.NewGauge(prometheus.GaugeOpts{Name: "worker_jobs_active", Help: "Jobs currently executing"})
	ObjectsStored   = promauto.NewCounterVec(prometheus.CounterOpts{Name: "snapshot_objects_total", Help: "Objects by write result"}, []string{"result"})
	RestoreObjects  = promauto.NewCounterVec(prometheus.CounterOpts{Name: "restore_objects_total", Help: "Restored objects by result"}, []string{"result"})
)
