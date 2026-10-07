// Package graph is a thin Microsoft Graph client: app-only auth, pagination, delta paging, and
// first-class handling of throttling (429/503/504 + Retry-After + exponential backoff).
package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"m365rescue/internal/metrics"
	"m365rescue/internal/retry"
)

type HTTPError struct {
	Code int
	Body string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("graph: HTTP %d: %.200s", e.Code, e.Body) }

type Client struct {
	HTTP       *http.Client
	BaseURL    string
	TokenURL   string // fmt with tenant id
	ClientID   string
	Secret     string
	MaxRetries int
	// Token overrides token acquisition (tests).
	Token func(ctx context.Context, tenant string) (string, error)

	mu     sync.Mutex
	tokens map[string]tok
}

type tok struct {
	v   string
	exp time.Time
}

func New(clientID, secret string) *Client {
	return &Client{
		HTTP:       &http.Client{Timeout: 60 * time.Second},
		BaseURL:    "https://graph.microsoft.com/v1.0",
		TokenURL:   "https://login.microsoftonline.com/%s/oauth2/v2.0/token",
		ClientID:   clientID,
		Secret:     secret,
		MaxRetries: 8,
		tokens:     map[string]tok{},
	}
}

func (c *Client) token(ctx context.Context, tenant string) (string, error) {
	if c.Token != nil {
		return c.Token(ctx, tenant)
	}
	if c.ClientID == "" || c.Secret == "" {
		return "", fmt.Errorf("AZURE_CLIENT_ID / AZURE_CLIENT_SECRET are not configured")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.tokens[tenant]; ok && time.Until(t.exp) > 2*time.Minute {
		return t.v, nil
	}
	form := url.Values{"client_id": {c.ClientID}, "client_secret": {c.Secret},
		"scope": {"https://graph.microsoft.com/.default"}, "grant_type": {"client_credentials"}}
	req, _ := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf(c.TokenURL, tenant), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", &HTTPError{resp.StatusCode, string(b)}
	}
	var r struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return "", err
	}
	c.tokens[tenant] = tok{r.AccessToken, time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)}
	return r.AccessToken, nil
}

func (c *Client) invalidate(tenant string) {
	c.mu.Lock()
	delete(c.tokens, tenant)
	c.mu.Unlock()
}

// Do performs a request. Throttling is treated as a normal operating condition, not a failure.
func (c *Client) Do(ctx context.Context, tenant, method, path string, body []byte, ctype string) ([]byte, error) {
	u := path
	if !strings.HasPrefix(path, "http") {
		u = c.BaseURL + path
	}
	for attempt := 0; ; attempt++ {
		tk, err := c.token(ctx, tenant)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tk)
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := c.HTTP.Do(req)

		var wait time.Duration
		if err != nil {
			wait = retry.Backoff(attempt) // network error
		} else {
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			metrics.GraphRequests.WithLabelValues(strconv.Itoa(resp.StatusCode)).Inc()
			switch {
			case resp.StatusCode < 300:
				return data, nil
			case resp.StatusCode == 401 && attempt == 0:
				c.invalidate(tenant)
				continue
			case resp.StatusCode == 429 || resp.StatusCode == 503 || resp.StatusCode == 504:
				metrics.GraphThrottled.Inc()
				wait = retry.ParseRetryAfter(resp.Header.Get("Retry-After"))
				if wait == 0 {
					wait = retry.Backoff(attempt)
				}
			default:
				return nil, &HTTPError{resp.StatusCode, string(data)}
			}
		}
		if attempt >= c.MaxRetries {
			return nil, fmt.Errorf("graph: giving up after %d attempts on %s %s (last error: %v)", attempt+1, method, path, err)
		}
		if err := retry.Sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

func (c *Client) Get(ctx context.Context, tenant, path string) ([]byte, error) {
	return c.Do(ctx, tenant, "GET", path, nil, "")
}

// Pages follows @odata.nextLink until exhausted and returns the final @odata.deltaLink (if any).
func (c *Client) Pages(ctx context.Context, tenant, start string, fn func([]json.RawMessage) error) (string, error) {
	next, delta := start, ""
	for next != "" {
		b, err := c.Get(ctx, tenant, next)
		if err != nil {
			return "", err
		}
		var p struct {
			Value []json.RawMessage `json:"value"`
			Next  string            `json:"@odata.nextLink"`
			Delta string            `json:"@odata.deltaLink"`
		}
		if err := json.Unmarshal(b, &p); err != nil {
			return "", err
		}
		if err := fn(p.Value); err != nil {
			return "", err
		}
		if p.Delta != "" {
			delta = p.Delta
		}
		next = p.Next
	}
	return delta, nil
}
