# AGENTS.md Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `AGENTS.md` the canonical pgpool agent guide and make `pgpoolcli init` safely migrate its managed block from `CLAUDE.md` to `AGENTS.md`.

**Architecture:** Keep the CLI in its existing single-file `package main`. Generalize the current marker merge logic around neutral agent-instruction names, parse both destination and legacy files before any writes, write `AGENTS.md` first, then remove the legacy block from `CLAUDE.md`. Move the repository guide verbatim to `AGENTS.md`, retain a one-line Claude import, and update active README/CLI copy.

**Tech Stack:** Go standard library, Go `testing`, Markdown, Git.

## Global Constraints

- Keep `cmd/pgpoolcli/pgpoolcli.go` as one file and add no dependencies.
- `AGENTS.md` is the only default integration destination; do not add a destination flag or dual-write behavior.
- Do not create `CLAUDE.md` in consumer projects.
- Preserve bytes outside managed marker spans.
- Delete legacy `CLAUDE.md` only when removing its managed block leaves whitespace only.
- Parse every managed marker span in both instruction files before writing either; reject unmatched or nested begin markers, write `AGENTS.md` before legacy cleanup.
- Keep marker text and integration version `v:4` unchanged.
- Do not rewrite historical documents under `docs/superpowers/` merely to update old filename references.
- New files use mode `0644`; existing `os.WriteFile` behavior preserves existing file mode.

## File Structure

- Create: `AGENTS.md` — canonical repository instructions, copied from the current root guide.
- Modify: `CLAUDE.md` — one-line `@AGENTS.md` compatibility import.
- Modify: `cmd/pgpoolcli/pgpoolcli.go` — neutral marker names, two-file validation/migration, messages, help, and prime text.
- Modify: `cmd/pgpoolcli/pgpoolcli_test.go` — TDD coverage for AGENTS creation/update, CLAUDE migration, malformed input, and idempotency.
- Modify: `README.md` — canonical guide link, setup behavior, and AGENTS integration documentation.

---

### Task 1: Make CLI initialization target and migrate to AGENTS.md

**Files:**
- Modify: `cmd/pgpoolcli/pgpoolcli_test.go:95-178,345-360`
- Modify: `cmd/pgpoolcli/pgpoolcli.go:30-45,121-126,735-881,990-1000`

**Interfaces:**
- Consumes: Existing `cmdInit`, the `PGPOOL INTEGRATION` begin/end markers, `os.ReadFile`, `os.WriteFile`, and `os.Remove`.
- Produces: `agentSegment string`, `agentMergeAction`, `agentBlockSpan`, `locateAgentBlocks(existing []byte, path string) ([]agentBlockSpan, error)`, `mergeAgentBlock(existing []byte, path string) ([]byte, agentMergeAction, error)`, and `removeAgentBlock(existing []byte, path string) ([]byte, bool, error)`.

- [ ] **Step 1: Replace the init test helper with a two-file fixture**

Use pointers to distinguish an absent file from a present empty file, and return both post-run file states:

```go
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
```

Add a small test-only helper:

```go
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
```

- [ ] **Step 2: Write failing destination and idempotency tests**

Replace old CLAUDE-only init tests with these explicit AGENTS expectations:

```go
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
```

- [ ] **Step 3: Write failing migration tests**

Add tests that prove convergence to one block and preservation of unrelated Claude instructions:

```go
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
```

- [ ] **Step 4: Write failing complete-span convergence and malformed-marker tests**

Add focused tests that drive a complete marker scan rather than a first-match transform:

1. Seed `AGENTS.md` with a current block followed by an old block. Assert the first span becomes/remains the current block, every additional managed span is removed, exactly one begin marker remains, and all bytes outside the spans are byte-for-byte preserved.
2. Seed `CLAUDE.md` with two managed blocks separated and surrounded by unrelated bytes. Assert every managed span is removed and all unrelated bytes are byte-for-byte preserved.
3. Seed an instruction file with a valid managed block followed by an unmatched begin marker. Assert `cmdInit` returns an error naming that file before changing the config or either instruction file.
4. Seed each instruction file with a begin marker nested inside another managed span. Assert `cmdInit` returns an error naming that file before changing the config or either instruction file.
5. Retain the unmatched-first-begin cases for both files.

Call `cmdInit` directly for validation cases so the error and pre-write file bytes can be asserted. Read both instruction files before the call and compare their bytes afterward in addition to verifying that the config was not created.

- [ ] **Step 5: Run the focused tests and verify RED**

Run:

```bash
go test ./cmd/pgpoolcli -run 'TestCmdInit' -count=1
```

Expected: compile failures for undefined `agentSegment` / `agentBeginPrefix`, followed by behavioral failures until implementation exists. In the final fix wave, the focused regression run must fail because the second AGENTS/CLAUDE spans survive and trailing/nested begin markers are accepted.

- [ ] **Step 6: Rename marker and merge symbols to neutral agent terminology**

In `cmd/pgpoolcli/pgpoolcli.go`, keep marker values unchanged while renaming symbols:

```go
const (
	defaultURL       = "http://localhost:8080"
	defaultConfigRel = ".config/pgpool/pgpool.json"
	agentBeginPrefix = "<!-- BEGIN PGPOOL INTEGRATION"
	agentEndMarker   = "<!-- END PGPOOL INTEGRATION -->"
	httpTimeout      = 60 * time.Second
)

// agentSegment is what `pgpoolcli init` adds to AGENTS.md.
const agentSegment = `<!-- BEGIN PGPOOL INTEGRATION v:4 -->
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

type agentMergeAction int

const (
	agentUnchanged agentMergeAction = iota
	agentReplaced
	agentAppended
	agentCreated
)
```

Implement a shared complete-span locator so both transforms validate the entire file and report the correct path. Each begin marker must have a following end marker before any later begin marker; a later begin before that end is malformed nesting:

```go
type agentBlockSpan struct {
	begin int
	end   int
}

func locateAgentBlocks(existing []byte, path string) ([]agentBlockSpan, error) {
	var spans []agentBlockSpan
	for offset := 0; ; {
		beginRel := bytes.Index(existing[offset:], []byte(agentBeginPrefix))
		if beginRel < 0 {
			return spans, nil
		}
		beginIdx := offset + beginRel
		contentIdx := beginIdx + len(agentBeginPrefix)
		endRel := bytes.Index(existing[contentIdx:], []byte(agentEndMarker))
		if endRel < 0 {
			return nil, fmt.Errorf("%s has %q without matching %q", path, agentBeginPrefix, agentEndMarker)
		}
		endIdx := contentIdx + endRel
		if nestedRel := bytes.Index(existing[contentIdx:endIdx], []byte(agentBeginPrefix)); nestedRel >= 0 {
			return nil, fmt.Errorf("%s has nested %q markers", path, agentBeginPrefix)
		}
		spanEnd := endIdx + len(agentEndMarker)
		spans = append(spans, agentBlockSpan{begin: beginIdx, end: spanEnd})
		offset = spanEnd
	}
}
```

Refactor the transforms to consume every span. The AGENTS transform replaces the first span with the current block and removes later spans; the CLAUDE transform removes all spans. Copy only the ranges between spans so every byte outside managed spans is preserved:

```go
func mergeAgentBlock(existing []byte, path string) ([]byte, agentMergeAction, error) {
	if len(existing) == 0 {
		return append([]byte(agentSegment), '\n'), agentCreated, nil
	}

	spans, err := locateAgentBlocks(existing, path)
	if err != nil {
		return nil, agentUnchanged, err
	}
	if len(spans) == 0 {
		var b bytes.Buffer
		b.Write(existing)
		if !bytes.HasSuffix(existing, []byte("\n")) {
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
		b.WriteString(agentSegment)
		b.WriteByte('\n')
		return b.Bytes(), agentAppended, nil
	}
	if len(spans) == 1 && string(existing[spans[0].begin:spans[0].end]) == agentSegment {
		return existing, agentUnchanged, nil
	}

	var b bytes.Buffer
	b.Write(existing[:spans[0].begin])
	b.WriteString(agentSegment)
	cursor := spans[0].end
	for _, span := range spans[1:] {
		b.Write(existing[cursor:span.begin])
		cursor = span.end
	}
	b.Write(existing[cursor:])
	return b.Bytes(), agentReplaced, nil
}

func removeAgentBlock(existing []byte, path string) ([]byte, bool, error) {
	spans, err := locateAgentBlocks(existing, path)
	if err != nil || len(spans) == 0 {
		return existing, false, err
	}
	var b bytes.Buffer
	cursor := 0
	for _, span := range spans {
		b.Write(existing[cursor:span.begin])
		cursor = span.end
	}
	b.Write(existing[cursor:])
	return b.Bytes(), true, nil
}
```

- [ ] **Step 7: Implement two-file validation and ordered writes in `cmdInit`**

Before the existing config-writing switch, read both optional files, compute both transformations, and return on either parse error:

```go
agentsExisting, err := os.ReadFile("AGENTS.md")
if errors.Is(err, os.ErrNotExist) {
	agentsExisting = nil
} else if err != nil {
	return fmt.Errorf("read AGENTS.md: %w", err)
}
claudeExisting, err := os.ReadFile("CLAUDE.md")
if errors.Is(err, os.ErrNotExist) {
	claudeExisting = nil
} else if err != nil {
	return fmt.Errorf("read CLAUDE.md: %w", err)
}

nextAgents, agentAction, err := mergeAgentBlock(agentsExisting, "AGENTS.md")
if err != nil {
	return err
}
nextClaude, legacyRemoved, err := removeAgentBlock(claudeExisting, "CLAUDE.md")
if err != nil {
	return err
}
```

After the config-writing switch, write/update `AGENTS.md` unless unchanged. Do not return early on `agentUnchanged`. Then clean the legacy file only when `legacyRemoved`:

```go
if agentAction == agentUnchanged {
	fmt.Fprintln(out, "AGENTS.md already contains the current pgpool integration block - not modified")
} else {
	if err := os.WriteFile("AGENTS.md", nextAgents, 0o644); err != nil {
		return fmt.Errorf("write AGENTS.md: %w", err)
	}
	// Print created/appended/replaced message using AGENTS.md.
}

if legacyRemoved {
	if len(bytes.TrimSpace(nextClaude)) == 0 {
		if err := os.Remove("CLAUDE.md"); err != nil {
			return fmt.Errorf("remove empty CLAUDE.md after migrating pgpool integration: %w", err)
		}
		fmt.Fprintln(out, "removed legacy pgpool integration block and empty CLAUDE.md")
	} else {
		if err := os.WriteFile("CLAUDE.md", nextClaude, 0o644); err != nil {
			return fmt.Errorf("write CLAUDE.md after migrating pgpool integration: %w", err)
		}
		fmt.Fprintln(out, "removed legacy pgpool integration block from CLAUDE.md")
	}
}
```

- [ ] **Step 8: Update active CLI copy**

Change the `primeText` init description and `usage()` command summary from `CLAUDE.md` to `AGENTS.md`. Rename all comments and operator messages tied to the destination. Confirm active code has no old identifier or claim:

```bash
rg -n 'claudeSegment|claudeBegin|claudeEnd|claudeMerge|mergeClaude|append.*CLAUDE' cmd/pgpoolcli
```

Expected: no matches except explicit legacy-migration tests/messages that correctly name `CLAUDE.md`.

- [ ] **Step 9: Run focused tests and verify GREEN**

Run:

```bash
gofmt -w cmd/pgpoolcli/pgpoolcli.go cmd/pgpoolcli/pgpoolcli_test.go
go test ./cmd/pgpoolcli -run 'TestCmdInit' -count=1
```

Expected: PASS.

- [ ] **Step 10: Commit the CLI behavior**

```bash
git add cmd/pgpoolcli/pgpoolcli.go cmd/pgpoolcli/pgpoolcli_test.go
git commit -m "feat(cli): migrate integration docs to AGENTS.md"
```

---

### Task 2: Convert repository documentation and verify the release

**Files:**
- Create: `AGENTS.md`
- Modify: `CLAUDE.md`
- Modify: `README.md:7-10,62-72,168-174`

**Interfaces:**
- Consumes: Task 1's `pgpoolcli init` behavior and unchanged `agentSegment` content.
- Produces: Canonical root `AGENTS.md`, Claude compatibility import, and user documentation matching runtime behavior.

- [ ] **Step 1: Move the canonical repository guide**

Use Git to preserve history, then replace the Claude file with its import:

```bash
git mv CLAUDE.md AGENTS.md
printf '@AGENTS.md\n' > CLAUDE.md
```

Verify exact compatibility-file contents:

```bash
test "$(cat CLAUDE.md)" = '@AGENTS.md'
```

Expected: exit status 0.

- [ ] **Step 2: Update README active documentation**

Make these precise copy changes:

- `See CLAUDE.md for the full server spec` becomes `See AGENTS.md for the full server spec`.
- First-time setup says `pgpoolcli init` appends to `AGENTS.md`, for agents that read `AGENTS.md`.
- Rename `## CLAUDE.md integration` to `## AGENTS.md integration`.
- State that `init` creates or updates `AGENTS.md` and migrates/removes a legacy pgpool block from `CLAUDE.md` while preserving unrelated content.
- Keep the literal integration block synchronized with `agentSegment`; do not bump `v:4`.

- [ ] **Step 3: Check active filename references**

Run:

```bash
rg -n 'CLAUDE\.md' --glob '!docs/superpowers/**' .
```

Expected matches are limited to:

- root `CLAUDE.md` containing `@AGENTS.md` only indirectly through filename listing, not content;
- CLI legacy migration code/tests and README migration explanation that intentionally identify the old file.

Run:

```bash
rg -n 'claudeSegment|mergeClaudeBlock|CLAUDE\.md integration|append.*CLAUDE' --glob '!docs/superpowers/**' .
```

Expected: no stale symbols or claims; only intentional migration wording may mention `CLAUDE.md`.

- [ ] **Step 4: Run complete verification**

Run:

```bash
gofmt -w cmd/pgpoolcli/pgpoolcli.go cmd/pgpoolcli/pgpoolcli_test.go
go test ./...
go vet ./...
go build ./cmd/pgpool ./cmd/pgpoolcli
git diff --check
```

Expected: every command exits 0.

- [ ] **Step 5: Review the final diff against the design**

Run:

```bash
git status --short
git diff --stat HEAD
git diff HEAD -- AGENTS.md CLAUDE.md README.md cmd/pgpoolcli/pgpoolcli.go cmd/pgpoolcli/pgpoolcli_test.go
```

Confirm:

- `AGENTS.md` contains the former repository guide.
- `CLAUDE.md` contains only `@AGENTS.md` and a trailing newline.
- CLI tests prove creation, replacement, migration, cleanup, validation-before-write, and idempotency.
- Active help, prime text, and README use `AGENTS.md` as the destination.
- Historical docs are untouched.

- [ ] **Step 6: Commit documentation and plan**

```bash
git add AGENTS.md CLAUDE.md README.md docs/superpowers/plans/2026-08-04-agents-md-migration.md
git commit -m "docs: make AGENTS.md the canonical project guide"
```

- [ ] **Step 7: Verify the committed tree; parent pushes after final review**

Run:

```bash
go test ./...
go vet ./...
git status --short
git log -3 --oneline --decorate
```

Expected: tests and vet pass and the tracked tree is clean. Do not push during Task 2; the parent owns `git push origin main` after completing the required broad final review.
