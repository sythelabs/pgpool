package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML []byte

const (
	mcpProtocolVersion = "2025-06-18"
	serverName         = "pgpool"

	labelPgpool   = "pgpool"
	labelRepo     = "pgpool.repo"
	labelWorktree = "pgpool.worktree"
	labelService  = "pgpool.service"

	dockerNameMax = 63
)

// ---------- service registry ----------

type EndpointSpec struct {
	Role          string // "primary" | "master" | "filer" | "s3" | ...
	ContainerPort int
	Scheme        string // "postgresql" | "http" | ...
}

type ServiceDef struct {
	Type            string
	ContainerPrefix string
	VolumePrefix    string
	Image           string
	DockerArgs      func(cfg Config, volume string) []string
	Endpoints       []EndpointSpec
	Readiness       func(ctx context.Context, s *Server, container string, hostPorts map[string]string) error
	BuildURL        func(cfg Config, role string, hostPort string) string
}

var serviceDefs = map[string]ServiceDef{}

var postgresDef = ServiceDef{
	Type:            "postgres",
	ContainerPrefix: "pg",
	VolumePrefix:    "pgvol",
	Image:           "postgres:17",
	DockerArgs: func(cfg Config, volume string) []string {
		return []string{
			"-v", volume + ":/var/lib/postgresql/data",
			"-e", "POSTGRES_PASSWORD=" + cfg.PgPassword,
			"-e", "POSTGRES_USER=" + cfg.PgUser,
			"-e", "POSTGRES_DB=" + cfg.PgDB,
		}
	},
	Endpoints: []EndpointSpec{
		{Role: "primary", ContainerPort: 5432, Scheme: "postgresql"},
	},
	Readiness: func(ctx context.Context, s *Server, container string, _ map[string]string) error {
		return s.pgIsReady(ctx, container)
	},
	BuildURL: func(cfg Config, role, hostPort string) string {
		pw := url.QueryEscape(cfg.PgPassword)
		return fmt.Sprintf("postgresql://%s:%s@%s:%s/%s",
			cfg.PgUser, pw, cfg.AdvertiseHost, hostPort, cfg.PgDB)
	},
}

func init() {
	serviceDefs[postgresDef.Type] = postgresDef
}

// serverVersion is set at link time via -ldflags "-X main.serverVersion=..."
var serverVersion = "dev"

type Config struct {
	ListenAddr     string
	AdvertiseHost  string
	PgImage        string
	PgUser         string
	PgPassword     string
	PgDB           string
	StartupTimeout time.Duration
	DockerBin      string
}

type Server struct {
	cfg Config
}

// ---------- naming ----------

var (
	reNonName = regexp.MustCompile(`[^a-z0-9-]+`)
	reDashRun = regexp.MustCompile(`-+`)
)

func normalize(s string) string {
	s = strings.ToLower(s)
	s = reNonName.ReplaceAllString(s, "-")
	s = reDashRun.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

func truncateWithHash(s string, max int) string {
	if len(s) <= max {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	short := hex.EncodeToString(sum[:])[:8]
	keep := max - len(short) - 1
	if keep < 1 {
		return short
	}
	return strings.TrimRight(s[:keep], "-") + "-" + short
}

func serviceContainerName(prefix, repo, worktree string) (string, error) {
	r := normalize(repo)
	w := normalize(worktree)
	if r == "" || w == "" {
		return "", errors.New("repo and worktree must not be empty after normalization")
	}
	name := prefix + "-" + r + "-" + w
	if len(name) > dockerNameMax {
		budget := dockerNameMax - len(prefix+"-"+r+"-")
		w = truncateWithHash(w, budget)
		name = prefix + "-" + r + "-" + w
		log.Printf("pgpool: container name exceeded %d chars, truncated worktree to %q", dockerNameMax, w)
	}
	return name, nil
}

func serviceVolumeName(prefix, repo, worktree string) (string, error) {
	r := normalize(repo)
	w := normalize(worktree)
	if r == "" || w == "" {
		return "", errors.New("repo and worktree must not be empty after normalization")
	}
	name := prefix + "-" + r + "-" + w
	if len(name) > dockerNameMax {
		budget := dockerNameMax - len(prefix+"-"+r+"-")
		w = truncateWithHash(w, budget)
		name = prefix + "-" + r + "-" + w
	}
	return name, nil
}

// ---------- docker ----------

type InspectState struct {
	Exists    bool
	Running   bool
	ID        string
	CreatedAt string
}

type containerJSON struct {
	ID      string `json:"Id"`
	Created string `json:"Created"`
	State   struct {
		Status  string `json:"Status"`
		Running bool   `json:"Running"`
	} `json:"State"`
}

func (s *Server) dockerCmd(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, s.cfg.DockerBin, args...)
}

func (s *Server) runDocker(ctx context.Context, args ...string) (string, string, error) {
	cmd := s.dockerCmd(ctx, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (s *Server) inspect(ctx context.Context, name string) (InspectState, error) {
	out, errOut, err := s.runDocker(ctx, "inspect", name)
	if err != nil {
		if strings.Contains(errOut, "No such object") || strings.Contains(errOut, "no such") {
			return InspectState{Exists: false}, nil
		}
		return InspectState{}, fmt.Errorf("docker inspect %s: %w: %s", name, err, strings.TrimSpace(errOut))
	}
	var arr []containerJSON
	if err := json.Unmarshal([]byte(out), &arr); err != nil {
		return InspectState{}, fmt.Errorf("parse docker inspect: %w", err)
	}
	if len(arr) == 0 {
		return InspectState{Exists: false}, nil
	}
	c := arr[0]
	return InspectState{
		Exists:    true,
		Running:   c.State.Running,
		ID:        c.ID,
		CreatedAt: c.Created,
	}, nil
}

func (s *Server) hostPort(ctx context.Context, name string, containerPort int) (string, error) {
	out, errOut, err := s.runDocker(ctx, "port", name, fmt.Sprintf("%d/tcp", containerPort))
	if err != nil {
		return "", fmt.Errorf("docker port %s %d/tcp: %w: %s", name, containerPort, err, strings.TrimSpace(errOut))
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		idx := strings.LastIndex(line, ":")
		if idx < 0 {
			continue
		}
		port := strings.TrimSpace(line[idx+1:])
		if port != "" {
			return port, nil
		}
	}
	return "", fmt.Errorf("docker port %s %d/tcp: no mapping found in %q", name, containerPort, out)
}

func (s *Server) logsTail(ctx context.Context, name string, n int) string {
	out, errOut, err := s.runDocker(ctx, "logs", "--tail", fmt.Sprint(n), name)
	if err != nil {
		return fmt.Sprintf("(failed to read logs: %s)", strings.TrimSpace(errOut))
	}
	return out
}

func (s *Server) volumeCreate(ctx context.Context, name string) error {
	_, errOut, err := s.runDocker(ctx, "volume", "create", name)
	if err != nil {
		return fmt.Errorf("docker volume create %s: %w: %s", name, err, strings.TrimSpace(errOut))
	}
	return nil
}

func (s *Server) volumeRemove(ctx context.Context, name string) error {
	_, errOut, err := s.runDocker(ctx, "volume", "rm", name)
	if err != nil {
		if strings.Contains(errOut, "No such volume") || strings.Contains(errOut, "no such") {
			return nil
		}
		return fmt.Errorf("docker volume rm %s: %w: %s", name, err, strings.TrimSpace(errOut))
	}
	return nil
}

func (s *Server) containerStart(ctx context.Context, name string) error {
	_, errOut, err := s.runDocker(ctx, "start", name)
	if err != nil {
		return fmt.Errorf("docker start %s: %w: %s", name, err, strings.TrimSpace(errOut))
	}
	return nil
}

func (s *Server) containerRemove(ctx context.Context, name string) error {
	_, errOut, err := s.runDocker(ctx, "rm", "-f", name)
	if err != nil {
		if strings.Contains(errOut, "No such container") || strings.Contains(errOut, "no such") {
			return nil
		}
		return fmt.Errorf("docker rm -f %s: %w: %s", name, err, strings.TrimSpace(errOut))
	}
	return nil
}

type runOpts struct {
	def                      ServiceDef
	container, volume, image string
	repo, worktree           string
}

func (s *Server) containerRun(ctx context.Context, o runOpts) error {
	args := []string{
		"run", "-d",
		"--name", o.container,
		"--restart", "unless-stopped",
	}
	for _, e := range o.def.Endpoints {
		args = append(args, "-p", fmt.Sprintf("0:%d", e.ContainerPort))
	}
	args = append(args, o.def.DockerArgs(s.cfg, o.volume)...)
	args = append(args,
		"--label", labelPgpool+"=true",
		"--label", labelRepo+"="+o.repo,
		"--label", labelWorktree+"="+o.worktree,
		"--label", labelService+"="+o.def.Type,
		o.image,
	)
	_, errOut, err := s.runDocker(ctx, args...)
	if err != nil {
		return fmt.Errorf("docker run %s: %w: %s", o.container, err, strings.TrimSpace(errOut))
	}
	return nil
}

func (s *Server) pgIsReady(ctx context.Context, container string) error {
	deadline := time.Now().Add(s.cfg.StartupTimeout)
	for {
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, _, err := s.runDocker(checkCtx, "exec", container, "pg_isready", "-U", s.cfg.PgUser)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("postgres not ready after %s", s.cfg.StartupTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

type ListedContainer struct {
	Container string `json:"container"`
	Repo      string `json:"repo"`
	Worktree  string `json:"worktree"`
	State     string `json:"state"`
	URL       string `json:"url,omitempty"`
	HostPort  string `json:"host_port,omitempty"`
	CreatedAt string `json:"created_at"`
}

type dockerPSRow struct {
	ID      string `json:"ID"`
	Names   string `json:"Names"`
	Labels  string `json:"Labels"`
	State   string `json:"State"`
	CreatedAt string `json:"CreatedAt"`
}

func (s *Server) listContainers(ctx context.Context) ([]ListedContainer, error) {
	out, errOut, err := s.runDocker(ctx, "ps", "-a",
		"--filter", "label="+labelPgpool+"=true",
		"--format", "{{json .}}",
	)
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w: %s", err, strings.TrimSpace(errOut))
	}
	var results []ListedContainer
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var row dockerPSRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("parse docker ps row: %w", err)
		}
		labels := parseDockerLabels(row.Labels)
		lc := ListedContainer{
			Container: row.Names,
			Repo:      labels[labelRepo],
			Worktree:  labels[labelWorktree],
			State:     row.State,
			CreatedAt: row.CreatedAt,
		}
		if row.State == "running" {
			port, err := s.hostPort(ctx, row.Names, 5432)
			if err == nil {
				lc.HostPort = port
				lc.URL = s.buildURL(port)
			}
		}
		results = append(results, lc)
	}
	return results, nil
}

func parseDockerLabels(s string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		i := strings.Index(kv, "=")
		if i < 0 {
			continue
		}
		out[kv[:i]] = kv[i+1:]
	}
	return out
}

func (s *Server) buildURL(port string) string {
	pw := url.QueryEscape(s.cfg.PgPassword)
	return fmt.Sprintf("postgresql://%s:%s@%s:%s/%s",
		s.cfg.PgUser, pw, s.cfg.AdvertiseHost, port, s.cfg.PgDB)
}

// ---------- core operations ----------

type UpRequest struct {
	Repo     string `json:"repo"`
	Worktree string `json:"worktree"`
	Image    string `json:"image,omitempty"`
}

type UpResponse struct {
	Container string `json:"container"`
	Volume    string `json:"volume"`
	URL       string `json:"url"`
	HostPort  string `json:"host_port"`
	Reused    bool   `json:"reused"`
}

func (s *Server) opUp(ctx context.Context, req UpRequest) (*UpResponse, error) {
	cname, err := serviceContainerName("pg", req.Repo, req.Worktree)
	if err != nil {
		return nil, err
	}
	vname, err := serviceVolumeName("pgvol", req.Repo, req.Worktree)
	if err != nil {
		return nil, err
	}
	image := req.Image
	if image == "" {
		image = s.cfg.PgImage
	}

	state, err := s.inspect(ctx, cname)
	if err != nil {
		return nil, err
	}

	reused := false
	switch {
	case state.Exists && state.Running:
		reused = true
	case state.Exists && !state.Running:
		if err := s.containerStart(ctx, cname); err != nil {
			return nil, err
		}
		if err := s.pgIsReady(ctx, cname); err != nil {
			tail := s.logsTail(ctx, cname, 50)
			return nil, fmt.Errorf("%w\nlast 50 log lines:\n%s", err, tail)
		}
		reused = true
	default:
		if err := s.volumeCreate(ctx, vname); err != nil {
			return nil, err
		}
		runErr := s.containerRun(ctx, runOpts{
			def:       postgresDef,
			container: cname, volume: vname, image: image,
			repo: normalize(req.Repo), worktree: normalize(req.Worktree),
		})
		if runErr != nil {
			if strings.Contains(runErr.Error(), "is already in use") {
				// race - reinspect and retry
				state2, err2 := s.inspect(ctx, cname)
				if err2 != nil {
					return nil, err2
				}
				if !state2.Exists {
					return nil, runErr
				}
				reused = true
			} else {
				return nil, runErr
			}
		}
		if !reused {
			if err := s.pgIsReady(ctx, cname); err != nil {
				tail := s.logsTail(ctx, cname, 50)
				return nil, fmt.Errorf("%w\nlast 50 log lines:\n%s", err, tail)
			}
		}
	}

	port, err := s.hostPort(ctx, cname, 5432)
	if err != nil {
		return nil, err
	}
	return &UpResponse{
		Container: cname,
		Volume:    vname,
		URL:       s.buildURL(port),
		HostPort:  port,
		Reused:    reused,
	}, nil
}

type DownRequest struct {
	Repo     string `json:"repo"`
	Worktree string `json:"worktree"`
}

type DownResponse struct {
	Container string `json:"container"`
	Volume    string `json:"volume"`
}

func (s *Server) opDown(ctx context.Context, req DownRequest) (*DownResponse, error) {
	cname, err := serviceContainerName("pg", req.Repo, req.Worktree)
	if err != nil {
		return nil, err
	}
	vname, err := serviceVolumeName("pgvol", req.Repo, req.Worktree)
	if err != nil {
		return nil, err
	}
	if err := s.containerRemove(ctx, cname); err != nil {
		return nil, err
	}
	if err := s.volumeRemove(ctx, vname); err != nil {
		return nil, err
	}
	return &DownResponse{Container: cname, Volume: vname}, nil
}

type StatusResponse struct {
	Repo      string `json:"repo"`
	Worktree  string `json:"worktree"`
	Container string `json:"container"`
	Volume    string `json:"volume"`
	State     string `json:"state"`
	URL       string `json:"url,omitempty"`
	HostPort  string `json:"host_port,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

func (s *Server) opStatus(ctx context.Context, repo, worktree string) (*StatusResponse, error) {
	cname, err := serviceContainerName("pg", repo, worktree)
	if err != nil {
		return nil, err
	}
	vname, err := serviceVolumeName("pgvol", repo, worktree)
	if err != nil {
		return nil, err
	}
	state, err := s.inspect(ctx, cname)
	if err != nil {
		return nil, err
	}
	resp := &StatusResponse{
		Repo: repo, Worktree: worktree, Container: cname, Volume: vname,
	}
	if !state.Exists {
		resp.State = "missing"
		return resp, nil
	}
	resp.CreatedAt = state.CreatedAt
	if !state.Running {
		resp.State = "stopped"
		return resp, nil
	}
	resp.State = "running"
	port, err := s.hostPort(ctx, cname, 5432)
	if err != nil {
		return nil, err
	}
	resp.HostPort = port
	resp.URL = s.buildURL(port)
	return resp, nil
}

// ---------- REST handlers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *Server) handleUp(w http.ResponseWriter, r *http.Request) {
	var req UpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("parse body: %w", err))
		return
	}
	resp, err := s.opUp(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDown(w http.ResponseWriter, r *http.Request) {
	var req DownRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("parse body: %w", err))
		return
	}
	resp, err := s.opDown(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	worktree := r.URL.Query().Get("worktree")
	if repo == "" || worktree == "" {
		writeError(w, http.StatusBadRequest, errors.New("repo and worktree query params required"))
		return
	}
	resp, err := s.opStatus(r.Context(), repo, worktree)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	items, err := s.listContainers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if items == nil {
		items = []ListedContainer{}
	}
	writeJSON(w, http.StatusOK, items)
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.status = code
	lrw.ResponseWriter.WriteHeader(code)
}

func (lrw *loggingResponseWriter) Write(b []byte) (int, error) {
	if lrw.status == 0 {
		lrw.status = http.StatusOK
	}
	n, err := lrw.ResponseWriter.Write(b)
	lrw.bytes += n
	return n, err
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w}
		next.ServeHTTP(lrw, r)
		log.Printf("%s %s %s %d %dB %s",
			r.RemoteAddr, r.Method, r.RequestURI, lrw.status, lrw.bytes, time.Since(start))
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"name":    serverName,
		"version": serverVersion,
	})
}

// ---------- MCP (JSON-RPC 2.0) ----------

type jsonrpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcErr     `json:"error,omitempty"`
}

type jsonrpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func (s *Server) tools() []mcpTool {
	rw := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":     map[string]any{"type": "string", "description": "Repository name"},
			"worktree": map[string]any{"type": "string", "description": "Worktree name"},
		},
		"required": []string{"repo", "worktree"},
	}
	up := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":     map[string]any{"type": "string", "description": "Repository name"},
			"worktree": map[string]any{"type": "string", "description": "Worktree name"},
			"image":    map[string]any{"type": "string", "description": "Optional Postgres image override"},
		},
		"required": []string{"repo", "worktree"},
	}
	empty := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
	return []mcpTool{
		{Name: "pgpool_up", Description: "Create or reuse a Postgres container for a worktree. Returns connection URL.", InputSchema: up},
		{Name: "pgpool_down", Description: "Destroy the Postgres container and its volume for a worktree.", InputSchema: rw},
		{Name: "pgpool_status", Description: "Report container state for a worktree.", InputSchema: rw},
		{Name: "pgpool_list", Description: "List all pgpool-managed containers on this host.", InputSchema: empty},
	}
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("POST only"))
		return
	}
	var req jsonrpcReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, jsonrpcResp{
			JSONRPC: "2.0",
			Error:   &jsonrpcErr{Code: -32700, Message: "parse error: " + err.Error()},
		})
		return
	}
	resp := s.dispatchMCP(r.Context(), req)
	// notifications (no id) get no response body
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) dispatchMCP(ctx context.Context, req jsonrpcReq) jsonrpcResp {
	resp := jsonrpcResp{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    serverName,
				"version": serverVersion,
			},
		}
	case "notifications/initialized", "initialized":
		// no-op notification
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": s.tools()}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &jsonrpcErr{Code: -32602, Message: "invalid params: " + err.Error()}
			return resp
		}
		result, err := s.callTool(ctx, p.Name, p.Arguments)
		if err != nil {
			resp.Result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}},
				"isError": true,
			}
			return resp
		}
		payload, _ := json.MarshalIndent(result, "", "  ")
		resp.Result = map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(payload)}},
			"isError": false,
		}
	default:
		resp.Error = &jsonrpcErr{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

func (s *Server) callTool(ctx context.Context, name string, args json.RawMessage) (any, error) {
	switch name {
	case "pgpool_up":
		var req UpRequest
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return nil, fmt.Errorf("parse arguments: %w", err)
			}
		}
		return s.opUp(ctx, req)
	case "pgpool_down":
		var req DownRequest
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return nil, fmt.Errorf("parse arguments: %w", err)
			}
		}
		return s.opDown(ctx, req)
	case "pgpool_status":
		var req struct {
			Repo     string `json:"repo"`
			Worktree string `json:"worktree"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return nil, fmt.Errorf("parse arguments: %w", err)
			}
		}
		return s.opStatus(ctx, req.Repo, req.Worktree)
	case "pgpool_list":
		items, err := s.listContainers(ctx)
		if err != nil {
			return nil, err
		}
		if items == nil {
			items = []ListedContainer{}
		}
		return items, nil
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

// ---------- main ----------

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	cfg := Config{
		ListenAddr:     getenv("PGPOOL_LISTEN", ":8080"),
		AdvertiseHost:  getenv("PGPOOL_ADVERTISE_HOST", "localhost"),
		PgImage:        getenv("PGPOOL_IMAGE", "postgres:17"),
		PgUser:         getenv("PGPOOL_PG_USER", "postgres"),
		PgPassword:     os.Getenv("PGPOOL_PG_PASSWORD"),
		PgDB:           getenv("PGPOOL_PG_DB", "postgres"),
		DockerBin:      getenv("PGPOOL_DOCKER_BIN", "docker"),
		StartupTimeout: 30 * time.Second,
	}

	flag.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP listen address")
	flag.StringVar(&cfg.AdvertiseHost, "advertise-host", cfg.AdvertiseHost, "hostname to include in connection URLs returned to clients")
	flag.StringVar(&cfg.PgImage, "image", cfg.PgImage, "default postgres image tag")
	flag.StringVar(&cfg.PgUser, "pg-user", cfg.PgUser, "postgres superuser")
	flag.StringVar(&cfg.PgPassword, "pg-password", cfg.PgPassword, "postgres superuser password (required)")
	flag.StringVar(&cfg.PgDB, "pg-db", cfg.PgDB, "default database name")
	flag.StringVar(&cfg.DockerBin, "docker-bin", cfg.DockerBin, "docker binary path")
	flag.DurationVar(&cfg.StartupTimeout, "startup-timeout", cfg.StartupTimeout, "postgres readiness timeout")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("%s %s\n", serverName, serverVersion)
		return
	}

	if cfg.PgPassword == "" {
		log.Fatal("pgpool: --pg-password (or PGPOOL_PG_PASSWORD) is required")
	}

	srv := &Server{cfg: cfg}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleIndex)
	mux.HandleFunc("GET /healthz", srv.handleHealth)
	mux.HandleFunc("POST /v1/up", srv.handleUp)
	mux.HandleFunc("POST /v1/down", srv.handleDown)
	mux.HandleFunc("GET /v1/status", srv.handleStatus)
	mux.HandleFunc("GET /v1/list", srv.handleList)
	mux.HandleFunc("POST /mcp", srv.handleMCP)

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           requestLogger(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	log.Printf("pgpool listening on %s (advertise-host=%s, image=%s)", cfg.ListenAddr, cfg.AdvertiseHost, cfg.PgImage)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("pgpool: %v", err)
	}
}
