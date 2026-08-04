package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRewriteEndpointHost_SwapsUnresolvableAdvertisedHost is the core of the
// reachability fix: when the server advertises a host the client cannot
// resolve, the CLI substitutes the control host (which it just reached) while
// preserving the data-plane port, scheme, userinfo, and path.
func TestRewriteEndpointHost_SwapsUnresolvableAdvertisedHost(t *testing.T) {
	resolves := func(h string) bool { return h == "192.168.1.125" }
	out, adv, ctrl, changed := rewriteEndpointHost(
		"postgresql://u:p@pgpool.tail22511b.ts.net:54321/d",
		"http://192.168.1.125:8080",
		resolves,
	)
	if !changed {
		t.Fatal("expected URL to be rewritten")
	}
	want := "postgresql://u:p@192.168.1.125:54321/d"
	if out != want {
		t.Errorf("rewritten URL = %q, want %q", out, want)
	}
	if adv != "pgpool.tail22511b.ts.net" || ctrl != "192.168.1.125" {
		t.Errorf("adv/ctrl hosts = %q/%q, want pgpool.tail22511b.ts.net/192.168.1.125", adv, ctrl)
	}
}

func TestRewriteEndpointHost_KeepsResolvableAdvertisedHost(t *testing.T) {
	in := "http://pgpool.tail22511b.ts.net:18333"
	out, _, _, changed := rewriteEndpointHost(in, "http://192.168.1.125:8080", func(string) bool { return true })
	if changed || out != in {
		t.Errorf("resolvable host should be left alone: out=%q changed=%v", out, changed)
	}
}

func TestRewriteEndpointHost_NoopWhenHostsMatch(t *testing.T) {
	in := "http://192.168.1.125:18333"
	out, _, _, changed := rewriteEndpointHost(in, "http://192.168.1.125:8080", func(string) bool { return false })
	if changed || out != in {
		t.Errorf("no rewrite expected when ep host == control host: out=%q changed=%v", out, changed)
	}
}

// TestCmdStatus_RewritesUnreachableAdvertisedHost exercises the whole path:
// the server returns an endpoint URL whose advertised host does not resolve
// here, and the printed output must use the control host instead.
func TestCmdStatus_RewritesUnreachableAdvertisedHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"repo":"r","worktree":"w","services":[{"type":"postgres","container":"pg-r-w","volume":"pgvol-r-w","endpoints":{"primary":{"url":"postgresql://u:p@pgpool.tail22511b.ts.net:54321/d","host_port":"54321","container_port":5432}}}]}`)
	}))
	t.Cleanup(srv.Close)

	controlHost := mustHostname(t, srv.URL)
	rc := &runCtx{
		client:  newClient(srv.URL),
		url:     srv.URL,
		resolve: func(h string) bool { return h != "pgpool.tail22511b.ts.net" },
	}
	_, restore := captureStdout(t)
	if err := cmdStatus(rc, "r", "w", ""); err != nil {
		t.Fatalf("cmdStatus: %v", err)
	}
	out := restore()

	if strings.Contains(out, "pgpool.tail22511b.ts.net") {
		t.Errorf("unresolvable advertised host still printed:\n%s", out)
	}
	if !strings.Contains(out, controlHost+":54321/d") {
		t.Errorf("expected control host %q with preserved port 54321 in output:\n%s", controlHost, out)
	}
}

func mustHostname(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Hostname()
}

type initFiles struct {
	agents *string
	claude *string
}

type initResult struct {
	agents       []byte
	agentsExists bool
	claude       []byte
	claudeExists bool
	output       string
	configExists bool
}

func stringPtr(s string) *string { return &s }

func writeInitFiles(t *testing.T, files initFiles) {
	t.Helper()
	if files.agents != nil {
		if err := os.WriteFile("AGENTS.md", []byte(*files.agents), 0o644); err != nil {
			t.Fatalf("seed AGENTS.md: %v", err)
		}
	}
	if files.claude != nil {
		if err := os.WriteFile("CLAUDE.md", []byte(*files.claude), 0o644); err != nil {
			t.Fatalf("seed CLAUDE.md: %v", err)
		}
	}
}

func initTestEnv(t *testing.T, files initFiles) initResult {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	writeInitFiles(t, files)

	cfgPath := filepath.Join(dir, "pgpool.json")
	rc := &runCtx{client: newClient("http://example"), url: "http://example", cfgPath: cfgPath}
	var out bytes.Buffer
	if err := cmdInit(rc, "http://example", false, true, strings.NewReader(""), &out); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}

	agents, agentsErr := os.ReadFile("AGENTS.md")
	claude, claudeErr := os.ReadFile("CLAUDE.md")
	return initResult{
		agents:       agents,
		agentsExists: agentsErr == nil,
		claude:       claude,
		claudeExists: claudeErr == nil,
		output:       out.String(),
		configExists: fileExists(cfgPath),
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func captureStderr(t *testing.T) func() string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	return func() string {
		_ = w.Close()
		os.Stderr = old
		buf, _ := io.ReadAll(r)
		return string(buf)
	}
}

func TestCmdInit_CreatesAgentsFileWhenAbsent(t *testing.T) {
	got := initTestEnv(t, initFiles{})
	if !got.agentsExists || !bytes.Contains(got.agents, []byte(agentSegment)) {
		t.Fatalf("AGENTS.md missing current integration block:\n%s", got.agents)
	}
	if got.claudeExists {
		t.Fatal("CLAUDE.md should not be created")
	}
	if !strings.Contains(got.output, "AGENTS.md") {
		t.Fatalf("operator output does not identify AGENTS.md: %q", got.output)
	}
}

func TestCmdInit_AppendsToAgentsWithoutClobberingContent(t *testing.T) {
	seed := "# Project\n\nkeep me\n"
	got := initTestEnv(t, initFiles{agents: stringPtr(seed)})
	if !bytes.HasPrefix(got.agents, []byte(seed)) || bytes.Count(got.agents, []byte(agentBeginPrefix)) != 1 {
		t.Fatalf("AGENTS.md was not appended safely:\n%s", got.agents)
	}
}

func TestCmdInit_ReplacesOlderAgentsBlock(t *testing.T) {
	old := "# Project\n\n<!-- BEGIN PGPOOL INTEGRATION v:1 -->\nold\n<!-- END PGPOOL INTEGRATION -->\n\nkeep\n"
	got := initTestEnv(t, initFiles{agents: stringPtr(old)})
	if bytes.Contains(got.agents, []byte("v:1")) || !bytes.Contains(got.agents, []byte("v:4")) {
		t.Fatalf("old AGENTS.md block was not replaced:\n%s", got.agents)
	}
	if !bytes.Contains(got.agents, []byte("keep")) {
		t.Fatalf("unrelated AGENTS.md content was lost:\n%s", got.agents)
	}
}

func TestCmdInit_LeavesCurrentAgentsBlockUntouched(t *testing.T) {
	seed := "# Project\n\n" + agentSegment + "\n"
	got := initTestEnv(t, initFiles{agents: stringPtr(seed)})
	if !bytes.Equal(got.agents, []byte(seed)) {
		t.Fatalf("current AGENTS.md changed:\n%s", got.agents)
	}
	if !strings.Contains(got.output, "already") {
		t.Fatalf("no-op output missing 'already': %q", got.output)
	}
}

func TestCmdInit_ConvergesMultipleAgentsBlocksPreservingOtherBytes(t *testing.T) {
	oldBlock := "<!-- BEGIN PGPOOL INTEGRATION v:1 -->\nold\n<!-- END PGPOOL INTEGRATION -->"
	seed := "before\n" + agentSegment + "\nbetween\n" + oldBlock + "\nafter\n"
	want := "before\n" + agentSegment + "\nbetween\n\nafter\n"

	got := initTestEnv(t, initFiles{agents: stringPtr(seed)})
	if !bytes.Equal(got.agents, []byte(want)) {
		t.Fatalf("AGENTS.md did not converge while preserving non-marker bytes:\ngot:  %q\nwant: %q", got.agents, want)
	}
	if bytes.Count(got.agents, []byte(agentBeginPrefix)) != 1 {
		t.Fatalf("AGENTS.md managed block count = %d, want 1", bytes.Count(got.agents, []byte(agentBeginPrefix)))
	}
}

func TestCmdInit_MigratesLegacyClaudeBlock(t *testing.T) {
	legacy := "# Claude only\n\n<!-- BEGIN PGPOOL INTEGRATION v:3 -->\nold\n<!-- END PGPOOL INTEGRATION -->\n\nkeep this\n"
	got := initTestEnv(t, initFiles{claude: stringPtr(legacy)})
	if !bytes.Contains(got.agents, []byte(agentSegment)) {
		t.Fatalf("AGENTS.md missing migrated block:\n%s", got.agents)
	}
	if !got.claudeExists || bytes.Contains(got.claude, []byte(agentBeginPrefix)) || !bytes.Contains(got.claude, []byte("keep this")) {
		t.Fatalf("CLAUDE.md cleanup was unsafe:\n%s", got.claude)
	}
}

func TestCmdInit_RemovesDuplicateClaudeBlock(t *testing.T) {
	agents := "# Agents\n\n" + agentSegment + "\n"
	claude := "# Claude\n\n" + agentSegment + "\n"
	got := initTestEnv(t, initFiles{agents: stringPtr(agents), claude: stringPtr(claude)})
	if bytes.Count(got.agents, []byte(agentBeginPrefix)) != 1 || bytes.Contains(got.claude, []byte(agentBeginPrefix)) {
		t.Fatalf("managed block did not converge: AGENTS=%q CLAUDE=%q", got.agents, got.claude)
	}
}

func TestCmdInit_RemovesAllClaudeBlocksPreservingOtherBytes(t *testing.T) {
	oldBlock := "<!-- BEGIN PGPOOL INTEGRATION v:2 -->\nold\n<!-- END PGPOOL INTEGRATION -->"
	claude := "before\n" + agentSegment + "\nbetween\n" + oldBlock + "\nafter\n"
	wantClaude := "before\n\nbetween\n\nafter\n"

	got := initTestEnv(t, initFiles{claude: stringPtr(claude)})
	if !got.claudeExists || !bytes.Equal(got.claude, []byte(wantClaude)) {
		t.Fatalf("CLAUDE.md cleanup did not preserve all non-marker bytes:\ngot:  %q\nwant: %q", got.claude, wantClaude)
	}
	if bytes.Contains(got.claude, []byte(agentBeginPrefix)) {
		t.Fatalf("CLAUDE.md still contains a managed block: %q", got.claude)
	}
}

func TestCmdInit_RemovesClaudeFileWhenOnlyLegacyBlockRemains(t *testing.T) {
	claude := "\n" + agentSegment + "\n\n"
	got := initTestEnv(t, initFiles{claude: stringPtr(claude)})
	if got.claudeExists {
		t.Fatalf("empty legacy CLAUDE.md should be removed: %q", got.claude)
	}
	if !got.agentsExists {
		t.Fatal("AGENTS.md was not created before legacy cleanup")
	}
}

func TestCmdInit_IsIdempotentAfterLegacyMigration(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeInitFiles(t, initFiles{claude: stringPtr(agentSegment + "\n")})
	cfgPath := filepath.Join(dir, "pgpool.json")
	rc := &runCtx{client: newClient("http://example"), url: "http://example", cfgPath: cfgPath}
	run := func() {
		if err := cmdInit(rc, "http://example", false, true, strings.NewReader(""), io.Discard); err != nil {
			t.Fatalf("cmdInit: %v", err)
		}
	}
	run()
	firstAgents, err := os.ReadFile("AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	firstConfig, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	run()
	secondAgents, _ := os.ReadFile("AGENTS.md")
	secondConfig, _ := os.ReadFile(cfgPath)
	if !bytes.Equal(firstAgents, secondAgents) || !bytes.Equal(firstConfig, secondConfig) || fileExists("CLAUDE.md") {
		t.Fatal("second init changed the converged files")
	}
}

func TestAgentDocumentationReferencesUseAgentsMD(t *testing.T) {
	if !strings.Contains(primeText, "./AGENTS.md") || strings.Contains(primeText, "./CLAUDE.md") {
		t.Fatalf("prime text has stale integration destination:\n%s", primeText)
	}
	restore := captureStderr(t)
	usage()
	got := restore()
	if !strings.Contains(got, "AGENTS.md") || strings.Contains(got, "append a block to CLAUDE.md") {
		t.Fatalf("usage has stale integration destination:\n%s", got)
	}
}

func TestCmdInit_ValidatesBothInstructionFilesBeforeWriting(t *testing.T) {
	validBlock := "<!-- BEGIN PGPOOL INTEGRATION v:1 -->\nold\n<!-- END PGPOOL INTEGRATION -->"
	for _, tc := range []struct {
		name  string
		files initFiles
		path  string
	}{
		{name: "agents", files: initFiles{agents: stringPtr(agentBeginPrefix + " v:1 -->\nmissing end")}, path: "AGENTS.md"},
		{name: "claude", files: initFiles{claude: stringPtr(agentBeginPrefix + " v:1 -->\nmissing end")}, path: "CLAUDE.md"},
		{name: "agents trailing unmatched begin", files: initFiles{agents: stringPtr(validBlock + "\nkeep\n" + agentBeginPrefix + " v:2 -->\nmissing end")}, path: "AGENTS.md"},
		{name: "agents nested begin", files: initFiles{agents: stringPtr(agentBeginPrefix + " v:1 -->\n" + agentBeginPrefix + " v:2 -->\nold\n" + agentEndMarker)}, path: "AGENTS.md"},
		{name: "claude nested begin", files: initFiles{claude: stringPtr(agentBeginPrefix + " v:1 -->\n" + agentBeginPrefix + " v:2 -->\nold\n" + agentEndMarker)}, path: "CLAUDE.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			writeInitFiles(t, tc.files)
			beforeAgents, _ := os.ReadFile("AGENTS.md")
			beforeClaude, _ := os.ReadFile("CLAUDE.md")
			cfgPath := filepath.Join(dir, "pgpool.json")
			rc := &runCtx{client: newClient("http://example"), url: "http://example", cfgPath: cfgPath}
			err := cmdInit(rc, "http://example", false, true, strings.NewReader(""), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("error = %v, want path %s", err, tc.path)
			}
			afterAgents, _ := os.ReadFile("AGENTS.md")
			afterClaude, _ := os.ReadFile("CLAUDE.md")
			if fileExists(cfgPath) || fileExists("AGENTS.md") != (tc.files.agents != nil) || fileExists("CLAUDE.md") != (tc.files.claude != nil) || !bytes.Equal(afterAgents, beforeAgents) || !bytes.Equal(afterClaude, beforeClaude) {
				t.Fatal("validation failure changed files")
			}
		})
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
	if err := cmdReload(rc, "r", "w", "", []string{"postgres"}); err != nil {
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
	err := cmdReload(rc, "r", "w", "", []string{"nope"})
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should mention 400: %v", err)
	}
}

// TestCmdUp_SendsImageInBody asserts that a non-empty image is forwarded to the
// server as the request body's "image" field, alongside repo/worktree/services.
// This is the per-worktree image-pinning path (e.g. pgvector/pgvector:pg18).
func TestCmdUp_SendsImageInBody(t *testing.T) {
	var capturedPath string
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"services":[{"type":"postgres","container":"pg-r-w","volume":"pgvol-r-w","reused":false}]}`)
	}))
	t.Cleanup(srv.Close)

	rc := &runCtx{client: newClient(srv.URL), url: srv.URL}
	_, restore := captureStdout(t)
	err := cmdUp(rc, "r", "w", "pgvector/pgvector:pg18", []string{"postgres"})
	restore()
	if err != nil {
		t.Fatalf("cmdUp: %v", err)
	}

	if capturedPath != "/v1/up" {
		t.Errorf("path = %q, want /v1/up", capturedPath)
	}
	if capturedBody["image"] != "pgvector/pgvector:pg18" {
		t.Errorf("body.image = %v, want pgvector/pgvector:pg18", capturedBody["image"])
	}
	if capturedBody["repo"] != "r" || capturedBody["worktree"] != "w" {
		t.Errorf("body missing repo/worktree: %+v", capturedBody)
	}
}

// TestCmdUp_OmitsImageWhenEmpty asserts that an empty image leaves the "image"
// field out of the body entirely, so the server falls back to its default.
func TestCmdUp_OmitsImageWhenEmpty(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"services":[]}`)
	}))
	t.Cleanup(srv.Close)

	rc := &runCtx{client: newClient(srv.URL), url: srv.URL}
	_, restore := captureStdout(t)
	err := cmdUp(rc, "r", "w", "", nil)
	restore()
	if err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	if _, present := capturedBody["image"]; present {
		t.Errorf("body should omit image when empty, got %+v", capturedBody)
	}
}

// TestCmdReload_SendsImageInBody mirrors the up path: reload recreates the
// postgres container, so a pinned image must survive a reload too.
func TestCmdReload_SendsImageInBody(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"services":[{"type":"postgres","container":"pg-r-w","volume":"pgvol-r-w","reused":false}]}`)
	}))
	t.Cleanup(srv.Close)

	rc := &runCtx{client: newClient(srv.URL), url: srv.URL}
	_, restore := captureStdout(t)
	err := cmdReload(rc, "r", "w", "pgvector/pgvector:pg18", []string{"postgres"})
	restore()
	if err != nil {
		t.Fatalf("cmdReload: %v", err)
	}
	if capturedBody["image"] != "pgvector/pgvector:pg18" {
		t.Errorf("body.image = %v, want pgvector/pgvector:pg18", capturedBody["image"])
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
