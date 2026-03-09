.PHONY: build run-api run-worker up down scale test lint clean

# Build binaries
build:
	go build -o bin/api ./cmd/api
	go build -o bin/worker ./cmd/worker

# Run locally
run-api:
	go run ./cmd/api

run-worker:
	go run ./cmd/worker

# Docker Compose
up:
	docker compose up --build -d

down:
	docker compose down

# Scale workers (usage: make scale N=10)
N ?= 5
scale:
	docker compose up --build -d --scale crawler_worker=$(N)

# Run tests
test:
	go test ./... -v -race -count=1

# Lint
lint:
	golangci-lint run ./...

# Clean
clean:
	rm -rf bin/
	docker compose down -v
