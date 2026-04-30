# pgpool

Single-binary server that manages ephemeral per-worktree services on the host it runs on. Each (repo, worktree) pair can run a configured set of services side-by-side; today's registry contains `postgres` and `seaweedfs`. Clients connect over HTTP (REST or MCP JSON-RPC); the server shells out to the local `docker` binary to create, inspect, and destroy containers.

## Shape of the project

- `pgpool.go` — entire program in `package main`. One file on purpose.
- `go.mod` — stdlib only; no third-party deps.
- Target: `go build` produces a single static binary named `pgpool`.

Keep it a single file until there is a concrete reason not to. Do not split into packages speculatively.

## Architecture

```
+----------+        HTTP         +------------------+     exec     +--------+
| client   |  -- REST or MCP --> |  pgpool (:8080)  |  --------->  | docker |
| (agent)  |                     |                  |              |  CLI   |
+----------+                     +------------------+              +--------+
                                         |                              |
                                         +----- runs on the Docker host ----+
```

- Server is stateless. All state lives in Docker (containers, volumes, labels including `pgpool.service`).
- Each (repo, worktree) can run multiple services. Service set per request is `services: [...]` in the body, or the server's `--services` default when absent.
- Clients pass `repo` and `worktree` explicitly. The server never derives identity from `$PWD`.
- Clients are responsible for their own `.env` file writing. The server only returns endpoint URLs.

## Transports

Both are served from the same process on the same port. Choose whichever is convenient.

### REST

- `POST /v1/up` - body `{"repo","worktree","services":["postgres","seaweedfs"]?, "image"?}` -> `{"services":[{type,container,volume,reused,endpoints:{role:{url,host_port,container_port}}}]}`. `services` defaults to the server's configured set; `image` (when present) applies to the postgres entry.
- `POST /v1/down` - body `{"repo","worktree","services"?}` -> `{"services":[{type,container,volume}]}`. Defaults to the configured set.
- `GET /v1/status?repo=X&worktree=Y[&service=Z]` -> `{repo,worktree,services:[...]}`. Optional `service` filter narrows to one entry.
- `GET /v1/list` -> array of `{type,container,volume,repo,worktree,state,created_at,endpoints?}`. One row per pgpool-labelled container with a known `pgpool.service` value. Containers missing the label or labelled with an unknown service are excluded.
- `GET /healthz` - liveness.

### MCP

- `POST /mcp` - JSON-RPC 2.0. Implements `initialize`, `tools/list`, `tools/call`, `ping`.
- Tools: `pgpool_up`, `pgpool_down`, `pgpool_status`, `pgpool_list`. Up and down accept an optional `services: string[]`; status accepts an optional `service: string`. Schemas mirror REST.
- Tool call results are returned as a single `text` content block containing pretty-printed JSON. Errors set `isError: true`.

## Container naming

- Container: `<service-prefix>-<repo>-<worktree>` - `pg-` for postgres, `weed-` for seaweedfs.
- Volume: `<service-volume-prefix>-<repo>-<worktree>` - `pgvol-` for postgres, `weedvol-` for seaweedfs.
- Names are normalized to `[a-z0-9-]`, runs of `-` collapsed, leading/trailing `-` stripped.
- If the composed name exceeds 63 chars (Docker limit), `<worktree>` is truncated and an 8-char SHA-256 prefix is appended. A warning is logged.
- All managed containers carry labels: `pgpool=true`, `pgpool.repo=<repo>`, `pgpool.worktree=<worktree>`, `pgpool.service=<type>`. `list` filters on `pgpool=true`.

## Lifecycle invariants

- `up` is idempotent per service. Running it twice returns the same endpoints, does not wipe data, does not recreate containers.
- `up` on an existing-but-stopped container starts it and re-runs the service's readiness probe.
- `up` on a missing container creates the volume (idempotent), runs the container with a `0:<container-port>` mapping per declared endpoint, and polls readiness every 500ms until `startup-timeout` (default 30s).
- `down` always destroys both the container and the volume for the named service. Missing container or missing volume is a successful no-op.
- Multi-service `up` and `down` process services sequentially. If service N fails, services 1..N-1 stay up; the response includes the partial successes plus an error.
- The server never auto-starts containers on its own boot. Clients must call `up`.

## Configuration

Flags (or equivalent env vars). `--pg-password` is the only required field:

| flag               | env                     | default       |
| ------------------ | ----------------------- | ------------- |
| `--listen`         | `PGPOOL_LISTEN`         | `:8080`       |
| `--services`       | `PGPOOL_SERVICES`       | `postgres`    |
| `--advertise-host` | `PGPOOL_ADVERTISE_HOST` | `localhost`   |
| `--image`          | `PGPOOL_IMAGE`          | `postgres:17` |
| `--pg-user`        | `PGPOOL_PG_USER`        | `postgres`    |
| `--pg-password`    | `PGPOOL_PG_PASSWORD`    | *(required)*  |
| `--pg-db`          | `PGPOOL_PG_DB`          | `postgres`    |
| `--docker-bin`     | `PGPOOL_DOCKER_BIN`     | `docker`      |
| `--startup-timeout`|                         | `30s`         |

`--advertise-host` is the hostname written into URLs returned to clients. Set it to the Tailscale name / LAN IP that remote clients use to reach Postgres. `localhost` only works for same-machine clients.

## Running

```
go build -o pgpool ./cmd/pgpool
./pgpool --pg-password hunter2 --services postgres,seaweedfs --advertise-host pgpool.tailnet.ts.net
```

Quick smoke test:

```
curl -s -X POST localhost:8080/v1/up \
  -H 'content-type: application/json' \
  -d '{"repo":"somni","worktree":"dublin-v1"}'

curl -s 'localhost:8080/v1/status?repo=somni&worktree=dublin-v1'
curl -s localhost:8080/v1/list
curl -s -X POST localhost:8080/v1/down \
  -H 'content-type: application/json' \
  -d '{"repo":"somni","worktree":"dublin-v1"}'
```

MCP smoke test:

```
curl -s -X POST localhost:8080/mcp \
  -H 'content-type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

## Coding conventions for this repo

- Stdlib only. A dependency needs a real justification before it lands in `go.mod`.
- Keep `pgpool.go` as a single file. Resist the urge to pre-split.
- Shell out to `docker`. Do not adopt the Docker SDK unless shell-out proves insufficient.
- No `.env` parsing or writing on the server. That belongs to clients.
- Errors include the docker command context (container name, stderr tail). No bare `err.Error()` returns to users.
- No retry loops on docker transport. One race case is handled (`up` retrying after "name already in use").

## Out of scope

- Multi-host pools / failover.
- Auth / TLS on the HTTP endpoint. Assumed to be bound to a private network (Tailnet or loopback).
- Seeding, migrations, fixtures. That is the consuming app's job.
- A CLI client. A thin Go or shell client can live in a separate repo if needed.

## Security posture

- The pg superuser password is shared across all containers. Acceptable because the server and Postgres are only reachable on a trusted network.
- No auth on the HTTP endpoint in v1. **Do not expose the port to the public internet.**
- Labels are trusted. `list` filters by label; anything else labelled `pgpool=true` will show up and be eligible for `down`. Do not hand-label unrelated containers with `pgpool=true`.
