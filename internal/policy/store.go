// Package policy stores per-client rate-limit policies in PostgreSQL and
// serves them to the hot path through a small in-process cache.
//
// PostgreSQL is deliberately NOT on the per-request path except on cache
// misses: the limiter's throughput is bounded by Redis, not by a SQL query.
package policy

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/rateguard/internal/limiter"
)

//go:embed schema.sql
var schema string

// Record is a stored policy row.
type Record struct {
	ClientID     string    `json:"client_id"`
	Capacity     int64     `json:"capacity"`
	RefillPerSec float64   `json:"refill_per_sec"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Validate checks a record before it is written.
func (r Record) Validate() error {
	switch {
	case r.ClientID == "" || len(r.ClientID) > 128:
		return errors.New("client_id must be 1-128 characters")
	case r.Capacity < 1:
		return errors.New("capacity must be >= 1")
	case r.RefillPerSec < 0:
		return errors.New("refill_per_sec must be >= 0")
	}
	return nil
}

// ErrNotFound is returned when a policy row does not exist.
var ErrNotFound = errors.New("policy not found")

// Sources reported to the lookup observer.
const (
	SourceCache   = "cache"
	SourceDB      = "db"
	SourceDefault = "default" // no row for this client
	SourceStale   = "stale"   // DB unavailable; served an expired cache entry
)

const (
	maxCacheEntries = 50_000
	dbTimeout       = 250 * time.Millisecond
	errorBackoff    = time.Second // how long to reuse stale/default data after a DB error
)

type entry struct {
	policy     limiter.Policy
	expires    time.Time
	refreshing bool // one goroutine refreshes an expired entry; the rest serve stale
}

// Store reads and writes policies.
type Store struct {
	pool    *pgxpool.Pool
	def     limiter.Policy
	ttl     time.Duration
	observe func(source string)
	log     *slog.Logger

	mu    sync.Mutex
	cache map[string]*entry
}

// NewStore creates a Store. observe (optional) is called once per lookup with
// the source that satisfied it.
func NewStore(pool *pgxpool.Pool, def limiter.Policy, ttl time.Duration, observe func(string), log *slog.Logger) *Store {
	if observe == nil {
		observe = func(string) {}
	}
	return &Store{
		pool:    pool,
		def:     def,
		ttl:     ttl,
		observe: observe,
		log:     log,
		cache:   make(map[string]*entry),
	}
}

// Migrate creates the schema. An advisory lock makes it safe for several
// instances to start at once.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	const lockID = 727_001 // arbitrary app-wide constant
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockID); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lockID) }()

	_, err = conn.Exec(ctx, schema)
	return err
}

// Ping checks database connectivity.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Get returns the effective policy for a client. It never fails: if the
// database is down it serves stale or default data, because a policy-store
// outage should not take the limiter down with it.
func (s *Store) Get(ctx context.Context, clientID string) limiter.Policy {
	now := time.Now()

	s.mu.Lock()
	e, ok := s.cache[clientID]
	if ok && now.Before(e.expires) {
		p := e.policy
		s.mu.Unlock()
		s.observe(SourceCache)
		return p
	}
	if ok && e.refreshing {
		// Someone else is already refreshing; serve the stale value.
		p := e.policy
		s.mu.Unlock()
		s.observe(SourceStale)
		return p
	}
	if ok {
		e.refreshing = true
	}
	s.mu.Unlock()

	p, source, err := s.load(ctx, clientID)
	if err != nil {
		s.log.Warn("policy lookup failed; serving fallback", "client", clientID, "err", err)
		s.mu.Lock()
		defer s.mu.Unlock()
		if e, ok := s.cache[clientID]; ok {
			e.refreshing = false
			e.expires = now.Add(errorBackoff)
			s.observe(SourceStale)
			return e.policy
		}
		s.put(clientID, s.def, now.Add(errorBackoff))
		s.observe(SourceDefault)
		return s.def
	}

	s.mu.Lock()
	s.put(clientID, p, now.Add(s.ttl))
	s.mu.Unlock()
	s.observe(source)
	return p
}

// load reads one policy from Postgres, falling back to the default when no row exists.
func (s *Store) load(ctx context.Context, clientID string) (limiter.Policy, string, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	var p limiter.Policy
	err := s.pool.QueryRow(ctx,
		`SELECT capacity, refill_per_sec FROM policies WHERE client_id = $1`, clientID,
	).Scan(&p.Capacity, &p.RefillPerSec)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return s.def, SourceDefault, nil
	case err != nil:
		return limiter.Policy{}, "", err
	}
	return p, SourceDB, nil
}

// put inserts into the cache, bounding its size so a flood of random client
// IDs cannot exhaust memory. Caller must hold s.mu.
func (s *Store) put(clientID string, p limiter.Policy, expires time.Time) {
	if _, exists := s.cache[clientID]; !exists && len(s.cache) >= maxCacheEntries {
		now := time.Now()
		for k, v := range s.cache { // sweep expired entries
			if now.After(v.expires) {
				delete(s.cache, k)
			}
		}
		if len(s.cache) >= maxCacheEntries {
			return // still full: skip caching rather than grow unbounded
		}
	}
	s.cache[clientID] = &entry{policy: p, expires: expires}
}

func (s *Store) invalidate(clientID string) {
	s.mu.Lock()
	delete(s.cache, clientID)
	s.mu.Unlock()
}

// Upsert creates or updates a policy. Other instances pick up the change
// within the cache TTL.
func (s *Store) Upsert(ctx context.Context, r Record) (Record, error) {
	if err := r.Validate(); err != nil {
		return Record{}, err
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO policies (client_id, capacity, refill_per_sec)
		VALUES ($1, $2, $3)
		ON CONFLICT (client_id) DO UPDATE
		   SET capacity = EXCLUDED.capacity,
		       refill_per_sec = EXCLUDED.refill_per_sec,
		       updated_at = now()
		RETURNING updated_at`,
		r.ClientID, r.Capacity, r.RefillPerSec,
	).Scan(&r.UpdatedAt)
	if err != nil {
		return Record{}, fmt.Errorf("upsert policy: %w", err)
	}
	s.invalidate(r.ClientID)
	return r, nil
}

// GetRecord returns the stored row (not the effective/default policy).
func (s *Store) GetRecord(ctx context.Context, clientID string) (Record, error) {
	var r Record
	err := s.pool.QueryRow(ctx,
		`SELECT client_id, capacity, refill_per_sec, updated_at FROM policies WHERE client_id = $1`, clientID,
	).Scan(&r.ClientID, &r.Capacity, &r.RefillPerSec, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	return r, err
}

// Delete removes a policy; the client falls back to the default policy.
func (s *Store) Delete(ctx context.Context, clientID string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM policies WHERE client_id = $1`, clientID)
	if err != nil {
		return err
	}
	s.invalidate(clientID)
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// List returns stored policies ordered by client_id.
func (s *Store) List(ctx context.Context, limit, offset int) ([]Record, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT client_id, capacity, refill_per_sec, updated_at
		FROM policies ORDER BY client_id LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Record{}
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.ClientID, &r.Capacity, &r.RefillPerSec, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
