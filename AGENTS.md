# pgpool

Single-binary server that manages ephemeral per-worktree services on the host it runs on. Each (repo, worktree) pair can run a configured set of services side-by-side; today's registry contains `postgres`, `seaweedfs`, and `fake-gcs`. Clients connect over HTTP (REST or MCP JSON-RPC); the server shells out to the local `docker` binary to create, inspect, and destroy containers.

## Shape of the project

- `cmd/pgpool/pgpool.go` — the server, entire program in `package main`. One file on purpose.
- `cmd/pgpoolcli/pgpoolcli.go` — the thin client CLI, also one-file `package main`.
- `internal/selfupdate/` — the only shared package: the `update` logic both binaries drive (reused because the published `install.sh` refreshes both binaries, so duplicating it in each `main` would let `installerURL`/env logic drift).
- `go.mod` — stdlib only; no third-party deps.
- Target: `go build` produces static binaries named `pgpool` and `pgpoolcli`.

Keep each `main` a single file until there is a concrete reason not to. Do not split into packages speculatively — `internal/selfupdate` exists only because two binaries genuinely share it.

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
- The server stamps a single `--advertise-host` into every endpoint URL. That host may not resolve from a given client (e.g. a MagicDNS name when MagicDNS is off there). `pgpoolcli` defends against this: for any endpoint whose advertised host does not resolve locally, it rewrites the host to the control-plane host it reached the server on (the data-plane port is preserved) and notes it on stderr. Resolvable hosts are left untouched. See `rewriteEndpointHost` / `localizeEndpoints` in `pgpoolcli.go`.
- `pgpoolcli update` / `pgpool update` self-update both binaries by re-running the published `install.sh` in the directory of the running binary (`internal/selfupdate`). The server's on-disk binary is replaced but the running daemon is not restarted — the command says so.

## Transports

Both are served from the same process on the same port. Choose whichever is convenient.

### REST

- `POST /v1/up` - body `{"repo","worktree","services":["postgres","seaweedfs"]?, "image"?}` -> `{"services":[{type,container,volume,reused,endpoints:{role:{url,host_port,container_port}}}]}`. `services` defaults to the server's configured set; `image` (when present) applies to the postgres entry.
- `POST /v1/down` - body `{"repo","worktree","services"?}` -> `{"services":[{type,container,volume}]}`. Defaults to the configured set.
- `POST /v1/reload` - body `{"repo","worktree","services"?, "image"?}` -> same shape as `/v1/up`. Equivalent to running `down` then `up` per service. Volumes are destroyed.
- `GET /v1/status?repo=X&worktree=Y[&service=Z]` -> `{repo,worktree,services:[...]}`. Optional `service` filter narrows to one entry.
- `GET /v1/logs?repo=X&worktree=Y[&service=Z][&tail=N]` -> `{repo,worktree,tail,services:[{type,container,state,logs}]}`. Defaults to the configured service set; `tail` defaults to 100 and is capped at 5000. `state` is `running` | `stopped` | `missing`; `logs` is omitted when the container is missing.
- `GET /v1/list` -> array of `{type,container,volume,repo,worktree,state,created_at,endpoints?}`. One row per pgpool-labelled container with a known `pgpool.service` value. Containers missing the label or labelled with an unknown service are excluded.
- `GET /healthz` - liveness. Returns `{status,name,version}`; the version is the build's `serverVersion` and lets clients spot CLI/server skew.

### MCP

- `POST /mcp` - JSON-RPC 2.0. Implements `initialize`, `tools/list`, `tools/call`, `ping`.
- Tools: `pgpool_up`, `pgpool_down`, `pgpool_reload`, `pgpool_status`, `pgpool_logs`, `pgpool_list`. Up and reload accept an optional `services: string[]`; down accepts the same; status and logs accept an optional `service: string`; logs additionally accepts `tail: integer`. Schemas mirror REST.
- Tool call results are returned as a single `text` content block containing pretty-printed JSON. Errors set `isError: true`.

## Container naming

- Container: `<service-prefix>-<repo>-<worktree>` - `pg-` for postgres, `weed-` for seaweedfs, `gcs-` for fake-gcs.
- Volume: `<service-volume-prefix>-<repo>-<worktree>` - `pgvol-` for postgres, `weedvol-` for seaweedfs, `gcsvol-` for fake-gcs.
- Names are normalized to `[a-z0-9-]`, runs of `-` collapsed, leading/trailing `-` stripped.
- If the composed name exceeds 63 chars (Docker limit), `<worktree>` is truncated and an 8-char SHA-256 prefix is appended. A warning is logged.
- All managed containers carry labels: `pgpool=true`, `pgpool.repo=<repo>`, `pgpool.worktree=<worktree>`, `pgpool.service=<type>`. `list` filters on `pgpool=true`.

## Lifecycle invariants

- `up` is idempotent per service. Running it twice returns the same endpoints, does not wipe data, does not recreate containers.
- `up` on an existing-but-stopped container starts it and re-runs the service's readiness probe.
- `up` on a missing container creates the volume (idempotent), runs the container with a `0:<container-port>` mapping per declared endpoint, and polls readiness every 500ms until `startup-timeout` (default 30s).
- `up` on a missing container reserves one free host port per declared endpoint *before* `docker run`, so services whose CLI flags need their public URL at startup (e.g. fake-gcs-server's `-external-url`) can have it baked in. There is a small race window between port reservation and bind; if Docker fails to bind, the error is surfaced unchanged.
- `down` always destroys both the container and the volume for the named service. Missing container or missing volume is a successful no-op.
- Multi-service `up` and `down` process services sequentially. If service N fails, services 1..N-1 stay up; the response includes the partial successes plus an error.
- The server never auto-starts containers on its own boot. Clients must call `up`.
- `reload` is `down` then `up` per service. Volumes are destroyed - this is the documented contract, not a bug. Use `up` if you only need to bring missing services online without losing data.

## Configuration

Flags (or equivalent env vars). `--pg-password` is the only required field:

| flag | env | default | scope |
| --- | --- | --- | --- |
| `--listen` | `PGPOOL_LISTEN` | `:8080` | pgpool HTTP server |
| `--services` | `PGPOOL_SERVICES` | `postgres` | default service selection |
| `--advertise-host` | `PGPOOL_ADVERTISE_HOST` | `localhost` | connection URLs |
| `--image` | `PGPOOL_IMAGE` | `pgvector/pgvector:pg18` | PostgreSQL image |
| `--pg-user` | `PGPOOL_PG_USER` | `postgres` | PostgreSQL credentials |
| `--pg-password` | `PGPOOL_PG_PASSWORD` | *(required)* | PostgreSQL credentials |
| `--pg-db` | `PGPOOL_PG_DB` | `postgres` | PostgreSQL credentials |
| `--pg-max-connections` | `PGPOOL_PG_MAX_CONNECTIONS` | `100` | PostgreSQL server command |
| `--docker-bin` | `PGPOOL_DOCKER_BIN` | `docker` | Docker invocation |
| `--startup-timeout` | | `30s` | service readiness |

`--pg-max-connections` is passed as Postgres's `max_connections` server setting
when pgpool creates a PostgreSQL container. It does not affect other services.
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

## Prefer Cloudflare's platform over hand-written implementations

This service runs on Cloudflare. Before writing code for something the platform already does, use the
platform. Never hand-roll a scheduler, queue, retry loop, lock, blob store or configuration loader.

- periodic work -> **Cron Triggers** (`scheduled()`), never a daemon, timer loop or "scheduler" service
- async work, fan-out, retries, backoff, dead-letter -> **Cloudflare Queues**, never Redis/bullmq and never
  hand-written retry code
- state and locking -> the rows you already have (Postgres `SELECT ... FOR UPDATE SKIP LOCKED`), not a separate
  lock service
- long, multi-step or resumable work -> **Workflows**; durable coordination -> **Durable Objects**
- objects and large payloads -> **R2**; small hot reads -> **KV** or the Cache API
- configuration and secrets -> Worker vars and secrets, not a bespoke loader
- tests -> the Workers test pool, not hand-rolled platform mocks

Prefer an official Cloudflare binding or client library over a hand-written client, and name the library in the PR
body. A supported adapter beats custom glue; when an adapter is third-party rather than first-party (for example
OpenNext for Next.js on Workers), say so plainly instead of implying it is native.

If, and only if, there is genuinely no native option, the glue you write must be declared in the PR body as
`custom - why there was no native option`. No silent custom layers.

Design to the real limits: a Queue consumer and a Cron Trigger each get 15 minutes of wall time, CPU is capped
(configurable up to 5 minutes), and a single request may hold only 6 simultaneous outgoing connections with 128 MB
of memory. Fan work out across messages and let the platform supply the concurrency.

<!-- sythelabs:layout-spacing-ownership:v1 -->
## Layout and component spacing ownership

This is a standing rule for UI work across Sythe Labs repositories.

- **Layouts own spacing between components.** Page and section layouts own outer padding, responsive gutters, alignment, and the gaps between sibling sections. Each inset has one owner; do not add it again in headers, toolbars, summaries, or content wrappers. Layout-owned padding does not authorize appearance or padding overrides on shared content primitives.
- **Components own their content.** Content components own their markup, state, behavior, and internal spacing. They do not add compensating outer margins, padding, spacer elements, or offsets to position neighboring sections or align themselves with a page gutter.
- **Use native layout primitives.** In Tailwind, prefer one statically padded stack (`p-*`) with `gap-*` for flex/grid children; use `space-y-*` for a normal block stack when appropriate. Use responsive utilities for responsive gutters. Do not calculate layout spacing in JavaScript or branch padding/margins on which slots are null, empty, loading, or populated.
- **Optional content only changes rendering.** Omit absent sections and let the layout's gap handle the remaining siblings. Do not require empty placeholder slots or components to manufacture spacing.
- **Fix shared layout defects at their owner.** For apps using `@sythelabs/ui`, reusable layout primitives belong in that package; consume its published release rather than copying its implementation or adding per-page overrides. Other stacks use their own shared native layout primitives. Preserve bounded scrolling and flex/grid sizing while changing spacing.

| Rule | Required verification |
| --- | --- |
| One layout owns page/section gutters and sibling gaps | Rendered layout regression checks with optional sections both present and absent |
| Components do not compensate for page-level spacing | Review component boundaries and callers; remove duplicate outer insets rather than hiding them with offsets |
| Responsive spacing stays consistent | Browser geometry checks at narrow and wide widths; check heading/action alignment, clipping, overflow, and scrolling |

Keep fixes scoped to spacing ownership. Do not change typography, business behavior, credentials, or deployment configuration as part of a spacing correction.
