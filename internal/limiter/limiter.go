// Package limiter implements a distributed token-bucket rate limiter backed
// by Redis. All state transitions happen inside a single Lua script, which
// Redis executes atomically.
package limiter

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed token_bucket.lua
var tokenBucketSrc string

// Policy describes a client's quota.
type Policy struct {
	Capacity     int64   // maximum burst size, in tokens
	RefillPerSec float64 // sustained rate, in tokens per second
}

// Result is the outcome of a single Allow call.
type Result struct {
	Allowed      bool
	Limit        int64         // bucket capacity
	Remaining    int64         // whole tokens left after this call
	RetryAfter   time.Duration // time until this request would succeed; 0 if allowed
	NeverRefills bool          // policy has RefillPerSec == 0 and the bucket is too empty
}

// ErrCostExceedsCapacity is returned when a request could never succeed.
var ErrCostExceedsCapacity = errors.New("cost exceeds bucket capacity")

// Limiter is safe for concurrent use and is intended to be shared by every
// goroutine (and every service instance, via Redis) that guards a resource.
type Limiter struct {
	rdb    redis.Scripter
	script *redis.Script
	prefix string
}

// New creates a Limiter. Keys are stored as "<prefix>:<clientID>".
func New(rdb redis.Scripter, prefix string) *Limiter {
	return &Limiter{
		rdb:    rdb,
		script: redis.NewScript(tokenBucketSrc),
		prefix: prefix,
	}
}

// Warm preloads the Lua script into Redis so the first requests can use
// EVALSHA instead of paying a NOSCRIPT round-trip each.
func (l *Limiter) Warm(ctx context.Context) error {
	return l.script.Load(ctx, l.rdb).Err()
}

// Allow attempts to consume cost tokens from the client's bucket.
func (l *Limiter) Allow(ctx context.Context, clientID string, p Policy, cost int64) (Result, error) {
	if cost < 1 {
		cost = 1
	}
	if cost > p.Capacity {
		return Result{}, ErrCostExceedsCapacity
	}

	key := l.prefix + ":" + clientID
	// Script.Run tries EVALSHA and transparently falls back to EVAL on NOSCRIPT.
	raw, err := l.script.Run(ctx, l.rdb, []string{key},
		p.Capacity,
		strconv.FormatFloat(p.RefillPerSec, 'f', -1, 64),
		cost,
	).Slice()
	if err != nil {
		return Result{}, fmt.Errorf("token bucket script: %w", err)
	}
	if len(raw) != 3 {
		return Result{}, fmt.Errorf("token bucket script: unexpected reply length %d", len(raw))
	}

	allowed, _ := raw[0].(int64)
	tokensStr, _ := raw[1].(string)
	retryMs, _ := raw[2].(int64)

	tokens, err := strconv.ParseFloat(tokensStr, 64)
	if err != nil {
		return Result{}, fmt.Errorf("token bucket script: bad tokens %q: %w", tokensStr, err)
	}

	res := Result{
		Allowed:   allowed == 1,
		Limit:     p.Capacity,
		Remaining: int64(tokens), // floor: never advertise a token we don't fully have
	}
	switch {
	case retryMs < 0:
		res.NeverRefills = true
	case retryMs > 0:
		res.RetryAfter = time.Duration(retryMs) * time.Millisecond
	}
	return res, nil
}
