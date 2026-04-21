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
- `cmd/migrate/` — Minimal migration runner (reads SQL files under `migrations/` in order).
- `cmd/demo/` — Demo app: concurrent debits + outbox worker to Redis.
- `docker-compose.yml` — Redis and Postgres services.
- `Makefile` — Convenience targets.

## Notes
- Redis is a cache/mirror here, not the source of truth.
- The outbox worker runs in-process for the demo; in production, run it as a separate service/cron and add observability and retries with backoff.

---

## Architecture

```
┌──────────────┐    HTTP/gRPC     ┌───────────────┐     SQL TX (ACID)      ┌─────────────┐
│ Clients      │ ───────────────▶ │ Demo App      │ ─────────────────────▶ │ PostgreSQL  │
└──────────────┘                  │  (Go)         │                         │  (source)   │
                                  │               │                         └─────────────┘
                                  │   Outbox      │   Redis INCRBY/Lua      ┌─────────────┐
                                  │   Worker  ─────────────────────────────▶ │ Redis      │
                                  └───────────────┘                         │ (mirror)   │
                                                                            └─────────────┘
```

- App writes the authoritative balance to Postgres inside a single transaction that also appends a ledger entry and enqueues an outbox row.
- A background worker polls unprocessed `outbox` rows, applies their deltas to Redis atomically via Lua (idempotent), and then marks those rows processed.
- Redis is eventually consistent with Postgres and can be rebuilt from the ledger if needed.

## Database schema (summary)
- `accounts(id PK, balance CHECK balance ≥ 0)` — current authoritative balance.
- `ledger_entries(id PK, account_id FK, amount, created_at)` — immutable history of debits/credits (negative = debit).
- `idempotency(account_id, key PRIMARY KEY)` — deduplicate client retries.
- `outbox(id PK, account_id, delta, created_at, processed_at NULLABLE)` — events to mirror balance changes to Redis.

See `migrations/001_init.sql` for DDL.

## End-to-end flows

- Debit (inside one SQL transaction):
  1. `INSERT INTO idempotency(account_id, key) ... ON CONFLICT DO NOTHING` — request de-dup.
  2. `UPDATE accounts SET balance = balance - $amount WHERE id = $id AND balance >= $amount` — overdraft-safe atomic update.
  3. `INSERT INTO ledger_entries(account_id, amount)` — append to ledger.
  4. `INSERT INTO outbox(account_id, delta)` — enqueue mirror update.
  5. Commit.

- Outbox worker (repeat):
  1. `SELECT ... FROM outbox WHERE processed_at IS NULL ORDER BY id LIMIT 100 FOR UPDATE SKIP LOCKED`.
  2. For each event: run Lua in Redis: if `outbox:processed:{id}` exists, skip; else `INCRBY` balance and `SET` processed key.
  3. `UPDATE outbox SET processed_at = NOW() WHERE id = ANY($ids)`.
  4. Commit.

Lua snippet used for idempotency on the Redis side:

```lua
-- KEYS[1] = balance key, KEYS[2] = processed key, ARGV[1] = delta
if redis.call('EXISTS', KEYS[2]) == 1 then return 0 end
redis.call('INCRBY', KEYS[1], ARGV[1])
redis.call('SET', KEYS[2], 1)
return 1
```

## Consistency and failure modes
- App crash before commit: nothing is applied (atomicity of the SQL transaction).
- App crash after commit, before Redis update: outbox still unprocessed; worker will catch up; Redis may briefly be stale.
- Worker crash after applying to Redis but before marking processed: Redis-side Lua deduplicates on restart, preventing double-apply.
- Redis loss: mirror can be rebuilt from Postgres (ledger replay, then `SET` balances).
- Postgres loss: outside scope of this demo, but use regular backups, PITR, replication, etc.

## Configuration and tuning
- Postgres URL: `DATABASE_URL` (defaults to README value). Host port is 55432.
- DB pool: adapt via environment variables or code (not exposed in this minimal demo). For high concurrency:
  - Reduce demo concurrency or
  - Increase Postgres `max_connections` and/or connection pool size.
- Worker throughput: batch size is 100; tune by editing the SQL `LIMIT`.

## Customizing the demo
- Edit constants in `cmd/demo/main.go`:
  - `initialFunds` (default 500)
  - `workersPerRun` (default 100)
  - `debitAmount` (default 10)
- Keys used in Redis mirror: `pg:balance:{account_id}` and `outbox:processed:{id}`.

## Troubleshooting
- "sorry, too many clients already":
  - Lower `workersPerRun` in `cmd/demo/main.go`, or
  - Increase Postgres `max_connections` in the container (e.g., custom image or config), or
  - Add a DB connection pool limit in code.
- Port conflicts:
  - Postgres uses host `55432`. Change in `docker-compose.yml` and `Makefile` if required.
- Go module checksum mismatch:
  - Run `rm -f go.sum && go mod tidy` in this folder.
- Redis not updating:
  - Check outbox has rows: `SELECT * FROM outbox WHERE processed_at IS NULL;`
  - Inspect worker logs; ensure Redis reachable on `localhost:6379`.

## Alternatives and extensions
- CDC instead of polling: Debezium / logical decoding publishing to a queue read by a Redis updater service.
- Exactly-once delivery: keep idempotency keys in Redis or Postgres and design processed markers with TTLs.
- Multi-key updates: use a durable transaction log and materialize multiple mirrors, or consider Redis Streams with consumer groups.
- Strong consistency for reads: read from Postgres; use Redis only for latency-sensitive paths tolerant to eventual consistency.

## Cleanup
```bash
make down
docker volume prune  # optional, if you added volumes
```
