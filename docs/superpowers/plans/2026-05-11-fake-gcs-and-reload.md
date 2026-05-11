# fake-gcs profile and reload command - Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a third pgpool service profile (`fake-gcs`, fsouza/fake-gcs-server) plus a `reload` lifecycle verb that runs down-then-up. Unify per-service func signatures behind a `ServiceBuildCtx` struct so future services don't churn every existing one.

**Architecture:** Refactor the `ServiceDef` func fields to take a single `ServiceBuildCtx`. Add server-side port pre-allocation so fake-gcs can pass its public URL into the container at startup (its `-external-url` flag needs the host port baked in). Register `fake-gcs` in the service map. Add `POST /v1/reload`, `pgpool_reload` MCP tool, and `pgpoolcli reload` CLI verb. Update embedded `claudeSegment` (bump `v:3` -> `v:4`) and `primeText`. Update `CLAUDE.md` and `README.md`.

**Tech Stack:** Go (stdlib only), Docker CLI, JSON-RPC 2.0 (MCP). Single `cmd/pgpool/pgpool.go` for the server and `cmd/pgpoolcli/pgpoolcli.go` for the CLI - both stay single-file.

---

## Task ordering rationale

Tasks 1-4 are the registry refactor and port reservation - they don't change observable behavior. Task 5 adds fake-gcs and is the first behaviorally visible change. Tasks 6-9 add reload across server, CLI, and MCP. Tasks 10-12 are docs. Tasks 13-14 are integration coverage that needs Docker.

Each task ends with a commit. The plan favors many small commits over a single large one - bug bisection later is easier.

---

### Task 1: Add `ServiceBuildCtx` type and convert postgres

This is the foundation refactor. Postgres uses the new signature with no behavior change. Seaweedfs follows in Task 2 to keep the diffs small and reviewable.

**Files:**
- Modify: `cmd/pgpool/pgpool.go` (registry types, postgres definition)
- Test: `cmd/pgpool/pgpool_test.go` (existing `TestServiceRegistry_Validity` must still pass)

- [ ] **Step 1: Add `ServiceBuildCtx` type and update `ServiceDef` field signatures**

In `cmd/pgpool/pgpool.go`, replace the existing `ServiceDef` block (currently lines ~42-58) with:

```go
type EndpointSpec struct {
	Role          string // "primary" | "master" | "filer" | "s3" | "storage" | ...
	ContainerPort int
	Scheme        string // "postgresql" | "http" | ...
}

// ServiceBuildCtx is the single argument passed into per-service builder funcs.
// Adding a new input here lets future services pick it up without churning every
// existing ServiceDef.
type ServiceBuildCtx struct {
	Cfg       Config
	Volume    string
	Image     string            // resolved (per-call override already applied)
	HostPorts map[string]string // role -> pre-allocated host port (as decimal string)
}

type ServiceDef struct {
	Type            string
	ContainerPrefix string
	VolumePrefix    string
	Image           string
	DockerArgs      func(bc ServiceBuildCtx) []string                                            // flags placed BEFORE the image
	DockerCommand   func(bc ServiceBuildCtx) []string                                            // args placed AFTER the image (container CMD)
	Endpoints       []EndpointSpec
	Readiness       func(ctx context.Context, s *Server, container string, bc ServiceBuildCtx) error
	BuildURL        func(bc ServiceBuildCtx, role, hostPort string) string
}
```

- [ ] **Step 2: Convert `postgresDef` to the new signature**

Replace the `postgresDef` block (currently at lines ~62-90) with:

```go
var postgresDef = ServiceDef{
	Type:            "postgres",
	ContainerPrefix: "pg",
	VolumePrefix:    "pgvol",
	Image:           "postgres:17",
	DockerArgs: func(bc ServiceBuildCtx) []string {
		return []string{
			"-v", bc.Volume + ":/var/lib/postgresql/data",
			"-e", "POSTGRES_PASSWORD=" + bc.Cfg.PgPassword,
			"-e", "POSTGRES_USER=" + bc.Cfg.PgUser,
			"-e", "POSTGRES_DB=" + bc.Cfg.PgDB,
		}
	},
	Endpoints: []EndpointSpec{
		{Role: "primary", ContainerPort: 5432, Scheme: "postgresql"},
	},
	Readiness: func(ctx context.Context, s *Server, container string, _ ServiceBuildCtx) error {
		return s.pgIsReady(ctx, container)
	},
	BuildURL: func(bc ServiceBuildCtx, _, hostPort string) string {
		u := &url.URL{
			Scheme: "postgresql",
			User:   url.UserPassword(bc.Cfg.PgUser, bc.Cfg.PgPassword),
			Host:   bc.Cfg.AdvertiseHost + ":" + hostPort,
			Path:   bc.Cfg.PgDB,
		}
		return u.String()
	},
}
```

- [ ] **Step 3: Update `buildEndpointInfo` to accept `ServiceBuildCtx`**

Replace `buildEndpointInfo` (currently at lines ~133-147) with:

```go
func buildEndpointInfo(bc ServiceBuildCtx, def ServiceDef, hostPorts map[string]string) map[string]EndpointInfo {
	out := map[string]EndpointInfo{}
	for _, e := range def.Endpoints {
		hp, ok := hostPorts[e.Role]
		if !ok {
			continue
		}
		out[e.Role] = EndpointInfo{
			URL:           def.BuildURL(bc, e.Role, hp),
			HostPort:      hp,
			ContainerPort: e.ContainerPort,
		}
	}
	return out
}
```

- [ ] **Step 4: Run tests and confirm seaweedfs is broken (we will fix it in Task 2)**

Run: `go vet ./cmd/pgpool/...`
Expected: errors about seaweedfs using the old function signatures.

This is fine - Task 2 fixes seaweedfs. Do not commit until seaweedfs compiles.

- [ ] **Step 5: Update existing tests that pass `cfg` instead of `bc`**

In `cmd/pgpool/pgpool_test.go`, replace `TestBuildEndpointInfo` (currently at lines 81-104) with:

```go
func TestBuildEndpointInfo(t *testing.T) {
	cfg := Config{
		AdvertiseHost: "host.example",
		PgUser:        "u",
		PgPassword:    "p p",
		PgDB:          "d",
	}
	hostPorts := map[string]string{"primary": "49160"}
	bc := ServiceBuildCtx{Cfg: cfg, HostPorts: hostPorts}
	endpoints := buildEndpointInfo(bc, postgresDef, hostPorts)
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

Also update `TestSeaweedfs_HasDockerCommand` (currently at lines 176-188) to use the new signature:

```go
func TestSeaweedfs_HasDockerCommand(t *testing.T) {
	def, ok := serviceDefs["seaweedfs"]
	if !ok {
		t.Fatal("seaweedfs not registered")
	}
	if def.DockerCommand == nil {
		t.Fatal("seaweedfs DockerCommand is nil")
	}
	cmd := def.DockerCommand(ServiceBuildCtx{})
	if len(cmd) == 0 || cmd[0] != "server" {
		t.Errorf("unexpected command: %v", cmd)
	}
}
```

Do not commit until Task 2 is done - the build is still broken.

---

### Task 2: Convert seaweedfs and the `containerRun` / `serviceUp` call sites

Wire the new signatures into every caller. After this task the build is green again and behavior is identical to before.

**Files:**
- Modify: `cmd/pgpool/pgpool.go` (seaweedfs def, `runOpts`, `containerRun`, `serviceUp`, `serviceStatus`, `listContainers`)

- [ ] **Step 1: Convert `seaweedfsDef`**

Replace the `seaweedfsDef` block (currently at lines ~96-119) with:

```go
var seaweedfsDef = ServiceDef{
	Type:            "seaweedfs",
	ContainerPrefix: "weed",
	VolumePrefix:    "weedvol",
	Image:           "chrislusf/seaweedfs:3.71",
	DockerArgs: func(bc ServiceBuildCtx) []string {
		return []string{"-v", bc.Volume + ":/data"}
	},
	DockerCommand: func(_ ServiceBuildCtx) []string {
		return []string{"server", "-dir=/data", "-master", "-volume", "-filer", "-s3"}
	},
	Endpoints: []EndpointSpec{
		{Role: "master", ContainerPort: 9333, Scheme: "http"},
		{Role: "volume", ContainerPort: 8080, Scheme: "http"},
		{Role: "filer", ContainerPort: 8888, Scheme: "http"},
		{Role: "s3", ContainerPort: 8333, Scheme: "http"},
	},
	Readiness: func(ctx context.Context, s *Server, container string, bc ServiceBuildCtx) error {
		return s.httpReady(ctx, "http://"+bc.Cfg.AdvertiseHost+":"+bc.HostPorts["master"]+"/cluster/status")
	},
	BuildURL: func(bc ServiceBuildCtx, _, hostPort string) string {
		return fmt.Sprintf("http://%s:%s", bc.Cfg.AdvertiseHost, hostPort)
	},
}
```

- [ ] **Step 2: Extend `runOpts` to carry `ServiceBuildCtx`**

Replace `runOpts` (currently at lines ~345-349) with:

```go
type runOpts struct {
	def            ServiceDef
	container      string
	repo, worktree string
	bc             ServiceBuildCtx
}
```

- [ ] **Step 3: Update `containerRun` to use `bc` and accept pre-allocated host ports**

Replace `containerRun` (currently at lines ~351-376) with:

```go
func (s *Server) containerRun(ctx context.Context, o runOpts) error {
	args := []string{
		"run", "-d",
		"--name", o.container,
		"--restart", "unless-stopped",
	}
	for _, e := range o.def.Endpoints {
		hp, ok := o.bc.HostPorts[e.Role]
		if !ok || hp == "" {
			return fmt.Errorf("%s: missing pre-allocated host port for role %q", o.def.Type, e.Role)
		}
		args = append(args, "-p", fmt.Sprintf("%s:%d", hp, e.ContainerPort))
	}
	args = append(args, o.def.DockerArgs(o.bc)...)
	args = append(args,
		"--label", labelPgpool+"=true",
		"--label", labelRepo+"="+o.repo,
		"--label", labelWorktree+"="+o.worktree,
		"--label", labelService+"="+o.def.Type,
	)
	args = append(args, o.bc.Image)
	if o.def.DockerCommand != nil {
		args = append(args, o.def.DockerCommand(o.bc)...)
	}
	_, errOut, err := s.runDocker(ctx, args...)
	if err != nil {
		return fmt.Errorf("docker run %s: %w: %s", o.container, err, strings.TrimSpace(errOut))
	}
	return nil
}
```

- [ ] **Step 4: Update `serviceUp` call sites to build and pass `ServiceBuildCtx`**

Replace `serviceUp` (currently at lines ~471-552) with this version. The `reuse` and `stopped` paths fetch host ports via `collectHostPorts` (the container already exists). The `create` path will be wired to `reserveHostPorts` in Task 3 - for now keep behavior identical by deferring port assignment to `docker run -p 0:CP` *would not work* because the new `containerRun` requires `HostPorts` to be set. To keep this task isolated, the create path will temporarily allocate ports inline here using the same `net.Listen("tcp", "127.0.0.1:0")` trick. Task 3 will extract this into `reserveHostPorts`.

```go
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

	bc := ServiceBuildCtx{Cfg: s.cfg, Volume: vname, Image: image}
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
		bc.HostPorts = hostPorts
		if err := def.Readiness(ctx, s, cname, bc); err != nil {
			tail := s.logsTail(ctx, cname, 50)
			return ServiceResult{}, fmt.Errorf("%s: %w\nlast 50 log lines:\n%s", def.Type, err, tail)
		}
		reused = true
	default:
		if err := s.volumeCreate(ctx, vname); err != nil {
			return ServiceResult{}, err
		}
		// Pre-allocate host ports so DockerArgs/DockerCommand can read them.
		hostPorts, err := reserveHostPorts(def.Endpoints)
		if err != nil {
			return ServiceResult{}, fmt.Errorf("%s: reserve host ports: %w", def.Type, err)
		}
		bc.HostPorts = hostPorts
		runErr := s.containerRun(ctx, runOpts{
			def: def, container: cname, repo: normalize(repo), worktree: normalize(worktree), bc: bc,
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
			if err := def.Readiness(ctx, s, cname, bc); err != nil {
				tail := s.logsTail(ctx, cname, 50)
				return ServiceResult{}, fmt.Errorf("%s: %w\nlast 50 log lines:\n%s", def.Type, err, tail)
			}
		}
	}

	finalPorts, err := s.collectHostPorts(ctx, cname, def)
	if err != nil {
		return ServiceResult{}, err
	}
	bc.HostPorts = finalPorts
	return ServiceResult{
		Type:      def.Type,
		Container: cname,
		Volume:    vname,
		Reused:    reused,
		Endpoints: buildEndpointInfo(bc, def, finalPorts),
	}, nil
}
```

Note: this references `reserveHostPorts` which will be added in Task 3. The build will still be broken after this step.

- [ ] **Step 5: Update `serviceStatus` to use `ServiceBuildCtx`**

Replace `serviceStatus` (currently at lines ~572-602) with:

```go
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
	bc := ServiceBuildCtx{Cfg: s.cfg, Volume: vname, Image: def.Image, HostPorts: hostPorts}
	res.Endpoints = buildEndpointInfo(bc, def, hostPorts)
	return res, nil
}
```

- [ ] **Step 6: Update `listContainers` to use `ServiceBuildCtx`**

Find the loop that calls `buildEndpointInfo` in `listContainers` (currently lines ~825-829) and change:

```go
		if row.State == "running" {
			hostPorts, err := s.collectHostPorts(ctx, row.Names, def)
			if err == nil {
				lc.Endpoints = buildEndpointInfo(s.cfg, def, hostPorts)
			}
		}
```

to:

```go
		if row.State == "running" {
			hostPorts, err := s.collectHostPorts(ctx, row.Names, def)
			if err == nil {
				bc := ServiceBuildCtx{Cfg: s.cfg, Volume: lc.Volume, Image: def.Image, HostPorts: hostPorts}
				lc.Endpoints = buildEndpointInfo(bc, def, hostPorts)
			}
		}
```

- [ ] **Step 7: Confirm build is still broken (waiting on `reserveHostPorts`)**

Run: `go build ./cmd/pgpool`
Expected: `undefined: reserveHostPorts`

Continue to Task 3 - we'll commit at the end of Task 3 when the tree is green.

---

### Task 3: Add `reserveHostPorts` helper and finalize the green build

This is the function `serviceUp` already calls. Once it exists, the tree compiles. We test it with a small focused unit test.

**Files:**
- Modify: `cmd/pgpool/pgpool.go` (add `reserveHostPorts`, add `"net"` import)
- Test: `cmd/pgpool/pgpool_test.go` (add `TestReserveHostPorts`)

- [ ] **Step 1: Add the failing test**

Append to `cmd/pgpool/pgpool_test.go`:

```go
func TestReserveHostPorts_AssignsDistinctNonZeroPorts(t *testing.T) {
	endpoints := []EndpointSpec{
		{Role: "a", ContainerPort: 1111, Scheme: "http"},
		{Role: "b", ContainerPort: 2222, Scheme: "http"},
		{Role: "c", ContainerPort: 3333, Scheme: "http"},
	}
	got, err := reserveHostPorts(endpoints)
	if err != nil {
		t.Fatalf("reserveHostPorts: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d: %v", len(got), got)
	}
	seen := map[string]bool{}
	for _, role := range []string{"a", "b", "c"} {
		p, ok := got[role]
		if !ok {
			t.Errorf("missing role %q in %v", role, got)
			continue
		}
		if p == "" || p == "0" {
			t.Errorf("role %q got zero/empty port %q", role, p)
		}
		if seen[p] {
			t.Errorf("duplicate port %q for role %q", p, role)
		}
		seen[p] = true
	}
}

func TestReserveHostPorts_ReleasesListeners(t *testing.T) {
	endpoints := []EndpointSpec{{Role: "a", ContainerPort: 1, Scheme: "http"}}
	got, err := reserveHostPorts(endpoints)
	if err != nil {
		t.Fatalf("reserveHostPorts: %v", err)
	}
	// The reserved port should be bindable again (listener was closed).
	port := got["a"]
	addr := "127.0.0.1:" + port
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("reserved port %s could not be rebound: %v", port, err)
	}
	_ = l.Close()
}
```

You will also need to add `"net"` to the imports in the test file.

- [ ] **Step 2: Run the test, confirm it fails**

Run: `go test ./cmd/pgpool/ -run TestReserveHostPorts -v`
Expected: FAIL with `undefined: reserveHostPorts` / `net.Listen` import error.

- [ ] **Step 3: Add `reserveHostPorts` to `cmd/pgpool/pgpool.go`**

Add `"net"` to the import block.

Add this function after `collectHostPorts` (currently around line 469):

```go
// reserveHostPorts asks the kernel for one free TCP port per endpoint by
// briefly opening then closing a listener on 127.0.0.1:0. The returned map is
// role -> decimal port string. Used on the create path so DockerArgs and
// DockerCommand can reference the host port at container-start time
// (fake-gcs-server bakes its public URL into responses via -external-url).
//
// There is a small race between Close and `docker run -p PORT:CP`; on a
// single-user dev host with no aggressive port consumers this is effectively
// never hit. If it is, docker fails fast with a bind error that surfaces
// unchanged to the caller.
func reserveHostPorts(endpoints []EndpointSpec) (map[string]string, error) {
	out := make(map[string]string, len(endpoints))
	listeners := make([]net.Listener, 0, len(endpoints))
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	for _, e := range endpoints {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("listen 127.0.0.1:0 for role %q: %w", e.Role, err)
		}
		listeners = append(listeners, l)
		addr := l.Addr().(*net.TCPAddr)
		out[e.Role] = strconv.Itoa(addr.Port)
	}
	return out, nil
}
```

Add `"strconv"` to the imports if it's not already there. (It is - used elsewhere already.)

- [ ] **Step 4: Build and run tests**

Run: `go build ./cmd/pgpool && go test ./cmd/pgpool/ -v`
Expected: all tests pass. `TestReserveHostPorts_AssignsDistinctNonZeroPorts` and `TestReserveHostPorts_ReleasesListeners` are new and pass. The existing registry / op tests still pass.

- [ ] **Step 5: Commit the registry refactor**

```bash
git add cmd/pgpool/pgpool.go cmd/pgpool/pgpool_test.go
git commit -m "$(cat <<'EOF'
refactor: ServiceDef builder funcs take a single ServiceBuildCtx

Adds reserveHostPorts and moves create-path port assignment server-side
(no behavior change for postgres / seaweedfs; needed for fake-gcs which
bakes its public URL into responses).
EOF
)"
```

---

### Task 4: Smoke-test the refactor with the existing integration suite

We should not move on without confirming the existing postgres and seaweedfs integration paths still behave identically.

**Files:**
- No code changes. Just run the integration tests.

- [ ] **Step 1: Build and run unit tests**

Run: `go test ./...`
Expected: all pass.

- [ ] **Step 2: Run the docker-gated integration tests if docker is available**

Run: `go test -tags=integration ./cmd/pgpool/ -v -run 'TestIntegration_Postgres|TestIntegration_MultiService|TestIntegration_Seaweed'`
Expected: all pass. If `docker` is not available, they will skip and that's fine.

If anything fails, stop. The refactor broke something - fix it before adding fake-gcs.

- [ ] **Step 3: No commit needed (no code changes).**

---

### Task 5: Register the `fake-gcs` service profile

Behaviorally visible: `--services postgres,fake-gcs` now works on the server. Out of the default services so existing users see no change.

**Files:**
- Modify: `cmd/pgpool/pgpool.go` (add `fakeGCSDef` and its `init()`)
- Test: `cmd/pgpool/pgpool_test.go` (registry validity already iterates all services; add a fake-gcs-specific sanity test)

- [ ] **Step 1: Add the failing test**

Append to `cmd/pgpool/pgpool_test.go`:

```go
func TestFakeGCS_RegisteredWithExpectedShape(t *testing.T) {
	def, ok := serviceDefs["fake-gcs"]
	if !ok {
		t.Fatal("fake-gcs not registered")
	}
	if def.ContainerPrefix != "gcs" {
		t.Errorf("ContainerPrefix = %q, want %q", def.ContainerPrefix, "gcs")
	}
	if def.VolumePrefix != "gcsvol" {
		t.Errorf("VolumePrefix = %q, want %q", def.VolumePrefix, "gcsvol")
	}
	if len(def.Endpoints) != 1 || def.Endpoints[0].Role != "storage" || def.Endpoints[0].ContainerPort != 4443 {
		t.Errorf("unexpected endpoints: %+v", def.Endpoints)
	}
	bc := ServiceBuildCtx{
		Cfg:       Config{AdvertiseHost: "host.example"},
		Volume:    "gcsvol-x-y",
		Image:     def.Image,
		HostPorts: map[string]string{"storage": "55555"},
	}
	args := def.DockerArgs(bc)
	if len(args) < 2 || args[0] != "-v" || args[1] != "gcsvol-x-y:/storage" {
		t.Errorf("DockerArgs unexpected: %v", args)
	}
	cmd := def.DockerCommand(bc)
	wantPublic := "host.example:55555"
	wantExternal := "http://host.example:55555"
	if !containsAdjacent(cmd, "-public-host", wantPublic) {
		t.Errorf("DockerCommand missing -public-host %q in %v", wantPublic, cmd)
	}
	if !containsAdjacent(cmd, "-external-url", wantExternal) {
		t.Errorf("DockerCommand missing -external-url %q in %v", wantExternal, cmd)
	}
	gotURL := def.BuildURL(bc, "storage", "55555")
	if gotURL != "http://host.example:55555" {
		t.Errorf("BuildURL = %q, want %q", gotURL, "http://host.example:55555")
	}
}

// containsAdjacent reports whether args contains flag followed immediately by value.
func containsAdjacent(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run the test, confirm it fails**

Run: `go test ./cmd/pgpool/ -run TestFakeGCS_RegisteredWithExpectedShape -v`
Expected: FAIL - `fake-gcs not registered`.

- [ ] **Step 3: Add the `fakeGCSDef` to `cmd/pgpool/pgpool.go`**

Insert after the existing `seaweedfsDef` block and its `init()` (around line 123):

```go
var fakeGCSDef = ServiceDef{
	Type:            "fake-gcs",
	ContainerPrefix: "gcs",
	VolumePrefix:    "gcsvol",
	Image:           "fsouza/fake-gcs-server:1.49",
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
	Endpoints: []EndpointSpec{
		{Role: "storage", ContainerPort: 4443, Scheme: "http"},
	},
	Readiness: func(ctx context.Context, s *Server, _ string, bc ServiceBuildCtx) error {
		return s.httpReady(ctx, "http://"+bc.Cfg.AdvertiseHost+":"+bc.HostPorts["storage"]+"/storage/v1/b")
	},
	BuildURL: func(bc ServiceBuildCtx, _, hostPort string) string {
		return "http://" + bc.Cfg.AdvertiseHost + ":" + hostPort
	},
}

func init() {
	serviceDefs[fakeGCSDef.Type] = fakeGCSDef
}
```

- [ ] **Step 4: Run all unit tests**

Run: `go test ./cmd/pgpool/`
Expected: all pass, including the new `TestFakeGCS_RegisteredWithExpectedShape`.

- [ ] **Step 5: Commit**

```bash
git add cmd/pgpool/pgpool.go cmd/pgpool/pgpool_test.go
git commit -m "$(cat <<'EOF'
feat: register fake-gcs-server service profile

Adds fsouza/fake-gcs-server as a third pgpool service. Pre-allocates the
host port server-side so -external-url returns reachable download URLs.
Not included in default --services; opt in via --services postgres,fake-gcs.
EOF
)"
```

---

### Task 6: Add `serviceReload` helper and `opReload` operation

Server-side internal plumbing first; the HTTP and MCP wiring comes in Task 7.

**Files:**
- Modify: `cmd/pgpool/pgpool.go` (add `opReload` near `opUp` / `opDown`, plus matching request/response types)
- Test: `cmd/pgpool/pgpool_test.go` (the unit-level "unknown service returns non-nil response" parity test)

- [ ] **Step 1: Add the failing test**

Append to `cmd/pgpool/pgpool_test.go`:

```go
func TestOpReload_UnknownServiceReturnsNonNilResponse(t *testing.T) {
	s := &Server{cfg: Config{DefaultServices: []string{"postgres"}}}
	resp, err := s.opReload(context.Background(), ReloadRequest{Repo: "r", Worktree: "w", Services: []string{"nope"}})
	if err == nil {
		t.Fatal("expected error for unknown service")
	}
	if resp == nil {
		t.Fatal("opReload must return non-nil response so handlers can read resp.Services without panicking")
	}
}
```

- [ ] **Step 2: Run, confirm it fails**

Run: `go test ./cmd/pgpool/ -run TestOpReload -v`
Expected: FAIL - `undefined: ReloadRequest` and `Server.opReload`.

- [ ] **Step 3: Add `ReloadRequest`, `ReloadResponse`, and `opReload`**

In `cmd/pgpool/pgpool.go`, near the existing `UpRequest` / `UpResponse` (around line 606), add:

```go
type ReloadRequest struct {
	Repo     string   `json:"repo"`
	Worktree string   `json:"worktree"`
	Services []string `json:"services,omitempty"`
	Image    string   `json:"image,omitempty"` // optional, applies to postgres if present
}

type ReloadResponse struct {
	Services []ServiceResult `json:"services"`
}
```

Near `opDown` (around line 703), add:

```go
// opReload is down-then-up per service. Same partial-failure semantics as opUp
// and opDown: if service N fails, services 1..N-1 are already reloaded and
// included in the response alongside the error.
func (s *Server) opReload(ctx context.Context, req ReloadRequest) (*ReloadResponse, error) {
	defs, err := s.resolveServices(req.Services)
	if err != nil {
		return &ReloadResponse{}, err
	}
	results := make([]ServiceResult, 0, len(defs))
	for _, def := range defs {
		if _, err := s.serviceDown(ctx, def, req.Repo, req.Worktree); err != nil {
			return &ReloadResponse{Services: results}, fmt.Errorf("%s: down: %w", def.Type, err)
		}
		image := ""
		if def.Type == "postgres" {
			image = req.Image
		}
		res, err := s.serviceUp(ctx, def, req.Repo, req.Worktree, image)
		if err != nil {
			return &ReloadResponse{Services: results}, fmt.Errorf("%s: up: %w", def.Type, err)
		}
		results = append(results, res)
	}
	return &ReloadResponse{Services: results}, nil
}
```

- [ ] **Step 4: Run the test, confirm it passes**

Run: `go test ./cmd/pgpool/ -run TestOpReload -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/pgpool/pgpool.go cmd/pgpool/pgpool_test.go
git commit -m "feat: add opReload (server-side down-then-up per service)"
```

---

### Task 7: Wire `POST /v1/reload` HTTP handler

Same shape as `/v1/up` and `/v1/down`. Hooked into the mux.

**Files:**
- Modify: `cmd/pgpool/pgpool.go` (add `handleReload`, register route)

- [ ] **Step 1: Add `handleReload`**

Insert near `handleDown` (around line 865) the following:

```go
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	var req ReloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("parse body: %w", err))
		return
	}
	resp, err := s.opReload(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":    err.Error(),
			"services": resp.Services,
		})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
```

- [ ] **Step 2: Register the route in `main()`**

In `main()` (around line 1289), add a new mux line after `POST /v1/down`:

```go
	mux.HandleFunc("POST /v1/reload", srv.handleReload)
```

So the block reads:

```go
	mux.HandleFunc("POST /v1/up", srv.handleUp)
	mux.HandleFunc("POST /v1/down", srv.handleDown)
	mux.HandleFunc("POST /v1/reload", srv.handleReload)
	mux.HandleFunc("GET /v1/status", srv.handleStatus)
	mux.HandleFunc("GET /v1/logs", srv.handleLogs)
	mux.HandleFunc("GET /v1/list", srv.handleList)
	mux.HandleFunc("POST /mcp", srv.handleMCP)
```

- [ ] **Step 3: Build**

Run: `go build ./cmd/pgpool`
Expected: success.

- [ ] **Step 4: Commit**

```bash
git add cmd/pgpool/pgpool.go
git commit -m "feat: POST /v1/reload HTTP handler"
```

---

### Task 8: Add `pgpool_reload` MCP tool

Mirrors `pgpool_up`'s schema. Same response convention (single text content block with pretty JSON).

**Files:**
- Modify: `cmd/pgpool/pgpool.go` (extend `tools()`, extend `callTool` switch)

- [ ] **Step 1: Extend `tools()` to include `pgpool_reload`**

In `tools()` (around line 1023), the `upSchema` variable can be reused for reload since `pgpool_reload` accepts the same fields. Update the `return` so the slice includes the new tool:

```go
	return []mcpTool{
		{Name: "pgpool_up", Description: "Bring up the configured services for a worktree. Returns one entry per service with its endpoints.", InputSchema: upSchema},
		{Name: "pgpool_down", Description: "Tear down services for a worktree. Defaults to all configured services.", InputSchema: rwSvc},
		{Name: "pgpool_reload", Description: "Tear down then re-create services for a worktree (destroys volumes). Defaults to all configured services.", InputSchema: upSchema},
		{Name: "pgpool_status", Description: "Report state of services for a worktree. Optionally filter to one service.", InputSchema: rwOptionalService},
		{Name: "pgpool_list", Description: "List all pgpool-managed containers on this host.", InputSchema: empty},
		{Name: "pgpool_logs", Description: "Tail container logs for one or all configured services in a worktree.", InputSchema: logsSchema},
	}
```

- [ ] **Step 2: Extend `callTool` switch**

In `callTool` (around line 1156), add a case after `pgpool_down`:

```go
	case "pgpool_reload":
		var req ReloadRequest
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return nil, fmt.Errorf("parse arguments: %w", err)
			}
		}
		return s.opReload(ctx, req)
```

- [ ] **Step 3: Build**

Run: `go build ./cmd/pgpool`
Expected: success.

- [ ] **Step 4: Commit**

```bash
git add cmd/pgpool/pgpool.go
git commit -m "feat: pgpool_reload MCP tool"
```

---

### Task 9: Add `pgpoolcli reload` CLI verb

Mirrors `up` / `down` exactly: positional args narrow to specific services; output is one block per service.

**Files:**
- Modify: `cmd/pgpoolcli/pgpoolcli.go` (add `cmdReload`, `runReload`, register in `main`, extend `usage()`)

- [ ] **Step 1: Add `cmdReload`**

In `cmd/pgpoolcli/pgpoolcli.go`, after `cmdDown` (around line 463), add:

```go
func cmdReload(rc *runCtx, repo, worktree string, services []string) error {
	body := map[string]any{"repo": repo, "worktree": worktree}
	if len(services) > 0 {
		body["services"] = services
	}
	var resp struct {
		Services []serviceResultJSON `json:"services"`
	}
	if err := rc.client.do(http.MethodPost, "/v1/reload", body, &resp); err != nil {
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

- [ ] **Step 2: Add `runReload`**

After `runDown` (around line 922), add:

```go
func runReload(args []string) {
	fs := flag.NewFlagSet("reload", flag.ExitOnError)
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
	fail(cmdReload(rc, r, w, fs.Args()))
}
```

- [ ] **Step 3: Register the subcommand in `main`**

In `main` (around line 852), in the second switch, add a `case` for `reload`:

```go
	switch cmd {
	case "up":
		runUp(args)
	case "down":
		runDown(args)
	case "reload":
		runReload(args)
	case "status":
		runStatus(args)
	case "list":
		runList(args)
	case "logs":
		runLogs(args)
	case "health":
		runHealth(args)
	case "config":
		runConfig(args)
	case "init":
		runInit(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
```

- [ ] **Step 4: Update `usage()`**

In `usage()` (around line 806), update the `Commands:` block to include `reload`:

```go
	fmt.Fprint(os.Stderr, `pgpoolcli - manage ephemeral Postgres containers via a pgpool server

Usage:
  pgpoolcli <command> [flags]

Commands:
  up       Create or reuse the configured services for this worktree
  down     Destroy the services and their volumes for this worktree
  reload   Down-then-up the services for this worktree (destroys volumes)
  status   Show state and connection URLs for this worktree
  logs     Tail container logs for one or all services in this worktree
  list     List all pgpool-managed containers on the server
  health   Check that the server is reachable (also reports server version)
  config   Print the resolved config
  init     Write a config file and append a block to CLAUDE.md
  prime    Print the full workflow reference

Global flags (all commands):
  --url URL         Server URL (env: PGPOOL_URL)
  --config PATH     Config file path (env: PGPOOL_CONFIG)
  --json            Print raw JSON instead of a summary

Config file: ~/.config/pgpool/pgpool.json
`)
```

- [ ] **Step 5: Build**

Run: `go build ./cmd/pgpoolcli`
Expected: success.

- [ ] **Step 6: Commit**

```bash
git add cmd/pgpoolcli/pgpoolcli.go
git commit -m "feat: pgpoolcli reload subcommand"
```

---

### Task 10: Bump `claudeSegment` to v:4 and update `primeText`

Adding `fake-gcs` and `reload` means agents reading these blobs need both surfaces. `pgpoolcli init` will replace the in-place `v:3` block.

**Files:**
- Modify: `cmd/pgpoolcli/pgpoolcli.go` (`claudeSegment` and `primeText` consts)
- Test: `cmd/pgpoolcli/pgpoolcli_test.go` (existing tests still pass against `v:4`; add one that explicitly replaces `v:3` -> `v:4`)

- [ ] **Step 1: Add the failing test**

Append to `cmd/pgpoolcli/pgpoolcli_test.go`:

```go
func TestCmdInit_ReplacesV3WithV4(t *testing.T) {
	const oldBlock = `<!-- BEGIN PGPOOL INTEGRATION v:3 -->
old v3 body
<!-- END PGPOOL INTEGRATION -->`
	seed := "# Project\n\n" + oldBlock + "\n"

	got, _ := initTestEnv(t, seed)

	if bytes.Count(got, []byte("<!-- BEGIN PGPOOL INTEGRATION")) != 1 {
		t.Fatalf("want exactly one block, got:\n%s", got)
	}
	if bytes.Contains(got, []byte("v:3")) {
		t.Errorf("v:3 marker should be gone:\n%s", got)
	}
	if !bytes.Contains(got, []byte("v:4")) {
		t.Errorf("v:4 marker missing:\n%s", got)
	}
	if !bytes.Contains(got, []byte("pgpoolcli reload")) {
		t.Errorf("expected reload to be mentioned in updated segment:\n%s", got)
	}
	if !bytes.Contains(got, []byte("fake-gcs")) {
		t.Errorf("expected fake-gcs to be mentioned in updated segment:\n%s", got)
	}
}
```

Also: the existing `TestCmdInit_ReplacesOlderIntegrationBlock` test checks for `v:3`. Update its assertion:

```go
	if !bytes.Contains(got, []byte("v:4")) {
		t.Errorf("new v:4 marker missing:\n%s", got)
	}
```

(was `v:3`; bump to `v:4`).

- [ ] **Step 2: Run, confirm tests fail**

Run: `go test ./cmd/pgpoolcli/`
Expected: FAIL - `v:4 marker missing`, `expected reload to be mentioned`, `expected fake-gcs`.

- [ ] **Step 3: Update `claudeSegment` in `cmd/pgpoolcli/pgpoolcli.go`**

Replace the entire `claudeSegment` const (currently at lines 42-69) with:

```go
const claudeSegment = `<!-- BEGIN PGPOOL INTEGRATION v:4 -->
## Per-worktree services (pgpool)
This project uses **pgpoolcli** to manage ephemeral per-worktree services (Postgres, SeaweedFS, and fake-gcs-server supported today).
Run ` + "`pgpoolcli prime`" + ` for full workflow context including the per-service endpoint catalog.
### Quick reference
` + "```bash" + `
pgpoolcli up                  # bring up all configured services
pgpoolcli up postgres         # just postgres
pgpoolcli status              # show all services for this worktree
pgpoolcli status seaweedfs    # filter to one service
pgpoolcli logs                # tail logs for all services in this worktree
pgpoolcli logs postgres       # tail logs for one service
pgpoolcli list                # all pgpool-managed containers on the host
pgpoolcli reload              # down-then-up everything for this worktree (destroys volumes)
pgpoolcli reload postgres     # reload just postgres
pgpoolcli down                # tear everything down for this worktree
pgpoolcli down postgres       # tear down only postgres
` + "```" + `
Repo and worktree auto-detect from git. Override with ` + "`--repo`" + ` / ` + "`--worktree`" + `.
### Endpoints
- ` + "`postgres`" + `: ` + "`primary`" + ` role -> ` + "`postgresql://USER:PASS@HOST:PORT/DB`" + ` (credentials are server-configured).
- ` + "`seaweedfs`" + `: ` + "`master`" + `, ` + "`volume`" + `, ` + "`filer`" + `, ` + "`s3`" + ` roles -> ` + "`http://HOST:PORT`" + ` per role.
- ` + "`fake-gcs`" + `: ` + "`storage`" + ` role -> ` + "`http://HOST:PORT`" + ` (GCS-compatible JSON API; point clients via ` + "`STORAGE_EMULATOR_HOST`" + `).
### Rules
- Use ` + "`pgpoolcli`" + ` to manage per-worktree services - do NOT hand-run ` + "`docker`" + ` commands against pgpool containers.
- ` + "`pgpoolcli up`" + ` is per-service idempotent. Re-running brings up missing services and reuses existing ones.
- ` + "`pgpoolcli down`" + ` destroys volumes - data is NOT recoverable.
- ` + "`pgpoolcli reload`" + ` is ` + "`down`" + ` followed by ` + "`up`" + ` per service - it ALSO destroys volumes. Use ` + "`up`" + ` to bring missing services up without losing data.
- The server does not write ` + "`.env`" + ` files - read endpoint URLs from ` + "`up`" + ` / ` + "`status`" + ` and write your own.
- One container per (repo, worktree, service) tuple - names are derived, not chosen.
- If ` + "`status`" + ` / ` + "`up`" + ` return empty service lists, the server is older than the CLI. Run ` + "`pgpoolcli health`" + ` to compare versions.
<!-- END PGPOOL INTEGRATION -->`
```

- [ ] **Step 4: Update `primeText` in `cmd/pgpoolcli/pgpoolcli.go`**

Replace `primeText` (currently at lines 73-157) with the version that includes `reload` and the fake-gcs catalog:

```go
const primeText = `pgpoolcli - per-worktree service management

Each (repo, worktree) pair gets one ephemeral container per registered service.
Today's services: postgres, seaweedfs, fake-gcs. The server is stateless; all
state lives in Docker labels and volumes. Auto-detection fills in repo and
worktree from git when you do not pass them.

Commands:
  pgpoolcli up [SERVICE...]
    Bring up the listed services for this worktree, or all configured services
    if no service is named. Idempotent. Returns one entry per service.

  pgpoolcli down [SERVICE...]
    Destroy the listed services (or all configured services). NOT REVERSIBLE -
    volumes are gone.

  pgpoolcli reload [SERVICE...]
    Down-then-up the listed services. Equivalent to running down followed by
    up. ALSO DESTROYS VOLUMES - data is gone. Use up (not reload) if you just
    want to bring missing services back online.

  pgpoolcli status [SERVICE]
    Report state for every configured service in this worktree, or just the
    named service.

  pgpoolcli logs [SERVICE] [--tail N]
    Tail the most recent log lines for one service or all configured services
    in this worktree. Default --tail is 100, max 5000.

  pgpoolcli list
    Inventory of every pgpool-managed container on the server's host.

  pgpoolcli health
    Liveness check against the server. Prints the server version - if it does
    not match the CLI version, status/up/list may return empty service lists
    because the response shape changed.

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

Service catalog:
  postgres
    image:     postgres:17 (override per-call via the up "image" field)
    endpoints: primary  (postgresql, container port 5432)
    URL form:  postgresql://USER:PASS@HOST:HOSTPORT/DB
    notes:     User, password, and DB are server-configured (--pg-user,
               --pg-password, --pg-db). Read primary.url from up/status
               responses; the server does not write a .env for you.

  seaweedfs
    image:     chrislusf/seaweedfs:3.71
    endpoints: master  (http, container 9333) - cluster control plane
               volume  (http, container 8080) - chunk storage
               filer   (http, container 8888) - filesystem API
               s3      (http, container 8333) - S3-compatible API
    URL form:  http://HOST:HOSTPORT for each role
    notes:     Readiness is checked against the master at /cluster/status.
               Use the s3 endpoint with any S3 SDK; access keys are not
               enforced in the default configuration.

  fake-gcs
    image:     fsouza/fake-gcs-server:1.49
    endpoints: storage (http, container 4443) - GCS-compatible JSON API
    URL form:  http://HOST:HOSTPORT
    notes:     Point Google Cloud Storage clients here via the
               STORAGE_EMULATOR_HOST env var. The server pre-allocates the
               host port and passes it as -external-url so download links
               returned by the API are reachable. Auth is NOT enforced in
               the default configuration.

Typical flow inside a worktree:
  1. pgpoolcli up                # all services
  2. read connection URLs from each service's "endpoints" map
  3. write into your .env (the server does not do this for you)
  4. pgpoolcli logs              # if a service does not look healthy
  5. pgpoolcli down              # when the worktree is done

Troubleshooting:
  - "status returns no services" or "up returns no URLs" usually means the
    server is on an older release than the CLI. Run pgpoolcli health to check
    the server version and restart the server with the matching binary.
  - "container does not exist" from logs means up has not been run yet (or
    down has been run since).
  - "404 Not Found" on pgpoolcli reload means the server is older than the
    CLI. Upgrade the server.
`
```

- [ ] **Step 5: Run CLI tests**

Run: `go test ./cmd/pgpoolcli/`
Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add cmd/pgpoolcli/pgpoolcli.go cmd/pgpoolcli/pgpoolcli_test.go
git commit -m "$(cat <<'EOF'
feat(cli): bump claudeSegment to v:4; document reload and fake-gcs

primeText and claudeSegment now mention pgpoolcli reload and the fake-gcs
service catalog. pgpoolcli init replaces stale v:3 blocks in place.
EOF
)"
```

---

### Task 11: Update `CLAUDE.md` (repo root)

The spec for the server itself. Add fake-gcs everywhere postgres / seaweedfs is mentioned, add the reload endpoint and tool, add the new container-name prefixes.

**Files:**
- Modify: `CLAUDE.md`

- [ ] **Step 1: Update the opening line**

Find: `today's registry contains \`postgres\` and \`seaweedfs\`` and replace with `today's registry contains \`postgres\`, \`seaweedfs\`, and \`fake-gcs\``.

- [ ] **Step 2: Update the REST section**

Find the `### REST` block. After the `POST /v1/down` line, add:

```
- `POST /v1/reload` - body `{"repo","worktree","services"?, "image"?}` -> same shape as `/v1/up`. Equivalent to running `down` then `up` per service. Volumes are destroyed.
```

- [ ] **Step 3: Update the MCP section**

Find: `Tools: \`pgpool_up\`, \`pgpool_down\`, \`pgpool_status\`, \`pgpool_logs\`, \`pgpool_list\``. Replace with:

```
- Tools: `pgpool_up`, `pgpool_down`, `pgpool_reload`, `pgpool_status`, `pgpool_logs`, `pgpool_list`. Up and reload accept an optional `services: string[]`; down accepts the same; status and logs accept an optional `service: string`; logs additionally accepts `tail: integer`. Schemas mirror REST.
```

- [ ] **Step 4: Update the container naming section**

Find the bulleted line that starts `Container: <service-prefix>-<repo>-<worktree>`. Replace it with:

```
- Container: `<service-prefix>-<repo>-<worktree>` - `pg-` for postgres, `weed-` for seaweedfs, `gcs-` for fake-gcs.
- Volume: `<service-volume-prefix>-<repo>-<worktree>` - `pgvol-` for postgres, `weedvol-` for seaweedfs, `gcsvol-` for fake-gcs.
```

- [ ] **Step 5: Add a brief note about port pre-allocation**

Under `## Lifecycle invariants`, after the bullet that begins `- \`up\` on a missing container creates the volume...`, add:

```
- `up` on a missing container reserves one free host port per declared endpoint *before* `docker run`, so services whose CLI flags need their public URL at startup (e.g. fake-gcs-server's `-external-url`) can have it baked in. There is a small race window between port reservation and bind; if Docker fails to bind, the error is surfaced unchanged.
```

- [ ] **Step 6: Add a brief note about reload**

Under `## Lifecycle invariants` (after the existing bullets), add:

```
- `reload` is `down` then `up` per service. Volumes are destroyed - this is the documented contract, not a bug. Use `up` if you only need to bring missing services online without losing data.
```

- [ ] **Step 7: Confirm CLAUDE.md is well-formed**

Run: `grep -c "fake-gcs" CLAUDE.md`
Expected: at least 4 matches.

Run: `grep -c "/v1/reload" CLAUDE.md`
Expected: at least 1 match.

- [ ] **Step 8: Commit**

```bash
git add CLAUDE.md
git commit -m "docs(claude): document fake-gcs service and /v1/reload endpoint"
```

---

### Task 12: Update `README.md`

Same spirit as CLAUDE.md but customer-facing.

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Update the opening paragraph**

Find: `manages ephemeral per-worktree services (Postgres\nand SeaweedFS today, more services pluggable via a Go service registry)` and replace `Postgres\nand SeaweedFS` with `Postgres, SeaweedFS, and fake-gcs-server`.

- [ ] **Step 2: Update the "Per-worktree workflow" code block**

Find the block at lines ~84-94 and replace with:

```
pgpoolcli up                # bring up all configured services
pgpoolcli up postgres       # bring up just postgres
pgpoolcli status            # show all services for this worktree
pgpoolcli status seaweedfs  # filter to one service
pgpoolcli logs              # tail logs for every service in this worktree
pgpoolcli logs postgres     # tail logs for one service
pgpoolcli list              # every pgpool-managed container on the server
pgpoolcli reload            # down-then-up everything for this worktree (destroys volumes)
pgpoolcli reload postgres   # reload just postgres
pgpoolcli down              # tear everything down for this worktree
pgpoolcli down postgres     # tear down only postgres
```

- [ ] **Step 3: Update the "CLAUDE.md integration" embedded block**

The README shows the literal `claudeSegment` text. Replace the entire block starting at `<!-- BEGIN PGPOOL INTEGRATION v:3 -->` and ending at `<!-- END PGPOOL INTEGRATION -->` with the new v:4 segment from Task 10. Pay attention to indentation - the README block is inside a markdown code fence.

- [ ] **Step 4: Update the "REST and MCP endpoints (reference)" code block**

Find the block at lines ~184-192 and replace with:

```
POST /v1/up      {"repo","worktree","services":[...]?,"image"?}
POST /v1/down    {"repo","worktree","services":[...]?}
POST /v1/reload  {"repo","worktree","services":[...]?,"image"?}
GET  /v1/status  ?repo=X&worktree=Y[&service=Z]
GET  /v1/logs    ?repo=X&worktree=Y[&service=Z][&tail=N]
GET  /v1/list
GET  /healthz
POST /mcp        JSON-RPC 2.0 - tools: pgpool_up, pgpool_down, pgpool_reload, pgpool_status, pgpool_logs, pgpool_list
```

- [ ] **Step 5: Update the endpoint summary paragraph**

Find: `each entry has its own \`endpoints\` map keyed by role (\`primary\` for postgres; \`master\`/\`volume\`/\`filer\`/\`s3\` for seaweedfs)` and replace with:

```
each entry has its own `endpoints` map keyed by role (`primary` for postgres; `master`/`volume`/`filer`/`s3` for seaweedfs; `storage` for fake-gcs).
```

- [ ] **Step 6: Commit**

```bash
git add README.md
git commit -m "docs(readme): cover fake-gcs and reload"
```

---

### Task 13: Add integration test for fake-gcs lifecycle (docker-gated)

**Files:**
- Modify: `cmd/pgpool/integration_test.go`

- [ ] **Step 1: Add the test**

Append to `cmd/pgpool/integration_test.go`:

```go
func TestIntegration_FakeGCSLifecycle(t *testing.T) {
	s := newTestServer(t, []string{"fake-gcs"})
	ctx := context.Background()
	defer s.opDown(ctx, DownRequest{Repo: "itest", Worktree: "gcs"})

	up, err := s.opUp(ctx, UpRequest{Repo: "itest", Worktree: "gcs"})
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if len(up.Services) != 1 || up.Services[0].Type != "fake-gcs" {
		t.Fatalf("unexpected up response: %+v", up)
	}
	storage, ok := up.Services[0].Endpoints["storage"]
	if !ok || storage.URL == "" {
		t.Fatalf("missing storage endpoint: %+v", up.Services[0])
	}

	// Create a bucket via the GCS-compatible API.
	createBody := `{"name":"itest-bucket"}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, storage.URL+"/storage/v1/b?project=itest", io.NopCloser(strings.NewReader(createBody)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	httpC := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpC.Do(req)
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create bucket status=%d body=%s", resp.StatusCode, body)
	}

	// List buckets and confirm ours is present.
	listResp, err := httpC.Get(storage.URL + "/storage/v1/b?project=itest")
	if err != nil {
		t.Fatalf("list buckets: %v", err)
	}
	defer listResp.Body.Close()
	body, _ := io.ReadAll(listResp.Body)
	if !strings.Contains(string(body), "itest-bucket") {
		t.Fatalf("itest-bucket not in list response: %s", body)
	}

	// The external URL embedded in responses should reference advertise-host:hostPort.
	var listJSON struct {
		Items []struct {
			SelfLink string `json:"selfLink"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &listJSON); err != nil {
		t.Fatalf("decode list body: %v: %s", err, body)
	}
	if len(listJSON.Items) > 0 && !strings.Contains(listJSON.Items[0].SelfLink, ":"+storage.HostPort) {
		t.Errorf("selfLink %q does not include host port %s", listJSON.Items[0].SelfLink, storage.HostPort)
	}
}
```

You may need to add `"strings"` to the imports - it is already there. `io` is already there. `time` is already there.

- [ ] **Step 2: Run the integration test**

Run: `go test -tags=integration ./cmd/pgpool/ -v -run TestIntegration_FakeGCSLifecycle`
Expected: PASS (requires Docker; will skip if `docker info` fails).

If the readiness probe times out, check the image tag is correct (`fsouza/fake-gcs-server:1.49` exists on Docker Hub) and that the readiness URL is `/storage/v1/b` (not `/_internal/healthcheck`).

- [ ] **Step 3: Commit**

```bash
git add cmd/pgpool/integration_test.go
git commit -m "test: integration coverage for fake-gcs service lifecycle"
```

---

### Task 14: Add integration test for reload (docker-gated)

Two assertions: (1) a service successfully reloads, and (2) reload destroys the volume (documented contract).

**Files:**
- Modify: `cmd/pgpool/integration_test.go`

- [ ] **Step 1: Add the test**

Append to `cmd/pgpool/integration_test.go`:

```go
func TestIntegration_ReloadDestroysPostgresData(t *testing.T) {
	s := newTestServer(t, []string{"postgres"})
	ctx := context.Background()
	defer s.opDown(ctx, DownRequest{Repo: "itest", Worktree: "reload-pg"})

	up, err := s.opUp(ctx, UpRequest{Repo: "itest", Worktree: "reload-pg"})
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	cname := up.Services[0].Container

	// Write a sentinel row inside the container so we can later prove it's gone.
	if _, _, err := s.runDocker(ctx, "exec", cname,
		"psql", "-U", s.cfg.PgUser, "-d", s.cfg.PgDB,
		"-c", "create table sentinel (x int); insert into sentinel values (1);",
	); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}

	// Reload.
	rel, err := s.opReload(ctx, ReloadRequest{Repo: "itest", Worktree: "reload-pg"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(rel.Services) != 1 || rel.Services[0].Type != "postgres" {
		t.Fatalf("unexpected reload response: %+v", rel)
	}
	newName := rel.Services[0].Container
	if newName != cname {
		// container name is deterministic from (repo, worktree, prefix), so the
		// name should be identical; if it isn't, the container was renamed.
		t.Logf("note: container name changed across reload: %s -> %s", cname, newName)
	}

	// The sentinel table should NOT exist - reload destroyed the volume.
	_, errOut, err := s.runDocker(ctx, "exec", newName,
		"psql", "-U", s.cfg.PgUser, "-d", s.cfg.PgDB,
		"-c", "select count(*) from sentinel;",
	)
	if err == nil {
		t.Fatalf("expected error querying sentinel table after reload; got success. stderr: %s", errOut)
	}
	if !strings.Contains(errOut, "does not exist") && !strings.Contains(errOut, "relation \"sentinel\"") {
		t.Fatalf("expected 'does not exist' error, got: %s", errOut)
	}
}

func TestIntegration_ReloadFakeGCS(t *testing.T) {
	s := newTestServer(t, []string{"fake-gcs"})
	ctx := context.Background()
	defer s.opDown(ctx, DownRequest{Repo: "itest", Worktree: "reload-gcs"})

	up, err := s.opUp(ctx, UpRequest{Repo: "itest", Worktree: "reload-gcs"})
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if len(up.Services) != 1 {
		t.Fatalf("unexpected up: %+v", up)
	}

	rel, err := s.opReload(ctx, ReloadRequest{Repo: "itest", Worktree: "reload-gcs", Services: []string{"fake-gcs"}})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(rel.Services) != 1 || rel.Services[0].Type != "fake-gcs" {
		t.Fatalf("unexpected reload response: %+v", rel)
	}
	storage, ok := rel.Services[0].Endpoints["storage"]
	if !ok || storage.URL == "" {
		t.Fatalf("missing storage endpoint after reload: %+v", rel.Services[0])
	}

	resp, err := http.Get(storage.URL + "/storage/v1/b?project=itest")
	if err != nil {
		t.Fatalf("storage endpoint not reachable post-reload: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("unexpected status %d after reload", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run the tests**

Run: `go test -tags=integration ./cmd/pgpool/ -v -run 'TestIntegration_Reload'`
Expected: both PASS (requires Docker).

- [ ] **Step 3: Commit**

```bash
git add cmd/pgpool/integration_test.go
git commit -m "test: integration coverage for reload (postgres data loss, fake-gcs)"
```

---

### Task 15: Final verification

**Files:** None modified - this task confirms green state.

- [ ] **Step 1: Run all unit tests**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 2: Run all integration tests (if Docker is available)**

Run: `go test -tags=integration ./cmd/pgpool/ -v`
Expected: PASS (or `SKIP: docker not available`).

- [ ] **Step 3: Build both binaries**

Run: `go build -o /tmp/pgpool ./cmd/pgpool && go build -o /tmp/pgpoolcli ./cmd/pgpoolcli`
Expected: success.

- [ ] **Step 4: End-to-end smoke check against a running server**

(Only if you have Docker locally.)

```bash
/tmp/pgpool --pg-password hunter2 --services postgres,fake-gcs &
PGPOOL_PID=$!
sleep 1

# fake-gcs up
/tmp/pgpoolcli up --repo smoke --worktree t1 fake-gcs
# Look for storage URL in output

# reload
/tmp/pgpoolcli reload --repo smoke --worktree t1 fake-gcs
# Look for storage URL in output

# down
/tmp/pgpoolcli down --repo smoke --worktree t1 fake-gcs

kill $PGPOOL_PID
```

If anything errors, do not move on. Surface the failure and fix at the root.

- [ ] **Step 5: No commit (verification only).**

---

## Self-review notes

**Spec coverage:**
- `ServiceBuildCtx` refactor: Tasks 1-2.
- Port pre-allocation (`reserveHostPorts`): Task 3.
- fake-gcs profile: Task 5.
- Reload endpoint: Tasks 6-7.
- Reload MCP tool: Task 8.
- Reload CLI verb: Task 9.
- `claudeSegment` v:3 -> v:4 and `primeText` update: Task 10.
- `CLAUDE.md` updates: Task 11.
- `README.md` updates: Task 12.
- Integration coverage (fake-gcs + reload): Tasks 13-14.
- Final E2E: Task 15.

**Placeholder scan:** no TBDs, no "TODO", no "add appropriate error handling" placeholders. Each step shows the exact code or command.

**Type consistency:** `ServiceBuildCtx` (with `Cfg`, `Volume`, `Image`, `HostPorts`) is referenced identically in Tasks 1-3, 5. `ReloadRequest` / `ReloadResponse` shape matches `UpRequest` / `UpResponse`. CLI function names (`cmdReload`, `runReload`) mirror `cmdUp`/`runUp`. Container/volume prefixes (`gcs` / `gcsvol`) are consistent across the def, the docs, and the v:4 claudeSegment.
