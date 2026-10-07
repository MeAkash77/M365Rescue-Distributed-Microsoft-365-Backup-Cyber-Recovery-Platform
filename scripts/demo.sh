#!/usr/bin/env bash
# CLI version of the guided demo: backup -> ransomware -> point-in-time restore -> verify. Needs curl + jq.
set -euo pipefail
API=${API:-http://localhost:8080/api/v1}; KEY=${KEY:-dev-admin-key}
c() { curl -fsS -H "X-API-Key: $KEY" -H 'Content-Type: application/json' "$@"; }
T=$(c -X POST "$API/tenants" -d '{"name":"Contoso CLI demo","demo":true}' | jq -r .id); echo "tenant $T"
c -X POST "$API/demo/$T/seed" | jq -c .
c -X POST "$API/tenants/$T/backup" | jq -c .
echo -n "waiting for backups"; until [ "$(c "$API/snapshots?tenant_id=$T" | jq '[.[]|select(.status=="complete")]|length')" -ge 4 ]; do echo -n .; sleep 2; done; echo
SNAPS=$(c "$API/snapshots?tenant_id=$T" | jq -r '.[]|select(.status=="complete")|.id')
echo "ATTACK:"; c -X POST "$API/demo/$T/attack" | jq -c .; c "$API/demo/$T/live" | jq -c .
for s in $SNAPS; do c -X POST "$API/restores" -d "{\"snapshot_id\":\"$s\"}" >/dev/null; done
sleep 8; echo "RESTORES:"; c "$API/restores" | jq -c '.[]|{workload,status,objects_restored,objects_verified,checksum_failures,duration_ms}'
echo "LIVE AFTER RESTORE:"; c "$API/demo/$T/live" | jq -c .
