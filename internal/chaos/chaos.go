// Package chaos provides runtime failure injection (toggle via POST /api/v1/chaos).
// Injection points: graph_429, fetch_error, storage_error, worker_crash.
package chaos

import (
	"math/rand"
	"sync"
)

var (
	mu    sync.RWMutex
	rates = map[string]float64{}
)

func Set(m map[string]float64) {
	mu.Lock()
	defer mu.Unlock()
	rates = map[string]float64{}
	for k, v := range m {
		if v > 0 {
			rates[k] = v
		}
	}
}

func Get() map[string]float64 {
	mu.RLock()
	defer mu.RUnlock()
	out := map[string]float64{}
	for k, v := range rates {
		out[k] = v
	}
	return out
}

// Hit reports whether the named fault should fire now.
func Hit(name string) bool {
	mu.RLock()
	r := rates[name]
	mu.RUnlock()
	return r > 0 && rand.Float64() < r
}
