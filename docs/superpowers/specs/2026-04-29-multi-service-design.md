# pgpool: multi-service support

Generalize pgpool from "one Postgres container per worktree" to "N services per worktree". First non-Postgres service: SeaweedFS. Registry is hardcoded in Go and ready to accept new services (redis, valkey, ...) by adding one struct entry.

## Goals

- A single worktree can run multiple services side-by-side (initially `postgres` and `seaweedfs`).
- `up` brings up the configured set; `down` tears it all down. Bare commands cascade.
- CLI positional arg scopes a single service: `pgpoolcli up postgres`, `pgpoolcli down seaweedfs`.
- Server stays stateless. State lives in Docker (containers, volumes, labels).
- Stdlib-only. Single file per binary preserved.

## Non-goals

- Per-repo config files. Dev-only posture - global server config is enough for now.
- Per-service distinct credentials. Shared password where applicable.
- Auth, TLS, multi-host.
- Adding redis/valkey on this branch. Registry shape is ready; entries aren't.

## Architecture

### Service registry

A package-level map in `cmd/pgpool/pgpool.go`, keyed by service type:

```go
type EndpointSpec struct {
    Role          string  // "primary" | "master" | "filer" | "s3" | ...
    ContainerPort int     // port inside the container
    Scheme        string  // "postgresql" | "http" | ...
}

type ServiceDef struct {
    Type            string
    ContainerPrefix string  // "pg", "weed"
    VolumePrefix    string  // "pgvol", "weedvol"
    Image           string  // default; overridable via per-service flag
    DockerArgs      func(cfg Config, volume string) []string  // env, volume mount, command args (no -p, no --label, no --name)
    Endpoints       []EndpointSpec
    Readiness       func(ctx context.Context, s *Server, container string, hostPorts map[string]string) error
    BuildURL        func(cfg Config, role string, hostPort string) string
}

var serviceDefs = map[string]ServiceDef{
    "postgres":  postgresDef,
    "seaweedfs": seaweedfsDef,
}
```

Registered services in v1:

| type      | image                       | endpoints                                                  | readiness                                  |
| --------- | --------------------------- | ---------------------------------------------------------- | ------------------------------------------ |
| postgres  | `postgres:17`               | `primary` 5432 (postgresql://)                             | `docker exec ... pg_isready -U <user>`     |
| seaweedfs | `chrislusf/seaweedfs:3.71`  | `master` 9333, `volume` 8080, `filer` 8888, `s3` 8333 (http://) | HTTP GET `/cluster/status` on master returns 200 |

The `weed server` command for seaweedfs: `server -dir=/data -master -volume -filer -s3`. Volume mount `/data`.

### Naming and labels

- Container: `<containerPrefix>-<repo>-<worktree>`
- Volume:    `<volumePrefix>-<repo>-<worktree>`
- Existing normalization (lowercase, `[a-z0-9-]`, collapse runs of `-`) and 63-char SHA-truncation logic applies per-service. Each service's prefix gets its own budget calculation.
- Labels on every container: `pgpool=true`, `pgpool.repo=<repo>`, `pgpool.worktree=<worktree>`, plus the new `pgpool.service=<type>`.
- `list` continues to filter by `pgpool=true`. `pgpool.service` is read from labels for output.

### Server config

| flag         | env               | default     | meaning                                                     |
| ------------ | ----------------- | ----------- | ----------------------------------------------------------- |
| `--services` | `PGPOOL_SERVICES` | `postgres`  | comma-separated default service set when request omits it   |

Existing `--pg-*` flags continue to feed `postgresDef.DockerArgs`. SeaweedFS needs no required config in v1 (no auth, no replication knobs surfaced). Per-service image overrides via `--seaweedfs-image` etc. can be added when needed; not required for v1.

### REST API

#### `POST /v1/up`

Request:
```json
{
  "repo": "foo",
  "worktree": "bar",
  "services": ["postgres", "seaweedfs"]
}
```

`services` is optional. When absent, server uses its `--services` default. Empty array or unknown service name → 400.

Response (200):
```json
{
  "services": [
    {
      "type": "postgres",
      "container": "pg-foo-bar",
      "volume": "pgvol-foo-bar",
      "reused": false,
      "endpoints": {
        "primary": {"url": "postgresql://postgres:pw@host:49734/postgres", "host_port": "49734", "container_port": 5432}
      }
    },
    {
      "type": "seaweedfs",
      "container": "weed-foo-bar",
      "volume": "weedvol-foo-bar",
      "reused": false,
      "endpoints": {
        "master": {"url": "http://host:49160", "host_port": "49160", "container_port": 9333},
        "volume": {"url": "http://host:49161", "host_port": "49161", "container_port": 8080},
        "filer":  {"url": "http://host:49162", "host_port": "49162", "container_port": 8888},
        "s3":     {"url": "http://host:49163", "host_port": "49163", "container_port": 8333}
      }
    }
  ]
}
```

Per-service idempotency carries over from today's lifecycle: existing+running → reused; existing+stopped → start + readiness; missing → create volume + run + readiness.

#### `POST /v1/down`

Request:
```json
{
  "repo": "foo",
  "worktree": "bar",
  "services": ["postgres"]
}
```

`services` optional; absent means "all services in `--services`". Missing containers / volumes are silent no-ops, as today.

Response:
```json
{
  "services": [
    {"type": "postgres", "container": "pg-foo-bar", "volume": "pgvol-foo-bar"}
  ]
}
```

#### `GET /v1/status?repo=X&worktree=Y[&service=postgres]`

Response:
```json
{
  "repo": "foo",
  "worktree": "bar",
  "services": [
    {
      "type": "postgres",
      "container": "pg-foo-bar",
      "volume": "pgvol-foo-bar",
      "state": "running",
      "created_at": "2026-04-29T...",
      "endpoints": {"primary": {"url": "...", "host_port": "49734", "container_port": 5432}}
    }
  ]
}
```

Without `service`, the server walks every type in `--services` and reports whatever exists. With `service=X`, the array contains only that entry (or is empty if missing). Unknown service name → 400.

#### `GET /v1/list`

Flat array; one row per pgpool-labelled container on the host:

```json
[
  {"type": "postgres", "container": "pg-foo-bar", "volume": "pgvol-foo-bar", "repo": "foo", "worktree": "bar", "state": "running", "created_at": "...", "endpoints": {...}},
  {"type": "seaweedfs", "container": "weed-foo-bar", "volume": "weedvol-foo-bar", "repo": "foo", "worktree": "bar", "state": "running", "created_at": "...", "endpoints": {...}}
]
```

`type` is read from the `pgpool.service` label. Containers labelled `pgpool=true` but missing `pgpool.service` (legacy postgres-only deployments) are reported with `type: "postgres"` so existing containers don't disappear from `list`.

### MCP tools

Tool names unchanged: `pgpool_up`, `pgpool_down`, `pgpool_status`, `pgpool_list`. Input schemas extended:

- `pgpool_up`, `pgpool_down`: optional `services: string[]`.
- `pgpool_status`: optional `service: string`.
- `pgpool_list`: unchanged.

Tool call results stay as a single text content block with pretty-printed JSON. `isError: true` on failure, as today.

### CLI

```
pgpoolcli up                  # all services in --services
pgpoolcli up postgres         # just postgres
pgpoolcli down                # all services
pgpoolcli down seaweedfs      # just seaweedfs
pgpoolcli status              # all services for this worktree
pgpoolcli status postgres     # filter to one
pgpoolcli list                # everything on the server
```

Positional arg, when present, becomes the single-element `services` field on the request. The CLI does not maintain its own allow-list - the server is the source of truth and rejects unknown service names with 400, which the CLI surfaces as the error message.

CLI output:

- `up` / `status`: one block per service. Header `=== <type> ===`, then `container`, `volume`, `reused` (up only), `state` (status only), then each endpoint as `<role>.url`, `<role>.host_port`.
- `list`: one row per container. Columns `TYPE  CONTAINER  REPO  WORKTREE  STATE  ENDPOINTS`. The endpoints column is `role=hostport` joined by spaces, truncated at 60 chars with an ellipsis.
- `--json` continues to print raw JSON.

The CLI's `pgpoolcli up --image=...` flag is ambiguous in the multi-service world (which service does it apply to?) - drop it. The server's `--image` flag (which sets the postgres image default) remains and now applies only to the postgres entry in the registry. SeaweedFS has no image-override flag in v1; add one when needed.

The CLAUDE.md integration block written by `pgpoolcli init` and the `prime` text both get rewritten to reflect multi-service usage.

### Lifecycle

Per service, in request order:

1. Resolve names from registry (`<containerPrefix>-...`, `<volumePrefix>-...`).
2. `docker inspect` container.
3. existing+running → mark `reused`. existing+stopped → `docker start` + readiness probe. missing → `docker volume create` + `docker run` + readiness probe.
4. After running, fetch host ports for every declared endpoint; populate response entry.

Services run sequentially. If service N fails, services 1..N-1 stay up. Re-running `up` is idempotent and picks up where it left off.

The existing "leave the failed container running so the user can `docker logs`" behavior carries over per-service, with the same "include last 50 log lines in the error" treatment.

### Error handling

- **Validation errors (400):** unknown service name, empty `services: []`, missing `repo`/`worktree`. No docker action attempted.
- **Per-service runtime error (500):** image pull / port allocation / readiness timeout. Response body includes the error and any partial successes:

```json
{
  "error": "seaweedfs: postgres readiness timed out after 30s\n<last 50 log lines>",
  "services": [{"type": "postgres", "container": "pg-foo-bar", ...}]
}
```

### Testing

Unit (no docker required):

- Each registered `ServiceDef` has non-empty `Type`, `ContainerPrefix`, `VolumePrefix`, `Image`, at least one `Endpoint`, non-nil `Readiness` and `BuildURL`.
- `BuildURL` covers every role in `Endpoints`.
- Naming: `<prefix>-<repo>-<worktree>` round-trips through normalization; long names hit the SHA-truncation path independently per prefix.
- Request validation: empty `services` rejected; unknown service rejected with the offending name in the error.
- Response shape: a stub `up` builds the exact schema documented above for postgres-only and postgres+seaweedfs.
- `list` legacy fallback: a container with `pgpool=true` but no `pgpool.service` label maps to `type: "postgres"`.

Integration (gated on `docker` available):

- postgres: up → status → down preserves the existing single-service test coverage.
- seaweedfs: up → status → down. Assert all four host ports respond on `master`/`volume`/`filer`/`s3` endpoints (HTTP 200 from `/cluster/status` on master; HTTP 200 from filer root; S3 list-buckets returns 200 with empty list).
- multi-service `up`: postgres + seaweedfs in one call, both reach `running` state, both URL sets are present and reachable.
- additivity: `up postgres`, then `up seaweedfs`, then `status` shows both running.
- scoped `down`: `down postgres` removes pg-* but leaves weed-* untouched.

## Migration

This is a breaking change to the wire format. The only client is the in-tree `pgpoolcli` (and any MCP-aware client the user has wired up); both ship in this PR.

Refactor strategy:

1. Introduce `ServiceDef` and the registry. Postgres becomes the first registered entry; the existing postgres-specific code paths (`opUp`, `opDown`, etc.) become generic operations that take a `ServiceDef`.
2. Add seaweedfs as the second registered entry.
3. Add `--services` flag and request-side `services` field.
4. Update CLI to new request/response shapes; rewrite `init` claudeSegment and `prime` text.
5. Update `index.html` web UI (already embedded) to render the new response shape: a service-grouped view in place of the single-postgres status panel.
6. Update README and CLAUDE.md project docs.

Single PR. No long-lived feature branch.

## Out of scope (intentionally deferred)

- Per-repo config files (`.pgpool.json` at repo root). Add when more than one project on a host wants different stacks.
- Per-service distinct credentials.
- Adding redis / valkey - registry is ready, entries aren't.
- Server-side persistence beyond Docker labels.
- Auth / TLS on the HTTP endpoint.
