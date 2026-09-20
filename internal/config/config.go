// Package config loads service configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr   string
	InstanceID string
	RedisAddr  string
	// REDIS_URL (redis:// or rediss://, password included) wins over REDIS_ADDR when set.
	RedisURL      string
	RedisPassword string
	DatabaseURL   string

	// Policy applied to any client without an explicit row in PostgreSQL.
	DefaultCapacity     int64
	DefaultRefillPerSec float64

	// How long an instance caches a client's policy before re-reading Postgres.
	PolicyCacheTTL time.Duration

	// If Redis is unreachable: true = allow traffic (availability),
	// false = reject with 503 (strictness).
	FailOpen bool

	// If set, /v1/policies* requires "Authorization: Bearer <token>".
	AdminToken string

	// Timeout for the Redis round-trip on the hot path.
	RedisTimeout time.Duration
}

func Load() (Config, error) {
	host, _ := os.Hostname()
	c := Config{
		HTTPAddr:            env("HTTP_ADDR", ":8080"),
		InstanceID:          env("INSTANCE_ID", host),
		RedisAddr:           env("REDIS_ADDR", "localhost:6379"),
		RedisURL:            env("REDIS_URL", ""),
		RedisPassword:       env("REDIS_PASSWORD", ""),
		DatabaseURL:         env("DATABASE_URL", "postgres://rateguard:rateguard@localhost:5432/rateguard?sslmode=disable"),
		AdminToken:          os.Getenv("ADMIN_TOKEN"),
		DefaultCapacity:     100,
		DefaultRefillPerSec: 50,
		PolicyCacheTTL:      5 * time.Second,
		RedisTimeout:        50 * time.Millisecond,
	}

	var err error
	if c.DefaultCapacity, err = envInt("DEFAULT_CAPACITY", c.DefaultCapacity); err != nil {
		return c, err
	}
	if c.DefaultRefillPerSec, err = envFloat("DEFAULT_REFILL_PER_SEC", c.DefaultRefillPerSec); err != nil {
		return c, err
	}
	if c.PolicyCacheTTL, err = envDuration("POLICY_CACHE_TTL", c.PolicyCacheTTL); err != nil {
		return c, err
	}
	if c.RedisTimeout, err = envDuration("REDIS_TIMEOUT", c.RedisTimeout); err != nil {
		return c, err
	}
	if c.FailOpen, err = envBool("FAIL_OPEN", false); err != nil {
		return c, err
	}
	if c.DefaultCapacity < 1 {
		return c, fmt.Errorf("DEFAULT_CAPACITY must be >= 1")
	}
	if c.DefaultRefillPerSec < 0 {
		return c, fmt.Errorf("DEFAULT_REFILL_PER_SEC must be >= 0")
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int64) (int64, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return n, nil
}

func envFloat(k string, def float64) (float64, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return f, nil
}

func envBool(k string, def bool) (bool, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", k, err)
	}
	return b, nil
}

func envDuration(k string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return d, nil
}
