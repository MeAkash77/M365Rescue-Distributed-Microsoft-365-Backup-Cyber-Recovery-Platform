# Architecture notes

**Control plane** – API (REST + dashboard + scheduler) writes jobs to PostgreSQL and pushes job IDs to a Redis Stream.

**Data plane** – N stateless workers claim jobs (consumer group), take a DB lease (30 s, heartbeat every 8 s), run a delta sync,
and write objects through the vault: `sha256 → dedup check → gzip → AES-256-GCM envelope → S3/MinIO (Object Lock)`.

**Snapshots** – every completed job seals a snapshot whose manifest (`snapshot_items`) is the *full* state at that moment
(parent manifest carried forward, then delta applied). Point-in-time restore = read one manifest; no replay chain.

**Crash/retry semantics**
| Failure | Behaviour |
|---|---|
| Worker killed mid-job | Message stays pending → `XAUTOCLAIM` after 15 s → lease expired → another worker resumes; items already stored at the same `mod` marker are skipped (checkpoint) |
| Duplicate delivery | Lease check: finished job → dropped; live lease → left pending |
| S3 write OK, DB insert lost | Replay rewrites the same deterministic key; `ON CONFLICT DO NOTHING` converges |
| Graph 429/503/504 | `Retry-After` honoured, otherwise exponential backoff + jitter, up to 8 tries |
| Job keeps failing | After `MAX_ATTEMPTS` the job is marked `failed` and acknowledged |
| Delta token | Persisted only in the same transaction that seals the snapshot |

**Tenant isolation** – every table is keyed by `tenant_id`; each tenant has its own derived encryption key (HMAC of master key),
its own object-key prefix, and AEAD associated data binding blobs to `tenant:sha256`.
