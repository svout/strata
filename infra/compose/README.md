# infra/compose

Local development stack: the gateway, PostgreSQL 16 and Redis 7 (SPEC-002/3b). Redpanda and the observability services (Prometheus, Grafana) come in later phases.

## Use

```sh
make dev        # build the gateway image, start the stack, wait until /readyz is 200
make migrate    # apply pending schema migrations to the stack's PostgreSQL
make dev-logs   # follow every service's logs (Ctrl-C to stop)
make dev-down   # stop the stack (keeps the PostgreSQL data volume)
```

`make dev` needs Docker Desktop running. It passes the Go version from `gateway/go.mod` to the image build, so run the stack through `make` rather than calling `docker compose` directly.

| Service | Port (127.0.0.1 only) |
|---|---|
| gateway | 8080: `/healthz`, `/readyz` |
| postgres | 5432: user, password and database `strata` (dev-only) |
| redis | 6379 |

If you already run PostgreSQL or Redis locally, ports 5432 or 6379 may be taken; stop the local service first.

The credentials are dev-only placeholders for a throwaway local database, not secrets.

## Migrations

`make dev` does not migrate the database: run `make migrate` after starting the stack, and again whenever a new migration lands. It runs `gateway/cmd/migrate up` on the host with the pinned Go (not in the gateway image), against `127.0.0.1:5432` with the dev credentials above. Set `DATABASE_URL` in the environment to migrate a different database; the DSN is never printed.

It only moves forward (`up`). Running it when nothing is pending is a no-op, and it is safe to run concurrently: each run takes a PostgreSQL advisory lock, so a second run waits (polling every second, for up to 5 minutes) and then finds nothing left to apply.

If `make dev` reports that the gateway is not ready, `make dev-logs` shows why.

## Reset the data

```sh
make dev-down
docker volume rm strata_pgdata
```

The stack was named `agentgate` until 2026-11-11. A stack started before then left containers and a volume under the old name: stop them with `docker compose -p agentgate down`, then `docker volume rm agentgate_pgdata`, or the old PostgreSQL keeps port 5432.

## Without Docker

```sh
cd gateway && DATABASE_URL=postgres://… go run ./cmd/gateway
```

The gateway starts without PostgreSQL or Redis; `/readyz` reports 503 until they are reachable.
