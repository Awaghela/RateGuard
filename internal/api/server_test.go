package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/yourname/rateguard/internal/api"
	"github.com/yourname/rateguard/internal/limiter"
	"github.com/yourname/rateguard/internal/metrics"
	"github.com/yourname/rateguard/internal/policy"
)

// fakeStore is an in-memory PolicyStore.
type fakeStore struct{ def limiter.Policy }

func (f fakeStore) Get(context.Context, string) limiter.Policy { return f.def }
func (f fakeStore) Upsert(_ context.Context, r policy.Record) (policy.Record, error) {
	return r, nil
}
func (f fakeStore) GetRecord(context.Context, string) (policy.Record, error) {
	return policy.Record{}, policy.ErrNotFound
}
func (f fakeStore) Delete(context.Context, string) error { return policy.ErrNotFound }
func (f fakeStore) List(context.Context, int, int) ([]policy.Record, error) {
	return []policy.Record{}, nil
}
func (f fakeStore) Ping(context.Context) error { return nil }

func newServer(t *testing.T, addr string, failOpen bool, def limiter.Policy) (*httptest.Server, *redis.Client) {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{
		Addr: addr, DialTimeout: 100 * time.Millisecond,
		ReadTimeout: 100 * time.Millisecond, WriteTimeout: 100 * time.Millisecond,
		MaxRetries: -1,
	})
	t.Cleanup(func() { _ = rdb.Close() })

	s := api.New(api.Options{
		InstanceID:   "test-1",
		FailOpen:     failOpen,
		RedisTimeout: 200 * time.Millisecond,
		Limiter:      limiter.New(rdb, fmt.Sprintf("rgtest:%d", rand.Int63())),
		Policies:     fakeStore{def: def},
		Metrics:      metrics.New(),
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, rdb
}

func requireRedis(t *testing.T) string {
	t.Helper()
	addr := "localhost:6379"
	c := redis.NewClient(&redis.Options{Addr: addr})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not available: %v", err)
	}
	return addr
}

func post(t *testing.T, url, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Post(url+"/v1/check", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestCheckAllowsThenDenies(t *testing.T) {
	ts, _ := newServer(t, requireRedis(t), false, limiter.Policy{Capacity: 3, RefillPerSec: 0})

	for i := 0; i < 3; i++ {
		resp, body := post(t, ts.URL, `{"client_id":"acme"}`)
		if resp.StatusCode != 200 || body["allowed"] != true {
			t.Fatalf("request %d: status=%d body=%v", i, resp.StatusCode, body)
		}
		if got := resp.Header.Get("X-RateLimit-Remaining"); got != fmt.Sprint(2-i) {
			t.Fatalf("request %d: X-RateLimit-Remaining=%q, want %d", i, got, 2-i)
		}
		if resp.Header.Get("X-RateGuard-Instance") != "test-1" {
			t.Fatal("missing X-RateGuard-Instance header")
		}
	}

	resp, body := post(t, ts.URL, `{"client_id":"acme"}`)
	if resp.StatusCode != http.StatusTooManyRequests || body["allowed"] != false {
		t.Fatalf("4th request: status=%d body=%v", resp.StatusCode, body)
	}
	// Zero-refill policy: no Retry-After is meaningful.
	if resp.Header.Get("Retry-After") != "" {
		t.Fatalf("unexpected Retry-After for zero-refill policy: %q", resp.Header.Get("Retry-After"))
	}
}

func TestDeniedResponseHasRetryAfter(t *testing.T) {
	ts, _ := newServer(t, requireRedis(t), false, limiter.Policy{Capacity: 1, RefillPerSec: 1})

	post(t, ts.URL, `{"client_id":"x"}`)
	resp, body := post(t, ts.URL, `{"client_id":"x"}`)
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q, want 1", resp.Header.Get("Retry-After"))
	}
	if ms, _ := body["retry_after_ms"].(float64); ms <= 0 || ms > 1000 {
		t.Fatalf("retry_after_ms = %v, want (0,1000]", body["retry_after_ms"])
	}
}

func TestValidation(t *testing.T) {
	ts, _ := newServer(t, requireRedis(t), false, limiter.Policy{Capacity: 5, RefillPerSec: 1})

	for name, body := range map[string]string{
		"bad json":       `{`,
		"missing client": `{"cost":1}`,
		"bad chars":      `{"client_id":"a b/c"}`,
		"negative cost":  `{"client_id":"a","cost":-1}`,
		"cost>capacity":  `{"client_id":"a","cost":6}`,
	} {
		resp, _ := post(t, ts.URL, body)
		if resp.StatusCode != 400 {
			t.Errorf("%s: status = %d, want 400", name, resp.StatusCode)
		}
	}
}

func TestGuardMiddleware(t *testing.T) {
	ts, _ := newServer(t, requireRedis(t), false, limiter.Policy{Capacity: 2, RefillPerSec: 0})
	get := func(client string) int {
		req, _ := http.NewRequest("GET", ts.URL+"/v1/resource", nil)
		if client != "" {
			req.Header.Set("X-Client-ID", client)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if get("") != 400 {
		t.Fatal("missing X-Client-ID should be 400")
	}
	if get("k") != 200 || get("k") != 200 {
		t.Fatal("first two requests should pass")
	}
	if get("k") != 429 {
		t.Fatal("third request should be limited")
	}
}

func TestRedisDownFailClosed(t *testing.T) {
	ts, _ := newServer(t, "127.0.0.1:1", false, limiter.Policy{Capacity: 5, RefillPerSec: 1})
	resp, _ := post(t, ts.URL, `{"client_id":"a"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when Redis is down and FAIL_OPEN=false", resp.StatusCode)
	}
}

func TestRedisDownFailOpen(t *testing.T) {
	ts, _ := newServer(t, "127.0.0.1:1", true, limiter.Policy{Capacity: 5, RefillPerSec: 1})
	resp, body := post(t, ts.URL, `{"client_id":"a"}`)
	if resp.StatusCode != 200 || body["degraded"] != true {
		t.Fatalf("status=%d body=%v; want 200 + degraded=true when FAIL_OPEN=true", resp.StatusCode, body)
	}
}
