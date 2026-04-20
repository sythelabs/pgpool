// pgpoolcli is a thin CLI over the pgpool HTTP server.
//
// Config is read from ~/.config/pgpool/pgpool.json by default. Override with
// --url, --config, or the PGPOOL_URL / PGPOOL_CONFIG env vars.
//
// Build:  go build -o pgpoolcli ./cmd/pgpoolcli
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultURL        = "http://localhost:8080"
	defaultConfigRel  = ".config/pgpool/pgpool.json"
	claudeBeginMarker = "<!-- BEGIN PGPOOL INTEGRATION v:1 -->"
	claudeEndMarker   = "<!-- END PGPOOL INTEGRATION -->"
	httpTimeout       = 60 * time.Second
)

// cliVersion is set at link time via -ldflags "-X main.cliVersion=..."
var cliVersion = "dev"

// claudeSegment is what `pgpoolcli init` appends to CLAUDE.md.
const claudeSegment = `<!-- BEGIN PGPOOL INTEGRATION v:1 -->
## Postgres Pool (pgpool)
This project uses **pgpoolcli** to manage ephemeral Postgres containers per worktree.
Run ` + "`pgpoolcli prime`" + ` to see full workflow context and commands.
### Quick Reference
` + "```bash" + `
pgpoolcli up                # Create or reuse a Postgres container for this worktree
pgpoolcli status            # Show current state and connection URL
pgpoolcli list              # List all pgpool-managed containers
pgpoolcli down              # Destroy the container and its volume
` + "```" + `
Repo and worktree auto-detect from git. Override with ` + "`--repo`" + ` / ` + "`--worktree`" + `.
### Rules
- Use ` + "`pgpoolcli`" + ` to manage per-worktree databases - do NOT hand-run ` + "`docker`" + ` commands against pgpool containers
- ` + "`pgpoolcli up`" + ` is idempotent - safe to run multiple times, does not wipe data
- ` + "`pgpoolcli down`" + ` destroys the volume - data is NOT recoverable
- The server does not write ` + "`.env`" + ` files - read the URL from ` + "`up`" + `/` + "`status`" + ` and write your own
- One container per (repo, worktree) pair - names are derived, not chosen
<!-- END PGPOOL INTEGRATION -->`

// primeText is what `pgpoolcli prime` prints. Gives an agent the full picture
// in one shot.
const primeText = `pgpoolcli - per-worktree Postgres management

Each (repo, worktree) pair gets one ephemeral Postgres container with its own
volume. The server is stateless; all state lives in Docker. Auto-detection
fills in repo and worktree from git when you do not pass them.

Commands:
  pgpoolcli up       [--repo R] [--worktree W] [--image IMG]
    Create or reuse a container. Idempotent. Returns {container, volume, url,
    host_port, reused}. Use the "url" as your DATABASE_URL.

  pgpoolcli status   [--repo R] [--worktree W]
    Report state (missing | stopped | running) and current url if running.

  pgpoolcli list
    List every pgpool-managed container on the server's host.

  pgpoolcli down     [--repo R] [--worktree W]
    Destroy the container and its volume. NOT REVERSIBLE - data is gone.

  pgpoolcli health
    Liveness check against the server.

  pgpoolcli config
    Print the resolved config (url, config path, detected repo/worktree).

  pgpoolcli init     [--url URL] [--force]
    Write ~/.config/pgpool/pgpool.json and append the pgpool block to
    ./CLAUDE.md if it is not already present.

  pgpoolcli prime
    Print this text.

Global flags (apply to every subcommand):
  --url URL          Server URL (env: PGPOOL_URL). Default from config file.
  --config PATH      Config file path (env: PGPOOL_CONFIG).
  --json             Print raw JSON response instead of a pretty summary.

Auto-detection:
  --repo      basename of the origin remote URL, else basename of the git toplevel
  --worktree  basename of the current working directory

Typical flow inside a worktree:
  1. pgpoolcli up                 (creates container, prints URL)
  2. write URL into your .env     (the server does not do this for you)
  3. work work work
  4. pgpoolcli down               (when the worktree is done)
`

// ---------- config ----------

type Config struct {
	URL string `json:"url"`
}

func defaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	return filepath.Join(home, defaultConfigRel), nil
}

func loadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

func writeConfig(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// resolveURL picks the URL in priority order: explicit flag > env > config > default.
func resolveURL(flagURL, cfgURL string) string {
	if flagURL != "" {
		return strings.TrimRight(flagURL, "/")
	}
	if v := os.Getenv("PGPOOL_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	if cfgURL != "" {
		return strings.TrimRight(cfgURL, "/")
	}
	return defaultURL
}

func resolveConfigPath(flagPath string) (string, error) {
	if flagPath != "" {
		return flagPath, nil
	}
	if v := os.Getenv("PGPOOL_CONFIG"); v != "" {
		return v, nil
	}
	return defaultConfigPath()
}

// ---------- repo / worktree auto-detection ----------

func detectRepo() string {
	if out, err := gitOut("remote", "get-url", "origin"); err == nil {
		return repoFromRemote(out)
	}
	if out, err := gitOut("rev-parse", "--show-toplevel"); err == nil {
		return filepath.Base(out)
	}
	return ""
}

func detectWorktree() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return filepath.Base(wd)
}

func gitOut(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// repoFromRemote extracts the repo name from an origin URL.
//   git@github.com:org/repo.git        -> repo
//   https://github.com/org/repo        -> repo
//   https://github.com/org/repo.git/   -> repo
func repoFromRemote(remote string) string {
	s := strings.TrimSpace(remote)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// ---------- HTTP client ----------

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("server returned %d: %s", e.Status, strings.TrimSpace(e.Body))
}

type client struct {
	baseURL string
	http    *http.Client
}

func newClient(baseURL string) *client {
	return &client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: httpTimeout},
	}
}

func (c *client) do(method, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, c.baseURL+path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return &apiError{Status: resp.StatusCode, Body: string(raw)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response: %w: %s", err, string(raw))
	}
	return nil
}

// ---------- command implementations ----------

type runCtx struct {
	client   *client
	jsonOnly bool
	url      string
	cfgPath  string
}

func cmdUp(rc *runCtx, repo, worktree, image string) error {
	body := map[string]string{"repo": repo, "worktree": worktree}
	if image != "" {
		body["image"] = image
	}
	var resp map[string]any
	if err := rc.client.do(http.MethodPost, "/v1/up", body, &resp); err != nil {
		return err
	}
	if rc.jsonOnly {
		return printJSON(resp)
	}
	fmt.Printf("container: %s\n", stringOr(resp["container"], "-"))
	fmt.Printf("volume:    %s\n", stringOr(resp["volume"], "-"))
	fmt.Printf("host_port: %s\n", stringOr(resp["host_port"], "-"))
	fmt.Printf("reused:    %v\n", resp["reused"])
	fmt.Printf("url:       %s\n", stringOr(resp["url"], "-"))
	return nil
}

func cmdDown(rc *runCtx, repo, worktree string) error {
	body := map[string]string{"repo": repo, "worktree": worktree}
	var resp map[string]any
	if err := rc.client.do(http.MethodPost, "/v1/down", body, &resp); err != nil {
		return err
	}
	if rc.jsonOnly {
		return printJSON(resp)
	}
	fmt.Printf("removed container: %s\n", stringOr(resp["container"], "-"))
	fmt.Printf("removed volume:    %s\n", stringOr(resp["volume"], "-"))
	return nil
}

func cmdStatus(rc *runCtx, repo, worktree string) error {
	q := url.Values{}
	q.Set("repo", repo)
	q.Set("worktree", worktree)
	var resp map[string]any
	if err := rc.client.do(http.MethodGet, "/v1/status?"+q.Encode(), nil, &resp); err != nil {
		return err
	}
	if rc.jsonOnly {
		return printJSON(resp)
	}
	fmt.Printf("repo:      %s\n", stringOr(resp["repo"], "-"))
	fmt.Printf("worktree:  %s\n", stringOr(resp["worktree"], "-"))
	fmt.Printf("container: %s\n", stringOr(resp["container"], "-"))
	fmt.Printf("volume:    %s\n", stringOr(resp["volume"], "-"))
	fmt.Printf("state:     %s\n", stringOr(resp["state"], "-"))
	if s := stringOr(resp["host_port"], ""); s != "" {
		fmt.Printf("host_port: %s\n", s)
	}
	if s := stringOr(resp["url"], ""); s != "" {
		fmt.Printf("url:       %s\n", s)
	}
	if s := stringOr(resp["created_at"], ""); s != "" {
		fmt.Printf("created:   %s\n", s)
	}
	return nil
}

func cmdList(rc *runCtx) error {
	var resp []map[string]any
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
	fmt.Printf("%-40s  %-10s  %-6s  %s\n", "CONTAINER", "STATE", "PORT", "URL")
	for _, row := range resp {
		fmt.Printf("%-40s  %-10s  %-6s  %s\n",
			truncate(stringOr(row["container"], "-"), 40),
			stringOr(row["state"], "-"),
			stringOr(row["host_port"], "-"),
			stringOr(row["url"], "-"),
		)
	}
	return nil
}

func cmdHealth(rc *runCtx) error {
	var resp map[string]any
	if err := rc.client.do(http.MethodGet, "/healthz", nil, &resp); err != nil {
		return err
	}
	if rc.jsonOnly {
		return printJSON(resp)
	}
	fmt.Printf("ok - %s %s\n", stringOr(resp["name"], "pgpool"), stringOr(resp["version"], "?"))
	return nil
}

func cmdConfig(rc *runCtx) error {
	out := map[string]any{
		"url":         rc.url,
		"config_path": rc.cfgPath,
		"detected": map[string]string{
			"repo":     detectRepo(),
			"worktree": detectWorktree(),
		},
	}
	return printJSON(out)
}

func cmdInit(rc *runCtx, flagURL string, force, yes bool, in io.Reader, out io.Writer) error {
	cfg, err := loadConfig(rc.cfgPath)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(rc.cfgPath)
	configExists := statErr == nil
	interactive := !yes && flagURL == ""
	br := bufio.NewReader(in)

	// Default value shown in the prompt: existing config URL > default.
	promptDefault := cfg.URL
	if promptDefault == "" {
		promptDefault = defaultURL
	}

	chosenURL := flagURL
	if chosenURL == "" {
		if interactive {
			fmt.Fprintf(out, "pgpool server URL [%s]: ", promptDefault)
			line, err := readLine(br)
			if err != nil {
				return fmt.Errorf("read url: %w", err)
			}
			if line == "" {
				chosenURL = promptDefault
			} else {
				chosenURL = line
			}
		} else {
			chosenURL = promptDefault
		}
	}
	chosenURL = strings.TrimRight(strings.TrimSpace(chosenURL), "/")
	if chosenURL == "" {
		return errors.New("url cannot be empty")
	}

	// writeConfig creates the parent directory if it is missing.
	switch {
	case !configExists:
		if err := writeConfig(rc.cfgPath, Config{URL: chosenURL}); err != nil {
			return err
		}
		fmt.Fprintf(out, "wrote config to %s (url=%s)\n", rc.cfgPath, chosenURL)
	case cfg.URL == chosenURL:
		fmt.Fprintf(out, "config at %s already has url=%s - not modified\n", rc.cfgPath, chosenURL)
	case force || flagURL != "" || (interactive && confirm(br, out, fmt.Sprintf("config exists at %s with url=%s. Overwrite with %s?", rc.cfgPath, cfg.URL, chosenURL))):
		if err := writeConfig(rc.cfgPath, Config{URL: chosenURL}); err != nil {
			return err
		}
		fmt.Fprintf(out, "updated config at %s (url=%s)\n", rc.cfgPath, chosenURL)
	default:
		fmt.Fprintf(out, "config already exists at %s (use --force to overwrite)\n", rc.cfgPath)
	}

	// CLAUDE.md in current dir.
	claudePath := "CLAUDE.md"
	existed := true
	existing, err := os.ReadFile(claudePath)
	if errors.Is(err, os.ErrNotExist) {
		existed = false
		existing = nil
	} else if err != nil {
		return fmt.Errorf("read CLAUDE.md: %w", err)
	}

	if bytes.Contains(existing, []byte(claudeBeginMarker)) {
		fmt.Fprintf(out, "CLAUDE.md already contains pgpool integration block - not modified\n")
		return nil
	}

	var next bytes.Buffer
	next.Write(existing)
	if existed && len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		next.WriteByte('\n')
	}
	if existed && len(existing) > 0 {
		next.WriteByte('\n')
	}
	next.WriteString(claudeSegment)
	next.WriteByte('\n')

	if err := os.WriteFile(claudePath, next.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write CLAUDE.md: %w", err)
	}
	if existed {
		fmt.Fprintf(out, "appended pgpool integration block to %s\n", claudePath)
	} else {
		fmt.Fprintf(out, "created %s with pgpool integration block\n", claudePath)
	}
	return nil
}

// ---------- interactive helpers ----------

func readLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func confirm(br *bufio.Reader, out io.Writer, question string) bool {
	fmt.Fprintf(out, "%s [y/N]: ", question)
	line, err := readLine(br)
	if err != nil {
		return false
	}
	switch strings.ToLower(line) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// ---------- helpers ----------

func stringOr(v any, fallback string) string {
	s, ok := v.(string)
	if !ok || s == "" {
		return fallback
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func requireDetected(name, value string) (string, error) {
	if value != "" {
		return value, nil
	}
	return "", fmt.Errorf("could not auto-detect --%s; pass it explicitly", name)
}

// ---------- flag plumbing ----------

type globalFlags struct {
	url      string
	config   string
	jsonOnly bool
}

func addGlobalFlags(fs *flag.FlagSet, g *globalFlags) {
	fs.StringVar(&g.url, "url", "", "pgpool server URL (overrides env and config)")
	fs.StringVar(&g.config, "config", "", "path to pgpool config file")
	fs.BoolVar(&g.jsonOnly, "json", false, "print raw JSON instead of a summary")
}

func newRunCtx(g globalFlags) (*runCtx, error) {
	cfgPath, err := resolveConfigPath(g.config)
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return nil, err
	}
	u := resolveURL(g.url, cfg.URL)
	return &runCtx{
		client:   newClient(u),
		jsonOnly: g.jsonOnly,
		url:      u,
		cfgPath:  cfgPath,
	}, nil
}

func usage() {
	fmt.Fprint(os.Stderr, `pgpoolcli - manage ephemeral Postgres containers via a pgpool server

Usage:
  pgpoolcli <command> [flags]

Commands:
  up       Create or reuse a Postgres container for this worktree
  down     Destroy the container and its volume
  status   Show state and connection URL for this worktree
  list     List all pgpool-managed containers on the server
  health   Check that the server is reachable
  config   Print the resolved config
  init     Write a config file and append a block to CLAUDE.md
  prime    Print the full workflow reference

Global flags (all commands):
  --url URL         Server URL (env: PGPOOL_URL)
  --config PATH     Config file path (env: PGPOOL_CONFIG)
  --json            Print raw JSON instead of a human summary

Config file: ~/.config/pgpool/pgpool.json
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "-h", "--help", "help":
		usage()
		return
	case "prime":
		fmt.Print(primeText)
		return
	case "-v", "--version", "version":
		fmt.Printf("pgpoolcli %s\n", cliVersion)
		return
	}

	switch cmd {
	case "up":
		runUp(args)
	case "down":
		runDown(args)
	case "status":
		runStatus(args)
	case "list":
		runList(args)
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
}

func runUp(args []string) {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	var g globalFlags
	addGlobalFlags(fs, &g)
	repo := fs.String("repo", "", "repository name (defaults to git-detected)")
	worktree := fs.String("worktree", "", "worktree name (defaults to $PWD basename)")
	image := fs.String("image", "", "override postgres image for this run")
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
	fail(cmdUp(rc, r, w, *image))
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
	fail(cmdDown(rc, r, w))
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
	fail(cmdStatus(rc, r, w))
}

func runList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	var g globalFlags
	addGlobalFlags(fs, &g)
	must(fs.Parse(args))
	rc, err := newRunCtx(g)
	fail(err)
	fail(cmdList(rc))
}

func runHealth(args []string) {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	var g globalFlags
	addGlobalFlags(fs, &g)
	must(fs.Parse(args))
	rc, err := newRunCtx(g)
	fail(err)
	fail(cmdHealth(rc))
}

func runConfig(args []string) {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	var g globalFlags
	addGlobalFlags(fs, &g)
	must(fs.Parse(args))
	rc, err := newRunCtx(g)
	fail(err)
	fail(cmdConfig(rc))
}

func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	var g globalFlags
	addGlobalFlags(fs, &g)
	force := fs.Bool("force", false, "overwrite an existing config file without prompting")
	yes := fs.Bool("yes", false, "skip interactive prompts; accept defaults")
	must(fs.Parse(args))
	rc, err := newRunCtx(g)
	fail(err)
	fail(cmdInit(rc, g.url, *force, *yes, os.Stdin, os.Stdout))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func fail(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
