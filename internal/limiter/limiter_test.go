package limiter_test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/yourname/rateguard/internal/limiter"
)

func redisAddr() string {
	if a := os.Getenv("REDIS_ADDR"); a != "" {
		return a
	}
	return "localhost:6379"
}

// newClient returns a connected Redis client or skips the test.
func newClient(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: redisAddr(), PoolSize: 64})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not available at %s: %v", redisAddr(), err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// uniquePrefix isolates each test's keys so tests can run repeatedly and in parallel.
func uniquePrefix(t *testing.T) string {
	return fmt.Sprintf("rgtest:%s:%d", t.Name(), rand.Int63())
}

// The core correctness property: with no refill, exactly `capacity` requests
// are admitted no matter how many goroutines, and no matter how many
// independent "service instances" (separate Redis clients) race.
func TestNeverOverAdmitsAcrossInstances(t *testing.T) {
	const (
		capacity  = 50
		requests  = 500
		instances = 2
	)
	prefix := uniquePrefix(t)
	policy := limiter.Policy{Capacity: capacity, RefillPerSec: 0}

	// Two separate clients+limiters model two app instances sharing one Redis.
	limiters := make([]*limiter.Limiter, instances)
	for i := range limiters {
		limiters[i] = limiter.New(newClient(t), prefix)
	}

	var allowed, denied atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release everyone at once to maximise contention
			res, err := limiters[i%instances].Allow(context.Background(), "client-a", policy, 1)
			if err != nil {
				t.Errorf("Allow: %v", err)
				return
			}
			if res.Allowed {
				allowed.Add(1)
			} else {
				denied.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := allowed.Load(); got != capacity {
		t.Fatalf("allowed = %d, want exactly %d (denied=%d)", got, capacity, denied.Load())
	}
	if got := denied.Load(); got != requests-capacity {
		t.Fatalf("denied = %d, want %d", got, requests-capacity)
	}
}

func TestRefillOverTime(t *testing.T) {
	c := newClient(t)
	l := limiter.New(c, uniquePrefix(t))
	ctx := context.Background()
	p := limiter.Policy{Capacity: 5, RefillPerSec: 10} // 1 token / 100ms

	for i := 0; i < 5; i++ {
		if res, err := l.Allow(ctx, "c", p, 1); err != nil || !res.Allowed {
			t.Fatalf("initial burst request %d: allowed=%v err=%v", i, res.Allowed, err)
		}
	}
	res, err := l.Allow(ctx, "c", p, 1)
	if err != nil || res.Allowed {
		t.Fatalf("6th request should be denied, got allowed=%v err=%v", res.Allowed, err)
	}
	if res.RetryAfter <= 0 || res.RetryAfter > 150*time.Millisecond {
		t.Fatalf("RetryAfter = %v, want (0, 150ms]", res.RetryAfter)
	}

	time.Sleep(350 * time.Millisecond) // ~3.5 tokens

	var got int
	for i := 0; i < 10; i++ {
		res, err := l.Allow(ctx, "c", p, 1)
		if err != nil {
			t.Fatal(err)
		}
		if res.Allowed {
			got++
		}
	}
	if got < 2 || got > 4 {
		t.Fatalf("after 350ms at 10/s expected ~3 tokens, got %d", got)
	}
}

func TestBucketNeverExceedsCapacity(t *testing.T) {
	c := newClient(t)
	l := limiter.New(c, uniquePrefix(t))
	ctx := context.Background()
	p := limiter.Policy{Capacity: 3, RefillPerSec: 50}

	if _, err := l.Allow(ctx, "c", p, 1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // uncapped, this would refill ~15 tokens

	start := time.Now()
	var got int
	for i := 0; i < 10; i++ {
		res, err := l.Allow(ctx, "c", p, 1)
		if err != nil {
			t.Fatal(err)
		}
		if res.Allowed {
			got++
		}
	}
	elapsed := time.Since(start)

	// The real invariant: capacity, plus whatever legitimately refilled while
	// the loop ran (+1 for millisecond rounding). Independent of machine speed.
	maxAllowed := float64(p.Capacity) + elapsed.Seconds()*float64(p.RefillPerSec) + 1
	if float64(got) > maxAllowed {
		t.Fatalf("admitted %d, but capacity %v plus %v of refill allows at most %.1f",
			got, p.Capacity, elapsed, maxAllowed)
	}
}

func TestClientsAreIsolated(t *testing.T) {
	c := newClient(t)
	l := limiter.New(c, uniquePrefix(t))
	ctx := context.Background()
	p := limiter.Policy{Capacity: 2, RefillPerSec: 0}

	for i := 0; i < 2; i++ {
		if res, _ := l.Allow(ctx, "noisy", p, 1); !res.Allowed {
			t.Fatal("noisy client should have 2 tokens")
		}
	}
	if res, _ := l.Allow(ctx, "noisy", p, 1); res.Allowed {
		t.Fatal("noisy client should be exhausted")
	}
	if res, _ := l.Allow(ctx, "quiet", p, 1); !res.Allowed {
		t.Fatal("quiet client must not be affected by noisy client")
	}
}

func TestWeightedCostAndRemaining(t *testing.T) {
	c := newClient(t)
	l := limiter.New(c, uniquePrefix(t))
	ctx := context.Background()
	p := limiter.Policy{Capacity: 10, RefillPerSec: 0}

	res, err := l.Allow(ctx, "c", p, 4)
	if err != nil || !res.Allowed || res.Remaining != 6 {
		t.Fatalf("cost 4: %+v err=%v", res, err)
	}
	res, _ = l.Allow(ctx, "c", p, 7)
	if res.Allowed {
		t.Fatal("cost 7 with 6 tokens left must be denied")
	}
	if !res.NeverRefills {
		t.Fatal("zero-refill policy should report NeverRefills")
	}
	// A denied request must not consume tokens.
	res, _ = l.Allow(ctx, "c", p, 6)
	if !res.Allowed || res.Remaining != 0 {
		t.Fatalf("cost 6 after denial: %+v", res)
	}
}

func TestCostExceedsCapacity(t *testing.T) {
	c := newClient(t)
	l := limiter.New(c, uniquePrefix(t))
	_, err := l.Allow(context.Background(), "c", limiter.Policy{Capacity: 5, RefillPerSec: 1}, 6)
	if err != limiter.ErrCostExceedsCapacity {
		t.Fatalf("err = %v, want ErrCostExceedsCapacity", err)
	}
}

func TestIdleBucketsExpire(t *testing.T) {
	c := newClient(t)
	prefix := uniquePrefix(t)
	l := limiter.New(c, prefix)
	ctx := context.Background()

	if _, err := l.Allow(ctx, "c", limiter.Policy{Capacity: 10, RefillPerSec: 10}, 1); err != nil {
		t.Fatal(err)
	}
	ttl, err := c.PTTL(ctx, prefix+":c").Result()
	if err != nil {
		t.Fatal(err)
	}
	// capacity/rate = 1s, + 1s cushion.
	if ttl <= 0 || ttl > 2100*time.Millisecond {
		t.Fatalf("bucket TTL = %v, want in (0, ~2s]", ttl)
	}
}
