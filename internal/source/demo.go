package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"m365rescue/internal/chaos"
	"m365rescue/internal/metrics"
)

// Demo is a synthetic Microsoft 365 tenant stored in PostgreSQL (table demo_live).
// It supports delta sync via a monotonically increasing sequence, like Graph delta tokens.
type Demo struct{ DB *pgxpool.Pool }

func (d *Demo) Workloads() []string { return []string{"exchange", "onedrive", "sharepoint", "teams"} }

func (d *Demo) Delta(ctx context.Context, t Tenant, workload, token string, fn func(Item) error) (string, error) {
	last, _ := strconv.ParseInt(token, 10, 64)
	rows, err := d.DB.Query(ctx, `SELECT resource_id, name, deleted, seq FROM demo_live WHERE tenant_id=$1 AND workload=$2 AND seq>$3 ORDER BY seq`, t.ID, workload, last)
	if err != nil {
		return "", err
	}
	var items []Item
	max := last
	for rows.Next() {
		var it Item
		var seq int64
		if err := rows.Scan(&it.ID, &it.Name, &it.Deleted, &seq); err != nil {
			rows.Close()
			return "", err
		}
		it.Mod = strconv.FormatInt(seq, 10)
		items = append(items, it)
		if seq > max {
			max = seq
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	for _, it := range items {
		if err := fn(it); err != nil {
			return "", err
		}
	}
	return strconv.FormatInt(max, 10), nil
}

func (d *Demo) Fetch(ctx context.Context, t Tenant, workload string, it Item) ([]byte, error) {
	for i := 0; i < 5 && chaos.Hit("graph_429"); i++ { // simulate Retry-After handling
		metrics.GraphThrottled.Inc()
		time.Sleep(150 * time.Millisecond)
	}
	if chaos.Hit("fetch_error") {
		return nil, fmt.Errorf("chaos: injected 503 from Graph for %s", it.ID)
	}
	var b []byte
	err := d.DB.QueryRow(ctx, `SELECT content FROM demo_live WHERE tenant_id=$1 AND workload=$2 AND resource_id=$3`, t.ID, workload, it.ID).Scan(&b)
	return b, err
}

func (d *Demo) Restore(ctx context.Context, t Tenant, workload string, it Item, data []byte) error {
	_, err := d.DB.Exec(ctx, `INSERT INTO demo_live(tenant_id,workload,resource_id,name,content,deleted,seq)
		VALUES($1,$2,$3,$4,$5,false,nextval('demo_seq'))
		ON CONFLICT (tenant_id,workload,resource_id) DO UPDATE
		SET content=EXCLUDED.content, name=EXCLUDED.name, deleted=false, seq=nextval('demo_seq')`,
		t.ID, workload, it.ID, it.Name, data)
	return err
}

func (d *Demo) Verify(ctx context.Context, t Tenant, workload string, it Item, sha string) (bool, error) {
	var b []byte
	var del bool
	if err := d.DB.QueryRow(ctx, `SELECT content, deleted FROM demo_live WHERE tenant_id=$1 AND workload=$2 AND resource_id=$3`, t.ID, workload, it.ID).Scan(&b, &del); err != nil {
		return false, err
	}
	s := sha256.Sum256(b)
	return !del && hex.EncodeToString(s[:]) == sha, nil
}

var seedNames = map[string]string{
	"onedrive":   "Documents/report-%04d.docx",
	"exchange":   "Inbox/Quarterly update %04d",
	"sharepoint": "Sites/Finance/Shared Documents/budget-%04d.xlsx",
	"teams":      "Teams/General/message-%04d",
}

// Seed inserts n synthetic items for a workload. Every 5th item shares identical content (to show dedup).
func (d *Demo) Seed(ctx context.Context, tenantID, workload string, n int) error {
	var have int
	if err := d.DB.QueryRow(ctx, `SELECT count(*) FROM demo_live WHERE tenant_id=$1 AND workload=$2`, tenantID, workload).Scan(&have); err != nil {
		return err
	}
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := have; i < have+n; i++ {
		var content string
		if i%5 == 0 {
			content = fmt.Sprintf("Shared boilerplate (%s). %s", workload, strings.Repeat("Lorem ipsum dolor sit amet. ", 150))
		} else {
			content = fmt.Sprintf("%s item %d\nowner: user%d@contoso.example\n%s", workload, i, r.Intn(25), strings.Repeat(fmt.Sprintf("payload-%d-%d ", i, r.Int()), 80))
		}
		_, err := d.DB.Exec(ctx, `INSERT INTO demo_live(tenant_id,workload,resource_id,name,content) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
			tenantID, workload, fmt.Sprintf("demo-%s-%05d", workload, i), fmt.Sprintf(seedNames[workload], i), []byte(content))
		if err != nil {
			return err
		}
	}
	return nil
}

// Attack simulates ransomware: deletes ~30% of items and "encrypts" (overwrites) ~25% of the rest.
func (d *Demo) Attack(ctx context.Context, tenantID string) (deleted, encrypted int64, err error) {
	tag, err := d.DB.Exec(ctx, `UPDATE demo_live SET deleted=true, seq=nextval('demo_seq') WHERE tenant_id=$1 AND deleted=false AND random()<0.3`, tenantID)
	if err != nil {
		return
	}
	deleted = tag.RowsAffected()
	tag, err = d.DB.Exec(ctx, `UPDATE demo_live SET content=convert_to('ENCRYPTED-BY-RANSOMWARE '||md5(random()::text),'UTF8'), seq=nextval('demo_seq') WHERE tenant_id=$1 AND deleted=false AND random()<0.25`, tenantID)
	if err != nil {
		return
	}
	encrypted = tag.RowsAffected()
	return
}
