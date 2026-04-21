# Redis + Postgres Ledger (Outbox Pattern) Demo

This example shows a production-style pattern where PostgreSQL is the source of truth (ACID ledger, idempotency, no overdraft) and Redis is a fast mirror updated asynchronously via an outbox worker.

- Postgres: authoritative `accounts` balance and `ledger_entries`.
- Idempotency: unique `(account_id, key)` per request to avoid double-charging.
- Outbox: each committed ledger entry emits an event to an `outbox` table.
- Worker: polls `outbox` and applies the delta to Redis atomically (`INCRBY`).

## What this demonstrates
- Overdraft-safe deductions using a single SQL statement in a transaction.
- Idempotent requests under retries.
- Eventually consistent Redis mirror that never goes negative and converges to Postgres.

## Prerequisites
- Go 1.22+
- Docker with Compose plugin

## Quick start
```bash
# 1) Start services
make up

# 2) Apply DB schema (idempotent)
make migrate

# 3) Run the demo
make demo

# 4) Stop services
make down
```

## Expected output (demo)
- 100 concurrent deductions of 10 units against an initial balance of 500.
- About 50 succeed, ~50 return INSUFFICIENT_FUNDS.
- Postgres final balance: 0
- Redis mirror final balance: 0 (matches Postgres after outbox processing)

## Configuration
- Postgres: `postgres://app:app@localhost:55432/appdb?sslmode=disable`
- Redis: `localhost:6379`
- Override Postgres with `DATABASE_URL` env var when running `go run`.

## Project layout
- `cmd/migrate/` — Minimal migration runner (embeds SQL under `migrations/`).
- `cmd/demo/` — Demo app: concurrent debits + outbox worker to Redis.
- `docker-compose.yml` — Redis and Postgres services.
- `Makefile` — Convenience targets.

## Notes
- Redis is a cache/mirror here, not the source of truth.
- The outbox worker runs in-process for the demo; in production, run it as a separate service/cron and add observability and retries with backoff.
