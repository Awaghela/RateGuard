package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/yourname/rateguard/internal/api"
	"github.com/yourname/rateguard/internal/config"
	"github.com/yourname/rateguard/internal/limiter"
	"github.com/yourname/rateguard/internal/metrics"
	"github.com/yourname/rateguard/internal/policy"
	"github.com/yourname/rateguard/internal/redisopt"
)

const redisPoolSize = 128

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Redis: holds the token buckets shared by every instance.
	ropts, err := redisopt.Options(cfg.RedisAddr, cfg.RedisURL, cfg.RedisPassword)
	if err != nil {
		return err
	}
	ropts.PoolSize = redisPoolSize
	ropts.MinIdleConns = redisPoolSize / 2
	ropts.DialTimeout = time.Second
	ropts.ReadTimeout = cfg.RedisTimeout
	ropts.WriteTimeout = cfg.RedisTimeout
	rdb := redis.NewClient(ropts)
	defer rdb.Close()

	// PostgreSQL: durable per-client policies. Retry so `docker compose up`
	// doesn't depend on container start order.
	pool, err := connectPostgres(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer pool.Close()

	m := metrics.New()
	store := policy.NewStore(pool,
		limiter.Policy{Capacity: cfg.DefaultCapacity, RefillPerSec: cfg.DefaultRefillPerSec},
		cfg.PolicyCacheTTL,
		func(source string) { m.PolicyLookups.WithLabelValues(source).Inc() },
		log,
	)
	if err := store.Migrate(ctx); err != nil {
		return err
	}

	lim := limiter.New(rdb, "rg:bucket")
	if err := warmRedis(ctx, rdb, lim, redisPoolSize/2); err != nil {
		return fmt.Errorf("warm redis: %w", err)
	}

	srv := api.New(api.Options{
		InstanceID:   cfg.InstanceID,
		AdminToken:   cfg.AdminToken,
		FailOpen:     cfg.FailOpen,
		RedisTimeout: cfg.RedisTimeout,
		RedisPing:    func(ctx context.Context) error { return rdb.Ping(ctx).Err() },
		Limiter:      lim,
		Policies:     store,
		Metrics:      m,
		Log:          log,
	})

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr, "instance", cfg.InstanceID,
			"default_capacity", cfg.DefaultCapacity, "default_refill_per_sec", cfg.DefaultRefillPerSec,
			"fail_open", cfg.FailOpen)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
	return nil
}

// warmRedis opens n Redis connections concurrently and preloads the Lua script
// before the server accepts traffic. Without this, a burst hitting a freshly
// started instance forces every request to dial its own connection, and those
// dials blow the hot-path timeout.
func warmRedis(ctx context.Context, rdb *redis.Client, lim *limiter.Limiter, n int) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := lim.Warm(ctx); err != nil {
		return err
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = rdb.Ping(ctx).Err() // in flight concurrently => distinct connections
		}()
	}
	wg.Wait()
	return nil
}

func connectPostgres(ctx context.Context, url string, log *slog.Logger) (*pgxpool.Pool, error) {
	var lastErr error
	for attempt := 1; attempt <= 30; attempt++ {
		pool, err := pgxpool.New(ctx, url)
		if err == nil {
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err = pool.Ping(pctx)
			cancel()
			if err == nil {
				return pool, nil
			}
			pool.Close()
		}
		lastErr = err
		log.Warn("waiting for postgres", "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, lastErr
}
