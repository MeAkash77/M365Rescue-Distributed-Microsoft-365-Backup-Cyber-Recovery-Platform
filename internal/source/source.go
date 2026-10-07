// Package source abstracts "where the protected data lives": real Microsoft 365 via Graph, or a synthetic demo tenant.
package source

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrUnsupported = errors.New("workload not implemented for this source")

type Tenant struct {
	ID         string
	Name       string
	MSTenantID string
	Demo       bool
}

type Item struct {
	ID      string
	Name    string
	Deleted bool
	Mod     string // change marker (lastModified / sequence) used for checkpoint-skip
	Meta    map[string]string
}

type Source interface {
	Workloads() []string
	// Delta streams items changed since token and returns the new token (empty token = full sync).
	Delta(ctx context.Context, t Tenant, workload, token string, fn func(Item) error) (string, error)
	Fetch(ctx context.Context, t Tenant, workload string, it Item) ([]byte, error)
	Restore(ctx context.Context, t Tenant, workload string, it Item, data []byte) error
}

// Verifier is optionally implemented by sources that can read back restored data.
type Verifier interface {
	Verify(ctx context.Context, t Tenant, workload string, it Item, sha string) (bool, error)
}

type Registry struct {
	Demo  *Demo
	Graph *Graph
}

func (r Registry) For(t Tenant) Source {
	if t.Demo {
		return r.Demo
	}
	return r.Graph
}

func LoadTenant(ctx context.Context, db *pgxpool.Pool, id string) (Tenant, error) {
	var t Tenant
	err := db.QueryRow(ctx, `SELECT id::text, name, ms_tenant_id, demo FROM tenants WHERE id=$1::uuid`, id).Scan(&t.ID, &t.Name, &t.MSTenantID, &t.Demo)
	return t, err
}
