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
