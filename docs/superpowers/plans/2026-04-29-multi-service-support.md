# Multi-Service Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Generalize pgpool from "one Postgres container per worktree" to a registry of services per worktree, with SeaweedFS as the second supported service.

**Architecture:** Introduce a Go map of `ServiceDef` structs keyed by service type. Each definition owns its image, container/volume prefixes, endpoint specs, readiness probe, URL builder, and a function that produces extra `docker run` args. Lifecycle operations (`up`, `down`, `status`, `list`) become per-service. Server stays stateless and stdlib-only; state continues to live in Docker labels (now including `pgpool.service=<type>`).

**Tech Stack:** Go 1.26 stdlib only; Docker CLI shell-out; HTTP/REST; MCP JSON-RPC 2.0.

**Spec reference:** `docs/superpowers/specs/2026-04-29-multi-service-design.md`

---

## File Map

- **Modify** `cmd/pgpool/pgpool.go` - the entire server (kept as one file per project convention)
- **Create** `cmd/pgpool/pgpool_test.go` - unit tests for naming, registry, validation, response shape
- **Create** `cmd/pgpool/integration_test.go` - docker-gated tests behind `//go:build integration`
- **Modify** `cmd/pgpool/index.html` - render new services-array response
- **Modify** `cmd/pgpoolcli/pgpoolcli.go` - update to new request/response shapes; positional arg parsing
- **Modify** `CLAUDE.md` - project-level docs reflect multi-service model
- **Modify** `README.md` - usage examples updated

Single file per binary is preserved.

---

## Phase A - Refactor naming and introduce the registry (no behavior change)

### Task 1: Test scaffolding for pure naming functions

**Files:**
- Create: `cmd/pgpool/pgpool_test.go`

- [ ] **Step 1: Write the failing tests**

```go
package main

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"FooBar":          "foobar",
		"foo_bar":         "foo-bar",
		"--foo--bar--":    "foo-bar",
		"a/b/c":           "a-b-c",
		"  spaced  ":      "spaced",
		"":                "",
	}
	for in, want := range cases {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncateWithHash(t *testing.T) {
	short := "abc"
	if got := truncateWithHash(short, 10); got != short {
		t.Errorf("short string changed: %q", got)
	}
	long := strings.Repeat("a", 100)
	got := truncateWithHash(long, 30)
	if len(got) > 30 {
		t.Errorf("len(got) = %d, want <= 30", len(got))
	}
}
```

- [ ] **Step 2: Run tests to verify they pass against current code**

Run: `go test ./cmd/pgpool/...`
Expected: PASS for `TestNormalize` and `TestTruncateWithHash`. (These test existing pure functions before we refactor them.)

- [ ] **Step 3: Commit**

```bash
git add cmd/pgpool/pgpool_test.go
git commit -m "test: cover normalize and truncateWithHash"
```

---

### Task 2: Generalize naming to take a service prefix

**Files:**
- Modify: `cmd/pgpool/pgpool.go` (replace `containerName` and `volumeName`)
- Modify: `cmd/pgpool/pgpool_test.go` (add new tests)

- [ ] **Step 1: Write the failing test for the new signature**

Add to `cmd/pgpool/pgpool_test.go`:

```go
func TestServiceContainerName(t *testing.T) {
	cases := []struct {
		prefix, repo, worktree, want string
	}{
		{"pg", "foo", "bar", "pg-foo-bar"},
		{"weed", "foo", "bar", "weed-foo-bar"},
		{"pg", "Foo_Bar", "BAZ", "pg-foo-bar-baz"},
	}
	for _, tc := range cases {
		got, err := serviceContainerName(tc.prefix, tc.repo, tc.worktree)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != tc.want {
			t.Errorf("serviceContainerName(%q,%q,%q) = %q, want %q",
				tc.prefix, tc.repo, tc.worktree, got, tc.want)
		}
	}
}

func TestServiceContainerName_TruncatesLongNames(t *testing.T) {
	long := strings.Repeat("x", 80)
	got, err := serviceContainerName("pg", "repo", long)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > dockerNameMax {
		t.Errorf("len(%q) = %d, want <= %d", got, len(got), dockerNameMax)
	}
	if !strings.HasPrefix(got, "pg-repo-") {
		t.Errorf("missing expected prefix: %q", got)
	}
}

func TestServiceVolumeName(t *testing.T) {
	got, err := serviceVolumeName("pgvol", "foo", "bar")
	if err != nil {
		t.Fatal(err)
	}
	if got != "pgvol-foo-bar" {
		t.Errorf("got %q, want pgvol-foo-bar", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/pgpool/...`
Expected: FAIL - `serviceContainerName` and `serviceVolumeName` undefined.

- [ ] **Step 3: Replace `containerName` and `volumeName` in `cmd/pgpool/pgpool.go`**

Replace the two functions with:

```go
func serviceContainerName(prefix, repo, worktree string) (string, error) {
	r := normalize(repo)
	w := normalize(worktree)
	if r == "" || w == "" {
		return "", errors.New("repo and worktree must not be empty after normalization")
	}
	name := prefix + "-" + r + "-" + w
	if len(name) > dockerNameMax {
		budget := dockerNameMax - len(prefix+"-"+r+"-")
		w = truncateWithHash(w, budget)
		name = prefix + "-" + r + "-" + w
		log.Printf("pgpool: container name exceeded %d chars, truncated worktree to %q", dockerNameMax, w)
	}
	return name, nil
}

func serviceVolumeName(prefix, repo, worktree string) (string, error) {
	r := normalize(repo)
	w := normalize(worktree)
	if r == "" || w == "" {
		return "", errors.New("repo and worktree must not be empty after normalization")
	}
	name := prefix + "-" + r + "-" + w
	if len(name) > dockerNameMax {
		budget := dockerNameMax - len(prefix+"-"+r+"-")
		w = truncateWithHash(w, budget)
		name = prefix + "-" + r + "-" + w
	}
	return name, nil
}
```

- [ ] **Step 4: Update existing callers in `cmd/pgpool/pgpool.go`**

Replace every call to `containerName(req.Repo, req.Worktree)` with `serviceContainerName("pg", req.Repo, req.Worktree)`. Same for `volumeName` → `serviceVolumeName("pgvol", ...)`. Affected functions: `opUp`, `opDown`, `opStatus`. Verify with: `grep -n 'containerName\|volumeName' cmd/pgpool/pgpool.go` - should print no matches except inside the new function definitions.

- [ ] **Step 5: Run tests**

Run: `go test ./cmd/pgpool/...` and `go build ./...`
Expected: PASS, build succeeds.

- [ ] **Step 6: Commit**

```bash
git add cmd/pgpool/pgpool.go cmd/pgpool/pgpool_test.go
git commit -m "refactor: parameterize container and volume name prefixes"
```

---

### Task 3: Define `ServiceDef`, `EndpointSpec`, and the registry skeleton

**Files:**
- Modify: `cmd/pgpool/pgpool.go` (add types and empty registry)
- Modify: `cmd/pgpool/pgpool_test.go` (add registry validity test)

- [ ] **Step 1: Write the failing test**

Append to `cmd/pgpool/pgpool_test.go`:

```go
func TestServiceRegistry_Validity(t *testing.T) {
	if len(serviceDefs) == 0 {
		t.Fatal("serviceDefs is empty")
	}
	for typ, def := range serviceDefs {
		if def.Type != typ {
			t.Errorf("serviceDefs[%q].Type = %q", typ, def.Type)
		}
		if def.ContainerPrefix == "" {
			t.Errorf("%s: ContainerPrefix is empty", typ)
		}
		if def.VolumePrefix == "" {
			t.Errorf("%s: VolumePrefix is empty", typ)
		}
		if def.Image == "" {
			t.Errorf("%s: Image is empty", typ)
		}
		if len(def.Endpoints) == 0 {
			t.Errorf("%s: Endpoints is empty", typ)
		}
		if def.Readiness == nil {
			t.Errorf("%s: Readiness is nil", typ)
		}
		if def.BuildURL == nil {
			t.Errorf("%s: BuildURL is nil", typ)
		}
		if def.DockerArgs == nil {
			t.Errorf("%s: DockerArgs is nil", typ)
		}
		seenRoles := map[string]bool{}
		for _, e := range def.Endpoints {
			if e.Role == "" {
				t.Errorf("%s: endpoint role is empty", typ)
			}
			if seenRoles[e.Role] {
				t.Errorf("%s: duplicate endpoint role %q", typ, e.Role)
			}
			seenRoles[e.Role] = true
			if e.ContainerPort <= 0 || e.ContainerPort > 65535 {
				t.Errorf("%s: endpoint %q invalid port %d", typ, e.Role, e.ContainerPort)
			}
			if e.Scheme == "" {
				t.Errorf("%s: endpoint %q has empty Scheme", typ, e.Role)
			}
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/pgpool/...`
Expected: FAIL - `serviceDefs`, `ServiceDef`, `EndpointSpec` undefined.

- [ ] **Step 3: Add the types and an empty registry to `cmd/pgpool/pgpool.go`**

Add a new section after the constants block (around line 36):

```go
// ---------- service registry ----------

type EndpointSpec struct {
	Role          string // "primary" | "master" | "filer" | "s3" | ...
	ContainerPort int
	Scheme        string // "postgresql" | "http" | ...
}

type ServiceDef struct {
	Type            string
	ContainerPrefix string
	VolumePrefix    string
	Image           string
	DockerArgs      func(cfg Config, volume string) []string
	Endpoints       []EndpointSpec
	Readiness       func(ctx context.Context, s *Server, container string, hostPorts map[string]string) error
	BuildURL        func(cfg Config, role string, hostPort string) string
}

var serviceDefs = map[string]ServiceDef{}
```

Also add a new label constant near the existing label constants:

```go
labelService = "pgpool.service"
```

- [ ] **Step 4: Run to verify the test still fails (registry empty)**

Run: `go test ./cmd/pgpool/...`
Expected: FAIL - "serviceDefs is empty". This is correct; the next task fills it.

- [ ] **Step 5: Commit**

```bash
git add cmd/pgpool/pgpool.go cmd/pgpool/pgpool_test.go
git commit -m "refactor: add ServiceDef registry skeleton"
```

---

### Task 4: Register `postgresDef`

**Files:**
- Modify: `cmd/pgpool/pgpool.go`

- [ ] **Step 1: Add the postgres registration**

Add to `cmd/pgpool/pgpool.go` (anywhere after `serviceDefs` declaration; group with other service code):

```go
var postgresDef = ServiceDef{
	Type:            "postgres",
	ContainerPrefix: "pg",
	VolumePrefix:    "pgvol",
	Image:           "postgres:17",
	DockerArgs: func(cfg Config, volume string) []string {
		return []string{
			"-v", volume + ":/var/lib/postgresql/data",
			"-e", "POSTGRES_PASSWORD=" + cfg.PgPassword,
			"-e", "POSTGRES_USER=" + cfg.PgUser,
			"-e", "POSTGRES_DB=" + cfg.PgDB,
		}
	},
	Endpoints: []EndpointSpec{
		{Role: "primary", ContainerPort: 5432, Scheme: "postgresql"},
	},
	Readiness: func(ctx context.Context, s *Server, container string, _ map[string]string) error {
		return s.pgIsReady(ctx, container)
	},
	BuildURL: func(cfg Config, role, hostPort string) string {
		pw := url.QueryEscape(cfg.PgPassword)
		return fmt.Sprintf("postgresql://%s:%s@%s:%s/%s",
			cfg.PgUser, pw, cfg.AdvertiseHost, hostPort, cfg.PgDB)
	},
}

func init() {
	serviceDefs[postgresDef.Type] = postgresDef
}
```

- [ ] **Step 2: Run tests**

Run: `go test ./cmd/pgpool/...`
Expected: PASS (`TestServiceRegistry_Validity` now finds postgres registered).

- [ ] **Step 3: Commit**

```bash
git add cmd/pgpool/pgpool.go
git commit -m "refactor: register postgres ServiceDef"
```

---

## Phase B - Make lifecycle operations per-service

### Task 5: Generalize `hostPort` to take a container port

**Files:**
- Modify: `cmd/pgpool/pgpool.go` (function `hostPort`)
- Modify: `cmd/pgpool/pgpool_test.go` (no test added; pure refactor verified by callers)

- [ ] **Step 1: Replace `hostPort` to accept a port arg**

In `cmd/pgpool/pgpool.go`, replace:

```go
func (s *Server) hostPort(ctx context.Context, name string) (string, error) {
	out, errOut, err := s.runDocker(ctx, "port", name, "5432/tcp")
```

with:

```go
func (s *Server) hostPort(ctx context.Context, name string, containerPort int) (string, error) {
	out, errOut, err := s.runDocker(ctx, "port", name, fmt.Sprintf("%d/tcp", containerPort))
```

Update both occurrences of the error string `"docker port %s"` to also include the container port: `"docker port %s %d/tcp"`.

- [ ] **Step 2: Update every caller**

Run: `grep -n 'hostPort(' cmd/pgpool/pgpool.go`
Three call sites: `opUp`, `opStatus`, `listContainers`. Pass `5432` as the new arg in each (we still only handle postgres; full generalization comes in Task 8). All three lines change to e.g. `port, err := s.hostPort(ctx, cname, 5432)`.

- [ ] **Step 3: Build and run tests**

Run: `go build ./... && go test ./cmd/pgpool/...`
Expected: PASS, build succeeds.

- [ ] **Step 4: Commit**

```bash
git add cmd/pgpool/pgpool.go
git commit -m "refactor: hostPort takes a container port"
```

---

### Task 6: Generalize `containerRun` to use `ServiceDef.DockerArgs`

**Files:**
- Modify: `cmd/pgpool/pgpool.go` (`runOpts`, `containerRun`)

- [ ] **Step 1: Replace `runOpts` and `containerRun`**

In `cmd/pgpool/pgpool.go`, replace the existing `runOpts` and `containerRun`:

```go
type runOpts struct {
	def              ServiceDef
	container, volume, image string
	repo, worktree   string
}

func (s *Server) containerRun(ctx context.Context, o runOpts) error {
	args := []string{
		"run", "-d",
		"--name", o.container,
		"--restart", "unless-stopped",
	}
	for _, e := range o.def.Endpoints {
		args = append(args, "-p", fmt.Sprintf("0:%d", e.ContainerPort))
	}
	args = append(args, o.def.DockerArgs(s.cfg, o.volume)...)
	args = append(args,
		"--label", labelPgpool+"=true",
		"--label", labelRepo+"="+o.repo,
		"--label", labelWorktree+"="+o.worktree,
		"--label", labelService+"="+o.def.Type,
		o.image,
	)
	_, errOut, err := s.runDocker(ctx, args...)
	if err != nil {
		return fmt.Errorf("docker run %s: %w: %s", o.container, err, strings.TrimSpace(errOut))
	}
	return nil
}
```

- [ ] **Step 2: Update `opUp` to populate the new field**

Replace the existing `s.containerRun(ctx, runOpts{...})` call inside `opUp` with:

```go
runErr := s.containerRun(ctx, runOpts{
	def: postgresDef,
	container: cname, volume: vname, image: image,
	repo: normalize(req.Repo), worktree: normalize(req.Worktree),
})
```

- [ ] **Step 3: Build and run tests**

Run: `go build ./... && go test ./cmd/pgpool/...`
Expected: PASS, build succeeds.

- [ ] **Step 4: Commit**

```bash
git add cmd/pgpool/pgpool.go
git commit -m "refactor: containerRun uses ServiceDef for env, ports, labels"
```

---

### Task 7: Per-service operation primitives

**Files:**
- Modify: `cmd/pgpool/pgpool.go`
- Modify: `cmd/pgpool/pgpool_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/pgpool/pgpool_test.go`:

```go
func TestBuildEndpointInfo(t *testing.T) {
	cfg := Config{
		AdvertiseHost: "host.example",
		PgUser:        "u",
		PgPassword:    "p p",
		PgDB:          "d",
	}
	hostPorts := map[string]string{"primary": "49160"}
	endpoints := buildEndpointInfo(cfg, postgresDef, hostPorts)
	got, ok := endpoints["primary"]
	if !ok {
		t.Fatal("missing primary endpoint")
	}
	wantURL := "postgresql://u:p%20p@host.example:49160/d"
	if got.URL != wantURL {
		t.Errorf("URL = %q, want %q", got.URL, wantURL)
	}
	if got.HostPort != "49160" {
		t.Errorf("HostPort = %q", got.HostPort)
	}
	if got.ContainerPort != 5432 {
		t.Errorf("ContainerPort = %d, want 5432", got.ContainerPort)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/pgpool/...`
Expected: FAIL - `buildEndpointInfo`, `EndpointInfo` undefined.

- [ ] **Step 3: Add `EndpointInfo` and `buildEndpointInfo`**

Add to `cmd/pgpool/pgpool.go`:

```go
type EndpointInfo struct {
	URL           string `json:"url"`
	HostPort      string `json:"host_port"`
	ContainerPort int    `json:"container_port"`
}

func buildEndpointInfo(cfg Config, def ServiceDef, hostPorts map[string]string) map[string]EndpointInfo {
	out := map[string]EndpointInfo{}
	for _, e := range def.Endpoints {
		hp, ok := hostPorts[e.Role]
		if !ok {
			continue
		}
		out[e.Role] = EndpointInfo{
			URL:           def.BuildURL(cfg, e.Role, hp),
			HostPort:      hp,
			ContainerPort: e.ContainerPort,
		}
	}
	return out
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/pgpool/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/pgpool/pgpool.go cmd/pgpool/pgpool_test.go
git commit -m "feat: build endpoint info from ServiceDef and host ports"
```

---

### Task 8: `serviceUp`, `serviceDown`, `serviceStatus` primitives

**Files:**
- Modify: `cmd/pgpool/pgpool.go`

This task converts `opUp`, `opDown`, `opStatus` (which currently handle only postgres) into per-service primitives that operate on a single `ServiceDef`. The existing top-level `opUp`/`opDown`/`opStatus` functions are kept temporarily as thin shims that call the postgres primitive; later tasks make them iterate over multiple services.

- [ ] **Step 1: Add per-service primitives**

Add to `cmd/pgpool/pgpool.go` (replace the bodies of the existing `opUp`, `opDown`, `opStatus`):

```go
type ServiceResult struct {
	Type      string                  `json:"type"`
	Container string                  `json:"container"`
	Volume    string                  `json:"volume"`
	State     string                  `json:"state,omitempty"`
	CreatedAt string                  `json:"created_at,omitempty"`
	Reused    bool                    `json:"reused,omitempty"`
	Endpoints map[string]EndpointInfo `json:"endpoints,omitempty"`
}

func (s *Server) serviceUp(ctx context.Context, def ServiceDef, repo, worktree, imageOverride string) (ServiceResult, error) {
	cname, err := serviceContainerName(def.ContainerPrefix, repo, worktree)
	if err != nil {
		return ServiceResult{}, err
	}
	vname, err := serviceVolumeName(def.VolumePrefix, repo, worktree)
	if err != nil {
		return ServiceResult{}, err
	}
	image := imageOverride
	if image == "" {
		image = def.Image
	}

	state, err := s.inspect(ctx, cname)
	if err != nil {
		return ServiceResult{}, err
	}

	reused := false
	switch {
	case state.Exists && state.Running:
		reused = true
	case state.Exists && !state.Running:
		if err := s.containerStart(ctx, cname); err != nil {
			return ServiceResult{}, err
		}
		hostPorts, err := s.collectHostPorts(ctx, cname, def)
		if err != nil {
			return ServiceResult{}, err
		}
		if err := def.Readiness(ctx, s, cname, hostPorts); err != nil {
			tail := s.logsTail(ctx, cname, 50)
			return ServiceResult{}, fmt.Errorf("%s: %w\nlast 50 log lines:\n%s", def.Type, err, tail)
		}
		reused = true
	default:
		if err := s.volumeCreate(ctx, vname); err != nil {
			return ServiceResult{}, err
		}
		runErr := s.containerRun(ctx, runOpts{
			def: def, container: cname, volume: vname, image: image,
			repo: normalize(repo), worktree: normalize(worktree),
		})
		if runErr != nil {
			if strings.Contains(runErr.Error(), "is already in use") {
				state2, err2 := s.inspect(ctx, cname)
				if err2 != nil {
					return ServiceResult{}, err2
				}
				if !state2.Exists {
					return ServiceResult{}, runErr
				}
				reused = true
			} else {
				return ServiceResult{}, runErr
			}
		}
		if !reused {
			hostPorts, err := s.collectHostPorts(ctx, cname, def)
			if err != nil {
				return ServiceResult{}, err
			}
			if err := def.Readiness(ctx, s, cname, hostPorts); err != nil {
				tail := s.logsTail(ctx, cname, 50)
				return ServiceResult{}, fmt.Errorf("%s: %w\nlast 50 log lines:\n%s", def.Type, err, tail)
			}
		}
	}

	hostPorts, err := s.collectHostPorts(ctx, cname, def)
	if err != nil {
		return ServiceResult{}, err
	}
	return ServiceResult{
		Type:      def.Type,
		Container: cname,
		Volume:    vname,
		Reused:    reused,
		Endpoints: buildEndpointInfo(s.cfg, def, hostPorts),
	}, nil
}

func (s *Server) serviceDown(ctx context.Context, def ServiceDef, repo, worktree string) (ServiceResult, error) {
	cname, err := serviceContainerName(def.ContainerPrefix, repo, worktree)
	if err != nil {
		return ServiceResult{}, err
	}
	vname, err := serviceVolumeName(def.VolumePrefix, repo, worktree)
	if err != nil {
		return ServiceResult{}, err
	}
	if err := s.containerRemove(ctx, cname); err != nil {
		return ServiceResult{}, err
	}
	if err := s.volumeRemove(ctx, vname); err != nil {
		return ServiceResult{}, err
	}
	return ServiceResult{Type: def.Type, Container: cname, Volume: vname}, nil
}

func (s *Server) serviceStatus(ctx context.Context, def ServiceDef, repo, worktree string) (ServiceResult, error) {
	cname, err := serviceContainerName(def.ContainerPrefix, repo, worktree)
	if err != nil {
		return ServiceResult{}, err
	}
	vname, err := serviceVolumeName(def.VolumePrefix, repo, worktree)
	if err != nil {
		return ServiceResult{}, err
	}
	state, err := s.inspect(ctx, cname)
	if err != nil {
		return ServiceResult{}, err
	}
	res := ServiceResult{Type: def.Type, Container: cname, Volume: vname}
	if !state.Exists {
		res.State = "missing"
		return res, nil
	}
	res.CreatedAt = state.CreatedAt
	if !state.Running {
		res.State = "stopped"
		return res, nil
	}
	res.State = "running"
	hostPorts, err := s.collectHostPorts(ctx, cname, def)
	if err != nil {
		return ServiceResult{}, err
	}
	res.Endpoints = buildEndpointInfo(s.cfg, def, hostPorts)
	return res, nil
}

func (s *Server) collectHostPorts(ctx context.Context, container string, def ServiceDef) (map[string]string, error) {
	out := map[string]string{}
	for _, e := range def.Endpoints {
		hp, err := s.hostPort(ctx, container, e.ContainerPort)
		if err != nil {
			return nil, fmt.Errorf("%s: lookup %s host port: %w", def.Type, e.Role, err)
		}
		out[e.Role] = hp
	}
	return out, nil
}
```

- [ ] **Step 2: Delete the old `opUp`, `opDown`, `opStatus` and their request/response structs**

Remove from `cmd/pgpool/pgpool.go`:
- `UpRequest`, `UpResponse`, `DownRequest`, `DownResponse`, `StatusResponse`, `ListedContainer` types
- `opUp`, `opDown`, `opStatus`, `listContainers` functions

These are replaced wholesale in the next task. The handlers that reference them will be updated next; until then the code does not build. That is intentional - the next task lands the handler refactor in the same compilation unit.

- [ ] **Step 3: Confirm the build is broken before moving on**

Run: `go build ./...`
Expected: FAIL - undefined references to `opUp` / `opDown` / `opStatus` / `listContainers` / `UpRequest` / etc. from REST and MCP handlers. Continue to Task 9.

(Do not commit yet. Tasks 8+9 land as one commit because the build is intentionally broken between them.)

---

### Task 9: New request/response types and handler rewrite

**Files:**
- Modify: `cmd/pgpool/pgpool.go`

- [ ] **Step 1: Add new request/response types**

Add to `cmd/pgpool/pgpool.go`:

```go
type UpRequest struct {
	Repo     string   `json:"repo"`
	Worktree string   `json:"worktree"`
	Services []string `json:"services,omitempty"`
	Image    string   `json:"image,omitempty"` // optional, applies to postgres if present
}

type UpResponse struct {
	Services []ServiceResult `json:"services"`
}

type DownRequest struct {
	Repo     string   `json:"repo"`
	Worktree string   `json:"worktree"`
	Services []string `json:"services,omitempty"`
}

type DownResponse struct {
	Services []ServiceResult `json:"services"`
}

type StatusResponse struct {
	Repo     string          `json:"repo"`
	Worktree string          `json:"worktree"`
	Services []ServiceResult `json:"services"`
}

type ListedContainer struct {
	Type      string                  `json:"type"`
	Container string                  `json:"container"`
	Volume    string                  `json:"volume,omitempty"`
	Repo      string                  `json:"repo"`
	Worktree  string                  `json:"worktree"`
	State     string                  `json:"state"`
	CreatedAt string                  `json:"created_at"`
	Endpoints map[string]EndpointInfo `json:"endpoints,omitempty"`
}
```

- [ ] **Step 2: Add `resolveServices` helper**

Add to `cmd/pgpool/pgpool.go`:

```go
func (s *Server) resolveServices(requested []string) ([]ServiceDef, error) {
	if len(requested) == 0 {
		requested = s.cfg.DefaultServices
	}
	if len(requested) == 0 {
		return nil, errors.New("no services requested and no server default configured")
	}
	out := make([]ServiceDef, 0, len(requested))
	for _, name := range requested {
		def, ok := serviceDefs[name]
		if !ok {
			return nil, fmt.Errorf("unknown service %q", name)
		}
		out = append(out, def)
	}
	return out, nil
}
```

(`Config.DefaultServices` is added in Task 10. The build remains broken for one more step.)

- [ ] **Step 3: Add the multi-service `opUp`, `opDown`, `opStatus`, `listContainers`**

Add to `cmd/pgpool/pgpool.go`:

```go
func (s *Server) opUp(ctx context.Context, req UpRequest) (*UpResponse, error) {
	defs, err := s.resolveServices(req.Services)
	if err != nil {
		return nil, err
	}
	results := make([]ServiceResult, 0, len(defs))
	for _, def := range defs {
		image := ""
		if def.Type == "postgres" {
			image = req.Image
		}
		res, err := s.serviceUp(ctx, def, req.Repo, req.Worktree, image)
		if err != nil {
			return &UpResponse{Services: results}, err
		}
		results = append(results, res)
	}
	return &UpResponse{Services: results}, nil
}

func (s *Server) opDown(ctx context.Context, req DownRequest) (*DownResponse, error) {
	defs, err := s.resolveServices(req.Services)
	if err != nil {
		return nil, err
	}
	results := make([]ServiceResult, 0, len(defs))
	for _, def := range defs {
		res, err := s.serviceDown(ctx, def, req.Repo, req.Worktree)
		if err != nil {
			return &DownResponse{Services: results}, err
		}
		results = append(results, res)
	}
	return &DownResponse{Services: results}, nil
}

func (s *Server) opStatus(ctx context.Context, repo, worktree, service string) (*StatusResponse, error) {
	var defs []ServiceDef
	if service != "" {
		def, ok := serviceDefs[service]
		if !ok {
			return nil, fmt.Errorf("unknown service %q", service)
		}
		defs = []ServiceDef{def}
	} else {
		var err error
		defs, err = s.resolveServices(nil)
		if err != nil {
			return nil, err
		}
	}
	results := make([]ServiceResult, 0, len(defs))
	for _, def := range defs {
		res, err := s.serviceStatus(ctx, def, repo, worktree)
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	return &StatusResponse{Repo: repo, Worktree: worktree, Services: results}, nil
}

func (s *Server) listContainers(ctx context.Context) ([]ListedContainer, error) {
	out, errOut, err := s.runDocker(ctx, "ps", "-a",
		"--filter", "label="+labelPgpool+"=true",
		"--format", "{{json .}}",
	)
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w: %s", err, strings.TrimSpace(errOut))
	}
	var results []ListedContainer
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var row dockerPSRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("parse docker ps row: %w", err)
		}
		labels := parseDockerLabels(row.Labels)
		typ := labels[labelService]
		if typ == "" {
			typ = "postgres" // legacy fallback
		}
		def, defKnown := serviceDefs[typ]
		lc := ListedContainer{
			Type:      typ,
			Container: row.Names,
			Repo:      labels[labelRepo],
			Worktree:  labels[labelWorktree],
			State:     row.State,
			CreatedAt: row.CreatedAt,
		}
		if defKnown {
			vname, _ := serviceVolumeName(def.VolumePrefix, lc.Repo, lc.Worktree)
			lc.Volume = vname
		}
		if row.State == "running" && defKnown {
			hostPorts, err := s.collectHostPorts(ctx, row.Names, def)
			if err == nil {
				lc.Endpoints = buildEndpointInfo(s.cfg, def, hostPorts)
			}
		}
		results = append(results, lc)
	}
	return results, nil
}
```

- [ ] **Step 4: Update REST handlers**

In `cmd/pgpool/pgpool.go`, replace `handleUp`, `handleDown`, `handleStatus`, `handleList`:

```go
func (s *Server) handleUp(w http.ResponseWriter, r *http.Request) {
	var req UpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("parse body: %w", err))
		return
	}
	resp, err := s.opUp(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":    err.Error(),
			"services": resp.Services,
		})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDown(w http.ResponseWriter, r *http.Request) {
	var req DownRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("parse body: %w", err))
		return
	}
	resp, err := s.opDown(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":    err.Error(),
			"services": resp.Services,
		})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	worktree := r.URL.Query().Get("worktree")
	service := r.URL.Query().Get("service")
	if repo == "" || worktree == "" {
		writeError(w, http.StatusBadRequest, errors.New("repo and worktree query params required"))
		return
	}
	resp, err := s.opStatus(r.Context(), repo, worktree, service)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	items, err := s.listContainers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if items == nil {
		items = []ListedContainer{}
	}
	writeJSON(w, http.StatusOK, items)
}
```

- [ ] **Step 5: Update MCP `tools()` and `callTool`**

In `cmd/pgpool/pgpool.go`, replace `tools()` and the body of `callTool`:

```go
func (s *Server) tools() []mcpTool {
	rwSvc := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":     map[string]any{"type": "string", "description": "Repository name"},
			"worktree": map[string]any{"type": "string", "description": "Worktree name"},
			"services": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional subset of service types to act on. Defaults to server's --services list.",
			},
		},
		"required": []string{"repo", "worktree"},
	}
	rwOptionalService := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":     map[string]any{"type": "string", "description": "Repository name"},
			"worktree": map[string]any{"type": "string", "description": "Worktree name"},
			"service":  map[string]any{"type": "string", "description": "Optional single service type to filter to."},
		},
		"required": []string{"repo", "worktree"},
	}
	upSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":     map[string]any{"type": "string", "description": "Repository name"},
			"worktree": map[string]any{"type": "string", "description": "Worktree name"},
			"services": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional subset of service types to bring up. Defaults to server's --services list.",
			},
			"image": map[string]any{"type": "string", "description": "Optional postgres image override."},
		},
		"required": []string{"repo", "worktree"},
	}
	empty := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
	return []mcpTool{
		{Name: "pgpool_up", Description: "Bring up the configured services for a worktree. Returns one entry per service with its endpoints.", InputSchema: upSchema},
		{Name: "pgpool_down", Description: "Tear down services for a worktree. Defaults to all configured services.", InputSchema: rwSvc},
		{Name: "pgpool_status", Description: "Report state of services for a worktree. Optionally filter to one service.", InputSchema: rwOptionalService},
		{Name: "pgpool_list", Description: "List all pgpool-managed containers on this host.", InputSchema: empty},
	}
}

func (s *Server) callTool(ctx context.Context, name string, args json.RawMessage) (any, error) {
	switch name {
	case "pgpool_up":
		var req UpRequest
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return nil, fmt.Errorf("parse arguments: %w", err)
			}
		}
		return s.opUp(ctx, req)
	case "pgpool_down":
		var req DownRequest
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return nil, fmt.Errorf("parse arguments: %w", err)
			}
		}
		return s.opDown(ctx, req)
	case "pgpool_status":
		var req struct {
			Repo     string `json:"repo"`
			Worktree string `json:"worktree"`
			Service  string `json:"service"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return nil, fmt.Errorf("parse arguments: %w", err)
			}
		}
		return s.opStatus(ctx, req.Repo, req.Worktree, req.Service)
	case "pgpool_list":
		items, err := s.listContainers(ctx)
		if err != nil {
			return nil, err
		}
		if items == nil {
			items = []ListedContainer{}
		}
		return items, nil
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}
```

- [ ] **Step 6: Confirm build still fails (until Task 10 lands `Config.DefaultServices`)**

Run: `go build ./...`
Expected: FAIL on `s.cfg.DefaultServices` undefined. Continue.

---

### Task 10: `--services` flag and config field

**Files:**
- Modify: `cmd/pgpool/pgpool.go`
- Modify: `cmd/pgpool/pgpool_test.go`

- [ ] **Step 1: Add `DefaultServices` to `Config`**

In `cmd/pgpool/pgpool.go`, change the `Config` struct to include:

```go
DefaultServices []string
```

- [ ] **Step 2: Wire the flag in `main`**

In `main()`, add (alongside the other flag assignments):

```go
servicesCSV := getenv("PGPOOL_SERVICES", "postgres")
flag.StringVar(&servicesCSV, "services", servicesCSV, "comma-separated list of service types to bring up by default")
```

After `flag.Parse()`, parse the CSV:

```go
cfg.DefaultServices = parseServicesCSV(servicesCSV)
if len(cfg.DefaultServices) == 0 {
	log.Fatal("pgpool: --services must be non-empty")
}
for _, name := range cfg.DefaultServices {
	if _, ok := serviceDefs[name]; !ok {
		log.Fatalf("pgpool: unknown service %q in --services", name)
	}
}
```

Add the helper:

```go
func parseServicesCSV(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
```

- [ ] **Step 3: Write a test for parseServicesCSV**

Append to `cmd/pgpool/pgpool_test.go`:

```go
func TestParseServicesCSV(t *testing.T) {
	cases := map[string][]string{
		"postgres":              {"postgres"},
		"postgres,seaweedfs":    {"postgres", "seaweedfs"},
		" postgres , seaweedfs": {"postgres", "seaweedfs"},
		"":                      {},
		",,,":                   {},
	}
	for in, want := range cases {
		got := parseServicesCSV(in)
		if len(got) != len(want) {
			t.Errorf("parseServicesCSV(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("parseServicesCSV(%q)[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
}
```

- [ ] **Step 4: Build and run tests**

Run: `go build ./... && go test ./cmd/pgpool/...`
Expected: PASS, build succeeds. Tasks 8+9+10 form one logical change.

- [ ] **Step 5: Commit**

```bash
git add cmd/pgpool/pgpool.go cmd/pgpool/pgpool_test.go
git commit -m "feat: per-service lifecycle and multi-service request/response shape"
```

---

### Task 11: Validation tests for `resolveServices`

**Files:**
- Modify: `cmd/pgpool/pgpool_test.go`

- [ ] **Step 1: Write validation tests**

Append to `cmd/pgpool/pgpool_test.go`:

```go
func TestResolveServices(t *testing.T) {
	s := &Server{cfg: Config{DefaultServices: []string{"postgres"}}}

	got, err := s.resolveServices(nil)
	if err != nil || len(got) != 1 || got[0].Type != "postgres" {
		t.Errorf("default fallback failed: %v %v", got, err)
	}

	got, err = s.resolveServices([]string{"postgres"})
	if err != nil || len(got) != 1 {
		t.Errorf("explicit single failed: %v %v", got, err)
	}

	_, err = s.resolveServices([]string{"nope"})
	if err == nil {
		t.Error("expected error for unknown service")
	}

	empty := &Server{cfg: Config{DefaultServices: nil}}
	_, err = empty.resolveServices(nil)
	if err == nil {
		t.Error("expected error when no defaults and no request")
	}
}
```

- [ ] **Step 2: Run tests**

Run: `go test ./cmd/pgpool/...`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add cmd/pgpool/pgpool_test.go
git commit -m "test: cover resolveServices validation"
```

---

## Phase C - Add SeaweedFS

### Task 12: Register `seaweedfsDef`

**Files:**
- Modify: `cmd/pgpool/pgpool.go`

- [ ] **Step 1: Add the seaweedfs registration**

Add to `cmd/pgpool/pgpool.go`:

```go
var seaweedfsDef = ServiceDef{
	Type:            "seaweedfs",
	ContainerPrefix: "weed",
	VolumePrefix:    "weedvol",
	Image:           "chrislusf/seaweedfs:3.71",
	DockerArgs: func(_ Config, volume string) []string {
		return []string{
			"-v", volume + ":/data",
			// args after the image are the container's command:
			"--",
			"server",
			"-dir=/data",
			"-master",
			"-volume",
			"-filer",
			"-s3",
		}
	},
	Endpoints: []EndpointSpec{
		{Role: "master", ContainerPort: 9333, Scheme: "http"},
		{Role: "volume", ContainerPort: 8080, Scheme: "http"},
		{Role: "filer", ContainerPort: 8888, Scheme: "http"},
		{Role: "s3", ContainerPort: 8333, Scheme: "http"},
	},
	Readiness: func(ctx context.Context, s *Server, container string, hostPorts map[string]string) error {
		return s.httpReady(ctx, "http://"+s.cfg.AdvertiseHost+":"+hostPorts["master"]+"/cluster/status")
	},
	BuildURL: func(cfg Config, role, hostPort string) string {
		return fmt.Sprintf("http://%s:%s", cfg.AdvertiseHost, hostPort)
	},
}

func init() {
	serviceDefs[seaweedfsDef.Type] = seaweedfsDef
}
```

There is a subtlety with `containerRun`: the current implementation appends the image at the end, but seaweedfs needs args AFTER the image (the docker `[image] [cmd...]` syntax). The trick we use here is the `"--"` token in `DockerArgs` followed by command args - but `docker run` parses positional args after the image as the container command. The `DockerArgs` builder for postgres returns only `-v` / `-e` flags, which go BEFORE the image. For seaweedfs we need flags before the image (`-v`) AND command args after. This requires the `containerRun` builder to split flags vs. command args.

- [ ] **Step 2: Add post-image command-args support**

Change `ServiceDef`:

```go
type ServiceDef struct {
	Type            string
	ContainerPrefix string
	VolumePrefix    string
	Image           string
	DockerArgs      func(cfg Config, volume string) []string         // flags placed BEFORE the image
	DockerCommand   func(cfg Config) []string                        // args placed AFTER the image (the container CMD)
	Endpoints       []EndpointSpec
	Readiness       func(ctx context.Context, s *Server, container string, hostPorts map[string]string) error
	BuildURL        func(cfg Config, role string, hostPort string) string
}
```

Update `seaweedfsDef`:

```go
DockerArgs: func(_ Config, volume string) []string {
	return []string{"-v", volume + ":/data"}
},
DockerCommand: func(_ Config) []string {
	return []string{"server", "-dir=/data", "-master", "-volume", "-filer", "-s3"}
},
```

Update `postgresDef` to leave `DockerCommand` nil.

Update `containerRun` to append `DockerCommand` after the image:

```go
args = append(args, o.image)
if o.def.DockerCommand != nil {
	args = append(args, o.def.DockerCommand(s.cfg)...)
}
```

(Move the `o.image` append outside the prior `append` and add the conditional immediately after.)

- [ ] **Step 3: Update the registry validity test**

In `TestServiceRegistry_Validity`, `DockerCommand` is optional (postgres leaves it nil), so no test change is required. But add a test that seaweedfs has a non-nil `DockerCommand`:

```go
func TestSeaweedfs_HasDockerCommand(t *testing.T) {
	def, ok := serviceDefs["seaweedfs"]
	if !ok {
		t.Fatal("seaweedfs not registered")
	}
	if def.DockerCommand == nil {
		t.Fatal("seaweedfs DockerCommand is nil")
	}
	cmd := def.DockerCommand(Config{})
	if len(cmd) == 0 || cmd[0] != "server" {
		t.Errorf("unexpected command: %v", cmd)
	}
}
```

- [ ] **Step 4: Add `httpReady` helper**

Add to `cmd/pgpool/pgpool.go`:

```go
func (s *Server) httpReady(ctx context.Context, url string) error {
	deadline := time.Now().Add(s.cfg.StartupTimeout)
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("http readiness probe %s timed out after %s", url, s.cfg.StartupTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
```

- [ ] **Step 5: Build and test**

Run: `go build ./... && go test ./cmd/pgpool/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/pgpool/pgpool.go cmd/pgpool/pgpool_test.go
git commit -m "feat: register seaweedfs service"
```

---

### Task 13: Integration tests (docker-gated)

**Files:**
- Create: `cmd/pgpool/integration_test.go`

- [ ] **Step 1: Write the integration test file**

Create `cmd/pgpool/integration_test.go`:

```go
//go:build integration

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"testing"
	"time"
)

func dockerAvailable(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available")
	}
}

func newTestServer(t *testing.T, services []string) *Server {
	t.Helper()
	dockerAvailable(t)
	return &Server{cfg: Config{
		AdvertiseHost:   "localhost",
		PgUser:          "postgres",
		PgPassword:      "test-password-do-not-reuse",
		PgDB:            "postgres",
		DockerBin:       "docker",
		StartupTimeout:  90 * time.Second,
		DefaultServices: services,
	}}
}

func TestIntegration_PostgresLifecycle(t *testing.T) {
	s := newTestServer(t, []string{"postgres"})
	ctx := context.Background()
	defer s.opDown(ctx, DownRequest{Repo: "itest", Worktree: "pg"})

	up, err := s.opUp(ctx, UpRequest{Repo: "itest", Worktree: "pg"})
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if len(up.Services) != 1 || up.Services[0].Type != "postgres" {
		t.Fatalf("unexpected up response: %+v", up)
	}
	primary, ok := up.Services[0].Endpoints["primary"]
	if !ok || primary.URL == "" {
		t.Fatalf("missing primary endpoint: %+v", up.Services[0])
	}

	st, err := s.opStatus(ctx, "itest", "pg", "")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Services) != 1 || st.Services[0].State != "running" {
		t.Fatalf("status not running: %+v", st)
	}
}

func TestIntegration_SeaweedfsLifecycle(t *testing.T) {
	s := newTestServer(t, []string{"seaweedfs"})
	ctx := context.Background()
	defer s.opDown(ctx, DownRequest{Repo: "itest", Worktree: "weed"})

	up, err := s.opUp(ctx, UpRequest{Repo: "itest", Worktree: "weed"})
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if len(up.Services) != 1 || up.Services[0].Type != "seaweedfs" {
		t.Fatalf("unexpected up response: %+v", up)
	}
	for _, role := range []string{"master", "volume", "filer", "s3"} {
		ep, ok := up.Services[0].Endpoints[role]
		if !ok || ep.HostPort == "" {
			t.Errorf("missing endpoint %s", role)
		}
	}
	master := up.Services[0].Endpoints["master"]
	resp, err := http.Get(master.URL + "/cluster/status")
	if err != nil {
		t.Fatalf("master GET: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("master status: %d", resp.StatusCode)
	}
}

func TestIntegration_MultiServiceUp(t *testing.T) {
	s := newTestServer(t, []string{"postgres", "seaweedfs"})
	ctx := context.Background()
	defer s.opDown(ctx, DownRequest{Repo: "itest", Worktree: "multi"})

	up, err := s.opUp(ctx, UpRequest{Repo: "itest", Worktree: "multi"})
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if len(up.Services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(up.Services))
	}
	types := map[string]bool{}
	for _, svc := range up.Services {
		types[svc.Type] = true
	}
	if !types["postgres"] || !types["seaweedfs"] {
		t.Fatalf("missing service types: %+v", up.Services)
	}
}

func TestIntegration_ScopedDownLeavesOthers(t *testing.T) {
	s := newTestServer(t, []string{"postgres", "seaweedfs"})
	ctx := context.Background()
	defer s.opDown(ctx, DownRequest{Repo: "itest", Worktree: "scoped"})

	if _, err := s.opUp(ctx, UpRequest{Repo: "itest", Worktree: "scoped"}); err != nil {
		t.Fatalf("up: %v", err)
	}
	if _, err := s.opDown(ctx, DownRequest{Repo: "itest", Worktree: "scoped", Services: []string{"postgres"}}); err != nil {
		t.Fatalf("scoped down: %v", err)
	}
	st, err := s.opStatus(ctx, "itest", "scoped", "")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, svc := range st.Services {
		if svc.Type == "postgres" && svc.State != "missing" {
			t.Errorf("postgres should be missing, got %q", svc.State)
		}
		if svc.Type == "seaweedfs" && svc.State != "running" {
			t.Errorf("seaweedfs should be running, got %q", svc.State)
		}
	}
	_ = json.Marshal // keep import in case future tests need it
}
```

- [ ] **Step 2: Run integration tests locally**

Run: `go test -tags=integration ./cmd/pgpool/... -timeout 5m`
Expected: PASS, all four tests. Pulling the seaweedfs image on first run takes time; the per-service `StartupTimeout` of 90s accommodates that.

- [ ] **Step 3: Commit**

```bash
git add cmd/pgpool/integration_test.go
git commit -m "test: docker-gated integration coverage for postgres, seaweedfs, multi-service, scoped down"
```

---

## Phase D - Update CLI

### Task 14: Update pgpoolcli to new request/response shapes

**Files:**
- Modify: `cmd/pgpoolcli/pgpoolcli.go`

- [ ] **Step 1: Replace `cmdUp`**

In `cmd/pgpoolcli/pgpoolcli.go`, replace `cmdUp`:

```go
func cmdUp(rc *runCtx, repo, worktree string, services []string) error {
	body := map[string]any{"repo": repo, "worktree": worktree}
	if len(services) > 0 {
		body["services"] = services
	}
	var resp struct {
		Services []serviceResultJSON `json:"services"`
	}
	if err := rc.client.do(http.MethodPost, "/v1/up", body, &resp); err != nil {
		return err
	}
	if rc.jsonOnly {
		return printJSON(resp)
	}
	for _, svc := range resp.Services {
		printServiceBlock(svc, true)
	}
	return nil
}
```

- [ ] **Step 2: Replace `cmdDown`**

```go
func cmdDown(rc *runCtx, repo, worktree string, services []string) error {
	body := map[string]any{"repo": repo, "worktree": worktree}
	if len(services) > 0 {
		body["services"] = services
	}
	var resp struct {
		Services []serviceResultJSON `json:"services"`
	}
	if err := rc.client.do(http.MethodPost, "/v1/down", body, &resp); err != nil {
		return err
	}
	if rc.jsonOnly {
		return printJSON(resp)
	}
	for _, svc := range resp.Services {
		fmt.Printf("removed %s container: %s\n", svc.Type, svc.Container)
		fmt.Printf("removed %s volume:    %s\n", svc.Type, svc.Volume)
	}
	return nil
}
```

- [ ] **Step 3: Replace `cmdStatus`**

```go
func cmdStatus(rc *runCtx, repo, worktree, service string) error {
	q := url.Values{}
	q.Set("repo", repo)
	q.Set("worktree", worktree)
	if service != "" {
		q.Set("service", service)
	}
	var resp struct {
		Repo     string              `json:"repo"`
		Worktree string              `json:"worktree"`
		Services []serviceResultJSON `json:"services"`
	}
	if err := rc.client.do(http.MethodGet, "/v1/status?"+q.Encode(), nil, &resp); err != nil {
		return err
	}
	if rc.jsonOnly {
		return printJSON(resp)
	}
	fmt.Printf("repo:     %s\n", resp.Repo)
	fmt.Printf("worktree: %s\n", resp.Worktree)
	for _, svc := range resp.Services {
		printServiceBlock(svc, false)
	}
	return nil
}
```

- [ ] **Step 4: Replace `cmdList`**

```go
func cmdList(rc *runCtx) error {
	var resp []listedJSON
	if err := rc.client.do(http.MethodGet, "/v1/list", nil, &resp); err != nil {
		return err
	}
	if rc.jsonOnly {
		return printJSON(resp)
	}
	if len(resp) == 0 {
		fmt.Println("(no pgpool-managed containers)")
		return nil
	}
	fmt.Printf("%-12s  %-32s  %-12s  %-10s  %s\n", "TYPE", "CONTAINER", "WORKTREE", "STATE", "ENDPOINTS")
	for _, row := range resp {
		fmt.Printf("%-12s  %-32s  %-12s  %-10s  %s\n",
			row.Type,
			truncate(row.Container, 32),
			truncate(row.Worktree, 12),
			row.State,
			endpointsSummary(row.Endpoints, 60),
		)
	}
	return nil
}
```

- [ ] **Step 5: Add helper types and printers**

Add to `cmd/pgpoolcli/pgpoolcli.go`:

```go
type endpointJSON struct {
	URL           string `json:"url"`
	HostPort      string `json:"host_port"`
	ContainerPort int    `json:"container_port"`
}

type serviceResultJSON struct {
	Type      string                  `json:"type"`
	Container string                  `json:"container"`
	Volume    string                  `json:"volume"`
	State     string                  `json:"state,omitempty"`
	Reused    bool                    `json:"reused,omitempty"`
	CreatedAt string                  `json:"created_at,omitempty"`
	Endpoints map[string]endpointJSON `json:"endpoints,omitempty"`
}

type listedJSON struct {
	Type      string                  `json:"type"`
	Container string                  `json:"container"`
	Volume    string                  `json:"volume"`
	Repo      string                  `json:"repo"`
	Worktree  string                  `json:"worktree"`
	State     string                  `json:"state"`
	CreatedAt string                  `json:"created_at"`
	Endpoints map[string]endpointJSON `json:"endpoints,omitempty"`
}

func printServiceBlock(svc serviceResultJSON, includeReused bool) {
	fmt.Printf("\n=== %s ===\n", svc.Type)
	fmt.Printf("container: %s\n", svc.Container)
	fmt.Printf("volume:    %s\n", svc.Volume)
	if svc.State != "" {
		fmt.Printf("state:     %s\n", svc.State)
	}
	if includeReused {
		fmt.Printf("reused:    %v\n", svc.Reused)
	}
	for _, role := range sortedRoles(svc.Endpoints) {
		ep := svc.Endpoints[role]
		fmt.Printf("%-9s  %s\n", role+".url:", ep.URL)
	}
}

func sortedRoles(m map[string]endpointJSON) []string {
	roles := make([]string, 0, len(m))
	for r := range m {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	return roles
}

func endpointsSummary(m map[string]endpointJSON, maxLen int) string {
	if len(m) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(m))
	for _, role := range sortedRoles(m) {
		parts = append(parts, role+"="+m[role].HostPort)
	}
	out := strings.Join(parts, " ")
	if len(out) > maxLen {
		return out[:maxLen-3] + "..."
	}
	return out
}
```

Add `"sort"` to the imports if not already present.

- [ ] **Step 6: Update CLI command parsers to accept positional args**

In `runUp`, `runDown`, `runStatus`, parse trailing positional args after flag parsing. Replace the existing functions:

```go
func runUp(args []string) {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	var g globalFlags
	addGlobalFlags(fs, &g)
	repo := fs.String("repo", "", "repository name (defaults to git-detected)")
	worktree := fs.String("worktree", "", "worktree name (defaults to $PWD basename)")
	must(fs.Parse(args))

	if *repo == "" {
		*repo = detectRepo()
	}
	if *worktree == "" {
		*worktree = detectWorktree()
	}
	r, err := requireDetected("repo", *repo)
	fail(err)
	w, err := requireDetected("worktree", *worktree)
	fail(err)

	rc, err := newRunCtx(g)
	fail(err)
	fail(cmdUp(rc, r, w, fs.Args()))
}

func runDown(args []string) {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	var g globalFlags
	addGlobalFlags(fs, &g)
	repo := fs.String("repo", "", "repository name (defaults to git-detected)")
	worktree := fs.String("worktree", "", "worktree name (defaults to $PWD basename)")
	must(fs.Parse(args))

	if *repo == "" {
		*repo = detectRepo()
	}
	if *worktree == "" {
		*worktree = detectWorktree()
	}
	r, err := requireDetected("repo", *repo)
	fail(err)
	w, err := requireDetected("worktree", *worktree)
	fail(err)

	rc, err := newRunCtx(g)
	fail(err)
	fail(cmdDown(rc, r, w, fs.Args()))
}

func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	var g globalFlags
	addGlobalFlags(fs, &g)
	repo := fs.String("repo", "", "repository name (defaults to git-detected)")
	worktree := fs.String("worktree", "", "worktree name (defaults to $PWD basename)")
	must(fs.Parse(args))

	if *repo == "" {
		*repo = detectRepo()
	}
	if *worktree == "" {
		*worktree = detectWorktree()
	}
	r, err := requireDetected("repo", *repo)
	fail(err)
	w, err := requireDetected("worktree", *worktree)
	fail(err)

	rc, err := newRunCtx(g)
	fail(err)

	var service string
	if rest := fs.Args(); len(rest) > 0 {
		service = rest[0]
	}
	fail(cmdStatus(rc, r, w, service))
}
```

- [ ] **Step 7: Build and run**

Run: `go build ./...`
Expected: build succeeds.

Smoke check (server already running locally with seaweedfs):

```bash
go run ./cmd/pgpoolcli up
go run ./cmd/pgpoolcli status
go run ./cmd/pgpoolcli list
go run ./cmd/pgpoolcli down postgres
```

- [ ] **Step 8: Commit**

```bash
git add cmd/pgpoolcli/pgpoolcli.go
git commit -m "feat(cli): multi-service request/response and positional args"
```

---

### Task 15: Update `init` claudeSegment and `prime` text

**Files:**
- Modify: `cmd/pgpoolcli/pgpoolcli.go`

- [ ] **Step 1: Bump the integration marker version**

In `cmd/pgpoolcli/pgpoolcli.go`, change:

```go
claudeBeginMarker = "<!-- BEGIN PGPOOL INTEGRATION v:1 -->"
```

to:

```go
claudeBeginMarker = "<!-- BEGIN PGPOOL INTEGRATION v:2 -->"
```

This causes `pgpoolcli init` to no-op for stale v1 blocks (the user can replace them by hand or with `--force`-equivalent on a future iteration).

- [ ] **Step 2: Replace `claudeSegment`**

```go
const claudeSegment = `<!-- BEGIN PGPOOL INTEGRATION v:2 -->
## Per-worktree services (pgpool)
This project uses **pgpoolcli** to manage ephemeral per-worktree services (Postgres and SeaweedFS supported today).
Run ` + "`pgpoolcli prime`" + ` for full workflow context.
### Quick reference
` + "```bash" + `
pgpoolcli up                  # bring up all configured services
pgpoolcli up postgres         # just postgres
pgpoolcli status              # show all services for this worktree
pgpoolcli status seaweedfs    # filter to one service
pgpoolcli list                # all pgpool-managed containers on the host
pgpoolcli down                # tear everything down for this worktree
pgpoolcli down postgres       # tear down only postgres
` + "```" + `
Repo and worktree auto-detect from git. Override with ` + "`--repo`" + ` / ` + "`--worktree`" + `.
### Rules
- Use ` + "`pgpoolcli`" + ` to manage per-worktree services - do NOT hand-run ` + "`docker`" + ` commands against pgpool containers.
- ` + "`pgpoolcli up`" + ` is per-service idempotent. Re-running brings up missing services and reuses existing ones.
- ` + "`pgpoolcli down`" + ` destroys volumes - data is NOT recoverable.
- The server does not write ` + "`.env`" + ` files - read endpoint URLs from ` + "`up`" + ` / ` + "`status`" + ` and write your own.
- One container per (repo, worktree, service) tuple - names are derived, not chosen.
<!-- END PGPOOL INTEGRATION -->`
```

- [ ] **Step 3: Replace `primeText`**

```go
const primeText = `pgpoolcli - per-worktree service management

Each (repo, worktree) pair gets one ephemeral container per registered service.
Today's services: postgres, seaweedfs. The server is stateless; all state lives
in Docker labels and volumes. Auto-detection fills in repo and worktree from
git when you do not pass them.

Commands:
  pgpoolcli up [SERVICE...]
    Bring up the listed services for this worktree, or all configured services
    if no service is named. Idempotent. Returns one entry per service.

  pgpoolcli down [SERVICE...]
    Destroy the listed services (or all configured services). NOT REVERSIBLE -
    volumes are gone.

  pgpoolcli status [SERVICE]
    Report state for every configured service in this worktree, or just the
    named service.

  pgpoolcli list
    Inventory of every pgpool-managed container on the server's host.

  pgpoolcli health
    Liveness check against the server.

  pgpoolcli config
    Print the resolved CLI config (url, config path, detected repo/worktree).

  pgpoolcli init [--url URL] [--force]
    Write ~/.config/pgpool/pgpool.json and append the pgpool block to
    ./CLAUDE.md if not already present.

  pgpoolcli prime
    Print this text.

Global flags (apply to every subcommand):
  --url URL          Server URL (env: PGPOOL_URL).
  --config PATH      Config file path (env: PGPOOL_CONFIG).
  --json             Print raw JSON instead of a human summary.

Auto-detection:
  --repo      basename of the origin remote URL, else basename of the git toplevel
  --worktree  basename of the current working directory

Typical flow inside a worktree:
  1. pgpoolcli up                # all services
  2. read connection URLs from each service's "endpoints" map
  3. write into your .env (the server does not do this for you)
  4. pgpoolcli down              # when the worktree is done
`
```

- [ ] **Step 4: Build**

Run: `go build ./...`
Expected: success.

- [ ] **Step 5: Commit**

```bash
git add cmd/pgpoolcli/pgpoolcli.go
git commit -m "docs(cli): claudeSegment and prime text reflect multi-service usage"
```

---

## Phase E - Web UI and project docs

### Task 16: Update `cmd/pgpool/index.html` to render the multi-service shape

**Files:**
- Modify: `cmd/pgpool/index.html`

- [ ] **Step 1: Replace the body and script**

Replace the contents of `cmd/pgpool/index.html` with:

```html
<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>pgpool</title>
<style>
  body { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; max-width: 880px; margin: 1.5rem auto; padding: 0 1rem; }
  section { margin-bottom: 1.5rem; }
  pre { background: #f4f4f4; padding: .75rem; overflow-x: auto; }
  label { display: inline-block; margin-right: .75rem; }
  input { padding: 2px 4px; }
  button { padding: 4px 10px; }
</style>
</head>
<body>

<h1>pgpool</h1>

<section>
  <h2>up</h2>
  <form id="up-form">
    <label>repo <input name="repo" required></label>
    <label>worktree <input name="worktree" required></label>
    <label>services (csv, optional) <input name="services" placeholder="postgres,seaweedfs"></label>
    <button type="submit">up</button>
  </form>
  <pre id="up-out"></pre>
</section>

<hr>

<section>
  <h2>down</h2>
  <form id="down-form">
    <label>repo <input name="repo" required></label>
    <label>worktree <input name="worktree" required></label>
    <label>services (csv, optional) <input name="services" placeholder="postgres"></label>
    <button type="submit">down</button>
  </form>
  <pre id="down-out"></pre>
</section>

<hr>

<section>
  <h2>status</h2>
  <form id="status-form">
    <label>repo <input name="repo" required></label>
    <label>worktree <input name="worktree" required></label>
    <label>service (optional) <input name="service" placeholder="postgres"></label>
    <button type="submit">status</button>
  </form>
  <pre id="status-out"></pre>
</section>

<hr>

<section>
  <h2>list</h2>
  <button id="list-btn">refresh</button>
  <pre id="list-out"></pre>
</section>

<script>
function show(id, data) {
  document.getElementById(id).textContent =
    typeof data === 'string' ? data : JSON.stringify(data, null, 2);
}

function csvField(s) {
  return s.split(',').map(x => x.trim()).filter(Boolean);
}

async function postJSON(path, body) {
  const r = await fetch(path, {
    method: 'POST',
    headers: {'content-type': 'application/json'},
    body: JSON.stringify(body),
  });
  return r.json();
}

async function getJSON(path) {
  const r = await fetch(path);
  return r.json();
}

document.getElementById('up-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  show('up-out', 'running...');
  const fd = new FormData(e.target);
  const body = {repo: fd.get('repo'), worktree: fd.get('worktree')};
  const svc = csvField(fd.get('services') || '');
  if (svc.length) body.services = svc;
  try { show('up-out', await postJSON('/v1/up', body)); }
  catch (err) { show('up-out', String(err)); }
});

document.getElementById('down-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  show('down-out', 'running...');
  const fd = new FormData(e.target);
  const body = {repo: fd.get('repo'), worktree: fd.get('worktree')};
  const svc = csvField(fd.get('services') || '');
  if (svc.length) body.services = svc;
  try { show('down-out', await postJSON('/v1/down', body)); }
  catch (err) { show('down-out', String(err)); }
});

document.getElementById('status-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  show('status-out', 'running...');
  const fd = new FormData(e.target);
  const q = new URLSearchParams();
  q.set('repo', fd.get('repo'));
  q.set('worktree', fd.get('worktree'));
  const svc = (fd.get('service') || '').trim();
  if (svc) q.set('service', svc);
  try { show('status-out', await getJSON('/v1/status?' + q.toString())); }
  catch (err) { show('status-out', String(err)); }
});

document.getElementById('list-btn').addEventListener('click', async () => {
  show('list-out', 'loading...');
  try { show('list-out', await getJSON('/v1/list')); }
  catch (err) { show('list-out', String(err)); }
});

document.getElementById('list-btn').click();
</script>

</body>
</html>
```

- [ ] **Step 2: Build to confirm `go:embed` still picks the file up**

Run: `go build -o /tmp/pgpool-build-check ./cmd/pgpool && rm /tmp/pgpool-build-check`
Expected: build succeeds.

- [ ] **Step 3: Commit**

```bash
git add cmd/pgpool/index.html
git commit -m "feat(ui): render multi-service request and response in index.html"
```

---

### Task 17: Update CLAUDE.md (project-level docs)

**Files:**
- Modify: `CLAUDE.md`

- [ ] **Step 1: Replace the opening paragraph of CLAUDE.md**

Replace:

```
Single-binary server that manages ephemeral PostgreSQL containers on the host it runs on. Clients connect over HTTP (REST or MCP JSON-RPC); the server shells out to the local `docker` binary to create, inspect, and destroy containers.
```

With:

```
Single-binary server that manages ephemeral per-worktree services on the host it runs on. Each (repo, worktree) pair can run a configured set of services side-by-side; today's registry contains `postgres` and `seaweedfs`. Clients connect over HTTP (REST or MCP JSON-RPC); the server shells out to the local `docker` binary to create, inspect, and destroy containers.
```

- [ ] **Step 2: Replace the Architecture section's stateless paragraph**

Replace:

```
- Server is stateless. All state lives in Docker (containers, volumes, labels).
- Clients pass `repo` and `worktree` explicitly. The server never derives identity from `$PWD` (the original CLI spec did; this server does not).
- Clients are responsible for their own `.env` file writing. The server only returns connection URLs.
```

With:

```
- Server is stateless. All state lives in Docker (containers, volumes, labels including `pgpool.service`).
- Each (repo, worktree) can run multiple services. Service set per request is `services: [...]` in the body, or the server's `--services` default when absent.
- Clients pass `repo` and `worktree` explicitly. The server never derives identity from `$PWD`.
- Clients are responsible for their own `.env` file writing. The server only returns endpoint URLs.
```

- [ ] **Step 3: Replace the REST section**

Replace the entire `### REST` subsection with:

````
### REST

- `POST /v1/up` - body `{"repo","worktree","services":["postgres","seaweedfs"]?, "image"?}` -> `{"services":[{type,container,volume,reused,endpoints:{role:{url,host_port,container_port}}}]}`. `services` defaults to the server's configured set; `image` (when present) applies to the postgres entry.
- `POST /v1/down` - body `{"repo","worktree","services"?}` -> `{"services":[{type,container,volume}]}`. Defaults to the configured set.
- `GET /v1/status?repo=X&worktree=Y[&service=Z]` -> `{repo,worktree,services:[...]}`. Optional `service` filter narrows to one entry.
- `GET /v1/list` -> array of `{type,container,volume,repo,worktree,state,created_at,endpoints?}`. One row per pgpool-labelled container. Containers without a `pgpool.service` label are reported as `type: "postgres"` (legacy fallback).
- `GET /healthz` - liveness.
````

- [ ] **Step 4: Replace the MCP section**

Replace the `### MCP` subsection with:

```
### MCP

- `POST /mcp` - JSON-RPC 2.0. Implements `initialize`, `tools/list`, `tools/call`, `ping`.
- Tools: `pgpool_up`, `pgpool_down`, `pgpool_status`, `pgpool_list`. Up and down accept an optional `services: string[]`; status accepts an optional `service: string`. Schemas mirror REST.
- Tool call results are returned as a single `text` content block containing pretty-printed JSON. Errors set `isError: true`.
```

- [ ] **Step 5: Replace the Container naming section**

Replace the `## Container naming` section with:

```
## Container naming

- Container: `<service-prefix>-<repo>-<worktree>` - `pg-` for postgres, `weed-` for seaweedfs.
- Volume: `<service-volume-prefix>-<repo>-<worktree>` - `pgvol-` for postgres, `weedvol-` for seaweedfs.
- Names are normalized to `[a-z0-9-]`, runs of `-` collapsed, leading/trailing `-` stripped.
- If the composed name exceeds 63 chars (Docker limit), `<worktree>` is truncated and an 8-char SHA-256 prefix is appended. A warning is logged.
- All managed containers carry labels: `pgpool=true`, `pgpool.repo=<repo>`, `pgpool.worktree=<worktree>`, `pgpool.service=<type>`. `list` filters on `pgpool=true`.
```

- [ ] **Step 6: Replace the Lifecycle invariants section**

Replace `## Lifecycle invariants` section with:

```
## Lifecycle invariants

- `up` is idempotent per service. Running it twice returns the same endpoints, does not wipe data, does not recreate containers.
- `up` on an existing-but-stopped container starts it and re-runs the service's readiness probe.
- `up` on a missing container creates the volume (idempotent), runs the container with a `0:<container-port>` mapping per declared endpoint, and polls readiness every 500ms until `startup-timeout` (default 30s).
- `down` always destroys both the container and the volume for the named service. Missing container or missing volume is a successful no-op.
- Multi-service `up` and `down` process services sequentially. If service N fails, services 1..N-1 stay up; the response includes the partial successes plus an error.
- The server never auto-starts containers on its own boot. Clients must call `up`.
```

- [ ] **Step 7: Add `--services` to the Configuration table**

Insert this row into the configuration table between `--listen` and `--advertise-host`:

```
| `--services`       | `PGPOOL_SERVICES`       | `postgres`    |
```

- [ ] **Step 8: Replace the Running smoke-test block**

Replace the smoke test snippet under `## Running` with:

````
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
````

- [ ] **Step 9: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: update CLAUDE.md for multi-service model"
```

---

### Task 18: Update README.md

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Replace the opening paragraph (lines 3-7)**

Replace:

```
Single-binary server that manages ephemeral PostgreSQL containers on its host,
plus a thin CLI (`pgpoolcli`) for clients. Clients speak HTTP (REST or MCP
JSON-RPC); the server shells out to `docker`. This system is designed for
multi-agent workflows where you have lots of agents thrashing on features.
It gets really annoying when they all try to spin up docker.
```

With:

```
Single-binary server that manages ephemeral per-worktree services (Postgres
and SeaweedFS today, more services pluggable via a Go service registry), plus
a thin CLI (`pgpoolcli`) for clients. Clients speak HTTP (REST or MCP JSON-RPC);
the server shells out to `docker`. Designed for multi-agent workflows where
many agents thrash on features and need isolated, ephemeral infra per worktree.
```

- [ ] **Step 2: Replace the Run-the-server snippet**

Replace lines 47-49:

```
./pgpool --pg-password hunter2 --advertise-host pgpool.tailnet.ts.net
```

With:

```
./pgpool --pg-password hunter2 --services postgres,seaweedfs --advertise-host pgpool.tailnet.ts.net
```

- [ ] **Step 3: Replace the Per-worktree workflow section**

Replace lines 80-103 (the `### Per-worktree workflow` section through the `--image` example) with:

````
### Per-worktree workflow

Inside a git worktree:

```
pgpoolcli up                # bring up all configured services
pgpoolcli up postgres       # bring up just postgres
pgpoolcli status            # show all services for this worktree
pgpoolcli status seaweedfs  # filter to one service
pgpoolcli list              # every pgpool-managed container on the server
pgpoolcli down              # tear everything down for this worktree
pgpoolcli down postgres     # tear down only postgres
```

`up` is per-service idempotent. `down` destroys volumes - data is gone.

`repo` and `worktree` auto-detect:

- `repo`: basename of the `origin` remote URL, else basename of the git toplevel
- `worktree`: basename of `$PWD`

Override with flags on any command:

```
pgpoolcli up --repo myrepo --worktree feature-x
```
````

- [ ] **Step 4: Replace the CLAUDE.md integration block snippet**

Replace lines 139-159 (the example `<!-- BEGIN PGPOOL INTEGRATION v:1 -->` block) with the v:2 block matching the new `claudeSegment` constant from Task 15. Bump the version marker and the rules accordingly. The block in this README is illustrative and must match what `pgpoolcli init` actually writes.

```markdown
<!-- BEGIN PGPOOL INTEGRATION v:2 -->
## Per-worktree services (pgpool)
This project uses **pgpoolcli** to manage ephemeral per-worktree services (Postgres and SeaweedFS supported today).
Run `pgpoolcli prime` for full workflow context.
### Quick reference
` ``bash
pgpoolcli up                  # bring up all configured services
pgpoolcli up postgres         # just postgres
pgpoolcli status              # show all services for this worktree
pgpoolcli status seaweedfs    # filter to one service
pgpoolcli list                # all pgpool-managed containers on the host
pgpoolcli down                # tear everything down for this worktree
pgpoolcli down postgres       # tear down only postgres
` ``
Repo and worktree auto-detect from git. Override with `--repo` / `--worktree`.
### Rules
- Use `pgpoolcli` to manage per-worktree services - do NOT hand-run `docker` commands against pgpool containers.
- `pgpoolcli up` is per-service idempotent.
- `pgpoolcli down` destroys volumes - data is NOT recoverable.
- The server does not write `.env` files - read endpoint URLs from `up` / `status` and write your own.
- One container per (repo, worktree, service) tuple - names are derived, not chosen.
<!-- END PGPOOL INTEGRATION -->
```

- [ ] **Step 5: Replace the REST and MCP endpoints reference**

Replace lines 165-176 (the `## REST and MCP endpoints (reference)` section) with:

````
## REST and MCP endpoints (reference)

The CLI is a thin wrapper. If you want to hit the server directly:

```
POST /v1/up      {"repo","worktree","services":[...]?,"image"?}
POST /v1/down    {"repo","worktree","services":[...]?}
GET  /v1/status  ?repo=X&worktree=Y[&service=Z]
GET  /v1/list
GET  /healthz
POST /mcp        JSON-RPC 2.0 - tools: pgpool_up, pgpool_down, pgpool_status, pgpool_list
```

`up` and `down` default to the server's `--services` set when `services` is omitted. Responses always have a `services` array; each entry has its own `endpoints` map keyed by role (`primary` for postgres; `master`/`volume`/`filer`/`s3` for seaweedfs).
````

- [ ] **Step 6: Build to make sure docs aren't accidentally embedded into Go**

Run: `go build ./...`
Expected: success.

- [ ] **Step 7: Commit**

```bash
git add README.md
git commit -m "docs: update README for multi-service model"
```

---

## Verification

Before opening a PR, run:

```bash
go build ./...
go test ./cmd/pgpool/...
go test -tags=integration ./cmd/pgpool/... -timeout 5m   # requires docker
```

Manual smoke (one terminal as the server, another as the CLI):

```bash
# terminal 1
go run ./cmd/pgpool --pg-password password --services postgres,seaweedfs

# terminal 2
go run ./cmd/pgpoolcli up
go run ./cmd/pgpoolcli status
go run ./cmd/pgpoolcli list
curl -s 'localhost:8080/v1/status?repo=$(basename $PWD | sed s/-.*//)&worktree=$(basename $PWD)' | jq .
go run ./cmd/pgpoolcli down seaweedfs
go run ./cmd/pgpoolcli status                # postgres still running, seaweedfs missing
go run ./cmd/pgpoolcli down                  # tears everything down
```

Browser smoke: visit `http://localhost:8080/` and exercise each form.

If everything passes, the branch is PR-ready.
