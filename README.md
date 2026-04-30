# pgpool

Single-binary server that manages ephemeral per-worktree services (Postgres
and SeaweedFS today, more services pluggable via a Go service registry), plus
a thin CLI (`pgpoolcli`) for clients. Clients speak HTTP (REST or MCP JSON-RPC);
the server shells out to `docker`. Designed for multi-agent workflows where
many agents thrash on features and need isolated, ephemeral infra per worktree.

See `CLAUDE.md` for the full server spec. What follows is how to use it.

## Install

One-liner - grabs the latest release from GitHub and installs both `pgpool` and
`pgpoolcli` into `/usr/local/bin`:

```
curl -fsSL https://raw.githubusercontent.com/sythelabs/pgpool/main/install.sh | sh
```

Linux and macOS, amd64 and arm64. The script resolves the latest tag via the
GitHub releases API, downloads the matching `.tar.gz`, and installs both
binaries. It uses `sudo` only if the install dir is not writable.

Overrides:

```
# pin a specific version
curl -fsSL https://raw.githubusercontent.com/sythelabs/pgpool/main/install.sh | PGPOOL_VERSION=v1.2.3 sh

# install into a user-owned dir (no sudo)
curl -fsSL https://raw.githubusercontent.com/sythelabs/pgpool/main/install.sh | INSTALL_DIR="$HOME/.local/bin" sh
```

Windows users: grab the `.zip` from the
[releases page](https://github.com/sythelabs/pgpool/releases/latest).

## Build from source

```
go build -o pgpool ./cmd/pgpool
go build -o pgpoolcli ./cmd/pgpoolcli
```

Both binaries are `stdlib`-only. No third-party deps.

## Run the server

```
./pgpool --pg-password hunter2 --services postgres,seaweedfs --advertise-host pgpool.tailnet.ts.net
```

`--advertise-host` is the hostname written into connection URLs returned to
clients. Use the Tailscale name / LAN IP that your other machines use to reach
this host. `localhost` only works for same-machine clients.

## Use the CLI

### First-time setup on a client machine

```
pgpoolcli init
```

Interactive. Prompts for the server URL (press Enter to accept the default /
existing value, or paste your deployment URL). Then:

1. Creates `~/.config/pgpool/` if missing and writes `pgpool.json` with your URL.
2. Appends a `pgpool` block to `CLAUDE.md` in the current directory so Claude
   Code (and other agents that read `CLAUDE.md`) know how to use the CLI.
   Re-running is a no-op if the block is already present.

Non-interactive variants:

```
pgpoolcli init --url http://pgpool.tailnet.ts.net:8080   # explicit URL, no prompts
pgpoolcli init --yes                                     # accept defaults, no prompts
pgpoolcli init --force                                   # overwrite an existing config
```

### Per-worktree workflow

Inside a git worktree:

```
pgpoolcli up                # bring up all configured services
pgpoolcli up postgres       # bring up just postgres
pgpoolcli status            # show all services for this worktree
pgpoolcli status seaweedfs  # filter to one service
pgpoolcli logs              # tail logs for every service in this worktree
pgpoolcli logs postgres     # tail logs for one service
pgpoolcli list              # every pgpool-managed container on the server
pgpoolcli down              # tear everything down for this worktree
pgpoolcli down postgres     # tear down only postgres
```

`logs` accepts `--tail N` (default 100, max 5000).

`up` is per-service idempotent. `down` destroys volumes - data is gone.

`repo` and `worktree` auto-detect:

- `repo`: basename of the `origin` remote URL, else basename of the git toplevel
- `worktree`: basename of `$PWD`

Override with flags on any command:

```
pgpoolcli up --repo myrepo --worktree feature-x
```

### Config resolution (highest priority wins)

1. `--url` flag
2. `PGPOOL_URL` env var
3. `url` field in the config file
4. Default: `http://localhost:8080`

Config path resolution:

1. `--config` flag
2. `PGPOOL_CONFIG` env var
3. Default: `~/.config/pgpool/pgpool.json`

### Other commands

```
pgpoolcli health      # liveness probe
pgpoolcli config      # print resolved url + detected repo/worktree
pgpoolcli prime       # full workflow reference (same text agents get)
```

Pass `--json` to any command to get the raw server JSON instead of the
human summary:

```
pgpoolcli up --json
```

## CLAUDE.md integration

Running `pgpoolcli init` in a project appends the block below to `CLAUDE.md`
(or creates the file). You can also paste it in by hand. The begin/end markers
make re-running `init` idempotent - it will not duplicate.

```markdown
<!-- BEGIN PGPOOL INTEGRATION v:3 -->
## Per-worktree services (pgpool)
This project uses **pgpoolcli** to manage ephemeral per-worktree services (Postgres and SeaweedFS supported today).
Run `pgpoolcli prime` for full workflow context including the per-service endpoint catalog.
### Quick reference
```bash
pgpoolcli up                  # bring up all configured services
pgpoolcli up postgres         # just postgres
pgpoolcli status              # show all services for this worktree
pgpoolcli status seaweedfs    # filter to one service
pgpoolcli logs                # tail logs for all services in this worktree
pgpoolcli logs postgres       # tail logs for one service
pgpoolcli list                # all pgpool-managed containers on the host
pgpoolcli down                # tear everything down for this worktree
pgpoolcli down postgres       # tear down only postgres
```
Repo and worktree auto-detect from git. Override with `--repo` / `--worktree`.
### Endpoints
- `postgres`: `primary` role -> `postgresql://USER:PASS@HOST:PORT/DB` (credentials are server-configured).
- `seaweedfs`: `master`, `volume`, `filer`, `s3` roles -> `http://HOST:PORT` per role.
### Rules
- Use `pgpoolcli` to manage per-worktree services - do NOT hand-run `docker` commands against pgpool containers.
- `pgpoolcli up` is per-service idempotent.
- `pgpoolcli down` destroys volumes - data is NOT recoverable.
- The server does not write `.env` files - read endpoint URLs from `up` / `status` and write your own.
- One container per (repo, worktree, service) tuple - names are derived, not chosen.
- If `status` / `up` return empty service lists, the server is older than the CLI. Run `pgpoolcli health` to compare versions.
<!-- END PGPOOL INTEGRATION -->
```

(The ` `` ` in the snippet above is shown with a space so GitHub renders the
README correctly. The real block - and what `pgpoolcli init` writes - uses
plain triple backticks.)

## REST and MCP endpoints (reference)

The CLI is a thin wrapper. If you want to hit the server directly:

```
POST /v1/up      {"repo","worktree","services":[...]?,"image"?}
POST /v1/down    {"repo","worktree","services":[...]?}
GET  /v1/status  ?repo=X&worktree=Y[&service=Z]
GET  /v1/logs    ?repo=X&worktree=Y[&service=Z][&tail=N]
GET  /v1/list
GET  /healthz
POST /mcp        JSON-RPC 2.0 - tools: pgpool_up, pgpool_down, pgpool_status, pgpool_logs, pgpool_list
```

`up` and `down` default to the server's `--services` set when `services` is omitted. Responses always have a `services` array; each entry has its own `endpoints` map keyed by role (`primary` for postgres; `master`/`volume`/`filer`/`s3` for seaweedfs).

## Security posture

- No auth on the HTTP endpoint in v1. Bind to a private network (Tailnet or
  loopback). Do **not** expose the port to the public internet.
- The Postgres superuser password is shared across all containers on a given
  server. Acceptable only on a trusted network.
