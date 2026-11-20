.PHONY: up down logs enqueue status test test-unit lint guards check-docs build

up: ## Build and start the stack: Postgres, Redis, migrate, API, worker, mock provider
	docker compose up --build -d --wait api worker mockprovider
	@echo "API: http://localhost:8080/readyz   Mock provider: http://localhost:9090/stats"

down: ## Stop the stack and delete its data
	docker compose down -v

logs:
	docker compose logs -f api worker

N ?= 100
enqueue: ## Add N pending notifications per channel straight into the database (the API comes in session 6)
	docker compose exec -T postgres psql -U notifications -q -c "INSERT INTO notifications \
	  (id, recipient, channel, content, priority, status, max_attempts, ready_at) \
	  SELECT gen_random_uuid(), '+905551234567', c, 'hello', (i % 3), 'pending', 6, now() \
	  FROM generate_series(1, $(N)) AS i, unnest(ARRAY['sms', 'email', 'push']) AS c"

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
