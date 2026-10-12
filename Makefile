COMPOSE := docker compose -f deploy/compose.yaml

.PHONY: build test up down logs migrate-status loadtest loadtest-down

## build: build both binaries into bin/
build:
	CGO_ENABLED=0 go build -trimpath -o bin/ ./cmd/...

## test: run every test, integration tests included (needs Docker)
test:
	go test -race -count=1 ./...

## up: build the image and start Postgres, the migration and the gateway
up:
	$(COMPOSE) up --build -d

## down: stop the stack; the Postgres volume is kept
down:
	$(COMPOSE) down

## logs: follow the gateway's logs
logs:
	$(COMPOSE) logs -f gateway

## migrate-status: show which migrations are applied
migrate-status:
	$(COMPOSE) run --rm migrate migrate status

## loadtest: measure the latency the gateway adds, against the fake provider (see docs/benchmarks.md)
loadtest:
	loadtests/run.sh

## loadtest-down: remove the loadtest stack and its Postgres volume
loadtest-down:
	docker compose -p llm-gateway-loadtest -f deploy/compose.yaml -f deploy/compose.loadtest.yaml --profile loadtest down -v
