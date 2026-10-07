// Package retry implements exponential backoff with jitter.
package retry

import (
	"context"
	"math/rand"
	"strconv"
	"time"
)

// Base is the first backoff step (variable so tests can shrink it).
var Base = 500 * time.Millisecond

func Backoff(attempt int) time.Duration {
	if attempt > 6 {
		attempt = 6
	}
	d := Base << attempt
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1))
}

// ParseRetryAfter understands the delta-seconds form; returns 0 when absent/invalid.
func ParseRetryAfter(h string) time.Duration {
	if n, err := strconv.Atoi(h); err == nil && n > 0 {
		if n > 120 {
			n = 120
		}
		return time.Duration(n) * time.Second
	}
	return 0
}

func Sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// Do runs fn up to attempts times, backing off between failures.
func Do(ctx context.Context, attempts int, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		if i == attempts-1 {
			break
		}
		if e := Sleep(ctx, Backoff(i)); e != nil {
			return e
		}
	}
	return err
}
