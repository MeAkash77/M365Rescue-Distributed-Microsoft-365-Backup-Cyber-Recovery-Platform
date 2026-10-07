# M365Rescue

**Distributed Microsoft 365 Backup & Cyber-Recovery Platform**

Protect Exchange Online, OneDrive, SharePoint and Teams data with incremental Microsoft Graph delta synchronization,
distributed backup workers, encrypted & deduplicated storage, immutable (Object Lock) snapshots and point-in-time recovery
with integrity verification.

```
Microsoft 365 → Graph API → Distributed Workers → Dedup + Compress + Encrypt → Immutable S3 → Point-in-Time Recovery
```

> **Run it in two minutes — no Microsoft 365 tenant required.** A built-in *demo mode* simulates a tenant, a ransomware
> attack, and a verified recovery end to end.

---

## Table of contents
1. [Quick start](#quick-start)
2. [Guided demo](#guided-demo)
3. [Implementation status (read this)](#implementation-status-read-this)
4. [Architecture](#architecture)
5. [How it works](#how-it-works)
6. [Technology stack](#technology-stack)
7. [REST API](#rest-api)
8. [Configuration](#configuration)
9. [Security model](#security-model)
10. [Fault tolerance & failure injection](#fault-tolerance--failure-injection)
11. [Observability](#observability)
12. [Connecting a real Microsoft 365 tenant](#connecting-a-real-microsoft-365-tenant)
13. [Testing](#testing)
14. [Deployment (Kubernetes / AWS)](#deployment-kubernetes--aws)
15. [Benchmarking](#benchmarking)
16. [Project structure](#project-structure)
17. [Roadmap](#roadmap)
18. [Design decisions & trade-offs](#design-decisions--trade-offs)

---

## Quick start

**Prerequisites:** Docker with Compose v2 and internet access for the first build (Go modules + images are downloaded).

```bash
unzip M365Rescue.zip && cd M365Rescue
docker compose up --build        # or: make up
```

| Service | URL | Notes |
|---|---|---|
| Dashboard & API | http://localhost:8080 | API key `dev-admin-key` (pre-filled) |
| MinIO console | http://localhost:9001 | `minioadmin` / `minioadmin` — browse the encrypted objects |
| Prometheus | http://localhost:9090 | scrapes API + every worker |

Scale workers: `docker compose up -d --scale worker=5` (or `make scale`).
Stop: `docker compose down` (add `-v` to wipe all data).

> The first build runs `go mod tidy` inside Docker to resolve dependencies, so it needs network access.

## Guided demo

Open the dashboard and press **▶ Run full demo**. It will:

1. create a synthetic Microsoft 365 tenant,
2. seed it with OneDrive files, Exchange mail, SharePoint documents and Teams messages (some deliberately duplicated),
3. queue four backup jobs that your workers pick up in parallel,
4. seal four immutable snapshots,
5. simulate **ransomware** (≈30 % of items deleted, ≈25 % overwritten with garbage),
6. restore from the pre-attack snapshots and **verify** every object (SHA-256 against the manifest, plus read-back),
7. print restored / verified / checksum-failure counts.

Prefer the terminal? `make demo` (needs `curl` and `jq`) does the same.

Then try the failure-injection buttons (**Throttle + errors**, **Crash workers**), run a backup, and watch retries,
re-delivery and resume happen in the jobs table and in Prometheus.

---

## Implementation status (read this)

This repository is honest about what is built and what is not.

| Capability | Status |
|---|---|
| Multi-tenant model, REST API, embedded dashboard | ✅ implemented |
| Redis Streams queue, worker pool, DB leases + heartbeats, crash re-delivery | ✅ implemented |
| Incremental backup via delta tokens (demo source + Graph OneDrive/Exchange) | ✅ implemented |
| SHA-256 dedup, gzip, AES-256-GCM envelope encryption, per-tenant keys | ✅ implemented |
| S3/MinIO storage with Object Lock retention (WORM) | ✅ implemented |
| Snapshots with full manifests, manifest hash, point-in-time restore | ✅ implemented |
| Item-level and full-snapshot restore with verification | ✅ implemented |
| Idempotent writes, checkpoint/resume, retry/backoff, Graph throttling handling | ✅ implemented |
| Failure injection (429, 503, storage error, hard worker crash) | ✅ implemented |
| RBAC (admin/operator/auditor via API keys) and audit log | ✅ implemented |
| Prometheus metrics, Prometheus in Compose | ✅ implemented |
| Synthetic demo tenant with ransomware simulation | ✅ implemented |
| Kubernetes manifests + HPA, CI workflow | ✅ provided (reference, not deployed by me) |
| Real Graph backup: OneDrive + Exchange inbox | ⚠️ implemented, **not validated against a live tenant** |
| Real Graph restore (OneDrive PUT, mail re-create) | ⚠️ best effort, **not validated against a live tenant** |
| SharePoint and Teams on real tenants | ❌ not implemented (demo tenants simulate them) |
| Graph change-notification webhooks | ❌ not implemented (delta polling only) |
| Entra ID login / OIDC | ❌ not implemented (static API keys) |
| React frontend, OpenTelemetry traces, Grafana dashboards, Terraform | ❌ not implemented (plain HTML dashboard, Prometheus metrics) |
| AI recovery assistant / anomaly detection | ❌ not implemented |
| Backup policies table / per-policy retention | ❌ not implemented (global `RETENTION_DAYS`, optional global `SCHEDULE_EVERY`) |

**Verification note:** the code was written without access to a Go toolchain or network in the authoring environment,
so it has **not been compiled or run** by the author. Expect to fix small compile errors on first build (`make test` runs
`go vet` and the unit tests). If `docker compose up --build` fails, the compiler message will point at the exact line.

---

## Architecture

```
                       ┌────────────────────────────┐
                       │  Microsoft 365 / Demo tenant│
                       └──────────────┬─────────────┘
                                      │ Graph (delta, throttling-aware)
┌────────────┐   REST   ┌─────────────▼─────────────┐   job ids   ┌──────────────────┐
│ Dashboard  ├─────────►│  API  (+ scheduler, RBAC, │────────────►│  Redis Streams   │
│ (embedded) │          │  audit, restore, metrics)  │             │  consumer group  │
└────────────┘          └─────────────┬─────────────┘             └────────┬─────────┘
                                      │ SQL                                │ claim
                        ┌─────────────▼─────────────┐             ┌────────▼─────────┐
                        │ PostgreSQL                 │◄────────────┤ Worker 1..N      │
                        │ tenants • jobs • snapshots │  leases,    │ lease → delta →  │
                        │ manifests • objects • audit│  manifests  │ fetch → vault    │
                        └────────────────────────────┘             └────────┬─────────┘
                                                                            │ sha256 → dedup → gzip → AES-GCM
                                                                   ┌────────▼─────────┐
                                                                   │ S3 / MinIO       │
                                                                   │ Object Lock (WORM)│
                                                                   └──────────────────┘
```

Single Go binary, two roles: `m365rescue api` and `m365rescue worker`. The same image runs both.

## How it works

**Backup job lifecycle**

1. API (or scheduler) inserts a `backup_jobs` row and pushes its ID onto the Redis Stream.
2. A worker reads the message and atomically takes a **lease** in PostgreSQL (`status='running'`, `lease_until=now()+30s`,
   heartbeat every 8 s). Only one worker can hold a live lease.
3. The worker creates (or reuses, on retry) the job's **snapshot**, copying the parent snapshot's manifest forward.
4. It calls the source's **delta** API with the saved token and processes each changed item:
   deleted → remove from manifest; unchanged-since-checkpoint → skip; otherwise fetch → `vault.Put`.
5. `vault.Put`: SHA-256 → if `(tenant, sha256)` exists, reuse (dedup) → else gzip → AES-256-GCM envelope → S3 `PutObject`
   with retention → insert `objects` row (`ON CONFLICT DO NOTHING`).
6. Finalise: compute the **manifest hash**, mark the snapshot `complete`, persist the new delta token and `last_sync`
   in **one transaction**, mark the job `success`, ack the queue message.

**Restore**: pick a snapshot (point in time) → for each manifest entry decrypt + decompress + verify SHA-256 → write to the
source → read back and compare (demo source) → record `restored / verified / checksum_failures / duration`.
Restores are non-destructive: they recreate or overwrite items from the snapshot but never delete items created later.

**Encryption**: `master key → HMAC → tenant key → AES-256-GCM wraps a random per-object data key → AES-256-GCM encrypts data`.
Authenticated data = `tenantID:sha256`, so a blob cannot be replayed under another tenant or hash.

**Dedup scope**: per tenant (never across tenants — keeps keys and blast radius isolated). The dashboard "Dedup+compress ratio"
is `logical bytes across all snapshot manifests ÷ encrypted bytes actually stored`, so it also reflects incremental snapshots.

## Technology stack

Go 1.24 • PostgreSQL 16 (pgx) • Redis 7 Streams (go-redis) • MinIO / AWS S3 (minio-go, Object Lock) • Microsoft Graph (REST,
app-only OAuth2 client credentials) • Prometheus client • Docker / Compose • Kubernetes manifests • GitHub Actions • Trivy.

## REST API

All `/api/v1/*` routes require header `X-API-Key`. Roles: **admin** (everything), **operator** (backup, restore, read),
**auditor** (read + audit log).

| Method & path | Role | Purpose |
|---|---|---|
| `GET /healthz` · `GET /metrics` | – | liveness · Prometheus |
| `GET /api/v1/stats` | read | totals, dedup inputs, queue depth |
| `GET/POST /api/v1/tenants` | read / admin | list / create `{name, ms_tenant_id, demo}` |
| `DELETE /api/v1/tenants/{id}` | admin | remove tenant metadata (locked objects persist until retention ends) |
| `POST /api/v1/tenants/{id}/backup` | operate | enqueue backup `{workloads?: [...]}` → `202 {job_ids}` |
| `GET /api/v1/jobs` | read | recent backup jobs |
| `GET /api/v1/snapshots?tenant_id=` | read | snapshots |
| `GET /api/v1/snapshots/{id}/items` | read | manifest entries |
| `GET /api/v1/snapshots/{id}/download?resource_id=` | operate | decrypt & download one item |
| `GET/POST /api/v1/restores` | read / operate | list / start `{snapshot_id, resource_id?}` |
| `POST /api/v1/demo/{tenantId}/seed` | admin | seed synthetic data `{onedrive, exchange, sharepoint, teams}` counts |
| `POST /api/v1/demo/{tenantId}/attack` | admin | simulate ransomware |
| `GET /api/v1/demo/{tenantId}/live` | read | live/deleted/encrypted counts |
| `GET/POST /api/v1/chaos` | read / admin | failure-injection rates |
| `GET /api/v1/audit` | audit | audit events |

Example:
```bash
curl -H "X-API-Key: dev-admin-key" -H 'Content-Type: application/json' \
  -d '{"name":"Contoso","demo":true}' http://localhost:8080/api/v1/tenants
```

## Configuration

Environment variables (see `.env.example`; Compose passes sane dev defaults):

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | local postgres | PostgreSQL DSN |
| `REDIS_URL` | `redis://localhost:6379/0` | Redis |
| `S3_ENDPOINT` / `S3_BUCKET` / `S3_ACCESS_KEY` / `S3_SECRET_KEY` / `S3_USE_SSL` | MinIO defaults | object store |
| `S3_OBJECT_LOCK` | `true` | create bucket with Object Lock and set retention per object |
| `S3_RETENTION_MODE` | `GOVERNANCE` | `GOVERNANCE` or `COMPLIANCE` |
| `RETENTION_DAYS` | `30` | WORM retention for stored objects and snapshots |
| `ENCRYPTION_MASTER_KEY` | dev value | 64 hex chars (32 bytes) or any passphrase (hashed). **Change it.** |
| `API_KEYS` | dev keys | `role:key,role:key` |
| `AZURE_CLIENT_ID` / `AZURE_CLIENT_SECRET` | – | Entra app for real tenants |
| `SCHEDULE_EVERY` | `0` | e.g. `1h` to auto-enqueue backups for all tenants |
| `WORKER_CONCURRENCY` | `4` | parallel jobs per worker process |
| `MAX_ATTEMPTS` | `5` | retries before a job is marked failed |

## Security model

- **Least privilege Graph permissions** and app-only auth (see `docs/graph-setup.md`).
- **Envelope encryption** with per-tenant keys; plaintext never touches object storage.
- **Immutability**: S3 Object Lock retention on every stored version (bucket versioning is enabled automatically by Object Lock).
- **RBAC + audit log** for tenant creation, backups, restores, downloads, demo attacks and chaos changes.
- **Tenant isolation** by key derivation, object-key prefix, AAD binding and `tenant_id` on every row.
- See `docs/threat-model.md` for gaps. **Before production:** KMS-managed master key, Entra ID OIDC, TLS everywhere,
  Postgres RLS, COMPLIANCE-mode retention in a separate AWS account, secrets in a secret manager.

## Fault tolerance & failure injection

| Failure | What happens |
|---|---|
| Graph `429/503/504` | `Retry-After` honoured, else exponential backoff + jitter (≤ 8 tries); counted in `graph_throttled_total` |
| Transient fetch/storage errors | per-item retry (3×) then job-level retry (up to `MAX_ATTEMPTS`) |
| Worker killed mid-job | message stays pending → `XAUTOCLAIM` after 15 s → lease expires → another worker resumes from checkpoint |
| Duplicate delivery | finished jobs are dropped; live leases are left alone |
| Crash between S3 write and DB commit | deterministic key + `ON CONFLICT DO NOTHING` → replay converges, no duplicate metadata |
| Delta token safety | stored only when the snapshot is sealed, so a failed run never skips changes |

Inject faults at runtime (admin):
```bash
curl -X POST -H "X-API-Key: dev-admin-key" -d '{"graph_429":0.3,"fetch_error":0.1,"storage_error":0.1}' localhost:8080/api/v1/chaos
curl -X POST -H "X-API-Key: dev-admin-key" -d '{"worker_crash":0.02}' localhost:8080/api/v1/chaos   # hard os.Exit(137)
curl -X POST -H "X-API-Key: dev-admin-key" -d '{}' localhost:8080/api/v1/chaos                        # clear
```
Run a backup afterwards and watch attempts climb and the job still reach `success`.
(`worker_crash` relies on the Compose `restart: unless-stopped` policy to bring workers back.)

## Observability

Prometheus metrics (API on `:8080/metrics`, workers on `:9100/metrics`):
`backup_jobs_total{status}`, `backup_bytes_total`, `backup_duration_seconds`, `restore_duration_seconds`,
`graph_requests_total{code}`, `graph_throttled_total`, `worker_jobs_active`, `snapshot_objects_total{result="new|dedup"}`,
`restore_objects_total{result}`. Structured traces (OpenTelemetry) and Grafana dashboards are on the roadmap.

## Connecting a real Microsoft 365 tenant

Follow `docs/graph-setup.md` (register an Entra app, grant application permissions, admin consent), put the credentials in
`.env`, then `POST /api/v1/tenants {"name":"Acme","ms_tenant_id":"<directory-id>"}` and trigger a backup. Supported workloads
for real tenants: `onedrive`, `exchange` (inbox). Use a **test tenant** first — restore paths are unvalidated.

## Testing

```bash
make test          # go mod tidy && go vet ./... && go test ./...
```
Included unit tests: envelope encryption round-trip / tenant isolation / tamper detection; Graph client throttling retry
(`429` + `Retry-After`) and pagination / delta-link handling. End-to-end behaviour is exercised by the dashboard demo and
`scripts/demo.sh`. Integration tests with Testcontainers and Playwright UI tests are on the roadmap.

## Deployment (Kubernetes / AWS)

Reference manifests: `deployments/kubernetes/m365rescue.yaml` (API ×2, workers ×2 with HPA, config + secret wiring).
Target AWS layout: EKS for the services, RDS PostgreSQL, ElastiCache Redis, S3 (versioning + Object Lock) with IAM roles
for service accounts, Secrets Manager for credentials. Scale workers on queue depth with KEDA's Redis Streams scaler.

## Benchmarking

No benchmark numbers are claimed here. To produce real ones, seed a larger demo tenant
(`POST /api/v1/demo/{id}/seed {"onedrive":10000,"exchange":2000}`), time a full backup and a restore from the dashboard/API
(`backup_duration_seconds`, `restore_duration_seconds`, `duration_ms` on restores), vary `docker compose --scale worker=N`,
and read throughput from Prometheus. Only quote figures you measured on your own hardware.

## Project structure

```
cmd/m365rescue/        entrypoint: `api` | `worker`
internal/api/          REST API, RBAC, audit, scheduler, embedded dashboard (web/index.html)
internal/app/          dependency wiring, job enqueueing
internal/backup/       backup engine: delta → vault → snapshot sealing
internal/recovery/     restore + verification
internal/vault/        dedup, gzip, envelope encryption, object-store IO
internal/crypto/       AES-256-GCM envelope encryption (+ tests)
internal/storage/      S3/MinIO client with Object Lock
internal/graph/        Graph HTTP client: auth, pagination, delta, throttling (+ tests)
internal/source/       Source interface; Graph source; synthetic demo source
internal/queue/        Redis Streams queue with crash recovery
internal/worker/       job claim, leases, heartbeat, retry policy
internal/chaos/        failure injection
internal/retry/        backoff helpers
internal/metrics/      Prometheus metrics
internal/db/           schema + migrations
deployments/           kubernetes manifests, prometheus config
docs/                  architecture, threat model, Graph setup
scripts/demo.sh        CLI demo
```

## Roadmap

SharePoint (sites → document libraries) and Teams on real tenants • Graph change-notification webhooks to trigger delta runs •
Entra ID OIDC login • backup policies with per-policy retention and scheduling • React + TypeScript console •
OpenTelemetry tracing and Grafana dashboards • Testcontainers integration tests and Playwright E2E • Terraform for AWS •
KMS-backed key management and key rotation • Postgres row-level security • AI recovery assistant with human confirmation
and anomaly detection (mass deletion, failure spikes).

## Design decisions & trade-offs

- **Full manifest per snapshot** (copy-forward) makes point-in-time restore a single manifest read, at the cost of more
  `snapshot_items` rows; fine for this scale, switch to chunked/tree manifests for very large tenants.
- **Per-tenant dedup** sacrifices cross-tenant savings for key isolation.
- **Redis Streams + DB lease** rather than Kafka: simpler to run, still gives consumer groups, redelivery and horizontal scale.
- **Restore runs in the API process** (goroutine) for simplicity; move it onto the queue for large restores.
- **Retention is not extended on dedup hits**: an object referenced by newer snapshots keeps its original lock expiry.
  A production system should refresh retention on reuse.
- **Governance-mode lock by default** so you can clean up a dev bucket; use Compliance mode for real ransomware protection.

## License

MIT — see `LICENSE`.
