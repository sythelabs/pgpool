# pgpool: fake-gcs-server profile and reload command

Add a third service (`fake-gcs`, https://github.com/fsouza/fake-gcs-server) to the registry and a `reload` lifecycle verb that is `down`-then-`up` for a worktree's services. Adding fake-gcs forces one structural change: the registry's per-service func fields move from positional args to a single `ServiceBuildCtx` struct, because fake-gcs needs its host port baked into its CLI flags at container-start time.

## Goals

- New service profile `fake-gcs` runs `fsouza/fake-gcs-server` with a working `-external-url` so download/media-link URLs returned by the storage API are reachable from clients.
- New endpoint `POST /v1/reload` and matching `pgpool_reload` MCP tool. CLI verb `pgpoolcli reload [SERVICE...]` mirrors `up` / `down` semantics.
- Registry func signatures unified behind `ServiceBuildCtx` so future services can pick up new inputs without churning every existing service.
- Stdlib-only. Single file per binary preserved. Server stays stateless.

## Non-goals

- A "restart" command that preserves volumes. `reload` destroys volumes (it is literally `down`+`up`); a no-data-loss restart is a separate future verb if anyone asks.
- Auth on fake-gcs (the upstream image supports it; we use the open default).
- HTTPS / self-signed certs for fake-gcs. HTTP only, same posture as seaweedfs.
- Adding fake-gcs to the default service set. Opt in with `--services postgres,fake-gcs`.

## Architecture

### Registry refactor: `ServiceBuildCtx`

Today the `ServiceDef` func fields take fixed positional args (`DockerArgs(cfg, volume)`, `Readiness(ctx, s, container, hostPorts)`, `BuildURL(cfg, role, hostPort)`). Adding a fourth input would force every service to change its signature. Switch to a single context struct:

```go
type ServiceBuildCtx struct {
    Cfg       Config
    Volume    string
    Image     string              // resolved (per-call override applied)
    HostPorts map[string]string   // role -> pre-allocated host port (as string)
}

type ServiceDef struct {
    Type            string
    ContainerPrefix string
    VolumePrefix    string
    Image           string
    Endpoints       []EndpointSpec
    DockerArgs      func(ServiceBuildCtx) []string
    DockerCommand   func(ServiceBuildCtx) []string
    Readiness       func(ctx context.Context, s *Server, container string, bc ServiceBuildCtx) error
    BuildURL        func(bc ServiceBuildCtx, role, hostPort string) string
}
```

Postgres and seaweedfs are converted mechanically - they each read `bc.Cfg`, `bc.Volume`, and (for seaweedfs readiness) `bc.HostPorts` off the struct. No behavior changes for either.

### Port pre-allocation

fake-gcs-server bakes its public URL into responses via `-external-url`. The flag is a static string at process start, so the host port must be known *before* `docker run`. The other two services don't care: their URLs are constructed by the server after Docker assigns an ephemeral port.

New helper, server-side:

```go
// reserveHostPorts opens a listener per endpoint role on 127.0.0.1:0,
// reads the assigned port, closes the listener, and returns role -> port.
// Used on the "create container" path. Existing containers keep their old ports.
func reserveHostPorts(endpoints []EndpointSpec) (map[string]string, error)
```

`serviceUp` flow change (create path only):

1. `reserveHostPorts(def.Endpoints)` -> `hostPorts`.
2. Build `ServiceBuildCtx{Cfg, Volume, Image, HostPorts: hostPorts}`.
3. `docker run -p <hostPorts[role]>:<containerPort>` per endpoint (was `-p 0:<containerPort>`).
4. Pass `bc` to `DockerArgs` / `DockerCommand`.
5. Readiness probe and URL construction use `hostPorts` directly (skip `collectHostPorts` on the create path).

`collectHostPorts` stays. It is still used on the reuse path (existing container, port was assigned at original create time) and by `status`.

**Race window.** There is a small gap between `listener.Close()` and `docker run` binding the port. On a single-user dev host with no competing port-bind activity this is effectively never hit; if it does happen, Docker fails fast with a bind error that surfaces unchanged to the client. We do not retry.

### fake-gcs-server profile

```go
var fakeGCSDef = ServiceDef{
    Type:            "fake-gcs",
    ContainerPrefix: "gcs",
    VolumePrefix:    "gcsvol",
    Image:           "fsouza/fake-gcs-server:1.49",
    Endpoints:       []EndpointSpec{{Role: "storage", ContainerPort: 4443, Scheme: "http"}},
    DockerArgs: func(bc ServiceBuildCtx) []string {
        return []string{"-v", bc.Volume + ":/storage"}
    },
    DockerCommand: func(bc ServiceBuildCtx) []string {
        port := bc.HostPorts["storage"]
        return []string{
            "-scheme", "http",
            "-public-host", bc.Cfg.AdvertiseHost + ":" + port,
            "-external-url", "http://" + bc.Cfg.AdvertiseHost + ":" + port,
        }
    },
    Readiness: func(ctx context.Context, s *Server, container string, bc ServiceBuildCtx) error {
        return s.httpReady(ctx, "http://"+bc.Cfg.AdvertiseHost+":"+bc.HostPorts["storage"]+"/storage/v1/b")
    },
    BuildURL: func(bc ServiceBuildCtx, role, hostPort string) string {
        return "http://" + bc.Cfg.AdvertiseHost + ":" + hostPort
    },
}

func init() {
    serviceDefs[fakeGCSDef.Type] = fakeGCSDef
}
```

- Image pinned to `fsouza/fake-gcs-server:1.49`. No `latest`.
- One endpoint, role `storage`, scheme `http`.
- Volume mounted at `/storage` (the image's default data dir).
- Readiness: `GET /storage/v1/b` (list buckets). 200 + empty JSON when ready.
- URL form: `http://<advertise-host>:<host-port>` (no path, no credentials - clients point their GCS SDK at this base via `STORAGE_EMULATOR_HOST`).

### Reload endpoint

`POST /v1/reload`

Request body: `{"repo":"...","worktree":"...","services":["..."]?,"image":"..."?}`. `services` defaults to the server's configured set; `image` (when present) is applied to the postgres entry only, identical to `/v1/up`.

Response: same shape as `/v1/up` - `{"services":[{type,container,volume,reused,endpoints:{...}}]}`. `reused` is always `false` for reload (the container was freshly created).

Implementation:

1. Resolve services (same `resolveServices` helper as up/down).
2. For each service sequentially: `serviceDown` then `serviceUp`.
3. Aggregate `ServiceResult` per service into the response.
4. Partial-failure semantics match up/down: if service N fails mid-reload, services 1..N-1 are already reloaded; response includes their results plus the error from N.

No new helper functions on `Server` - reuses `serviceDown` and `serviceUp` directly.

### MCP tool

`pgpool_reload` mirrors `pgpool_up`'s schema: required `repo`, `worktree`, optional `services: string[]`, optional `image: string`. Result is a single `text` content block with pretty-printed JSON (same convention as the other tools). Listed by `tools/list` alongside the existing five.

### CLI

`pgpoolcli reload [SERVICE...]` with the same flag set and auto-detection rules as `up` and `down`:

- `--repo`, `--worktree` (defaults from git).
- Positional args narrow to specific services.
- Output mirrors `up`: per-service block with container, volume, state, reused, endpoint URLs.

Wired through `runReload` -> `cmdReload` -> `POST /v1/reload`, structured identically to the existing `runUp` / `cmdUp` pair.

## Documentation updates

- **Repo `CLAUDE.md`**: add `fake-gcs` alongside `postgres` / `seaweedfs` in registry mentions; add `/v1/reload` to the REST section and `pgpool_reload` to the MCP section; add fake-gcs to the container-naming table (prefixes `gcs-` / `gcsvol-`); add a fake-gcs container-port (4443) note.
- **Repo `README.md`**: same updates - service list, transport sections, name prefixes.
- **`pgpoolcli.go` `claudeSegment`**: bump marker version to `v:4` (so `pgpoolcli init` replaces old `v:3` blocks). Add `pgpoolcli reload` rows to the quick reference. Add fake-gcs to the endpoints list. Update the rules block to note that `reload` destroys volumes.
- **`pgpoolcli.go` `primeText`**: add `reload` command entry. Add fake-gcs to the service catalog block with image, endpoint, URL form, and notes (mention `STORAGE_EMULATOR_HOST` as the recommended client config).
- **`pgpoolcli.go` `usage()`**: add `reload` to the command list.

## Testing

- **Unit, `pgpool_test.go`**:
  - `resolveServices` known set now includes `fake-gcs`; existing unknown-service rejection test still passes.
  - `reserveHostPorts` returns one port per endpoint and the returned ports are non-zero. Listeners are closed (assert by re-binding the same ports succeeds).
- **Integration, `integration_test.go` (docker-gated)**:
  - `fake-gcs` up: container reaches ready state; `GET <storage-url>/storage/v1/b` returns 200; create a bucket via `POST /storage/v1/b`, GET it back, assert presence.
  - `reload` postgres: up, write a row, reload, expect the row to be **gone** (asserts the documented "reload destroys volumes" contract so it can't silently regress).
  - `reload` with `services: ["fake-gcs"]` returns one result and the storage endpoint is reachable after reload.
- **CLI unit, `pgpoolcli_test.go`**:
  - `pgpoolcli reload` builds the right request body shape (parity with `up` / `down` tests).
  - `claudeSegment` v:4 replaces a v:3 block via the existing `mergeClaudeBlock` path.

## Migration / compatibility

- Server bump is backwards-compatible at the wire level - new endpoint and a new service type. Old CLIs talking to the new server continue to work; they just don't know about `reload` or `fake-gcs`.
- A new CLI talking to an old server will get a 404 on `/v1/reload`. `pgpoolcli health` already surfaces server version skew, so the failure is explainable.
- `claudeSegment` version bump (`v:3` -> `v:4`) means `pgpoolcli init` rewrites in-place. No user action needed.

## Open issues / future work

- A `pgpoolcli restart` verb (no volume destruction) is a plausible future request and is intentionally not bundled here. If added, it would `docker restart` the container and re-run the readiness probe.
- fake-gcs HTTPS + cert provisioning is out of scope; the upstream image supports it but the dev posture is loopback / Tailnet only, so HTTP suffices.
