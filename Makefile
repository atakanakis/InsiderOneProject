.PHONY: up down logs e2e status test test-unit lint guards check-docs build

up: ## Build and start the stack: Postgres, Redis, migrate, API, worker, mock provider
	docker compose up --build -d --wait api worker mockprovider
	@echo "API: http://localhost:8080/readyz   Mock provider: http://localhost:9090/stats"

down: ## Stop the stack and delete its data
	docker compose down -v

logs:
	docker compose logs -f api worker

N ?= 100
e2e: ## Send N notifications per channel through the API and wait until all are sent; KEY=... when API_KEYS is set
	go run ./tools/e2e -n $(N) $(if $(KEY),-key $(KEY))

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

build:
	go build -o bin/ ./cmd/...
