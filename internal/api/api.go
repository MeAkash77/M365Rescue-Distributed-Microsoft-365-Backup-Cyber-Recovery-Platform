// Package api exposes the REST API, the embedded dashboard, Prometheus metrics and the backup scheduler.
package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"m365rescue/internal/app"
	"m365rescue/internal/chaos"
)

//go:embed web/index.html
var dashboard []byte

type Server struct{ d *app.Deps }

type handler func(w http.ResponseWriter, r *http.Request, actor string)

// RBAC: read = everyone, operate = backup/restore, admin = tenants/demo/chaos, audit = audit log.
var perms = map[string][]string{
	"read":    {"admin", "operator", "auditor"},
	"operate": {"admin", "operator"},
	"admin":   {"admin"},
	"audit":   {"admin", "auditor"},
}

func (s *Server) auth(perm string, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		role, ok := s.d.Cfg.APIKeys[r.Header.Get("X-API-Key")]
		if !ok {
			httpErr(w, 401, "missing or invalid X-API-Key")
			return
		}
		for _, p := range perms[perm] {
			if p == role {
				h(w, r, role)
				return
			}
		}
		httpErr(w, 403, "role "+role+" lacks permission "+perm)
	}
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeRaw(w http.ResponseWriter, code int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(b)
}

func (s *Server) audit(ctx context.Context, actor, action, tenant, detail, result string) {
	s.d.DB.Exec(ctx, `INSERT INTO audit_events(actor,action,tenant_id,detail,result) VALUES($1,$2,$3,$4,$5)`, actor, action, tenant, detail, result)
}

// list runs a SELECT and returns the rows as a JSON array (computed by PostgreSQL).
func (s *Server) list(w http.ResponseWriter, r *http.Request, q string, args ...any) {
	var b []byte
	if err := s.d.DB.QueryRow(r.Context(), `SELECT coalesce(jsonb_agg(t),'[]'::jsonb) FROM (`+q+`) t`, args...).Scan(&b); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeRaw(w, 200, b)
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}

func Run(ctx context.Context, d *app.Deps) error {
	s := &Server{d}
	if d.Cfg.ScheduleEvery > 0 {
		go s.scheduler(ctx)
	}
	srv := &http.Server{Addr: d.Cfg.Listen, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(c)
	}()
	log.Printf("api listening on %s", d.Cfg.Listen)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(dashboard)
	})
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.d.DB.Ping(r.Context()); err != nil {
			httpErr(w, 503, "db down")
			return
		}
		w.Write([]byte("ok"))
	})
	m.Handle("GET /metrics", promhttp.Handler())

	m.HandleFunc("GET /api/v1/stats", s.auth("read", s.stats))
	m.HandleFunc("GET /api/v1/tenants", s.auth("read", func(w http.ResponseWriter, r *http.Request, _ string) {
		s.list(w, r, `SELECT id, name, ms_tenant_id, demo, status, created_at, last_sync FROM tenants ORDER BY created_at DESC`)
	}))
	m.HandleFunc("POST /api/v1/tenants", s.auth("admin", s.createTenant))
	m.HandleFunc("DELETE /api/v1/tenants/{id}", s.auth("admin", s.deleteTenant))
	m.HandleFunc("POST /api/v1/tenants/{id}/backup", s.auth("operate", s.backup))

	m.HandleFunc("GET /api/v1/jobs", s.auth("read", func(w http.ResponseWriter, r *http.Request, _ string) {
		s.list(w, r, `SELECT j.id, t.name AS tenant, j.tenant_id, j.workload, j.status, j.attempts, j.items_changed, j.bytes, j.error, j.created_at, j.finished_at
			FROM backup_jobs j JOIN tenants t ON t.id=j.tenant_id ORDER BY j.created_at DESC LIMIT 60`)
	}))
	m.HandleFunc("GET /api/v1/snapshots", s.auth("read", func(w http.ResponseWriter, r *http.Request, _ string) {
		s.list(w, r, `SELECT id, tenant_id, workload, parent_id, status, created_at, object_count, total_bytes, manifest_hash, retention_until
			FROM snapshots WHERE ($1='' OR tenant_id::text=$1) ORDER BY created_at DESC LIMIT 100`, r.URL.Query().Get("tenant_id"))
	}))
	m.HandleFunc("GET /api/v1/snapshots/{id}/items", s.auth("read", func(w http.ResponseWriter, r *http.Request, _ string) {
		s.list(w, r, `SELECT resource_id, name, sha256, size FROM snapshot_items WHERE snapshot_id=$1::uuid ORDER BY resource_id LIMIT 500`, r.PathValue("id"))
	}))
	m.HandleFunc("GET /api/v1/snapshots/{id}/download", s.auth("operate", s.download))

	m.HandleFunc("GET /api/v1/restores", s.auth("read", func(w http.ResponseWriter, r *http.Request, _ string) {
		s.list(w, r, `SELECT r.id, t.name AS tenant, r.workload, r.snapshot_id, r.resource_id, r.status, r.objects_restored, r.objects_verified,
			r.checksum_failures, r.duration_ms, r.error, r.actor, r.created_at FROM restore_jobs r JOIN tenants t ON t.id=r.tenant_id ORDER BY r.created_at DESC LIMIT 30`)
	}))
	m.HandleFunc("POST /api/v1/restores", s.auth("operate", s.restore))

	m.HandleFunc("POST /api/v1/demo/{id}/seed", s.auth("admin", s.demoSeed))
	m.HandleFunc("POST /api/v1/demo/{id}/attack", s.auth("admin", s.demoAttack))
	m.HandleFunc("GET /api/v1/demo/{id}/live", s.auth("read", func(w http.ResponseWriter, r *http.Request, _ string) {
		s.list(w, r, `SELECT workload, count(*) FILTER (WHERE NOT deleted) AS live, count(*) FILTER (WHERE deleted) AS deleted,
			count(*) FILTER (WHERE convert_from(content,'UTF8') LIKE 'ENCRYPTED-BY-RANSOMWARE%' AND NOT deleted) AS encrypted
			FROM demo_live WHERE tenant_id=$1::uuid GROUP BY workload ORDER BY workload`, r.PathValue("id"))
	}))

	m.HandleFunc("GET /api/v1/chaos", s.auth("read", func(w http.ResponseWriter, _ *http.Request, _ string) { writeJSON(w, 200, chaos.Get()) }))
	m.HandleFunc("POST /api/v1/chaos", s.auth("admin", func(w http.ResponseWriter, r *http.Request, actor string) {
		var rates map[string]float64
		if err := decode(r, &rates); err != nil {
			httpErr(w, 400, "body must be {\"graph_429\":0.2,\"fetch_error\":0.1,\"storage_error\":0.1,\"worker_crash\":0.01}")
			return
		}
		chaos.Set(rates)
		s.audit(r.Context(), actor, "CHAOS_SET", "", fmt.Sprint(rates), "SUCCESS")
		writeJSON(w, 200, chaos.Get())
	}))
	m.HandleFunc("GET /api/v1/audit", s.auth("audit", func(w http.ResponseWriter, r *http.Request, _ string) {
		s.list(w, r, `SELECT id, ts, actor, action, tenant_id, detail, result FROM audit_events ORDER BY id DESC LIMIT 100`)
	}))
	return m
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request, _ string) {
	var b []byte
	err := s.d.DB.QueryRow(r.Context(), `SELECT jsonb_build_object(
		'tenants',(SELECT count(*) FROM tenants),
		'snapshots',(SELECT count(*) FROM snapshots WHERE status='complete'),
		'objects',(SELECT count(*) FROM objects),
		'logical_bytes',(SELECT coalesce(sum(size),0) FROM snapshot_items),
		'unique_bytes',(SELECT coalesce(sum(size),0) FROM objects),
		'stored_bytes',(SELECT coalesce(sum(stored_size),0) FROM objects),
		'jobs_total',(SELECT count(*) FROM backup_jobs),
		'jobs_success',(SELECT count(*) FROM backup_jobs WHERE status='success'),
		'jobs_failed',(SELECT count(*) FROM backup_jobs WHERE status='failed'),
		'jobs_retrying',(SELECT count(*) FROM backup_jobs WHERE status='retry'),
		'jobs_running',(SELECT count(*) FROM backup_jobs WHERE status='running'))`).Scan(&b)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	m["queue_depth"] = s.d.Queue.Depth(r.Context())
	writeJSON(w, 200, m)
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request, actor string) {
	var in struct {
		Name       string `json:"name"`
		MSTenantID string `json:"ms_tenant_id"`
		Demo       bool   `json:"demo"`
	}
	if err := decode(r, &in); err != nil || strings.TrimSpace(in.Name) == "" {
		httpErr(w, 400, "name is required")
		return
	}
	if !in.Demo && in.MSTenantID == "" {
		httpErr(w, 400, "ms_tenant_id is required for real tenants (or set demo=true)")
		return
	}
	var b []byte
	if err := s.d.DB.QueryRow(r.Context(), `INSERT INTO tenants(name,ms_tenant_id,demo) VALUES($1,$2,$3) RETURNING to_jsonb(tenants)`, in.Name, in.MSTenantID, in.Demo).Scan(&b); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	s.audit(r.Context(), actor, "TENANT_CREATE", "", in.Name, "SUCCESS")
	writeRaw(w, 201, b)
}

func (s *Server) deleteTenant(w http.ResponseWriter, r *http.Request, actor string) {
	tag, err := s.d.DB.Exec(r.Context(), `DELETE FROM tenants WHERE id=$1::uuid`, r.PathValue("id"))
	if err != nil || tag.RowsAffected() == 0 {
		httpErr(w, 404, "tenant not found")
		return
	}
	// Metadata is removed; WORM-locked objects in the bucket remain until retention expires (by design).
	s.audit(r.Context(), actor, "TENANT_DELETE", r.PathValue("id"), "", "SUCCESS")
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (s *Server) backup(w http.ResponseWriter, r *http.Request, actor string) {
	var in struct {
		Workloads []string `json:"workloads"`
	}
	_ = decode(r, &in) // body optional
	ids, err := s.d.EnqueueBackup(r.Context(), r.PathValue("id"), in.Workloads)
	if err != nil {
		s.audit(r.Context(), actor, "BACKUP_START", r.PathValue("id"), err.Error(), "FAILURE")
		httpErr(w, 400, err.Error())
		return
	}
	s.audit(r.Context(), actor, "BACKUP_START", r.PathValue("id"), strings.Join(in.Workloads, ","), "SUCCESS")
	writeJSON(w, 202, map[string]any{"job_ids": ids})
}

func (s *Server) restore(w http.ResponseWriter, r *http.Request, actor string) {
	var in struct {
		SnapshotID string `json:"snapshot_id"`
		ResourceID string `json:"resource_id"`
	}
	if err := decode(r, &in); err != nil || in.SnapshotID == "" {
		httpErr(w, 400, "snapshot_id is required (resource_id optional for single-item restore)")
		return
	}
	var id string
	err := s.d.DB.QueryRow(r.Context(), `INSERT INTO restore_jobs(tenant_id,snapshot_id,workload,resource_id,actor)
		SELECT tenant_id, id, workload, $2::text, $3::text FROM snapshots WHERE id=$1::uuid AND status='complete' RETURNING id::text`, in.SnapshotID, in.ResourceID, actor).Scan(&id)
	if err != nil {
		httpErr(w, 404, "completed snapshot not found")
		return
	}
	s.audit(r.Context(), actor, "RESTORE_SNAPSHOT", "", "snapshot="+in.SnapshotID+" resource="+in.ResourceID, "SUCCESS")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		s.d.Recovery.Run(ctx, id)
	}()
	writeJSON(w, 202, map[string]string{"restore_id": id})
}

func (s *Server) download(w http.ResponseWriter, r *http.Request, actor string) {
	rid := r.URL.Query().Get("resource_id")
	var tid, sha, name string
	err := s.d.DB.QueryRow(r.Context(), `SELECT s.tenant_id::text, i.sha256, i.name FROM snapshot_items i JOIN snapshots s ON s.id=i.snapshot_id
		WHERE i.snapshot_id=$1::uuid AND i.resource_id=$2`, r.PathValue("id"), rid).Scan(&tid, &sha, &name)
	if err != nil {
		httpErr(w, 404, "item not found in snapshot")
		return
	}
	data, err := s.d.Vault.Get(r.Context(), tid, sha)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	s.audit(r.Context(), actor, "ITEM_DOWNLOAD", tid, rid, "SUCCESS")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", strings.ReplaceAll(name, "/", "_")))
	w.Write(data)
}

func (s *Server) demoSeed(w http.ResponseWriter, r *http.Request, actor string) {
	id := r.PathValue("id")
	var demo bool
	if err := s.d.DB.QueryRow(r.Context(), `SELECT demo FROM tenants WHERE id=$1::uuid`, id).Scan(&demo); err != nil || !demo {
		httpErr(w, 400, "not a demo tenant")
		return
	}
	counts := map[string]int{"onedrive": 120, "exchange": 80, "sharepoint": 40, "teams": 40}
	var in map[string]int
	if decode(r, &in) == nil {
		for k, v := range in {
			if _, ok := counts[k]; ok && v >= 0 && v <= 20000 {
				counts[k] = v
			}
		}
	}
	for wl, n := range counts {
		if err := s.d.Reg.Demo.Seed(r.Context(), id, wl, n); err != nil {
			httpErr(w, 500, err.Error())
			return
		}
	}
	s.audit(r.Context(), actor, "DEMO_SEED", id, fmt.Sprint(counts), "SUCCESS")
	writeJSON(w, 200, counts)
}

func (s *Server) demoAttack(w http.ResponseWriter, r *http.Request, actor string) {
	id := r.PathValue("id")
	del, enc, err := s.d.Reg.Demo.Attack(r.Context(), id)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	s.audit(r.Context(), actor, "DEMO_RANSOMWARE_ATTACK", id, fmt.Sprintf("deleted=%d encrypted=%d", del, enc), "SUCCESS")
	writeJSON(w, 200, map[string]int64{"deleted": del, "encrypted": enc})
}

// scheduler enqueues backups for every active tenant. A Redis lock makes it safe with multiple API replicas.
func (s *Server) scheduler(ctx context.Context) {
	t := time.NewTicker(s.d.Cfg.ScheduleEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ok, err := s.d.Redis.SetNX(ctx, "m365rescue:scheduler:lock", "1", s.d.Cfg.ScheduleEvery/2).Result()
			if err != nil || !ok {
				continue
			}
			rows, err := s.d.DB.Query(ctx, `SELECT id::text FROM tenants WHERE status='active'`)
			if err != nil {
				continue
			}
			var ids []string
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
			for _, id := range ids {
				if _, err := s.d.EnqueueBackup(ctx, id, nil); err != nil {
					log.Printf("scheduler: tenant %s: %v", id, err)
				}
			}
			log.Printf("scheduler: enqueued backups for %d tenants", len(ids))
		}
	}
}
