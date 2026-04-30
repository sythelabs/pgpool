package main

import (
	"bytes"
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
	if !bytes.Contains(got, []byte("v:3")) {
		t.Errorf("new v:3 marker missing:\n%s", got)
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
