# RateGuard

**A distributed API rate limiter written in Go.** It decides, for every request, whether a client is still inside its quota, and it keeps that decision consistent even when several server instances share the traffic.

**Stack:** Go · Redis (Lua) · PostgreSQL · Docker Compose · nginx · Prometheus · REST

---

## What it does, in plain words

Every API needs a way to stop one customer from hogging it. RateGuard gives each client a **bucket of tokens**:

- Every request costs one token.
- The bucket refills at a steady rate (say 5 tokens per second) up to a maximum size.
- If the bucket has a token, the request is **allowed** (`200`). If it is empty, the request is **rejected** (`429`) with a `Retry-After` hint.

That is the *token bucket* algorithm: it allows short bursts but caps the long-run average.

**The hard part** is running more than one server. If each server counted on its own, a client could get double the limit. RateGuard keeps every bucket in **Redis** and updates it with a **single atomic Lua script**, so two servers can never spend the same token. Each client's quota (bucket size and refill rate) is stored in **PostgreSQL**, so limits are configurable per client without redeploying.

## Features

- Token-bucket limiting with atomic Redis operations (no read-modify-write race)
- Per-client policies stored in PostgreSQL, managed through a REST API
- Consistent limits across multiple instances behind a load balancer
- Weighted requests (`cost`), `Retry-After` and `X-RateLimit-*` headers
- Explicit failure mode when Redis is down (fail closed or fail open)
- Prometheus metrics for every decision, plus a live dashboard
- Automated load tests that also verify no client ever exceeds its quota

## Architecture

```
            ┌────────────┐      ┌──────────────────┐
 clients ──▶│   nginx    │─────▶│  rateguard-1     │──┐
            │ round-robin│      └──────────────────┘  │  atomic Lua script      ┌───────┐
            │            │─────▶┌──────────────────┐  ├────────────────────────▶│ Redis │
            └────────────┘      │  rateguard-2     │──┤                         └───────┘
                                └──────────────────┘  │  policy lookups         ┌──────────┐
                                        ▲             └────(cached, 5 s)───────▶│ Postgres │
                             scrape /metrics                                     └──────────┘
                                   ┌────────────┐
                                   │ Prometheus │
                                   └────────────┘
```

| Component | Role |
|---|---|
| **nginx** | Front door. Round-robins requests across the instances and serves the dashboard. |
| **RateGuard instances** (Go) | Look up the client's policy, run the token-bucket script, answer allowed or rejected. |
| **Redis** | Shared state: one token bucket per client. |
| **PostgreSQL** | Source of truth for per-client policies. Not on the hot path (cached per instance). |
| **Prometheus** | Collects the decision metrics that feed the charts. |

### How one request flows

1. nginx forwards `POST /v1/check` to one of the instances.
2. The instance resolves the client's policy from its in-memory cache (PostgreSQL only on a cache miss).
3. It runs one Lua script in Redis: refill for the time that passed, take a token if there is one, return the result.
4. The instance answers `200` or `429` and records the decision in Prometheus.

### The guarantee

For any client over any time window `T`, the number of allowed requests is at most **`capacity + refill_rate × T`**. Example: bucket of 5, refill 1/s. Five quick requests pass, the sixth gets `429`, and after 3 seconds three more pass.

## Quick start

**Requirements:** Docker with Compose v2 and `make`. Go 1.22+ is only needed for `make test` and `make loadtest`.

```bash
go mod tidy          # once: creates go.sum (needed for make test / make loadtest)
make up              # redis, postgres, 2 instances, nginx, prometheus
```

Try it:

```bash
# give client "acme" a bucket of 5 that refills 1 token per second
curl -X PUT localhost:8080/v1/policies/acme \
  -H 'Authorization: Bearer dev-admin-token' \
  -d '{"capacity": 5, "refill_per_sec": 1}'

# send 8 requests: about five 200s, then 429s. Watch X-RateGuard-Instance alternate.
for i in $(seq 8); do
  curl -si -X POST localhost:8080/v1/check -d '{"client_id":"acme"}' \
    | grep -iE '^HTTP|x-rateguard-instance|x-ratelimit-remaining|retry-after' | paste -sd' '
done
```

Other commands: `make test` (unit and integration tests) · `make loadtest` · `make logs` · `make down` (**also deletes the Postgres volume**).

| Port | What |
|---|---|
| 8080 | nginx: API and dashboard |
| 8081 / 8082 | the two instances directly |
| 9090 | Prometheus UI |
| 5432 / 6379 | Postgres / Redis (development only) |

## Dashboard

Open **http://localhost:8080/dashboard/** after `make up`. It is a single static file (`deploy/dashboard/index.html`) with no build step.

- **Live request flow:** every request is an animated dot (client → nginx → server → Redis and back), teal if allowed, coral if rejected.
- **Run demo:** a narrated 50-second scenario (normal traffic, a burst, a noisy neighbor, recovery) with a scoreboard showing that one abusive client does not affect the others.
- **Try it and Test drive:** set a bucket size and refill rate, send requests, and watch the bucket drain. Five tasks tick themselves off as a real user works through them.
- **Guided checks:** three one-click tests, including one that proves two servers share a single limit.
- **Charts:** allowed and rejected per second, rejection rate, p95 decision time, load per server. Dark and light themes.

nginx also exposes Prometheus' read-only query API at `/prom/api/v1/` so the page can read metrics from the same origin. On localhost the admin-token field is pre-filled; on any other host you paste your `ADMIN_TOKEN`.

## API

| Method | Path | Description |
|---|---|---|
| `POST` | `/v1/check` | Body `{"client_id": "...", "cost": 1}`. **200** allowed, **429** rejected (with `Retry-After`), **503** if Redis is down and `FAIL_OPEN=false`. |
| `GET` | `/v1/resource` | Demo endpoint protected by the limiter; identifies the caller by the `X-Client-ID` header. |
| `PUT` | `/v1/policies/{client_id}` | Create or update a policy: `{"capacity": N, "refill_per_sec": R}`. Needs the admin token. |
| `GET` `DELETE` | `/v1/policies/{client_id}` | Read or remove a policy (the client falls back to the default). |
| `GET` | `/v1/policies` | List policies (`?limit=&offset=`). |
| `GET` | `/healthz` | Redis and Postgres reachability. |
| `GET` | `/metrics` | Prometheus metrics. |

Every response carries `X-RateGuard-Instance`. Check responses carry `X-RateLimit-Limit` and `X-RateLimit-Remaining`. Admin calls use `Authorization: Bearer <ADMIN_TOKEN>`.

## Configuration

Environment variables (defaults in brackets):

| Variable | Meaning |
|---|---|
| `HTTP_ADDR` (`:8080`) | Listen address |
| `INSTANCE_ID` (hostname) | Name reported in `X-RateGuard-Instance` |
| `REDIS_ADDR` (`localhost:6379`) | Redis host and port |
| `REDIS_URL` | Full Redis URL including the password, e.g. `redis://default:secret@host:6379` (`rediss://` for TLS). Wins over `REDIS_ADDR`. Hosted Redis, such as Railway's, provides this. |
| `REDIS_PASSWORD` | Password to use together with `REDIS_ADDR` |
| `DATABASE_URL` | PostgreSQL connection string |
| `ADMIN_TOKEN` | Bearer token for the policy API |
| `DEFAULT_CAPACITY` (`100`) / `DEFAULT_REFILL_PER_SEC` (`50`) | Policy for clients with none saved |
| `POLICY_CACHE_TTL` (`5s`) | How long an instance caches a policy |
| `REDIS_TIMEOUT` (`50ms`) | Hot-path limit on each Redis call |
| `FAIL_OPEN` (`false`) | If Redis is down: `false` rejects requests (503), `true` allows them and marks the response `degraded` |

## Metrics

| Metric | Labels | Meaning |
|---|---|---|
| `rateguard_decisions_total` | `result`: allowed, denied, error_allowed, error_denied | Every decision |
| `rateguard_decision_duration_seconds` | `result` | Policy lookup plus Redis round trip (histogram) |
| `rateguard_policy_lookups_total` | `source`: cache, db, default, stale | Where policies came from |
| `rateguard_redis_errors_total` | | Failed Redis calls |
| `rateguard_http_requests_total` | `route`, `code` | HTTP request counts |
| `rateguard_tokens_requested_total` | | Total token cost requested |

There is deliberately no per-client label: thousands of clients would create thousands of time series.

```promql
# rejection rate
sum(rate(rateguard_decisions_total{result="denied"}[1m])) / sum(rate(rateguard_decisions_total[1m]))
# p95 decision latency
histogram_quantile(0.95, sum by (le) (rate(rateguard_decision_duration_seconds_bucket[1m])))
```

## Testing and load testing

**Tests** (`make test`) cover the limiter (burst then reject, refill over time, capacity cap, client isolation, weighted cost, expiry), the HTTP layer (headers, validation, Redis-down behavior), and the policy store. The key one, `TestNeverOverAdmitsAcrossInstances`, fires 500 simultaneous requests from two independent Redis clients at a 50-token bucket and asserts that exactly 50 are admitted. Integration tests skip themselves if Redis or Postgres is not reachable.

**Load test** (`make loadtest`) sends 13,000 requests through nginx across three profiles and reports throughput, allowed and rejected counts, and p50/p95/p99 latency (measured by the client, end to end):

| Profile | Shape |
|---|---|
| `burst` | one client, 200 workers, unpaced: a thundering herd on one bucket |
| `sustained` | one client at 300 req/s against a 100/s refill for 10 s |
| `multi-client` | 100 clients with small buckets, unpaced |

It also **checks correctness**: for every client, admitted requests must not exceed `capacity + refill × elapsed`, and the tool exits non-zero if any client is over quota.

Example results from one run (two instances, Redis, Postgres and the load generator all sharing a single CPU core, so treat unpaced latency as a lower bound on what a real machine would show; run `make loadtest` and use your own numbers):

| Profile | Requests | Throughput | Rejected | p95 | Clients over quota |
|---|---|---|---|---|---|
| burst | 5,000 | ~10,900 req/s | 95.1% | 32.6 ms | 0 |
| sustained | 3,000 | 300 req/s (paced) | 60.0% | 0.40 ms | 0 |
| multi-client | 5,000 | ~11,300 req/s | 52.0% | 16.9 ms | 0 |

## Design decisions and trade-offs

- **Atomic Lua script.** Refill, check and spend happen in one step inside Redis, so concurrent instances cannot over-admit. Time comes from Redis (`TIME`), not the app, so clock skew between instances cannot matter.
- **Postgres off the hot path.** Policies are cached per instance. One goroutine refreshes an expired entry while others serve the stale value; if Postgres is down, stale or default policies keep limiting. Clients with no policy are cached too, and the cache is size-bounded.
- **Idle buckets expire.** Each key gets a TTL equal to the time to fully refill plus a margin, which bounds Redis memory.
- **Explicit failure mode.** `FAIL_OPEN` chooses availability or strictness when Redis is unreachable, and both outcomes are counted in metrics.
- **Warm start.** Each instance opens its Redis pool and preloads the script before serving. Without this, a burst on a fresh instance made requests dial new connections and about 0.5% timed out.
- **Known limits.** Redis is a single point of failure and a throughput ceiling (production would add replicas or Sentinel; keys are single-key operations, so sharding is possible). After a Redis failover a promoted replica may briefly over-admit, which is fine for API quotas but not for billing. Policy changes reach other instances within one cache TTL. Redis authentication and TLS are supported through `REDIS_URL` or `REDIS_PASSWORD`, but Redis should still sit on a private network. `/v1/check` has no authentication and is meant to be called by your own services.

## Deployment

**One server:** `docker-compose.prod.yml` is a hardened variant for a single server: automatic HTTPS (Caddy), Redis, Postgres and Prometheus on a private network with no published ports, secrets from `.env`, restart policies and memory limits. Step-by-step instructions are in [DEPLOY.md](DEPLOY.md). Do not put the development `docker-compose.yml` on the internet: it publishes Redis and Postgres and uses a well-known admin token.

**Railway:** to deploy straight from GitHub (dashboard and backend both), follow [RAILWAY.md](RAILWAY.md). The `railway/` folder holds the small Dockerfiles it uses.


## Project layout

```
cmd/rateguard/        service entrypoint (graceful shutdown, warm-up)
cmd/loadtest/         load generator and quota verifier
internal/limiter/     Lua token bucket, Go wrapper, concurrency tests
internal/policy/      Postgres store, policy cache, schema
internal/api/         HTTP handlers, limiter middleware, metrics wiring
internal/metrics/     Prometheus instruments
internal/redisopt/    Redis connection options (URL, password, TLS)
deploy/               nginx, Prometheus, Caddy configs and the dashboard
railway/              Dockerfiles and configs for the Railway deployment
```

## Possible next steps

Pub/Sub cache invalidation so policy changes apply instantly, a sliding-window algorithm option, CI running the tests and load test on every push, and Redis replicas with Sentinel for high availability.
