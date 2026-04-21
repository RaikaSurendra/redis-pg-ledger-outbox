package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"

	_ "github.com/lib/pq"
)

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

	if err := db.PingContext(ctx); err != nil {
		log.Fatal(err)
	}

	// Read migrations from repo-root-relative "migrations" directory.
	pattern := filepath.Join("migrations", "*.sql")
	paths, err := filepath.Glob(pattern)
	if err != nil {
		log.Fatal(err)
	}
	if len(paths) == 0 {
		log.Fatalf("no migration files matched %q", pattern)
	}
	sort.Strings(paths)
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Applying %s...\n", filepath.Base(p))
		if _, err := db.ExecContext(ctx, string(b)); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Println("Migrations applied.")
}
