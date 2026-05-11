package main

import (
	"context"
	"net"
	"strings"
	"testing"
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
