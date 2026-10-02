# AgentGate — the only entry points (Reuse Registry: Make targets).
# Each service is skipped until its project file exists, so the build stays
# green while services are scaffolded one by one; real failures still fail.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

PY_SERVICES := control-plane harness

# Go toolchain for gateway lint and tests. Pinned so local runs match CI
# (setup-go installs the same version) and so golangci-lint v2.1.6 can load
# packages: it cannot type-check against much newer Go releases. Go downloads
# this toolchain on first use if it isn't installed.
GO_TOOLCHAIN ?= go1.23.12

.PHONY: help lint test dev

help: ## List the make targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'

lint: ## Lint every scaffolded service
	@if [ -f gateway/go.mod ]; then echo "== gateway"; (cd gateway && GOTOOLCHAIN=$(GO_TOOLCHAIN) golangci-lint run ./...); fi
	@for s in $(PY_SERVICES); do \
	  if [ -f $$s/pyproject.toml ]; then echo "== $$s"; (cd $$s && uv run ruff check .); fi; \
	done
	@if [ -f console/package.json ]; then echo "== console"; (cd console && pnpm install --frozen-lockfile && pnpm lint); fi

test: ## Run every scaffolded service's tests
	@if [ -f gateway/go.mod ]; then echo "== gateway"; (cd gateway && GOTOOLCHAIN=$(GO_TOOLCHAIN) go test -race ./...); fi
	@for s in $(PY_SERVICES); do \
	  if [ -f $$s/pyproject.toml ]; then echo "== $$s"; (cd $$s && uv run pytest) || [ $$? -eq 5 ]; fi; \
	done
	@if [ -f console/package.json ]; then echo "== console"; (cd console && pnpm install --frozen-lockfile && pnpm test); fi

dev: ## Run the gateway locally (the compose stack arrives in SPEC-002 subtask 3)
	@echo "Compose stack (PostgreSQL 16, Redis 7) comes with SPEC-002 subtask 3; running the gateway alone."
	cd gateway && go run ./cmd/gateway
