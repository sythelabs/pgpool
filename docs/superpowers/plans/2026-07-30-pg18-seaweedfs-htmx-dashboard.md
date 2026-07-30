# PG18, SeaweedFS 4.40, and HTMX Dashboard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship pgpool v2.5.0 with PG18 pgvector and SeaweedFS 4.40 defaults, plus a server-rendered HTMX lifecycle dashboard; deploy it to the pgpool host and destructively recreate existing ephemeral workloads.

**Architecture:** Keep REST and MCP JSON contracts unchanged. Add embedded HTMX and HTML-only UI routes that call existing `Server` lifecycle operations and render `html/template` fragments. Resolve the configured Postgres image before generic service creation so the existing server image flag, per-worktree override, CLI, UI, and Ansible all select the same PG18 pgvector image.

**Tech Stack:** Go 1.26 stdlib (`net/http`, `html/template`, `embed`), HTMX 2.0.10 as a pinned embedded static asset, Docker CLI, GitHub Actions releases, Ansible Core via `uvx`.

## Global Constraints

- Keep `cmd/pgpool/pgpool.go` as the server's single Go source file; do not add Go dependencies.
- Use `pgvector/pgvector:pg18` as the default Postgres image and `chrislusf/seaweedfs:4.40` as the SeaweedFS image.
- Keep the PG data volume mount at `/var/lib/postgresql`.
- Preserve every JSON REST and MCP response/operation contract.
- Serve the exact pinned HTMX 2.0.10 asset locally; do not add Node, npm, a CDN dependency, or a frontend build step.
- UI actions only invoke existing up, down, reload, status, logs, and list lifecycle operations.
- Down and reload must visibly require browser confirmation because they destroy volumes.
- Do not stage or alter the existing unrelated changes in `../infrastructure/roles/pgpool/tasks/main.yml` or `../infrastructure/proxmox-jobs.sqlite3-*`.
- Release `v2.5.0` before changing the infrastructure release pin.
- Use `uvx --from ansible-core ansible-playbook ...` (or the repository's `just pgpool` wrapper) for Ansible.

---

## File structure

| File | Responsibility |
| --- | --- |
| `cmd/pgpool/pgpool.go` | Image-default constants, configured-image resolution, embedded static asset route, UI request parsing, view models, HTML rendering, and UI handlers. |
| `cmd/pgpool/index.html` | Full dashboard shell, local HTMX import, styles, up form, and HTMX fragment target. |
| `cmd/pgpool/htmx-2.0.10.min.js` | Exact upstream HTMX 2.0.10 browser asset served by pgpool. |
| `cmd/pgpool/pgpool_test.go` | Unit coverage for image resolution and UI HTML handlers using injected Docker fakes. |
| `cmd/pgpool/integration_test.go` | Docker-backed PG18/pgvector and SeaweedFS 4.40 lifecycle coverage. |
| `README.md`, `CLAUDE.md`, `cmd/pgpoolcli/pgpoolcli.go`, `cmd/pgpoolcli/pgpoolcli_test.go` | User-facing default-image references and CLI examples. |
| `../infrastructure/host_vars/pgpool.yml` | Published release pin and server PG image configuration. |

## Task 1: Make configured PG18 pgvector and SeaweedFS 4.40 the tested defaults

**Files:**
- Modify: `cmd/pgpool/pgpool.go: service registry, image selection in opUp/opReload, main defaults`
- Modify: `cmd/pgpool/pgpool_test.go: image-resolution tests`
- Modify: `cmd/pgpool/integration_test.go: test server configuration and PostgreSQL extension assertion`
- Modify: `README.md: PG image examples`
- Modify: `CLAUDE.md: --image default table`
- Modify: `cmd/pgpoolcli/pgpoolcli.go: prime text and help examples`
- Modify: `cmd/pgpoolcli/pgpoolcli_test.go: expected example tags`

**Interfaces:**
- Produces: `const defaultPostgresImage = "pgvector/pgvector:pg18"` and `const defaultSeaweedfsImage = "chrislusf/seaweedfs:4.40"`.
- Produces: `func (s *Server) imageFor(def ServiceDef, override string) string`, which returns the explicit override only for Postgres, otherwise `s.cfg.PgImage` for Postgres when set, otherwise `def.Image`.
- Consumes: `UpRequest.Image` and `ReloadRequest.Image` without changing their JSON shape.

- [ ] **Step 1: Add failing tests for defaults and configured-image precedence**

Add the following test next to `TestServiceRegistry_Validity` in `cmd/pgpool/pgpool_test.go`:

```go
func TestServiceRegistry_UsesPG18PgvectorAndSeaweedFS440(t *testing.T) {
	if got := serviceDefs["postgres"].Image; got != "pgvector/pgvector:pg18" {
		t.Errorf("postgres image = %q, want pgvector/pgvector:pg18", got)
	}
	if got := serviceDefs["seaweedfs"].Image; got != "chrislusf/seaweedfs:4.40" {
		t.Errorf("seaweedfs image = %q, want chrislusf/seaweedfs:4.40", got)
	}
}

func TestImageFor_UsesConfiguredPostgresImageAndExplicitOverride(t *testing.T) {
	s := &Server{cfg: Config{PgImage: "pgvector/pgvector:pg18"}}
	if got := s.imageFor(postgresDef, ""); got != "pgvector/pgvector:pg18" {
		t.Errorf("configured postgres image = %q", got)
	}
	if got := s.imageFor(postgresDef, "pgvector/pgvector:pg18-bookworm"); got != "pgvector/pgvector:pg18-bookworm" {
		t.Errorf("explicit postgres image = %q", got)
	}
	if got := s.imageFor(seaweedfsDef, "ignored:tag"); got != "chrislusf/seaweedfs:4.40" {
		t.Errorf("seaweedfs image = %q", got)
	}
}
```

- [ ] **Step 2: Run the new unit tests and confirm they fail**

Run:

```bash
go test ./cmd/pgpool -run 'TestServiceRegistry_UsesPG18PgvectorAndSeaweedFS440|TestImageFor_UsesConfiguredPostgresImageAndExplicitOverride' -count=1
```

Expected: FAIL because the old image literals remain and `imageFor` is undefined.

- [ ] **Step 3: Centralize the defaults and make `--image` work as documented**

In `cmd/pgpool/pgpool.go`:

1. Define the two constants immediately before `postgresDef`:

```go
const (
	defaultPostgresImage  = "pgvector/pgvector:pg18"
	defaultSeaweedfsImage = "chrislusf/seaweedfs:4.40"
)
```

2. Use those constants for `postgresDef.Image`, `seaweedfsDef.Image`, and the fallback passed to `getenv("PGPOOL_IMAGE", ...)` in `main`.
3. Add this method before `opUp`:

```go
func (s *Server) imageFor(def ServiceDef, override string) string {
	if def.Type != "postgres" {
		return def.Image
	}
	if override != "" {
		return override
	}
	if s.cfg.PgImage != "" {
		return s.cfg.PgImage
	}
	return def.Image
}
```

4. In both `opUp` and `opReload`, replace the inline Postgres-only `image := ""` selection with `s.imageFor(def, req.Image)` when calling `serviceUp`.

This fixes the pre-existing bug where `Config.PgImage` (and therefore `--image`/`PGPOOL_IMAGE`/the Ansible unit) was parsed but never used for a request without an explicit per-call image. Non-Postgres services continue to ignore the request image exactly as documented.

- [ ] **Step 4: Run the image-resolution tests and the full unit suite**

Run:

```bash
go test ./cmd/pgpool -count=1
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 5: Make Docker-backed integration test actual PG18 pgvector support**

Set `PgImage: defaultPostgresImage` in the `Config` returned by `newTestServer`. In `TestIntegration_PostgresLifecycle`, after the running-state assertion, run:

```go
out, errOut, err := s.runDocker(ctx, "exec", up.Services[0].Container,
	"psql", "-v", "ON_ERROR_STOP=1", "-U", s.cfg.PgUser, "-d", s.cfg.PgDB,
	"-c", "CREATE EXTENSION IF NOT EXISTS vector; SELECT 1 FROM pg_extension WHERE extname = 'vector';",
)
if err != nil {
	t.Fatalf("create vector extension: %v: %s", err, errOut)
}
if !strings.Contains(out, "1") {
	t.Fatalf("vector extension was not listed: %q", out)
}
```

In `TestIntegration_SeaweedfsLifecycle`, after `up` succeeds, inspect `up.Services[0].Container` and assert the output of `docker inspect --format {{.Config.Image}}` equals `defaultSeaweedfsImage`.

- [ ] **Step 6: Run the targeted Docker integration tests**

Run:

```bash
go test -tags=integration ./cmd/pgpool -run 'TestIntegration_PostgresLifecycle|TestIntegration_SeaweedfsLifecycle' -count=1 -v
```

Expected: PASS, including `CREATE EXTENSION vector` against a Postgres 18 container and an inspect result of `chrislusf/seaweedfs:4.40`. If Docker is unavailable, the tests report `SKIP`; record that and perform the same checks during the host smoke test.

- [ ] **Step 7: Update only image-version documentation and examples**

Make these exact substitutions:

- `CLAUDE.md`: table default `postgres:17` → `pgvector/pgvector:pg18`.
- `README.md`: each `pgvector/pgvector:pg17` example → `pgvector/pgvector:pg18`.
- `cmd/pgpoolcli/pgpoolcli.go`: every PG17 example/default in `primeText`, CLI help, and integration block → `pgvector/pgvector:pg18`; SeaweedFS catalog value `3.71` → `4.40`.
- `cmd/pgpoolcli/pgpoolcli_test.go`: update only literal image expectations/examples from `pgvector/pgvector:pg17` to `pgvector/pgvector:pg18`.

Do not change public command syntax or JSON shapes.

- [ ] **Step 8: Run formatting, all tests, and build both binaries**

Run:

```bash
gofmt -w cmd/pgpool/pgpool.go cmd/pgpool/pgpool_test.go cmd/pgpool/integration_test.go cmd/pgpoolcli/pgpoolcli.go cmd/pgpoolcli/pgpoolcli_test.go
go test ./... -count=1
go build -o /tmp/pgpool ./cmd/pgpool
go build -o /tmp/pgpoolcli ./cmd/pgpoolcli
```

Expected: all commands exit 0.

- [ ] **Step 9: Commit the image upgrade**

```bash
git add CLAUDE.md README.md cmd/pgpool/pgpool.go cmd/pgpool/pgpool_test.go cmd/pgpool/integration_test.go cmd/pgpoolcli/pgpoolcli.go cmd/pgpoolcli/pgpoolcli_test.go
git commit -m "feat: default pgpool services to pg18 and seaweedfs 4.40"
```

## Task 2: Replace the raw JSON page with embedded HTMX lifecycle controls

**Files:**
- Create: `cmd/pgpool/htmx-2.0.10.min.js`
- Modify: `cmd/pgpool/index.html: dashboard shell and styles`
- Modify: `cmd/pgpool/pgpool.go: embeds, UI view models, templates, UI handlers, routes`
- Modify: `cmd/pgpool/pgpool_test.go: UI handler and escaping tests`

**Interfaces:**
- Produces: `GET /assets/htmx-2.0.10.min.js` with JavaScript content type and the exact embedded asset.
- Produces: `GET /ui/dashboard`, `POST /ui/up`, `POST /ui/down`, `POST /ui/reload`, `GET /ui/status`, and `GET /ui/logs`; all return HTML only.
- Produces: `func (s *Server) renderDashboard(ctx context.Context, notice uiNotice) ([]byte, error)` and `func uiRequestFromForm(r *http.Request) (repo, worktree, image string, services []string, err error)`.
- Consumes: existing `opUp`, `opDown`, `opReload`, `opStatus`, `opLogs`, and `listContainers` functions. No UI route may invoke Docker directly.

- [ ] **Step 1: Add failing handler tests for local HTMX, dashboard rendering, mutation errors, and escaping**

Add HTTP tests in `cmd/pgpool/pgpool_test.go` using `httptest.NewRecorder` and a `Server` with a fake Docker runner. Cover these exact cases:

```go
func TestHandleHTMX_ServesPinnedLocalAsset(t *testing.T) {
	// GET /assets/htmx-2.0.10.min.js returns 200, a JavaScript Content-Type,
	// and a body containing "htmx".
}

func TestHandleUIDashboard_RendersLifecycleTable(t *testing.T) {
	// A fake `docker ps` row for postgres renders repo/worktree, endpoint URL,
	// and Status, Logs, Reload, and Down controls; it does not render JSON braces.
}

func TestHandleUIUp_UsesFormImageAndReturnsDashboard(t *testing.T) {
	// POST /ui/up with repo, worktree, services=postgres, and image=pgvector/pgvector:pg18-bookworm
	// calls the normal lifecycle fake and returns an HTML success notice.
}

func TestHandleUIDashboard_EscapesDockerLabelsAndLogs(t *testing.T) {
	// A repo and log value containing <script>alert(1)</script> are emitted as
	// &lt;script&gt;alert(1)&lt;/script&gt; and never as executable markup.
}
```

The Docker fake must handle `ps`, `inspect`, `port`, `logs`, `volume create`, `run`, `exec`, `rm`, and `volume rm` with the existing `dockerExec` injection point. For the `ps` response, return one JSON line matching `dockerPSRow`, including labels `pgpool=true`, `pgpool.repo=repo`, `pgpool.worktree=worktree`, and `pgpool.service=postgres`.

- [ ] **Step 2: Run the UI tests and confirm they fail**

Run:

```bash
go test ./cmd/pgpool -run 'TestHandleHTMX|TestHandleUI' -count=1
```

Expected: FAIL because no UI asset route or handlers exist.

- [ ] **Step 3: Vendor and embed exactly HTMX 2.0.10**

Download the published npm tarball and extract only the minified browser distribution:

```bash
tmpdir="$(mktemp -d)"
expected='kdeJe7ZVwaS6QMz/ebBIVtZdpwen6L0OQ5GOhPV9MKBb196TCZeZu4yA7ZIQsaLKv7EpXz+So7KSXNuHXhj7Cw=='
curl -fsSL https://registry.npmjs.org/htmx.org/-/htmx.org-2.0.10.tgz -o "$tmpdir/htmx.tgz"
actual="$(openssl dgst -sha512 -binary "$tmpdir/htmx.tgz" | openssl base64 -A)"
test "$actual" = "$expected"
tar -xzf "$tmpdir/htmx.tgz" -C "$tmpdir" package/dist/htmx.min.js
cp "$tmpdir/package/dist/htmx.min.js" cmd/pgpool/htmx-2.0.10.min.js
rm -rf "$tmpdir"
```

Before extraction, verify the tarball's SHA-512 digest encoded as base64 equals the value in `expected-integrity.txt` (the npm registry's published integrity digest). Keep no npm metadata, `node_modules`, or build configuration in the repository.

Add:

```go
//go:embed htmx-2.0.10.min.js
var htmxJS []byte
```

next to the existing `indexHTML` embed. Add `handleHTMX` to set `Content-Type: application/javascript; charset=utf-8` and write `htmxJS`.

- [ ] **Step 4: Build the server-rendered dashboard shell**

Replace `cmd/pgpool/index.html` with a semantic full page that:

- Imports `/assets/htmx-2.0.10.min.js` with `defer`.
- Uses embedded CSS only; no external font, stylesheet, or script.
- Explains that Down and Reload destroy ephemeral volumes.
- Contains an Up form whose `hx-post` is `/ui/up` and whose target is `#dashboard`.
- Includes checkboxes named `services` for postgres, seaweedfs, and fake-gcs, an `image` input labelled “Postgres image override”, and required repo/worktree inputs.
- Contains `<main id="dashboard" hx-get="/ui/dashboard" hx-trigger="load, every 15s" hx-swap="outerHTML">` as the fragment target.

Use normal forms and buttons so controls remain meaningful if HTMX fails to load. Do not retain fetch, `JSON.stringify`, JSON parsing, or custom lifecycle JavaScript.

- [ ] **Step 5: Add typed UI view models and template rendering**

In `cmd/pgpool/pgpool.go`, import `html/template` and define these types near the existing response types:

```go
type uiNotice struct {
	Kind    string // "success" or "error"
	Message string
}

type uiWorktree struct {
	Repo     string
	Worktree string
	Services []ListedContainer
}

type uiDashboardData struct {
	Notice    uiNotice
	Worktrees []uiWorktree
}
```

Create package-level parsed templates for a dashboard fragment and a detail-cell fragment. The dashboard template must render the table as escaped HTML, group container rows by `(Repo, Worktree)`, use endpoint URLs as link destinations/text, include service-specific hidden lifecycle fields, and use `hx-confirm="This destroys the service volume. Continue?"` on both Reload and Down forms.

Add `renderDashboard` to call `listContainers`, group its results in stable repo/worktree/type order, and execute the dashboard template. If the list operation fails, return the error to the handler; the handler must return a small escaped `role="alert"` fragment with HTTP 500 rather than JSON.

Use a hash-derived DOM ID helper for each `(repo, worktree, service)` detail target; never use user-controlled labels directly as HTML IDs.

- [ ] **Step 6: Add form parsing and UI handlers that reuse the lifecycle operations**

Add `uiRequestFromForm` with this behavior:

```go
func uiRequestFromForm(r *http.Request) (repo, worktree, image string, services []string, err error) {
	if err := r.ParseForm(); err != nil {
		return "", "", "", nil, fmt.Errorf("parse form: %w", err)
	}
	repo, worktree, image = strings.TrimSpace(r.FormValue("repo")), strings.TrimSpace(r.FormValue("worktree")), strings.TrimSpace(r.FormValue("image"))
	if repo == "" || worktree == "" {
		return "", "", "", nil, errors.New("repo and worktree are required")
	}
	for _, value := range r.Form["services"] {
		services = append(services, parseServicesCSV(value)...)
	}
	return repo, worktree, image, services, nil
}
```

Implement the handlers as follows:

- `handleUIDashboard`: call `renderDashboard(ctx, uiNotice{})`.
- `handleUIUp`: parse form, call `opUp(ctx, UpRequest{Repo: repo, Worktree: worktree, Services: services, Image: image})`, then render a success or error notice plus dashboard.
- `handleUIDown`: parse form, call `opDown` with the submitted service set, then render a success or error notice plus dashboard.
- `handleUIReload`: parse form, call `opReload` with the submitted image/service set, then render a success or error notice plus dashboard. Preserve partial-success error text from the existing operation.
- `handleUIStatus`: require `repo`, `worktree`, and `service` query fields, call `opStatus`, and render only the selected row's escaped state/container/volume/endpoints as a detail cell.
- `handleUILogs`: require the same query fields, pass optional `tail` through `parseTailParam`, call `opLogs`, and render an escaped `<pre>` log detail cell.

For valid requests that fail at the lifecycle layer, respond with HTML and a 200 status so HTMX swaps the visible error notice. For malformed fields or a dashboard-rendering failure, use 400 or 500 with an escaped `role="alert"` HTML fragment. JSON handler behavior must not change.

Register these exact routes in `main`:

```go
mux.HandleFunc("GET /assets/htmx-2.0.10.min.js", srv.handleHTMX)
mux.HandleFunc("GET /ui/dashboard", srv.handleUIDashboard)
mux.HandleFunc("POST /ui/up", srv.handleUIUp)
mux.HandleFunc("POST /ui/down", srv.handleUIDown)
mux.HandleFunc("POST /ui/reload", srv.handleUIReload)
mux.HandleFunc("GET /ui/status", srv.handleUIStatus)
mux.HandleFunc("GET /ui/logs", srv.handleUILogs)
```

- [ ] **Step 7: Run the focused UI tests and complete their assertions**

Run:

```bash
gofmt -w cmd/pgpool/pgpool.go cmd/pgpool/pgpool_test.go
go test ./cmd/pgpool -run 'TestHandleHTMX|TestHandleUI' -count=1 -v
```

Expected: PASS. Confirm the escape test includes no literal `<script>` in the response and that the error-response test has `role="alert"` with no JSON object body.

- [ ] **Step 8: Run full tests and manually inspect the local dashboard**

Run:

```bash
go test ./... -count=1
go build -o /tmp/pgpool ./cmd/pgpool
/tmp/pgpool --pg-password local-test --services postgres,seaweedfs,fake-gcs --listen 127.0.0.1:18080 >/tmp/pgpool-ui.log 2>&1 &
pid=$!
trap 'kill "$pid" 2>/dev/null || true' EXIT
curl -fsS http://127.0.0.1:18080/ | rg 'htmx-2.0.10.min.js|hx-post="/ui/up"|id="dashboard"'
curl -fsS http://127.0.0.1:18080/assets/htmx-2.0.10.min.js | head -c 40
echo
curl -fsS http://127.0.0.1:18080/ui/dashboard | rg 'No managed worktrees|dashboard'
kill "$pid"
wait "$pid" 2>/dev/null || true
trap - EXIT
```

Expected: all commands exit 0; the UI shell loads local HTMX and the empty dashboard is readable HTML.

- [ ] **Step 9: Commit the dashboard**

```bash
git add cmd/pgpool/index.html cmd/pgpool/htmx-2.0.10.min.js cmd/pgpool/pgpool.go cmd/pgpool/pgpool_test.go
git commit -m "feat: add htmx pgpool lifecycle dashboard"
```

## Task 3: Final source verification and publish v2.5.0

**Files:**
- No source changes expected.

**Interfaces:**
- Produces: Git tag `v2.5.0` and a successful GitHub release containing `pgpool-v2.5.0-linux-amd64.tar.gz`.
- Consumes: Tasks 1 and 2 merged to `main`.

- [ ] **Step 1: Inspect the final source diff and test status**

Run:

```bash
git status --short
git log --oneline v2.4.1..HEAD
git diff --check v2.4.1..HEAD
go test ./... -count=1
go test -tags=integration ./cmd/pgpool -count=1 -v
go build -o /tmp/pgpool ./cmd/pgpool
go build -o /tmp/pgpoolcli ./cmd/pgpoolcli
```

Expected: no uncommitted source changes, no whitespace errors, unit tests pass, integration tests pass or explicitly skip only because Docker is unavailable, and both builds succeed.

- [ ] **Step 2: Merge the feature branch to `main` and push source commits**

From the primary pgpool worktree:

```bash
git checkout main
git pull --ff-only origin main
git merge --ff-only <feature-branch>
git push origin main
```

Expected: `main` contains the image and dashboard commits. Replace `<feature-branch>` with the branch created for these tasks; do not use a merge commit.

- [ ] **Step 3: Create and push the release tag**

```bash
git tag -a v2.5.0 -m "v2.5.0 - PG18 pgvector, SeaweedFS 4.40, HTMX dashboard"
git push origin v2.5.0
```

Expected: the tag triggers `.github/workflows/release.yml`.

- [ ] **Step 4: Wait for the release workflow and verify Linux artifact publication**

Run:

```bash
run_id="$(gh run list --repo sythelabs/pgpool --workflow release.yml --branch v2.5.0 --limit 1 --json databaseId --jq '.[0].databaseId')"
test -n "$run_id"
gh run watch "$run_id" --repo sythelabs/pgpool --exit-status
gh release view v2.5.0 --repo sythelabs/pgpool --json tagName,isDraft,isPrerelease,assets,url
```

Expected: workflow success; `isDraft` and `isPrerelease` are false; assets include `pgpool-v2.5.0-linux-amd64.tar.gz` and `SHA256SUMS`.

## Task 4: Pin and deploy v2.5.0 through infrastructure

**Files:**
- Modify: `../infrastructure/host_vars/pgpool.yml`

**Interfaces:**
- Produces: Ansible configuration that downloads v2.5.0 and runs pgpool with `pgvector/pgvector:pg18`.
- Consumes: published GitHub release artifact from Task 3.

- [ ] **Step 1: Inspect and preserve unrelated infrastructure changes**

Run:

```bash
git -C ../infrastructure status --short
git -C ../infrastructure diff -- roles/pgpool/tasks/main.yml
```

Expected: observe and leave untouched the pre-existing `roles/pgpool/tasks/main.yml` edit and `proxmox-jobs.sqlite3-shm` / `proxmox-jobs.sqlite3-wal` files. Do not reset, stash, add, or commit them.

- [ ] **Step 2: Update only the pgpool host variables**

In `../infrastructure/host_vars/pgpool.yml`, make these exact values:

```yaml
pgpool_version: v2.5.0
pgpool_image: pgvector/pgvector:pg18
```

Leave the password, advertise host, reaper settings, Docker users, and enabled service list untouched.

- [ ] **Step 3: Validate Ansible configuration before applying**

Run from `../infrastructure`:

```bash
uvx --from ansible-core ansible-playbook playbook.yml --limit pgpool --syntax-check
uvx --from ansible-core ansible-lint
uvx --from ansible-core ansible-playbook playbook.yml --limit pgpool --check
```

Expected: syntax and lint pass. Check mode identifies the release directory, binary symlink, and/or systemd unit change without reporting an error.

- [ ] **Step 4: Commit and push only the host-variable update**

Run from `../infrastructure`:

```bash
git add host_vars/pgpool.yml
git commit -m "pgpool: deploy v2.5.0 with pgvector pg18"
git push origin main
git status --short
```

Expected: the commit contains only `host_vars/pgpool.yml`; the unrelated pre-existing changes still appear as unstaged/untracked.

- [ ] **Step 5: Apply the pgpool target and verify the new daemon version**

Run from `../infrastructure`:

```bash
just pgpool
ssh leaf@pgpool 'curl -fsS http://localhost:8080/healthz'
```

Expected: Ansible finishes with `failed=0`; health response has `"version":"v2.5.0"`. Do not proceed to destructive migration if either command fails.

## Task 5: Destructively recreate existing workloads and prove the deployed services

**Files:**
- No repository changes expected.

**Interfaces:**
- Consumes: `GET /v1/list`, `POST /v1/reload`, pgpool v2.5.0 deployed on the host.
- Produces: every pre-existing pgpool worktree recreated with its exact service set and the new image defaults.

- [ ] **Step 1: Snapshot the managed worktrees before mutation**

Run:

```bash
ssh leaf@pgpool 'curl -fsS http://localhost:8080/v1/list' | tee /tmp/pgpool-v2.5.0-pre-reload.json
python3 - <<'PY'
import json
items = json.load(open("/tmp/pgpool-v2.5.0-pre-reload.json"))
groups = {}
for item in items:
    groups.setdefault((item["repo"], item["worktree"]), set()).add(item["type"])
for (repo, worktree), services in sorted(groups.items()):
    print(f"{repo}/{worktree}: {','.join(sorted(services))}")
PY
```

Expected: every live worktree appears once with its current, exact service set. Stop if the list request fails or JSON cannot be decoded.

- [ ] **Step 2: Reload each exact service set through the pgpool API**

Run:

```bash
ssh leaf@pgpool 'python3 -' <<'PY'
import json
import urllib.request

base = "http://localhost:8080"
with urllib.request.urlopen(base + "/v1/list", timeout=15) as response:
    items = json.load(response)
groups = {}
for item in items:
    groups.setdefault((item["repo"], item["worktree"]), set()).add(item["type"])
for (repo, worktree), services in sorted(groups.items()):
    payload = json.dumps({
        "repo": repo,
        "worktree": worktree,
        "services": sorted(services),
        "image": "pgvector/pgvector:pg18",
    }).encode()
    request = urllib.request.Request(
        base + "/v1/reload", data=payload,
        headers={"Content-Type": "application/json"}, method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=180) as response:
            body = response.read().decode()
            if response.status != 200:
                raise RuntimeError(f"HTTP {response.status}: {body}")
    except Exception as exc:
        raise SystemExit(f"reload failed for {repo}/{worktree} ({','.join(sorted(services))}): {exc}")
    print(f"reloaded {repo}/{worktree}: {','.join(sorted(services))}")
PY
```

Expected: one successful reload line per snapshot worktree. The script stops at the first failure; do not continue manually past a failure because reload can have partial success and destructive data loss.

- [ ] **Step 3: Verify all recreated containers use the target images**

Run:

```bash
ssh leaf@pgpool 'docker ps -aq --filter label=pgpool=true | xargs -r docker inspect --format "{{.Name}} {{.Config.Image}}" | sort'
```

Expected: every Postgres container reports `pgvector/pgvector:pg18`; every SeaweedFS container reports `chrislusf/seaweedfs:4.40`; fake-gcs remains `fsouza/fake-gcs-server:1.49`.

- [ ] **Step 4: Create, test, and remove a dedicated full-service smoke worktree**

Run:

```bash
ssh leaf@pgpool 'pgpoolcli up --url http://localhost:8080 --repo pgpool-smoke --worktree v2-5-0 postgres seaweedfs fake-gcs'
ssh leaf@pgpool 'docker ps -q --filter label=pgpool.repo=pgpool-smoke --filter label=pgpool.worktree=v2-5-0 --filter label=pgpool.service=postgres | xargs -r docker exec -i {} psql -v ON_ERROR_STOP=1 -U postgres -d postgres -c "CREATE EXTENSION IF NOT EXISTS vector; SELECT extname FROM pg_extension WHERE extname = '\''vector'\'';"'
ssh leaf@pgpool 'pgpoolcli status --url http://localhost:8080 --repo pgpool-smoke --worktree v2-5-0 --json'
ssh leaf@pgpool 'pgpoolcli down --url http://localhost:8080 --repo pgpool-smoke --worktree v2-5-0 postgres seaweedfs fake-gcs'
```

Expected: up and status show all three services; `CREATE EXTENSION` reports `vector`; down succeeds and leaves no smoke containers or volumes.

- [ ] **Step 5: Verify the dashboard and Ansible idempotence**

Run:

```bash
curl -fsS http://pgpool.tail22511b.ts.net:8080/ | rg 'htmx-2.0.10.min.js|hx-post="/ui/up"|id="dashboard"'
curl -fsS http://pgpool.tail22511b.ts.net:8080/ui/dashboard | rg 'Status|Logs|Reload|Down'
cd ../infrastructure && just pgpool
```

Expected: the page references only local HTMX; the dashboard exposes lifecycle controls without raw JSON; the second Ansible apply succeeds and reports no pgpool role changes.

- [ ] **Step 6: Record final evidence**

Run:

```bash
ssh leaf@pgpool 'curl -fsS http://localhost:8080/healthz; echo; curl -fsS http://localhost:8080/v1/list'
git -C /Users/jarredparr/Projects/network/pgpool status --short --branch
git -C /Users/jarredparr/Projects/network/infrastructure status --short --branch
```

Expected: health is v2.5.0, the service list is healthy, pgpool source is clean on pushed main, and infrastructure has only its pre-existing unrelated unstaged/untracked changes.
