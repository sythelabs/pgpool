package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initTestEnv writes seedClaude (when non-empty) into a fresh tempdir, runs
// cmdInit non-interactively against the server URL "http://example", and
// returns the resulting CLAUDE.md bytes plus the operator output.
func initTestEnv(t *testing.T, seedClaude string) ([]byte, string) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)

	cfgPath := filepath.Join(dir, "pgpool.json")
	if seedClaude != "" {
		if err := os.WriteFile("CLAUDE.md", []byte(seedClaude), 0o644); err != nil {
			t.Fatalf("seed CLAUDE.md: %v", err)
		}
	}
	rc := &runCtx{
		client:  newClient("http://example"),
		url:     "http://example",
		cfgPath: cfgPath,
	}
	var out bytes.Buffer
	if err := cmdInit(rc, "http://example", false, true, strings.NewReader(""), &out); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	got, err := os.ReadFile("CLAUDE.md")
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	return got, out.String()
}

func TestCmdInit_ReplacesOlderIntegrationBlock(t *testing.T) {
	const oldBlock = `<!-- BEGIN PGPOOL INTEGRATION v:1 -->
old pgpool docs that should disappear
<!-- END PGPOOL INTEGRATION -->`
	seed := "# Project\n\nintro\n\n" + oldBlock + "\n\n## Other\n\ntail content\n"

	got, _ := initTestEnv(t, seed)

	if bytes.Count(got, []byte("<!-- BEGIN PGPOOL INTEGRATION")) != 1 {
		t.Fatalf("expected exactly one PGPOOL block, got:\n%s", got)
	}
	if bytes.Contains(got, []byte("v:1")) {
		t.Errorf("old v:1 marker still present:\n%s", got)
	}
	if !bytes.Contains(got, []byte("v:4")) {
		t.Errorf("new v:4 marker missing:\n%s", got)
	}
	if bytes.Contains(got, []byte("old pgpool docs that should disappear")) {
		t.Errorf("old block body still present:\n%s", got)
	}
	if !bytes.Contains(got, []byte("## Other")) || !bytes.Contains(got, []byte("tail content")) {
		t.Errorf("non-pgpool content was clobbered:\n%s", got)
	}
}

func TestCmdInit_LeavesCurrentBlockUntouched(t *testing.T) {
	seed := "# Project\n\n" + claudeSegment + "\n"
	got, out := initTestEnv(t, seed)

	if !bytes.Equal(got, []byte(seed)) {
		t.Errorf("file modified when block already current:\nwant:\n%s\ngot:\n%s", seed, got)
	}
	if !strings.Contains(out, "already") {
		t.Errorf("expected 'already' in operator message, got %q", out)
	}
}

func TestCmdInit_AppendsWhenNoBlockPresent(t *testing.T) {
	seed := "# Project\n\nintro\n"
	got, _ := initTestEnv(t, seed)

	if !bytes.HasPrefix(got, []byte(seed)) {
		t.Errorf("preexisting content not preserved:\n%s", got)
	}
	if bytes.Count(got, []byte("<!-- BEGIN PGPOOL INTEGRATION")) != 1 {
		t.Errorf("expected one block, got:\n%s", got)
	}
}

func TestCmdInit_CreatesFileWhenAbsent(t *testing.T) {
	got, _ := initTestEnv(t, "")
	if !bytes.Contains(got, []byte(claudeSegment)) {
		t.Errorf("expected claudeSegment in fresh file:\n%s", got)
	}
}

// TestCmdReload_HTTPRoundTrip wires cmdReload against an httptest server and
// asserts: (1) the CLI POSTs to /v1/reload, (2) repo/worktree/services land
// in the body, (3) the printed output names every service in the response.
func TestCmdReload_HTTPRoundTrip(t *testing.T) {
	var capturedPath, capturedMethod string
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"services":[{"type":"postgres","container":"pg-r-w","volume":"pgvol-r-w","reused":false,"endpoints":{"primary":{"url":"postgresql://u:p@localhost:54321/d","host_port":"54321","container_port":5432}}}]}`)
	}))
	t.Cleanup(srv.Close)

	rc := &runCtx{client: newClient(srv.URL), url: srv.URL}
	stdout, restore := captureStdout(t)
	if err := cmdReload(rc, "r", "w", []string{"postgres"}); err != nil {
		t.Fatalf("cmdReload: %v", err)
	}
	out := restore()
	_ = stdout

	if capturedMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", capturedMethod)
	}
	if capturedPath != "/v1/reload" {
		t.Errorf("path = %q, want /v1/reload", capturedPath)
	}
	if capturedBody["repo"] != "r" || capturedBody["worktree"] != "w" {
		t.Errorf("body missing repo/worktree: %+v", capturedBody)
	}
	services, _ := capturedBody["services"].([]any)
	if len(services) != 1 || services[0] != "postgres" {
		t.Errorf("body.services = %v, want [postgres]", services)
	}
	if !strings.Contains(out, "postgres") || !strings.Contains(out, "pg-r-w") {
		t.Errorf("output missing service block: %q", out)
	}
}

// TestCmdReload_SurfacesServerError asserts the CLI returns a non-nil error
// when the server responds with 4xx/5xx (e.g. unknown service -> 400). The
// CLI does not need to parse the Failed shape - exit-non-zero is enough.
func TestCmdReload_SurfacesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"services":[],"failed":{"type":"","phase":"","error":"unknown service: \"nope\""}}`)
	}))
	t.Cleanup(srv.Close)

	rc := &runCtx{client: newClient(srv.URL), url: srv.URL}
	err := cmdReload(rc, "r", "w", []string{"nope"})
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should mention 400: %v", err)
	}
}

// captureStdout redirects os.Stdout into an in-memory pipe for the duration
// of the returned closure. cmdReload writes through fmt.Print* which targets
// os.Stdout, so swapping at the os level is the only way to capture without
// threading a writer through the CLI plumbing.
func captureStdout(t *testing.T) (*os.File, func() string) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	return w, func() string {
		w.Close()
		os.Stdout = old
		buf, _ := io.ReadAll(r)
		return string(buf)
	}
}

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
