# Deploying RateGuard to a server

This puts the whole stack (two RateGuard servers, Redis, PostgreSQL, Prometheus, nginx) on one small
Linux server, with automatic HTTPS. It takes about 15 minutes.

> Written and checked without being able to run Docker, so do the "Check it worked" step and read
> the troubleshooting list if anything looks off. Configuration files were validated (compose parses,
> Caddy accepts the Caddyfile and behaves as described below), but the full stack was not started
> from these files.

## What you need

- A server running **Ubuntu 24.04** with **2 GB of RAM** (1 GB works if you add swap; building the Go
  image is the hungry part). Any VPS provider works. Typical small servers cost a few dollars a month;
  check current pricing, and delete the server when you no longer need it.
- A **domain name** you control (or a free hostname service). You create one `A` record pointing a
  name such as `rateguard.yourdomain.com` at the server's IP address.
- This project folder on your computer.

## Steps

**1. Point your domain at the server.** In your DNS settings add an `A` record for
`rateguard.yourdomain.com` with the server's IP. Wait a few minutes. HTTPS certificates cannot be
issued until this resolves.

**2. Log in and install Docker.**

```bash
ssh root@YOUR_SERVER_IP
curl -fsSL https://get.docker.com | sh
```

**3. Open only the ports you need.**

```bash
ufw allow OpenSSH && ufw allow 80/tcp && ufw allow 443/tcp && ufw allow 443/udp && ufw --force enable
```

Redis, PostgreSQL and Prometheus are not published to the host at all, so nothing else is reachable.

**4. Copy the project to the server.** From your computer:

```bash
scp rateguard.zip root@YOUR_SERVER_IP:~
ssh root@YOUR_SERVER_IP "apt-get install -y unzip && unzip -o rateguard.zip"
```

**5. Create your settings file.**

```bash
cd ~/rateguard
cp .env.example .env
sed -i "s/^ADMIN_TOKEN=.*/ADMIN_TOKEN=$(openssl rand -hex 32)/"        .env
sed -i "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=$(openssl rand -hex 24)/" .env
nano .env        # set DOMAIN=rateguard.yourdomain.com, then save
grep ADMIN_TOKEN .env   # copy this value somewhere safe: you will paste it into the dashboard
```

**6. Start everything.**

```bash
docker compose -f docker-compose.prod.yml up -d --build
docker compose -f docker-compose.prod.yml ps
```

The first build takes a few minutes. All services should show as running.

## Check it worked

```bash
curl https://rateguard.yourdomain.com/healthz      # {"status":...} and HTTP 200
curl -o /dev/null -w '%{http_code}\n' https://rateguard.yourdomain.com/metrics   # 404 (hidden on purpose)
```

Then open `https://rateguard.yourdomain.com/dashboard/`, paste your `ADMIN_TOKEN` into the token field,
press **Run demo**, and confirm the green "Connected" badge and "Servers up: 2 of 2".

## Day-to-day

```bash
docker compose -f docker-compose.prod.yml logs -f rateguard-1 rateguard-2   # watch the app
docker compose -f docker-compose.prod.yml up -d --build                     # after replacing files
docker compose -f docker-compose.prod.yml exec postgres pg_dump -U rateguard rateguard > policies.sql   # back up policies
docker compose -f docker-compose.prod.yml down            # stop (keeps data)
docker compose -f docker-compose.prod.yml down -v         # stop and DELETE all data
```

## What is protected, and what is not

Protected:

- Redis, PostgreSQL, Prometheus and the app servers are on a private network with no published ports.
- Secrets live in `.env`, not in the code. The compose file refuses to start if they are missing.
- HTTPS is automatic. The raw `/metrics` endpoint is hidden. The dashboard no longer pre-fills the dev
  token on a real hostname.
- The policy API needs the admin token. Use a long random one (step 5 does).

Not protected (know these before you share the link):

- `POST /v1/check` is public on purpose, since it is the rate limiter's job. Strangers can send requests,
  and those show up in your charts.
- The dashboard and its read-only Prometheus window are public. To password-protect them, follow the
  comments in `deploy/Caddyfile` (two minutes), then run
  `docker compose -f docker-compose.prod.yml restart caddy`.
- Redis has no password. That is acceptable only because it is unreachable from outside the private network.
- Anyone you give the admin token to can create policies. Do not paste it into screenshots.
- There is no automated backup, monitoring alert, or CI pipeline.

## For the interview

Hosting is optional. Screen-sharing the copy on your own machine (`make up`) is more reliable and
exposes nothing. If you do use the hosted copy, keep the admin token private, consider turning on the
dashboard password, and delete the server afterwards.

## Troubleshooting

- **Browser says the connection is not secure, or Caddy logs a certificate error.** DNS is not pointing at
  the server yet, or your provider's own firewall is blocking ports 80 and 443. Check both.
- **`set ADMIN_TOKEN in .env` or `set POSTGRES_PASSWORD in .env`.** That value is empty in `.env`.
- **The build is killed or hangs.** The server ran out of memory. Add swap:
  `fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile`.
- **Dashboard says "admin token was rejected".** You pasted the wrong value. Run `grep ADMIN_TOKEN .env`.
- **Dashboard charts say they can't reach Prometheus.** Run `docker compose -f docker-compose.prod.yml ps`
  and check the `prometheus` service is running.
