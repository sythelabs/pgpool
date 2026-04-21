# pgpool

Single-binary server that manages ephemeral PostgreSQL containers on its host,
plus a thin CLI (`pgpoolcli`) for clients. Clients speak HTTP (REST or MCP
JSON-RPC); the server shells out to `docker`. This system is designed for
multi-agent workflows where you have lots of agents thrashing on features.
It gets really annoying when they all try to spin up docker.

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
./pgpool --pg-password hunter2 --advertise-host pgpool.tailnet.ts.net
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
pgpoolcli up          # create or reuse a Postgres container for this worktree
pgpoolcli status      # show state and connection URL
pgpoolcli list        # every pgpool-managed container on the server
pgpoolcli down        # destroy this worktree's container and volume
```

`up` is idempotent. `down` destroys the volume - data is gone.

`repo` and `worktree` auto-detect:

- `repo`: basename of the `origin` remote URL, else basename of the git toplevel
- `worktree`: basename of `$PWD`

Override with flags on any command:

```
pgpoolcli up --repo myrepo --worktree feature-x
pgpoolcli up --image postgres:16        # override the Postgres image
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
<!-- BEGIN PGPOOL INTEGRATION v:1 -->
## Postgres Pool (pgpool)
This project uses **pgpoolcli** to manage ephemeral Postgres containers per worktree.
Run `pgpoolcli prime` to see full workflow context and commands.
### Quick Reference
` ``bash
pgpoolcli up                # Create or reuse a Postgres container for this worktree
pgpoolcli status            # Show current state and connection URL
pgpoolcli list              # List all pgpool-managed containers
pgpoolcli down              # Destroy the container and its volume
` ``
Repo and worktree auto-detect from git. Override with `--repo` / `--worktree`.
### Rules
- Use `pgpoolcli` to manage per-worktree databases - do NOT hand-run `docker` commands against pgpool containers
- `pgpoolcli up` is idempotent - safe to run multiple times, does not wipe data
- `pgpoolcli down` destroys the volume - data is NOT recoverable
- The server does not write `.env` files - read the URL from `up`/`status` and write your own
- One container per (repo, worktree) pair - names are derived, not chosen
<!-- END PGPOOL INTEGRATION -->
```

(The ` `` ` in the snippet above is shown with a space so GitHub renders the
README correctly. The real block - and what `pgpoolcli init` writes - uses
plain triple backticks.)

## REST and MCP endpoints (reference)

The CLI is a thin wrapper. If you want to hit the server directly:

```
POST /v1/up      {"repo","worktree","image?"}
POST /v1/down    {"repo","worktree"}
GET  /v1/status  ?repo=X&worktree=Y
GET  /v1/list
GET  /healthz
POST /mcp        JSON-RPC 2.0 - tools: pgpool_up, pgpool_down, pgpool_status, pgpool_list
```

## Security posture

- No auth on the HTTP endpoint in v1. Bind to a private network (Tailnet or
  loopback). Do **not** expose the port to the public internet.
- The Postgres superuser password is shared across all containers on a given
  server. Acceptable only on a trusted network.
