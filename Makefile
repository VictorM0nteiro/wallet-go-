DATABASE_URL ?= postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable
MIGRATIONS_DIR := migrations

.PHONY: build run test test-race lint db-up db-down migrate-up migrate-down migrate-create

build:
      go build ./...

run:
      go run ./cmd/wallet

test:
      go test ./...

test-race:
      go test -race ./...

lint:
      golangci-lint run

db-up:
      docker compose up -d

db-down:
      docker compose down

migrate-up:
      migrate -path $(MIGRATIONS_DIR) -database "$(DATABASE_URL)" up

migrate-down:
      migrate -path $(MIGRATIONS_DIR) -database "$(DATABASE_URL)" down 1

migrate-create:
      migrate create -ext sql -dir $(MIGRATIONS_DIR) -seq $(name)