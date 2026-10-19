.PHONY: test vet lint run fake up down trace

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

lint:
	golangci-lint run

fake:
	go run ./cmd/fakeupstream -latency 300ms

run:
	UPSTREAM_URL=http://localhost:9090 ADMIN_TOKEN=dev-admin-token go run ./cmd/llmcache

up:
	docker compose -f deploy/docker-compose.yml up --build

down:
	docker compose -f deploy/docker-compose.yml down

trace:
	go run ./loadtest/gentrace -n 10000 -m 1000 -s 1.1
