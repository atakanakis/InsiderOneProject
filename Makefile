.PHONY: up up-observability down logs e2e load status test test-unit lint guards check-docs alerts build

up: ## Build and start the stack: Postgres, Redis, migrate, API, worker, mock provider
	docker compose up --build -d --wait api worker mockprovider
	@echo "API: http://localhost:8080/readyz   Mock provider: http://localhost:9090/stats"

up-observability: ## The stack with Prometheus (localhost:9091, the alert rules) and Jaeger (localhost:16686); tracing on
	OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:4318 docker compose --profile observability up --build -d --wait
	@echo "Prometheus: http://localhost:9091/alerts   Jaeger: http://localhost:16686"

down: ## Stop the stack and delete its data
	docker compose --profile observability down -v

logs:
	docker compose logs -f api worker

N ?= 100
e2e: ## Follow one notification over the WebSocket, send N per channel through the API, wait until all are sent; KEY=... when API_KEYS is set
	go run ./tools/e2e -n $(N) $(if $(KEY),-key $(KEY))

LOAD_N ?= 30000
load: ## A burst of LOAD_N notifications against the running stack; prints acceptance, memory, drain, high-priority latency and the provider's peak (tools/load)
	go run ./tools/load -n $(LOAD_N) $(if $(KEY),-key $(KEY))

status: ## Count the notifications by status
	docker compose exec -T postgres psql -U notifications -c \
	  "SELECT status, count(*) FROM notifications GROUP BY status ORDER BY status"

test: ## Every test, with the race detector; integration tests start Postgres and Redis (needs Docker)
	go test -race -count=1 ./...

test-unit: ## Tests that need no Docker
	go test -race -count=1 -short ./...

lint:
	docker run --rm -v "$(CURDIR)":/app -w /app golangci/golangci-lint:v2.15.0 golangci-lint run ./...

guards: ## Disable each guard in turn and check that its named test fails (tools/guards)
	go run ./tools/guards

check-docs: ## Every test named in docs/ and README.md exists in the code
	go run ./tools/testnames

PROMETHEUS_IMAGE = prom/prometheus:v3.5.0
PROMTOOL = docker run --rm -v "$(CURDIR)/deploy/prometheus":/rules:ro -w /rules --entrypoint promtool $(PROMETHEUS_IMAGE)
alerts: ## promtool checks and tests the alert rules; every rule must have a firing test case
	$(PROMTOOL) check rules alerts.yml
	$(PROMTOOL) test rules alerts_test.yml
	go run ./tools/alertcheck

build:
	go build -o bin/ ./cmd/...
