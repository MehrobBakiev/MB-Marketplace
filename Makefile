.PHONY: generate build test vet integration run up down e2e

generate:
	go generate ./api

build: generate
	go build -o bin/marketplace ./cmd

test: generate
	go test -race ./...

vet: generate
	go vet ./...

integration: generate
	TEST_DATABASE_URL="$${TEST_DATABASE_URL:-postgres://marketplace:marketplace_local@localhost:5432/marketplace?sslmode=disable}" go test -race ./internal/app -run TestIntegration -count=1 -v

run: generate
	go run ./cmd

up:
	docker compose up --build -d

down:
	docker compose down

e2e:
	python3 scripts/e2e.py
