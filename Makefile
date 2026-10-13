COMPOSE := docker compose -f deploy/compose.yaml
OBS := $(COMPOSE) -f deploy/compose.observability.yaml
# The image build stamps this into the gateway as service.version.
export VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)

.PHONY: build test up down logs migrate-status loadtest loadtest-down up-obs

## build: build both binaries into bin/
build:
	CGO_ENABLED=0 go build -trimpath -o bin/ ./cmd/...

## test: run every test, integration tests included (needs Docker)
test:
	go test -race -count=1 ./...

## up: build the image and start Postgres, the migration and the gateway
up:
	$(COMPOSE) up --build -d

## up-obs: start the stack with the observability services; the gateway exports to the collector
up-obs:
	$(OBS) up --build -d

## down: stop the stack, with or without the observability services; volumes are kept
down:
	$(OBS) down

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
