//go:build integration

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
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
	return newServer(Config{
		AdvertiseHost:   "localhost",
		PgUser:          "postgres",
		PgPassword:      "test-password-do-not-reuse",
		PgDB:            "postgres",
		PgImage:         defaultPostgresImage,
		DockerBin:       "docker",
		StartupTimeout:  90 * time.Second,
		DefaultServices: services,
	})
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
	primary, ok := up.Services[0].Endpoints[RolePrimary]
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
	image, errOut, err := s.runDocker(ctx, "inspect", "--format", "{{.Config.Image}}", up.Services[0].Container)
	if err != nil {
		t.Fatalf("inspect seaweedfs image: %v: %s", err, errOut)
	}
	if got := strings.TrimSpace(image); got != defaultSeaweedfsImage {
		t.Errorf("seaweedfs image = %q, want %q", got, defaultSeaweedfsImage)
	}
	for _, role := range []EndpointRole{RoleMaster, RoleVolume, RoleFiler, RoleS3} {
		ep, ok := up.Services[0].Endpoints[role]
		if !ok || ep.HostPort == "" {
			t.Errorf("missing endpoint %s", role)
		}
	}
	master := up.Services[0].Endpoints[RoleMaster]
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

func TestIntegration_LogsAfterUp(t *testing.T) {
	s := newTestServer(t, []string{"postgres"})
	ctx := context.Background()
	defer s.opDown(ctx, DownRequest{Repo: "itest", Worktree: "logs"})

	if _, err := s.opUp(ctx, UpRequest{Repo: "itest", Worktree: "logs"}); err != nil {
		t.Fatalf("up: %v", err)
	}
	resp, err := s.opLogs(ctx, "itest", "logs", "", defaultLogsTail)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if len(resp.Services) != 1 || resp.Services[0].Type != "postgres" {
		t.Fatalf("unexpected services: %+v", resp.Services)
	}
	if resp.Services[0].State != "running" {
		t.Errorf("state = %q, want running", resp.Services[0].State)
	}
	if resp.Services[0].Logs == "" {
		t.Error("expected non-empty logs from running postgres container")
	}
}

func TestIntegration_LogsMissingContainer(t *testing.T) {
	s := newTestServer(t, []string{"postgres"})
	ctx := context.Background()
	resp, err := s.opLogs(ctx, "itest", "no-such-worktree-zzz", "", defaultLogsTail)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if len(resp.Services) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(resp.Services))
	}
	if resp.Services[0].State != "missing" {
		t.Errorf("state = %q, want missing", resp.Services[0].State)
	}
	if resp.Services[0].Logs != "" {
		t.Errorf("missing container should not have logs: %q", resp.Services[0].Logs)
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
	storage, ok := up.Services[0].Endpoints[RoleStorage]
	if !ok || storage.URL == "" {
		t.Fatalf("missing storage endpoint: %+v", up.Services[0])
	}

	httpC := &http.Client{Timeout: 5 * time.Second}

	// Create a bucket via the GCS-compatible JSON API.
	createBody := `{"name":"itest-bucket"}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, storage.URL+"/storage/v1/b?project=itest", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpC.Do(req)
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("create bucket status=%d", resp.StatusCode)
	}

	// List buckets and confirm ours is present.
	listResp, err := httpC.Get(storage.URL + "/storage/v1/b?project=itest")
	if err != nil {
		t.Fatalf("list buckets: %v", err)
	}
	listBody, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if !strings.Contains(string(listBody), "itest-bucket") {
		t.Fatalf("itest-bucket not in list response: %s", listBody)
	}

	// Upload an object. fake-gcs's object response embeds the -external-url
	// in selfLink and mediaLink, so we can verify the advertise-host:port
	// plumbing without depending on bucket-list selfLink (which 1.49 omits).
	uploadURL := storage.URL + "/upload/storage/v1/b/itest-bucket/o?uploadType=media&name=hello.txt"
	uploadReq, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, strings.NewReader("hi"))
	if err != nil {
		t.Fatal(err)
	}
	uploadReq.Header.Set("Content-Type", "text/plain")
	uploadResp, err := httpC.Do(uploadReq)
	if err != nil {
		t.Fatalf("upload object: %v", err)
	}
	objBody, _ := io.ReadAll(uploadResp.Body)
	uploadResp.Body.Close()
	if uploadResp.StatusCode/100 != 2 {
		t.Fatalf("upload status=%d body=%s", uploadResp.StatusCode, objBody)
	}

	var obj struct {
		SelfLink  string `json:"selfLink"`
		MediaLink string `json:"mediaLink"`
	}
	if err := json.Unmarshal(objBody, &obj); err != nil {
		t.Fatalf("decode object body: %v: %s", err, objBody)
	}
	wantHostPort := ":" + storage.HostPort
	if !strings.Contains(obj.SelfLink, wantHostPort) {
		t.Errorf("selfLink %q does not include host port %s", obj.SelfLink, storage.HostPort)
	}
	if !strings.Contains(obj.MediaLink, wantHostPort) {
		t.Errorf("mediaLink %q does not include host port %s", obj.MediaLink, storage.HostPort)
	}
}

func TestIntegration_ReloadDestroysPostgresData(t *testing.T) {
	s := newTestServer(t, []string{"postgres"})
	ctx := context.Background()
	defer s.opDown(ctx, DownRequest{Repo: "itest", Worktree: "reload-pg"})

	up, err := s.opUp(ctx, UpRequest{Repo: "itest", Worktree: "reload-pg"})
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	cname := up.Services[0].Container

	if _, _, err := s.runDocker(ctx, "exec", cname,
		"psql", "-U", s.cfg.PgUser, "-d", s.cfg.PgDB,
		"-c", "create table sentinel (x int); insert into sentinel values (1);",
	); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}

	rel, err := s.opReload(ctx, ReloadRequest{Repo: "itest", Worktree: "reload-pg"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(rel.Services) != 1 || rel.Services[0].Type != "postgres" {
		t.Fatalf("unexpected reload response: %+v", rel)
	}
	newName := rel.Services[0].Container
	if newName != cname {
		t.Logf("note: container name changed across reload: %s -> %s", cname, newName)
	}

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
	storage, ok := rel.Services[0].Endpoints[RoleStorage]
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

// TestIntegration_ReloadDestroysFakeGCSData asserts the documented reload
// contract for fake-gcs: bucket data is gone after a reload. Mirror of the
// postgres sentinel test - exercises the same volume-destruction behaviour
// against a different service so the contract is not postgres-specific.
func TestIntegration_ReloadDestroysFakeGCSData(t *testing.T) {
	s := newTestServer(t, []string{"fake-gcs"})
	ctx := context.Background()
	defer s.opDown(ctx, DownRequest{Repo: "itest", Worktree: "reload-gcs-data"})

	up, err := s.opUp(ctx, UpRequest{Repo: "itest", Worktree: "reload-gcs-data"})
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	storage, ok := up.Services[0].Endpoints[RoleStorage]
	if !ok || storage.URL == "" {
		t.Fatalf("missing storage endpoint: %+v", up.Services[0])
	}

	httpC := &http.Client{Timeout: 5 * time.Second}

	sentinelBucket := "sentinel-reload-bucket"
	createReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		storage.URL+"/storage/v1/b?project=itest",
		strings.NewReader(`{"name":"`+sentinelBucket+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := httpC.Do(createReq)
	if err != nil {
		t.Fatalf("create sentinel bucket: %v", err)
	}
	createResp.Body.Close()
	if createResp.StatusCode/100 != 2 {
		t.Fatalf("create sentinel bucket status=%d", createResp.StatusCode)
	}

	rel, err := s.opReload(ctx, ReloadRequest{Repo: "itest", Worktree: "reload-gcs-data"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(rel.Services) != 1 || rel.Services[0].State == "destroyed" {
		t.Fatalf("unexpected reload result: %+v", rel)
	}
	newStorage, ok := rel.Services[0].Endpoints[RoleStorage]
	if !ok || newStorage.URL == "" {
		t.Fatalf("missing storage endpoint after reload: %+v", rel.Services[0])
	}

	listResp, err := httpC.Get(newStorage.URL + "/storage/v1/b?project=itest")
	if err != nil {
		t.Fatalf("list buckets post-reload: %v", err)
	}
	listBody, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if strings.Contains(string(listBody), sentinelBucket) {
		t.Fatalf("sentinel bucket %q still present after reload; volume not destroyed. body=%s", sentinelBucket, listBody)
	}
}
