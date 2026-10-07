package graph

import (
	"encoding/json"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"m365rescue/internal/retry"
)

func TestThrottlingIsRetried(t *testing.T) {
	retry.Base = time.Millisecond
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			return
		}
		w.Write([]byte(`{"value":[]}`))
	}))
	defer srv.Close()
	c := New("id", "secret")
	c.BaseURL = srv.URL
	c.Token = func(context.Context, string) (string, error) { return "t", nil }
	if _, err := c.Get(context.Background(), "tenant", "/users"); err != nil {
		t.Fatalf("expected success after retries: %v", err)
	}
	if calls != 3 {
		t.Fatalf("want 3 calls, got %d", calls)
	}
}

func TestPaginationAndDeltaLink(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			w.Write([]byte(`{"value":[{"id":"b"}],"@odata.deltaLink":"` + srv.URL + `/delta?token=xyz"}`))
			return
		}
		w.Write([]byte(`{"value":[{"id":"a"}],"@odata.nextLink":"` + srv.URL + `/list?page=2"}`))
	}))
	defer srv.Close()
	c := New("id", "secret")
	c.BaseURL = srv.URL
	c.Token = func(context.Context, string) (string, error) { return "t", nil }
	n := 0
	link, err := c.Pages(context.Background(), "t", "/list", func(v []json.RawMessage) error { n += len(v); return nil })
	if err != nil || n != 2 || link == "" {
		t.Fatalf("n=%d link=%q err=%v", n, link, err)
	}
}
