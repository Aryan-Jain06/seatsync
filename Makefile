# SeatSync developer commands.
# `make help` lists everything.

SHELL := /bin/bash
COMPOSE := docker compose
BACKEND := ./backend

# Load .env if present so the host-run targets see the same config as compose.
ifneq (,$(wildcard ./.env))
include .env
export
endif

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

# --- Environment -----------------------------------------------------------

.PHONY: env
env: ## Create .env from .env.example if it does not exist
	@test -f .env || (cp .env.example .env && echo "created .env")

# --- Docker ----------------------------------------------------------------

.PHONY: up
up: env ## Start the full stack (postgres, redis, backend, frontend)
	$(COMPOSE) up -d --build

.PHONY: infra
infra: env ## Start only postgres + redis (for running the backend on the host)
	$(COMPOSE) up -d postgres redis

.PHONY: down
down: ## Stop the stack, keeping volumes
	$(COMPOSE) down

.PHONY: clean
clean: ## Stop the stack and delete all data volumes
	$(COMPOSE) down -v

.PHONY: logs
logs: ## Tail logs from all services
	$(COMPOSE) logs -f

.PHONY: ps
ps: ## Show service status
	$(COMPOSE) ps

# --- Backend ---------------------------------------------------------------

.PHONY: migrate
migrate: ## Apply all database migrations
	cd $(BACKEND) && go run ./cmd/migrate up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration
	cd $(BACKEND) && go run ./cmd/migrate down 1

.PHONY: seed
seed: ## Load seed data (2 venues, 4 events, 400 seats per venue)
	cd $(BACKEND) && go run ./cmd/seed

.PHONY: run
run: ## Run the backend on the host (needs `make infra`)
	cd $(BACKEND) && go run ./cmd/server

.PHONY: build
build: ## Compile the backend binary to backend/bin/server
	cd $(BACKEND) && go build -o bin/server ./cmd/server

.PHONY: test
test: ## Run backend tests (repos/locks tests need postgres + redis running)
	cd $(BACKEND) && go test ./... -count=1

.PHONY: test-race
test-race: ## Run backend unit tests with the race detector
	cd $(BACKEND) && go test ./... -race -count=1

.PHONY: cover
cover: ## Run tests and write an HTML coverage report
	cd $(BACKEND) && go test ./... -coverprofile=coverage.out -count=1 \
		&& go tool cover -html=coverage.out -o coverage.html \
		&& echo "wrote backend/coverage.html"

.PHONY: tidy
tidy: ## Tidy Go module dependencies
	cd $(BACKEND) && go mod tidy

.PHONY: vet
vet: ## Run go vet
	cd $(BACKEND) && go vet ./...

# --- Frontend --------------------------------------------------------------

.PHONY: web-install
web-install: ## Install frontend dependencies
	cd frontend && npm install

.PHONY: web
web: ## Run the frontend dev server on :3000
	cd frontend && npm run dev

.PHONY: web-build
web-build: ## Production build of the frontend
	cd frontend && npm run build

# --- Load test -------------------------------------------------------------

.PHONY: loadtest
loadtest: ## Run the k6 concurrency proof (500 attempts on 50 seats)
	./loadtest/run.sh

.PHONY: verify-no-doubles
verify-no-doubles: ## SQL check: zero duplicate confirmed (event, seat) pairs
	./loadtest/verify.sh
