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

.PHONY: help install-lint lint test dev

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

test: ## Run every scaffolded service's tests
	@if [ -f gateway/go.mod ]; then echo "== gateway"; (cd gateway && GOTOOLCHAIN=$(GO_TOOLCHAIN) go test -race ./...); fi
	@for s in $(PY_SERVICES); do \
	  if [ -f $$s/pyproject.toml ]; then echo "== $$s"; (cd $$s && uv run pytest) || [ $$? -eq 5 ]; fi; \
	done
	@if [ -f console/package.json ]; then echo "== console"; (cd console && pnpm install --frozen-lockfile && pnpm test); fi

dev: ## Run the gateway locally (the compose stack arrives in SPEC-002 subtask 3)
	@echo "Compose stack (PostgreSQL 16, Redis 7) comes with SPEC-002 subtask 3; running the gateway alone."
	cd gateway && GOTOOLCHAIN=$(GO_TOOLCHAIN) go run ./cmd/gateway
