.PHONY: up down migrate demo

DB_URL=postgres://app:app@localhost:55432/appdb?sslmode=disable

up:
	docker compose up -d
	@echo "Postgres: $(DB_URL)"
	@echo "Redis: localhost:6379"

migrate:
	DATABASE_URL=$(DB_URL) go run ./cmd/migrate

demo:
	DATABASE_URL=$(DB_URL) go run ./cmd/demo

down:
	docker compose down
