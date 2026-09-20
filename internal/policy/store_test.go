package policy_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/rateguard/internal/limiter"
	"github.com/yourname/rateguard/internal/policy"
)

var def = limiter.Policy{Capacity: 100, RefillPerSec: 50}

func newStore(t *testing.T, ttl time.Duration) (*policy.Store, map[string]int) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://rateguard:rateguard@localhost:5432/rateguard?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		t.Skipf("postgres not available: %v", err)
	}
	t.Cleanup(pool.Close)

	var mu sync.Mutex
	sources := map[string]int{}
	s := policy.NewStore(pool, def, ttl, func(src string) {
		mu.Lock()
		sources[src]++
		mu.Unlock()
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s, sources
}

func uniqueID() string { return fmt.Sprintf("test-%d", rand.Int63()) }

func TestUpsertGetDelete(t *testing.T) {
	s, _ := newStore(t, time.Minute)
	ctx := context.Background()
	id := uniqueID()
	t.Cleanup(func() { _ = s.Delete(ctx, id) })

	if got := s.Get(ctx, id); got != def {
		t.Fatalf("unknown client should get default, got %+v", got)
	}

	if _, err := s.Upsert(ctx, policy.Record{ClientID: id, Capacity: 7, RefillPerSec: 2.5}); err != nil {
		t.Fatal(err)
	}
	// Upsert must invalidate this instance's cache (which held the default).
	if got := s.Get(ctx, id); got.Capacity != 7 || got.RefillPerSec != 2.5 {
		t.Fatalf("after upsert got %+v", got)
	}

	rec, err := s.GetRecord(ctx, id)
	if err != nil || rec.Capacity != 7 {
		t.Fatalf("GetRecord = %+v, %v", rec, err)
	}

	if err := s.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := s.Get(ctx, id); got != def {
		t.Fatalf("after delete should fall back to default, got %+v", got)
	}
	if err := s.Delete(ctx, id); err != policy.ErrNotFound {
		t.Fatalf("second delete err = %v, want ErrNotFound", err)
	}
}

func TestCachingAndTTL(t *testing.T) {
	s, sources := newStore(t, 100*time.Millisecond)
	ctx := context.Background()
	id := uniqueID()

	s.Get(ctx, id) // miss -> DB (no row -> default), then cached
	s.Get(ctx, id) // cache
	s.Get(ctx, id) // cache
	if sources[policy.SourceCache] != 2 {
		t.Fatalf("cache hits = %d, want 2 (sources=%v)", sources[policy.SourceCache], sources)
	}

	time.Sleep(150 * time.Millisecond)
	s.Get(ctx, id) // expired -> reload
	if sources[policy.SourceDefault] != 2 {
		t.Fatalf("expected 2 DB loads resolving to default, sources=%v", sources)
	}
}

func TestValidation(t *testing.T) {
	s, _ := newStore(t, time.Minute)
	for name, r := range map[string]policy.Record{
		"empty id":      {ClientID: "", Capacity: 1},
		"zero cap":      {ClientID: "a", Capacity: 0},
		"negative rate": {ClientID: "a", Capacity: 1, RefillPerSec: -1},
	} {
		if _, err := s.Upsert(context.Background(), r); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestList(t *testing.T) {
	s, _ := newStore(t, time.Minute)
	ctx := context.Background()
	id := uniqueID()
	t.Cleanup(func() { _ = s.Delete(ctx, id) })
	if _, err := s.Upsert(ctx, policy.Record{ClientID: id, Capacity: 3, RefillPerSec: 1}); err != nil {
		t.Fatal(err)
	}
	// Page through everything: other tests / load runs may have left many rows.
	found := false
	for offset := 0; !found; offset += 100 {
		recs, err := s.List(ctx, 100, offset)
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) == 0 {
			break
		}
		for _, r := range recs {
			if r.ClientID == id {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("upserted policy missing from List")
	}
}
