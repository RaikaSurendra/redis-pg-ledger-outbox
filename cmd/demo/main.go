package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

const (
	accountID     int64 = 1
	initialFunds  int64 = 500
	workersPerRun       = 100
	debitAmount   int64 = 10
)

var applyOutboxLua = redis.NewScript(`
-- KEYS[1] = balance key
-- KEYS[2] = processed key for outbox id
-- ARGV[1] = delta (can be negative)
if redis.call('EXISTS', KEYS[2]) == 1 then
  return 0
end
redis.call('INCRBY', KEYS[1], ARGV[1])
redis.call('SET', KEYS[2], 1)
return 1
`)

func main() {
	ctx := context.Background()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://app:app@localhost:5432/appdb?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatal(err)
	}

	balanceKey := fmt.Sprintf("pg:balance:%d", accountID)

	// Ensure start state in Postgres and Redis
	if _, err := db.ExecContext(ctx,
		"INSERT INTO accounts(id, balance) VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET balance = EXCLUDED.balance",
		accountID, initialFunds,
	); err != nil {
		log.Fatal(err)
	}
	if err := rdb.Set(ctx, balanceKey, initialFunds, 0).Err(); err != nil {
		log.Fatal(err)
	}

	// Start outbox worker
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go runOutboxWorker(workerCtx, db, rdb)

	fmt.Printf("Initial balance    : %d (in Postgres)\n", initialFunds)
	fmt.Printf("Goroutines         : %d (each tries to deduct %d)\n", workersPerRun, debitAmount)
	fmt.Printf("Total attempted    : %d\n", workersPerRun*int(debitAmount))
	fmt.Printf("Expected final     : 0 (no overdraft)\n\n")

	var succeeded, insufficient atomic.Int64
	var wg sync.WaitGroup
	rand.Seed(time.Now().UnixNano())
	for i := 0; i < workersPerRun; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("demo-%d-%d", i, rand.Int())
			ok, err := debitWithIdempotency(ctx, db, accountID, debitAmount, key)
			if err != nil {
				log.Printf("debit error: %v", err)
				return
			}
			if ok {
				succeeded.Add(1)
			} else {
				insufficient.Add(1)
			}
		}(i)
	}
	wg.Wait()

	// Wait for outbox to drain and Redis to catch up
	if err := waitForOutboxDrain(ctx, db, 5*time.Second); err != nil {
		log.Printf("warning: outbox not drained: %v", err)
	}
	// Small grace period to ensure last Redis updates are visible
	time.Sleep(200 * time.Millisecond)

	pgFinal, err := loadBalance(ctx, db, accountID)
	if err != nil {
		log.Fatal(err)
	}
	redisFinal, _ := rdb.Get(ctx, balanceKey).Int64()

	fmt.Printf("Succeeded          : %d\n", succeeded.Load())
	fmt.Printf("Insufficient funds : %d\n", insufficient.Load())
	fmt.Printf("Postgres final     : %d\n", pgFinal)
	fmt.Printf("Redis final        : %d\n", redisFinal)
	if pgFinal == redisFinal && pgFinal == 0 {
		fmt.Println("CORRECT — Postgres is authoritative; Redis mirror is consistent via outbox")
	} else {
		fmt.Println("MISMATCH — investigate worker/outbox processing")
	}
}

func debitWithIdempotency(ctx context.Context, db *sql.DB, accountID, amount int64, key string) (bool, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	// Idempotency guard (no-op on duplicate key)
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO idempotency (account_id, key) VALUES ($1, $2) ON CONFLICT DO NOTHING",
		accountID, key,
	); err != nil {
		return false, err
	}

	// Overdraft-safe atomic update
	res, err := tx.ExecContext(ctx,
		"UPDATE accounts SET balance = balance - $1 WHERE id = $2 AND balance >= $1",
		amount, accountID,
	)
	if err != nil {
		return false, err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		// Insufficient funds
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}

	// Record ledger and enqueue outbox for Redis mirror
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO ledger_entries(account_id, amount) VALUES ($1, $2)",
		accountID, -amount,
	); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO outbox(account_id, delta) VALUES ($1, $2)",
		accountID, -amount,
	); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func runOutboxWorker(ctx context.Context, db *sql.DB, rdb *redis.Client) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		tx, err := db.BeginTx(ctx, &sql.TxOptions{})
		if err != nil {
			log.Printf("worker begin tx: %v", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}

		rows, err := tx.QueryContext(ctx,
			"SELECT id, account_id, delta FROM outbox WHERE processed_at IS NULL ORDER BY id LIMIT 100 FOR UPDATE SKIP LOCKED",
		)
		if err != nil {
			_ = tx.Rollback()
			log.Printf("worker select: %v", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		type evt struct{ id, accountID, delta int64 }
		var events []evt
		for rows.Next() {
			var e evt
			if err := rows.Scan(&e.id, &e.accountID, &e.delta); err != nil {
				_ = rows.Close()
				_ = tx.Rollback()
				log.Printf("worker scan: %v", err)
				continue
			}
			events = append(events, e)
		}
		_ = rows.Close()

		if len(events) == 0 {
			_ = tx.Rollback()
			time.Sleep(100 * time.Millisecond)
			continue
		}

		var ids []int64
		for _, e := range events {
			balanceKey := fmt.Sprintf("pg:balance:%d", e.accountID)
			processedKey := fmt.Sprintf("outbox:processed:%d", e.id)
			if _, err := applyOutboxLua.Run(ctx, rdb, []string{balanceKey, processedKey}, e.delta).Int64(); err != nil {
				_ = tx.Rollback()
				log.Printf("worker redis apply: %v", err)
				continue
			}
			ids = append(ids, e.id)
		}

		if _, err := tx.ExecContext(ctx, "UPDATE outbox SET processed_at = NOW() WHERE id = ANY($1)", pq.Array(ids)); err != nil {
			_ = tx.Rollback()
			log.Printf("worker mark processed: %v", err)
			continue
		}
		if err := tx.Commit(); err != nil {
			log.Printf("worker commit: %v", err)
			continue
		}
	}
}

func waitForOutboxDrain(ctx context.Context, db *sql.DB, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM outbox WHERE processed_at IS NULL").Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for outbox to drain")
}

func loadBalance(ctx context.Context, db *sql.DB, accountID int64) (int64, error) {
	var bal int64
	if err := db.QueryRowContext(ctx, "SELECT balance FROM accounts WHERE id = $1", accountID).Scan(&bal); err != nil {
		return 0, err
	}
	return bal, nil
}
