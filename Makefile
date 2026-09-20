.PHONY: build test up down deps loadtest logs dashboard

ADMIN_TOKEN ?= dev-admin-token

build:
	go build -o bin/rateguard ./cmd/rateguard
	go build -o bin/loadtest ./cmd/loadtest

# Only Redis + Postgres on localhost (enough to run `make test`).
deps:
	docker compose up -d redis postgres

# Integration tests need Redis on :6379 and Postgres on :5432 (see `make deps`);
# tests skip themselves if those aren't reachable.
test:
	go test -race -count=1 ./...

up:
	docker compose up --build -d

down:
	docker compose down -v

logs:
	docker compose logs -f rateguard-1 rateguard-2

# Runs all profiles through nginx, which round-robins across both instances.
loadtest: build
	ADMIN_TOKEN=$(ADMIN_TOKEN) ./bin/loadtest -targets http://localhost:8080 -total 5000 -duration 10s

# Open the live dashboard (needs `make up`).
dashboard:
	@echo "Dashboard: http://localhost:8080/dashboard/"
	@(open http://localhost:8080/dashboard/ || xdg-open http://localhost:8080/dashboard/) >/dev/null 2>&1 || true
