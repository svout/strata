# infra/compose

Local development stack: the gateway, PostgreSQL 16 and Redis 7 (SPEC-002/3b). Redpanda and the observability services (Prometheus, Grafana) come in later phases.

## Use

```sh
make dev        # build the gateway image, start the stack, wait until /readyz is 200
make dev-down   # stop the stack (keeps the PostgreSQL data volume)
```

`make dev` needs Docker Desktop running. It passes the Go version from `gateway/go.mod` to the image build, so run the stack through `make` rather than calling `docker compose` directly.

| Service | Port (127.0.0.1 only) |
|---|---|
| gateway | 8080: `/healthz`, `/readyz` |
| postgres | 5432: user, password and database `agentgate` (dev-only) |
| redis | 6379 |

If you already run PostgreSQL or Redis locally, ports 5432 or 6379 may be taken; stop the local service first.

The credentials are dev-only placeholders for a throwaway local database, not secrets.

## Reset the data

```sh
make dev-down
docker volume rm agentgate_pgdata
```

## Without Docker

```sh
cd gateway && DATABASE_URL=postgres://… go run ./cmd/gateway
```

The gateway starts without PostgreSQL or Redis; `/readyz` reports 503 until they are reachable.
