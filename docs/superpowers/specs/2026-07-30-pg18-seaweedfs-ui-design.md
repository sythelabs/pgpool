# PG18, SeaweedFS 4.40, and HTMX dashboard design

## Goal

Upgrade pgpool's default services to Postgres 18 with pgvector and SeaweedFS 4.40. Replace the raw-JSON control page with a small, server-rendered HTMX dashboard that manages the existing pgpool lifecycle. Deploy the release through the infrastructure repository and destructively recreate all current ephemeral worktrees.

## Scope

- Default Postgres image becomes `pgvector/pgvector:pg18`.
- Default SeaweedFS image becomes `chrislusf/seaweedfs:4.40`.
- The existing Postgres volume mount at `/var/lib/postgresql` remains unchanged; it is compatible with Postgres 18 and older supported tags.
- JSON REST (`/v1/*`) and MCP behavior remain unchanged.
- The UI exposes only existing lifecycle operations: up, status, logs, reload, down, and list.
- No Docker image-cache management, authentication, API redesign, frontend build tooling, or unrelated service changes are included.

## UI architecture

`cmd/pgpool/index.html` becomes a dashboard shell. A pinned copy of HTMX 2.0.10 is served by pgpool from an embedded static asset, avoiding a browser dependency on a third-party CDN and adding no build tooling.

The Go server adds UI-only HTML fragment routes alongside the unchanged JSON and MCP routes. The UI routes call the existing operation methods (`opUp`, `opDown`, `opReload`, `opStatus`, `opLogs`, and `listContainers`) rather than duplicating Docker lifecycle logic. Go's standard `html/template` package renders escaped fragments.

The dashboard contains:

- An **Up** form with repo, worktree, explicit service selection, and an optional Postgres-only image override. An empty selection preserves the existing server-default service behavior.
- A periodically refreshed worktree table grouped by `(repo, worktree)`. Each managed service displays its state, container, volume, and endpoint links.
- Per-service **Status**, **Logs**, **Reload**, and **Down** controls. Status and logs expand escaped inline detail panels. Reload and down show browser confirmation prompts because they destroy volumes.
- A live notice area for success and failure results. Docker or validation failures render readable error text, never raw JSON.

The full dashboard fragment is refreshed after mutating actions and every 15 seconds. Detail requests update only the selected row so users do not lose table context.

## UI data flow

1. The initial HTML response loads HTMX and requests the dashboard fragment.
2. The fragment renderer obtains the current records through `listContainers` and groups them by worktree.
3. HTMX forms submit URL-encoded data to UI routes. Those routes validate their fields and call the corresponding existing operation.
4. A successful mutation returns a refreshed dashboard fragment and notice. A failed mutation returns the current dashboard plus an inline error notice.
5. Status and logs use their existing read operations and return an escaped service-detail fragment.

The UI has no JSON parsing or custom lifecycle JavaScript. HTMX performs the request and DOM swap; the server remains the only implementation of lifecycle behavior.

## Image upgrade and deployment

The pgpool release is `v2.5.0`, reflecting the new dashboard and upgraded defaults.

After release artifacts have published:

1. Update `../infrastructure/host_vars/pgpool.yml` to `pgpool_version: v2.5.0` and `pgpool_image: pgvector/pgvector:pg18`.
2. Apply only the pgpool Ansible target. The service restart activates the new binary and default configuration without changing running per-worktree containers.
3. Query `/v1/list`, group current records by repo and worktree, and invoke `/v1/reload` for each group with its exact existing service set. Reload deliberately removes existing containers and volumes, then recreates them with the new Postgres and SeaweedFS images. This avoids accidentally enabling a service that a worktree did not previously use.
4. Run and remove a dedicated smoke worktree covering Postgres, SeaweedFS, and fake-gcs. Confirm the Postgres container uses PG18, `CREATE EXTENSION vector` succeeds, SeaweedFS uses 4.40 and reaches readiness, and the health endpoint reports `v2.5.0`.

The destructive migration is a one-time deployment action. pgpool worktree data is documented as ephemeral, and the reaper will clean future stale instances normally.

## Testing and verification

Before tagging:

- Add focused unit tests for the new default image values and HTML UI handlers/fragments, including success and error rendering.
- Run `go test ./...`.
- Run Docker-backed pgpool integration tests. Postgres coverage explicitly creates the `vector` extension; SeaweedFS coverage exercises the existing readiness lifecycle using 4.40.
- Build both binaries.

For infrastructure and production verification:

- Run the Ansible syntax/lint checks and apply the `pgpool` target.
- Verify `healthz`, exact running container image tags, the vector extension, service endpoints, and a clean smoke-worktree teardown.
- Run the pgpool target a second time to verify Ansible idempotence.

## Error handling

Existing JSON/MCP error semantics and partial-success lifecycle behavior are retained. UI routes translate operation errors into an HTTP-friendly HTML notice while retaining visible lifecycle state. Destructive controls require explicit browser confirmation. The rollout stops on release, Ansible, migration, or smoke-test failure rather than proceeding to the next phase.
