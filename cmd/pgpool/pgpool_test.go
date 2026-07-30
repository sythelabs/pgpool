package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"FooBar":       "foobar",
		"foo_bar":      "foo-bar",
		"--foo--bar--": "foo-bar",
		"a/b/c":        "a-b-c",
		"  spaced  ":   "spaced",
		"":             "",
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

func TestBuildEndpointInfo(t *testing.T) {
	cfg := Config{
		AdvertiseHost: "host.example",
		PgUser:        "u",
		PgPassword:    "p p",
		PgDB:          "d",
	}
	hostPorts := map[EndpointRole]string{RolePrimary: "49160"}
	bc := ServiceBuildCtx{Cfg: cfg, HostPorts: hostPorts}
	endpoints := buildEndpointInfo(bc, postgresDef, hostPorts)
	got, ok := endpoints[RolePrimary]
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
		seenRoles := map[EndpointRole]bool{}
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

func TestOpUp_UnknownServiceReturnsNonNilResponse(t *testing.T) {
	s := &Server{cfg: Config{DefaultServices: []string{"postgres"}}}
	resp, err := s.opUp(context.Background(), UpRequest{Repo: "r", Worktree: "w", Services: []string{"nope"}})
	if err == nil {
		t.Fatal("expected error for unknown service")
	}
	if resp == nil {
		t.Fatal("opUp must return non-nil response so handlers can read resp.Services without panicking")
	}
}

func TestOpDown_UnknownServiceReturnsNonNilResponse(t *testing.T) {
	s := &Server{cfg: Config{DefaultServices: []string{"postgres"}}}
	resp, err := s.opDown(context.Background(), DownRequest{Repo: "r", Worktree: "w", Services: []string{"nope"}})
	if err == nil {
		t.Fatal("expected error for unknown service")
	}
	if resp == nil {
		t.Fatal("opDown must return non-nil response so handlers can read resp.Services without panicking")
	}
}

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

func TestOpLogs_RejectsEmptyRepoOrWorktree(t *testing.T) {
	s := &Server{cfg: Config{DefaultServices: []string{"postgres"}}}
	if _, err := s.opLogs(context.Background(), "", "wt", "", 50); err == nil {
		t.Error("empty repo: expected error")
	}
	if _, err := s.opLogs(context.Background(), "r", "", "", 50); err == nil {
		t.Error("empty worktree: expected error")
	}
}

func TestOpLogs_UnknownServiceReturnsError(t *testing.T) {
	s := &Server{cfg: Config{DefaultServices: []string{"postgres"}}}
	if _, err := s.opLogs(context.Background(), "r", "w", "nope", 50); err == nil {
		t.Fatal("expected error for unknown service")
	}
}

func TestOpLogs_NoDefaultsAndNoSelectionReturnsError(t *testing.T) {
	s := &Server{cfg: Config{DefaultServices: nil}}
	if _, err := s.opLogs(context.Background(), "r", "w", "", 50); err == nil {
		t.Fatal("expected error when no defaults and no service selection")
	}
}

func TestParseTailParam(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"", defaultLogsTail, false},
		{"50", 50, false},
		{"0", 0, true},
		{"-3", 0, true},
		{"abc", 0, true},
		{"99999", maxLogsTail, false},
	}
	for _, tc := range cases {
		got, err := parseTailParam(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseTailParam(%q) expected error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTailParam(%q) unexpected error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("parseTailParam(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

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
	for _, role := range []EndpointRole{"a", "b", "c"} {
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
	port := got["a"]
	addr := "127.0.0.1:" + port
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("reserved port %s could not be rebound: %v", port, err)
	}
	_ = l.Close()
}

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
	if len(def.Endpoints) != 1 || def.Endpoints[0].Role != RoleStorage || def.Endpoints[0].ContainerPort != 4443 {
		t.Errorf("unexpected endpoints: %+v", def.Endpoints)
	}
	bc := ServiceBuildCtx{
		Cfg:       Config{AdvertiseHost: "host.example"},
		Volume:    "gcsvol-x-y",
		Image:     def.Image,
		HostPorts: map[EndpointRole]string{RoleStorage: "55555"},
	}
	args := def.DockerArgs(bc)
	if !containsAdjacent(args, "-v", "gcsvol-x-y:/storage") {
		t.Errorf("DockerArgs missing -v %q in %v", "gcsvol-x-y:/storage", args)
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
	gotURL := def.BuildURL(bc, RoleStorage, "55555")
	if gotURL != "http://host.example:55555" {
		t.Errorf("BuildURL = %q, want %q", gotURL, "http://host.example:55555")
	}
}

// TestPostgres_MountsVolumeAtParent guards the postgres data-volume mount path.
// Official postgres 18+ images relocated PGDATA into a major-version subdir and
// require the volume mounted at /var/lib/postgresql (the parent); mounting at the
// legacy /var/lib/postgresql/data makes the image refuse to boot (exit 1) before
// Postgres logs anything, which surfaces as a readiness timeout. Earlier tags
// (e.g. pg17) keep their data under that same parent, so the parent mount is
// correct for every supported image.
func TestPostgres_MountsVolumeAtParent(t *testing.T) {
	def, ok := serviceDefs["postgres"]
	if !ok {
		t.Fatal("postgres not registered")
	}
	bc := ServiceBuildCtx{
		Cfg:    Config{PgUser: "u", PgPassword: "p", PgDB: "d"},
		Volume: "pgvol-x-y",
		Image:  def.Image,
	}
	args := def.DockerArgs(bc)
	if !containsAdjacent(args, "-v", "pgvol-x-y:/var/lib/postgresql") {
		t.Errorf("DockerArgs missing -v %q in %v", "pgvol-x-y:/var/lib/postgresql", args)
	}
	if containsAdjacent(args, "-v", "pgvol-x-y:/var/lib/postgresql/data") {
		t.Errorf("DockerArgs mounts at legacy /var/lib/postgresql/data, which breaks postgres 18+: %v", args)
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

// TestOpReload_PartialFailureShape exercises the documented contract: when
// service N's up phase fails after a successful down, the response carries
// the prior services' success entries, a State="destroyed" entry for the
// failing service, and a typed Failed{Phase:"up"}. The error string follows
// the "<type>: up: <inner>" format.
func TestOpReload_PartialFailureShape(t *testing.T) {
	cfg := Config{
		DefaultServices: []string{"postgres", "fake-gcs"},
		AdvertiseHost:   "localhost",
		PgUser:          "u",
		PgPassword:      "p",
		PgDB:            "d",
		StartupTimeout:  100 * time.Millisecond,
		DockerBin:       "docker",
	}
	s := &Server{cfg: cfg}
	s.docker = newFakeDockerReloadPartial(t)

	resp, err := s.opReload(context.Background(), ReloadRequest{
		Repo: "r", Worktree: "w",
		Services: []string{"postgres", "fake-gcs"},
	})
	if err == nil {
		t.Fatal("expected error when fake-gcs up fails")
	}
	if !strings.HasPrefix(err.Error(), "fake-gcs: up: ") {
		t.Errorf("error format: got %q, want prefix %q", err.Error(), "fake-gcs: up: ")
	}
	if resp == nil {
		t.Fatal("nil response")
	}
	if resp.Failed == nil {
		t.Fatal("Failed must be populated on partial failure")
	}
	if resp.Failed.Type != "fake-gcs" {
		t.Errorf("Failed.Type = %q, want %q", resp.Failed.Type, "fake-gcs")
	}
	if resp.Failed.Phase != reloadPhaseUp {
		t.Errorf("Failed.Phase = %q, want %q", resp.Failed.Phase, reloadPhaseUp)
	}
	if resp.Failed.Err == "" {
		t.Error("Failed.Err must be non-empty")
	}
	if len(resp.Services) != 2 {
		t.Fatalf("Services length = %d, want 2 (postgres up + fake-gcs destroyed), got %+v", len(resp.Services), resp.Services)
	}
	if resp.Services[0].Type != "postgres" || resp.Services[0].State == "destroyed" {
		t.Errorf("first entry should be successful postgres: %+v", resp.Services[0])
	}
	if resp.Services[1].Type != "fake-gcs" || resp.Services[1].State != "destroyed" {
		t.Errorf("second entry should be destroyed fake-gcs: %+v", resp.Services[1])
	}
}

// newFakeDockerReloadPartial returns a fake dockerExec that walks through a
// successful postgres reload and a failing fake-gcs up phase. State is enough
// to satisfy serviceDown (rm -f + volume rm + inspect) and the first
// serviceUp (inspect "no such" -> volume create -> run -> exec pg_isready ->
// port lookup), and to fail the second serviceUp at "docker run".
func newFakeDockerReloadPartial(t *testing.T) dockerExec {
	t.Helper()
	var runCalls int32
	return func(ctx context.Context, args ...string) (string, string, error) {
		if len(args) == 0 {
			return "", "", errors.New("no args")
		}
		switch args[0] {
		case "rm":
			return "", "", nil
		case "volume":
			if len(args) > 1 && args[1] == "rm" {
				return "", "", nil
			}
			if len(args) > 1 && args[1] == "create" {
				return "", "", nil
			}
		case "inspect":
			return "", "Error: No such object: " + args[1], errors.New("exit 1")
		case "run":
			n := atomic.AddInt32(&runCalls, 1)
			if n == 2 {
				return "", "Error response from daemon: container failed to start", errors.New("exit 1")
			}
			return "containerid\n", "", nil
		case "exec":
			return "accepting connections\n", "", nil
		case "port":
			return "0.0.0.0:54321\n", "", nil
		case "logs":
			return "fake logs\n", "", nil
		}
		return "", "fake docker: unhandled args " + strings.Join(args, " "), errors.New("unhandled")
	}
}

// TestMCP_ReloadDispatch verifies tools/call name=pgpool_reload routes into
// opReload. We trigger an unknown-service failure so the test does not need
// a real or fake docker - only routing through the JSON-RPC dispatcher.
func TestMCP_ReloadDispatch(t *testing.T) {
	s := &Server{cfg: Config{DefaultServices: []string{"postgres"}}}

	params, err := json.Marshal(map[string]any{
		"name": "pgpool_reload",
		"arguments": map[string]any{
			"repo": "r", "worktree": "w",
			"services": []string{"definitely-not-a-service"},
		},
	})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	resp := s.dispatchMCP(context.Background(), jsonrpcReq{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/call",
		Params:  params,
	})
	if resp.Error != nil {
		t.Fatalf("unexpected rpc error: %+v", resp.Error)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result not a map: %T", resp.Result)
	}
	isErr, _ := result["isError"].(bool)
	if !isErr {
		t.Fatalf("expected isError=true for unknown service, got %+v", result)
	}
	content, _ := result["content"].([]map[string]any)
	if len(content) == 0 {
		t.Fatalf("expected content block, got %+v", result)
	}
	text, _ := content[0]["text"].(string)
	if !strings.Contains(text, "unknown service") {
		t.Errorf("expected unknown-service text in %q", text)
	}
}
