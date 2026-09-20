# Deploy RateGuard on Railway (frontend and backend)

Everything runs on Railway, built straight from your GitHub repo. You end up with five services.

| Service | What it is | Public? |
|---|---|---|
| `Postgres` | Railway's PostgreSQL database | No |
| `Redis` | Railway's Redis database (it has a password; the app supports that through `REDIS_URL`) | No |
| `rateguard` | The Go API, 2 replicas, built from the root `Dockerfile` | No |
| `prometheus` | Metrics, built from `railway/prometheus.Dockerfile` | No |
| `web` | Caddy: the dashboard (frontend) plus a proxy to the API and metrics | **Yes** |

Names matter only where another service refers to them. The variable references below use `Postgres` and `Redis`; if you named yours differently, use your names (capitalisation counts). The names `rateguard` and `prometheus` must match exactly, because Caddy and Prometheus look them up at `rateguard.railway.internal` and `prometheus.railway.internal`.

## 1. Push the project to GitHub

```bash
cd rateguard
git init && git add . && git commit -m "RateGuard"
# create an EMPTY repo on github.com first, then:
git remote add origin https://github.com/<you>/rateguard.git
git branch -M main && git push -u origin main
```

## 2. Create the project and the two databases

1. Railway dashboard, **New Project**, **Deploy PostgreSQL**.
2. In the project, **+ New**, **Database**, **Add Redis**.
3. For both: **Settings**, **Networking**. If a public TCP proxy is listed, remove it.

## 3. Add the three services from your repo

For each one: **+ New**, **GitHub Repo**, pick your repo. Rename the service under **Settings** and leave **Root Directory empty**. Open **Variables**, add what is listed, then deploy.

**`rateguard`** (no Dockerfile variable: it uses the root Dockerfile)

- `ADMIN_TOKEN` = a long random secret. Make one with `openssl rand -hex 32` and keep a copy.
- `DATABASE_URL` = `${{Postgres.DATABASE_URL}}`
- `REDIS_URL` = `${{Redis.REDIS_URL}}`
- `FAIL_OPEN` = `false`
- Then **Settings**, **Scale (Replicas)** = **2**, and apply the staged change.
- Do not add a domain. If one was generated, delete it.

**`prometheus`**

- `RAILWAY_DOCKERFILE_PATH` = `railway/prometheus.Dockerfile`
- Do not add a domain.

**`web`**

- `RAILWAY_DOCKERFILE_PATH` = `railway/web.Dockerfile`
- `PORT` = `8080`
- **Settings**, **Networking**, **Generate Domain**, target port **8080**.

## 4. Check it worked

```bash
curl https://<your-web-domain>/healthz     # HTTP 200
```

Open `https://<your-web-domain>/`. It redirects to the dashboard. Paste your `ADMIN_TOKEN` into the token field, press **Run demo**, and confirm: green "Connected" badge, **Servers up: 2 of 2**, and two different names under "Answered by".

Every push to `main` redeploys automatically.

## Troubleshooting

- **`NOAUTH Authentication required` in the `rateguard` logs.** The app is still using an address without a password. Check that `REDIS_URL` is set on the `rateguard` service and that the deployed code includes `internal/redisopt` (push the latest code).
- **`REDIS_URL is not a valid redis:// or rediss:// URL`.** The variable reference did not resolve. Open the Redis service, **Variables**, and check the name; the reference must be `${{<Redis service name>.REDIS_URL}}`.
- **`rateguard` connects to `localhost` or `[::1]`.** No Redis variable reached the service. Add `REDIS_URL` and press **Deploy**, since Railway stages variable changes.
- **502 or "can't reach the API".** Open the `rateguard` logs. The service must be named exactly `rateguard` and both replicas must be running.
- **"Servers up" is empty or 0 of 0.** Prometheus found no targets. In `railway/prometheus.yml` change `type: A` to `type: AAAA` (older Railway environments are IPv6-only), commit, push.
- **Only one server ever answers.** The replica count is not 2 (your plan may limit replicas).
- **"Dockerfile does not exist".** Root Directory must be empty. If it persists, try a leading slash: `/railway/web.Dockerfile`.
- **"Admin token was rejected".** Paste the exact value of `ADMIN_TOKEN` from the `rateguard` variables.

## Safety and cost

- Only `web` has a public domain. Never add a TCP proxy to Redis or Postgres.
- `/v1/check` and the dashboard charts are public; the policy API needs your token. Do not share the token.
- Five always-on services use paid resources. Check **Usage** after a few days, and delete the project when you no longer need it.

## What was and was not verified

Verified: the Redis password handling was tested against a real password-protected Redis (it reproduces `NOAUTH` with an address alone and connects with `REDIS_URL`, including running a Lua script); `railway/Caddyfile` passes Caddy 2.9.1 validation; `railway/prometheus.yml` passes `promtool`; the dashboard renames servers correctly when names are random hostnames. Not verified: a full Railway deployment, and compiling the whole Go project (only the new `redisopt` package was compiled and tested in isolation). Replica discovery through `rateguard.railway.internal` follows Railway's documentation and forum answers.
