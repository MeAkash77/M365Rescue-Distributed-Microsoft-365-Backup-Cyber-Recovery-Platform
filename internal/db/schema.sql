CREATE SEQUENCE IF NOT EXISTS demo_seq;

CREATE TABLE IF NOT EXISTS tenants (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL,
  ms_tenant_id text NOT NULL DEFAULT '',
  demo boolean NOT NULL DEFAULT false,
  status text NOT NULL DEFAULT 'active',
  created_at timestamptz NOT NULL DEFAULT now(),
  last_sync timestamptz
);

CREATE TABLE IF NOT EXISTS backup_jobs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  workload text NOT NULL,
  status text NOT NULL DEFAULT 'queued',
  attempts int NOT NULL DEFAULT 0,
  snapshot_id uuid,
  error text NOT NULL DEFAULT '',
  items_changed int NOT NULL DEFAULT 0,
  bytes bigint NOT NULL DEFAULT 0,
  lease_until timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  started_at timestamptz,
  finished_at timestamptz
);

CREATE TABLE IF NOT EXISTS snapshots (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  workload text NOT NULL,
  parent_id uuid,
  status text NOT NULL DEFAULT 'running',
  created_at timestamptz NOT NULL DEFAULT now(),
  object_count int NOT NULL DEFAULT 0,
  total_bytes bigint NOT NULL DEFAULT 0,
  manifest_hash text NOT NULL DEFAULT '',
  retention_until timestamptz
);

-- one physical object per (tenant, plaintext sha256): per-tenant deduplication
CREATE TABLE IF NOT EXISTS objects (
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  sha256 text NOT NULL,
  size bigint NOT NULL,
  stored_size bigint NOT NULL,
  storage_key text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, sha256)
);

-- each snapshot carries a full manifest (parent items carried forward + delta applied)
CREATE TABLE IF NOT EXISTS snapshot_items (
  snapshot_id uuid NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
  resource_id text NOT NULL,
  name text NOT NULL DEFAULT '',
  sha256 text NOT NULL,
  size bigint NOT NULL,
  meta jsonb NOT NULL DEFAULT '{}',
  PRIMARY KEY (snapshot_id, resource_id)
);

CREATE TABLE IF NOT EXISTS delta_state (
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  workload text NOT NULL,
  token text NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, workload)
);

CREATE TABLE IF NOT EXISTS restore_jobs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  snapshot_id uuid NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
  workload text NOT NULL,
  resource_id text NOT NULL DEFAULT '',
  status text NOT NULL DEFAULT 'running',
  objects_restored int NOT NULL DEFAULT 0,
  objects_verified int NOT NULL DEFAULT 0,
  checksum_failures int NOT NULL DEFAULT 0,
  duration_ms bigint NOT NULL DEFAULT 0,
  error text NOT NULL DEFAULT '',
  actor text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS audit_events (
  id bigserial PRIMARY KEY,
  ts timestamptz NOT NULL DEFAULT now(),
  actor text NOT NULL,
  action text NOT NULL,
  tenant_id text NOT NULL DEFAULT '',
  detail text NOT NULL DEFAULT '',
  result text NOT NULL DEFAULT 'SUCCESS'
);

-- "live" synthetic Microsoft 365 data used by demo tenants (ransomware simulation target)
CREATE TABLE IF NOT EXISTS demo_live (
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  workload text NOT NULL,
  resource_id text NOT NULL,
  name text NOT NULL,
  content bytea NOT NULL,
  deleted boolean NOT NULL DEFAULT false,
  seq bigint NOT NULL DEFAULT nextval('demo_seq'),
  PRIMARY KEY (tenant_id, workload, resource_id)
);
CREATE INDEX IF NOT EXISTS demo_live_seq ON demo_live (tenant_id, workload, seq);
CREATE INDEX IF NOT EXISTS snapshots_lookup ON snapshots (tenant_id, workload, status, created_at DESC);
