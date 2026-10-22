# AgentGate — the only entry points (Reuse Registry: Make targets).
# Each service is skipped until its project file exists, so the build stays
# green while services are scaffolded one by one; real failures still fail.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

PY_SERVICES := control-plane harness

# Versions (ADR-116), one source each:
#   Go            → the `go` line of gateway/go.mod (CI reads it via go-version-file)
#   golangci-lint → GOLANGCI_LINT_VERSION below (CI installs it via `make install-lint`)
GO_TOOLCHAIN := go$(shell awk '/^go /{print $$2}' gateway/go.mod 2>/dev/null)
GOLANGCI_LINT_VERSION ?= v2.14.0

# Local stack (SPEC-002/3b). Compose interpolates GO_VERSION for the gateway
# image build on every command (even `down` and `config`), so export it.
GO_VERSION := $(GO_TOOLCHAIN:go%=%)
export GO_VERSION
COMPOSE := docker compose -f infra/compose/compose.yml

# Database for `make migrate` (SPEC-002/4b): defaults to the `make dev`
# PostgreSQL, with the same dev-only placeholder credentials as compose.yml.
# Set DATABASE_URL in the environment to migrate another database. Recipes
# must never echo it: it can hold a real password.
DATABASE_URL ?= postgres://agentgate:agentgate@127.0.0.1:5432/agentgate?sslmode=disable

.PHONY: help install-lint lint test bench dev dev-down dev-logs migrate export-docs check-docs

help: ## List the make targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-13s %s\n", $$1, $$2}'

install-lint: ## Install the pinned golangci-lint, built with the pinned Go
	GOTOOLCHAIN=$(GO_TOOLCHAIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint: ## Lint every scaffolded service
	@if [ -f gateway/go.mod ]; then \
	  have=$$(golangci-lint version --short 2>/dev/null || echo none); \
	  if [ "$$have" != "$(GOLANGCI_LINT_VERSION:v%=%)" ]; then \
	    echo "golangci-lint $$have installed, $(GOLANGCI_LINT_VERSION) required: run make install-lint" >&2; exit 1; \
	  fi; \
	  echo "== gateway"; (cd gateway && GOTOOLCHAIN=$(GO_TOOLCHAIN) golangci-lint run ./...); \
	fi
	@for s in $(PY_SERVICES); do \
	  if [ -f $$s/pyproject.toml ]; then echo "== $$s"; (cd $$s && uv run ruff check .); fi; \
	done
	@if [ -f console/package.json ]; then echo "== console"; (cd console && pnpm install --frozen-lockfile && pnpm lint); fi
	@echo "== scripts"; python3 -m py_compile scripts/*.py
	@if [ -f infra/compose/compose.yml ]; then \
	  echo "== compose"; \
	  command -v docker >/dev/null || { echo "docker CLI not found: needed to validate infra/compose/compose.yml" >&2; exit 1; }; \
	  $(COMPOSE) config -q; \
	fi
	@$(MAKE) --no-print-directory check-docs

test: ## Run every scaffolded service's tests
	@if [ -f gateway/go.mod ]; then echo "== gateway"; (cd gateway && GOTOOLCHAIN=$(GO_TOOLCHAIN) go test -race ./...); fi
	@for s in $(PY_SERVICES); do \
	  if [ -f $$s/pyproject.toml ]; then echo "== $$s"; (cd $$s && uv run pytest) || [ $$? -eq 5 ]; fi; \
	done
	@if [ -f console/package.json ]; then echo "== console"; (cd console && pnpm install --frozen-lockfile && pnpm test); fi
	@echo "== scripts"; python3 -m unittest discover -s scripts -p 'test_*.py'

# Baseline for P6, ADR-119's evidence (SPEC-002/8b-ii) and the before/after
# for SPEC-002/10a-ii. Never under -race, not in CI (runners are too noisy).
# Refuses uncommitted changes in gateway/ or this Makefile (it holds the
# settings), so the printed SHA is what ran. Uncontended runs at full
# settings (the numbers P6 cites); the two k-sweeps (contended: duplicates of
# one key; concurrent-settled: distinct keys) at reduced ones (they measure
# shape). Output is also saved to bench-<sha>.txt for benchstat.
bench_go = (cd gateway && GOTOOLCHAIN=$(GO_TOOLCHAIN) go test -run '^$$' -timeout 60m $(1) ./internal/payments)
bench: ## Benchmark POST /v1/payments (with/without Redis, settled on fake/internal rail, contended and concurrent-settled sweeps); needs Docker
	@docker info >/dev/null 2>&1 || { echo "Docker is not running: start Docker Desktop" >&2; exit 1; }
	@if [ -n "$$(git status --porcelain -- gateway Makefile)" ]; then echo "bench: gateway/ or Makefile has uncommitted changes; commit first so the SHA matches" >&2; exit 1; fi
	@out=bench-$$(git rev-parse --short HEAD).txt; { \
	  echo "commit: $$(git rev-parse HEAD)"; \
	  echo "date: $$(date -u +%Y-%m-%dT%H:%M:%SZ)"; \
	  echo "host: $$(uname -srm)"; \
	  if command -v sysctl >/dev/null; then echo "machine: $$(sysctl -n hw.model 2>/dev/null), $$(sysctl -n machdep.cpu.brand_string 2>/dev/null), $$(sysctl -n hw.ncpu 2>/dev/null) cores, $$(( $$(sysctl -n hw.memsize 2>/dev/null || echo 0) / 1073741824 )) GiB"; fi; \
	  echo "docker: $$(docker info --format '{{.NCPU}} CPUs, {{.MemTotal}} B, server {{.ServerVersion}}')"; \
	  echo "go: $$(cd gateway && GOTOOLCHAIN=$(GO_TOOLCHAIN) go version)"; \
	  $(call bench_go,-bench '^BenchmarkCreatePayment$$' -benchtime 2000x -count 5); \
	  $(call bench_go,-bench '^BenchmarkCreatePaymentSettled$$' -benchtime 2000x -count 5); \
	  $(call bench_go,-bench '^BenchmarkCreatePaymentContended$$' -benchtime 200x -count 3); \
	  $(call bench_go,-bench '^BenchmarkCreatePaymentConcurrentSettled$$' -benchtime 200x -count 3); \
	} 2>&1 | tee "$$out"; echo "saved $$out"

dev: ## Start the local stack (gateway, PostgreSQL 16, Redis 7) and wait until /readyz is 200
	@docker info >/dev/null 2>&1 || { echo "Docker is not running: start Docker Desktop" >&2; exit 1; }
	$(COMPOSE) up -d --build --wait --wait-timeout 120
	@for i in $$(seq 30); do \
	  if curl -fsS http://127.0.0.1:8080/readyz; then echo; exit 0; fi; \
	  sleep 1; \
	done; \
	curl -sS http://127.0.0.1:8080/readyz >&2 || true; \
	echo "gateway not ready after 30 s; see: make dev-logs" >&2; exit 1

dev-down: ## Stop the local stack (keeps the pgdata volume)
	$(COMPOSE) down

dev-logs: ## Follow the local stack's logs (Ctrl-C to stop)
	$(COMPOSE) logs -f

# Runs on the host with the pinned Go, not in the gateway image. The DSN is
# passed through the environment (exported for this target only), so it never
# appears in the echoed command line. Safe to run concurrently: runs are
# serialised by goose's PostgreSQL advisory lock. $(value) passes it through
# unexpanded, so a `$` in a real password isn't eaten by make.
migrate: export DATABASE_URL := $(value DATABASE_URL)
migrate: ## Apply pending schema migrations to DATABASE_URL (default: the make dev database)
	cd gateway && GOTOOLCHAIN=$(GO_TOOLCHAIN) go run ./cmd/migrate up

export-docs: ## Export vault specs into docs/specs (one-way, ADR-107; needs STRATA_VAULT)
	@python3 scripts/export_docs.py export

check-docs: ## Check docs/ exports are stamped and unedited (no vault needed; CI runs it)
	@python3 scripts/export_docs.py check && echo "check-docs: ok"
