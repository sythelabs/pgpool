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
	DockerArgs      func(cfg Config, volume string) []string  // flags placed BEFORE the image
	DockerCommand   func(cfg Config) []string                 // args placed AFTER the image (the container CMD)
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
		u := &url.URL{
			Scheme: "postgresql",
			User:   url.UserPassword(cfg.PgUser, cfg.PgPassword),
			Host:   cfg.AdvertiseHost + ":" + hostPort,
			Path:   cfg.PgDB,
		}
		return u.String()
	},
}

func init() {
	serviceDefs[postgresDef.Type] = postgresDef
}

var seaweedfsDef = ServiceDef{
	Type:            "seaweedfs",
	ContainerPrefix: "weed",
	VolumePrefix:    "weedvol",
	Image:           "chrislusf/seaweedfs:3.71",
	DockerArgs: func(_ Config, volume string) []string {
		return []string{"-v", volume + ":/data"}
	},
	DockerCommand: func(_ Config) []string {
		return []string{"server", "-dir=/data", "-master", "-volume", "-filer", "-s3"}
	},
	Endpoints: []EndpointSpec{
		{Role: "master", ContainerPort: 9333, Scheme: "http"},
		{Role: "volume", ContainerPort: 8080, Scheme: "http"},
		{Role: "filer", ContainerPort: 8888, Scheme: "http"},
		{Role: "s3", ContainerPort: 8333, Scheme: "http"},
	},
	Readiness: func(ctx context.Context, s *Server, container string, hostPorts map[string]string) error {
		return s.httpReady(ctx, "http://"+s.cfg.AdvertiseHost+":"+hostPorts["master"]+"/cluster/status")
	},
	BuildURL: func(cfg Config, role, hostPort string) string {
		return fmt.Sprintf("http://%s:%s", cfg.AdvertiseHost, hostPort)
	},
}

func init() {
	serviceDefs[seaweedfsDef.Type] = seaweedfsDef
}

// ---------- endpoint helpers ----------

type EndpointInfo struct {
	URL           string `json:"url"`
	HostPort      string `json:"host_port"`
	ContainerPort int    `json:"container_port"`
}

func buildEndpointInfo(cfg Config, def ServiceDef, hostPorts map[string]string) map[string]EndpointInfo {
	out := map[string]EndpointInfo{}
	for _, e := range def.Endpoints {
		hp, ok := hostPorts[e.Role]
		if !ok {
			continue
		}
		out[e.Role] = EndpointInfo{
			URL:           def.BuildURL(cfg, e.Role, hp),
			HostPort:      hp,
			ContainerPort: e.ContainerPort,
		}
	}
	return out
}

// serverVersion is set at link time via -ldflags "-X main.serverVersion=..."
var serverVersion = "dev"

type Config struct {
	ListenAddr      string
	AdvertiseHost   string
	PgImage         string
	PgUser          string
	PgPassword      string
	PgDB            string
	StartupTimeout  time.Duration
	DockerBin       string
	DefaultServices []string
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
	)
	args = append(args, o.image)
	if o.def.DockerCommand != nil {
		args = append(args, o.def.DockerCommand(s.cfg)...)
	}
	_, errOut, err := s.runDocker(ctx, args...)
	if err != nil {
		return fmt.Errorf("docker run %s: %w: %s", o.container, err, strings.TrimSpace(errOut))
	}
	return nil
}

func (s *Server) httpReady(ctx context.Context, url string) error {
	deadline := time.Now().Add(s.cfg.StartupTimeout)
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("http readiness probe %s timed out after %s", url, s.cfg.StartupTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
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

type dockerPSRow struct {
	ID        string `json:"ID"`
	Names     string `json:"Names"`
	Labels    string `json:"Labels"`
	State     string `json:"State"`
	CreatedAt string `json:"CreatedAt"`
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

// ---------- service result types ----------

type ServiceResult struct {
	Type      string                  `json:"type"`
	Container string                  `json:"container"`
	Volume    string                  `json:"volume"`
	State     string                  `json:"state,omitempty"`
	CreatedAt string                  `json:"created_at,omitempty"`
	Reused    bool                    `json:"reused,omitempty"`
	Endpoints map[string]EndpointInfo `json:"endpoints,omitempty"`
}

// ---------- per-service primitives ----------

func (s *Server) collectHostPorts(ctx context.Context, container string, def ServiceDef) (map[string]string, error) {
	out := map[string]string{}
	for _, e := range def.Endpoints {
		hp, err := s.hostPort(ctx, container, e.ContainerPort)
		if err != nil {
			return nil, fmt.Errorf("%s: lookup %s host port: %w", def.Type, e.Role, err)
		}
		out[e.Role] = hp
	}
	return out, nil
}

func (s *Server) serviceUp(ctx context.Context, def ServiceDef, repo, worktree, imageOverride string) (ServiceResult, error) {
	cname, err := serviceContainerName(def.ContainerPrefix, repo, worktree)
	if err != nil {
		return ServiceResult{}, err
	}
	vname, err := serviceVolumeName(def.VolumePrefix, repo, worktree)
	if err != nil {
		return ServiceResult{}, err
	}
	image := imageOverride
	if image == "" {
		image = def.Image
	}

	state, err := s.inspect(ctx, cname)
	if err != nil {
		return ServiceResult{}, err
	}

	reused := false
	switch {
	case state.Exists && state.Running:
		reused = true
	case state.Exists && !state.Running:
		if err := s.containerStart(ctx, cname); err != nil {
			return ServiceResult{}, err
		}
		hostPorts, err := s.collectHostPorts(ctx, cname, def)
		if err != nil {
			return ServiceResult{}, err
		}
		if err := def.Readiness(ctx, s, cname, hostPorts); err != nil {
			tail := s.logsTail(ctx, cname, 50)
			return ServiceResult{}, fmt.Errorf("%s: %w\nlast 50 log lines:\n%s", def.Type, err, tail)
		}
		reused = true
	default:
		if err := s.volumeCreate(ctx, vname); err != nil {
			return ServiceResult{}, err
		}
		runErr := s.containerRun(ctx, runOpts{
			def: def, container: cname, volume: vname, image: image,
			repo: normalize(repo), worktree: normalize(worktree),
		})
		if runErr != nil {
			if strings.Contains(runErr.Error(), "is already in use") {
				state2, err2 := s.inspect(ctx, cname)
				if err2 != nil {
					return ServiceResult{}, err2
				}
				if !state2.Exists {
					return ServiceResult{}, runErr
				}
				reused = true
			} else {
				return ServiceResult{}, runErr
			}
		}
		if !reused {
			hostPorts, err := s.collectHostPorts(ctx, cname, def)
			if err != nil {
				return ServiceResult{}, err
			}
			if err := def.Readiness(ctx, s, cname, hostPorts); err != nil {
				tail := s.logsTail(ctx, cname, 50)
				return ServiceResult{}, fmt.Errorf("%s: %w\nlast 50 log lines:\n%s", def.Type, err, tail)
			}
		}
	}

	hostPorts, err := s.collectHostPorts(ctx, cname, def)
	if err != nil {
		return ServiceResult{}, err
	}
	return ServiceResult{
		Type:      def.Type,
		Container: cname,
		Volume:    vname,
		Reused:    reused,
		Endpoints: buildEndpointInfo(s.cfg, def, hostPorts),
	}, nil
}

func (s *Server) serviceDown(ctx context.Context, def ServiceDef, repo, worktree string) (ServiceResult, error) {
	cname, err := serviceContainerName(def.ContainerPrefix, repo, worktree)
	if err != nil {
		return ServiceResult{}, err
	}
	vname, err := serviceVolumeName(def.VolumePrefix, repo, worktree)
	if err != nil {
		return ServiceResult{}, err
	}
	if err := s.containerRemove(ctx, cname); err != nil {
		return ServiceResult{}, err
	}
	if err := s.volumeRemove(ctx, vname); err != nil {
		return ServiceResult{}, err
	}
	return ServiceResult{Type: def.Type, Container: cname, Volume: vname}, nil
}

func (s *Server) serviceStatus(ctx context.Context, def ServiceDef, repo, worktree string) (ServiceResult, error) {
	cname, err := serviceContainerName(def.ContainerPrefix, repo, worktree)
	if err != nil {
		return ServiceResult{}, err
	}
	vname, err := serviceVolumeName(def.VolumePrefix, repo, worktree)
	if err != nil {
		return ServiceResult{}, err
	}
	state, err := s.inspect(ctx, cname)
	if err != nil {
		return ServiceResult{}, err
	}
	res := ServiceResult{Type: def.Type, Container: cname, Volume: vname}
	if !state.Exists {
		res.State = "missing"
		return res, nil
	}
	res.CreatedAt = state.CreatedAt
	if !state.Running {
		res.State = "stopped"
		return res, nil
	}
	res.State = "running"
	hostPorts, err := s.collectHostPorts(ctx, cname, def)
	if err != nil {
		return ServiceResult{}, err
	}
	res.Endpoints = buildEndpointInfo(s.cfg, def, hostPorts)
	return res, nil
}

// ---------- request/response types ----------

type UpRequest struct {
	Repo     string   `json:"repo"`
	Worktree string   `json:"worktree"`
	Services []string `json:"services,omitempty"`
	Image    string   `json:"image,omitempty"` // optional, applies to postgres if present
}

type UpResponse struct {
	Services []ServiceResult `json:"services"`
}

type DownRequest struct {
	Repo     string   `json:"repo"`
	Worktree string   `json:"worktree"`
	Services []string `json:"services,omitempty"`
}

type DownResponse struct {
	Services []ServiceResult `json:"services"`
}

type StatusResponse struct {
	Repo     string          `json:"repo"`
	Worktree string          `json:"worktree"`
	Services []ServiceResult `json:"services"`
}

type ListedContainer struct {
	Type      string                  `json:"type"`
	Container string                  `json:"container"`
	Volume    string                  `json:"volume,omitempty"`
	Repo      string                  `json:"repo"`
	Worktree  string                  `json:"worktree"`
	State     string                  `json:"state"`
	CreatedAt string                  `json:"created_at"`
	Endpoints map[string]EndpointInfo `json:"endpoints,omitempty"`
}

// ---------- multi-service operations ----------

func (s *Server) resolveServices(requested []string) ([]ServiceDef, error) {
	if len(requested) == 0 {
		requested = s.cfg.DefaultServices
	}
	if len(requested) == 0 {
		return nil, errors.New("no services requested and no server default configured")
	}
	out := make([]ServiceDef, 0, len(requested))
	for _, name := range requested {
		def, ok := serviceDefs[name]
		if !ok {
			return nil, fmt.Errorf("unknown service %q", name)
		}
		out = append(out, def)
	}
	return out, nil
}

func (s *Server) opUp(ctx context.Context, req UpRequest) (*UpResponse, error) {
	defs, err := s.resolveServices(req.Services)
	if err != nil {
		return nil, err
	}
	results := make([]ServiceResult, 0, len(defs))
	for _, def := range defs {
		image := ""
		if def.Type == "postgres" {
			image = req.Image
		}
		res, err := s.serviceUp(ctx, def, req.Repo, req.Worktree, image)
		if err != nil {
			return &UpResponse{Services: results}, err
		}
		results = append(results, res)
	}
	return &UpResponse{Services: results}, nil
}

func (s *Server) opDown(ctx context.Context, req DownRequest) (*DownResponse, error) {
	defs, err := s.resolveServices(req.Services)
	if err != nil {
		return nil, err
	}
	results := make([]ServiceResult, 0, len(defs))
	for _, def := range defs {
		res, err := s.serviceDown(ctx, def, req.Repo, req.Worktree)
		if err != nil {
			return &DownResponse{Services: results}, err
		}
		results = append(results, res)
	}
	return &DownResponse{Services: results}, nil
}

func (s *Server) opStatus(ctx context.Context, repo, worktree, service string) (*StatusResponse, error) {
	var defs []ServiceDef
	if service != "" {
		def, ok := serviceDefs[service]
		if !ok {
			return nil, fmt.Errorf("unknown service %q", service)
		}
		defs = []ServiceDef{def}
	} else {
		var err error
		defs, err = s.resolveServices(nil)
		if err != nil {
			return nil, err
		}
	}
	results := make([]ServiceResult, 0, len(defs))
	for _, def := range defs {
		res, err := s.serviceStatus(ctx, def, repo, worktree)
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	return &StatusResponse{Repo: repo, Worktree: worktree, Services: results}, nil
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
		typ := labels[labelService]
		if typ == "" {
			continue
		}
		def, defKnown := serviceDefs[typ]
		if !defKnown {
			continue
		}
		vname, _ := serviceVolumeName(def.VolumePrefix, labels[labelRepo], labels[labelWorktree])
		lc := ListedContainer{
			Type:      typ,
			Container: row.Names,
			Volume:    vname,
			Repo:      labels[labelRepo],
			Worktree:  labels[labelWorktree],
			State:     row.State,
			CreatedAt: row.CreatedAt,
		}
		if row.State == "running" {
			hostPorts, err := s.collectHostPorts(ctx, row.Names, def)
			if err == nil {
				lc.Endpoints = buildEndpointInfo(s.cfg, def, hostPorts)
			}
		}
		results = append(results, lc)
	}
	return results, nil
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
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":    err.Error(),
			"services": resp.Services,
		})
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
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":    err.Error(),
			"services": resp.Services,
		})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	worktree := r.URL.Query().Get("worktree")
	service := r.URL.Query().Get("service")
	if repo == "" || worktree == "" {
		writeError(w, http.StatusBadRequest, errors.New("repo and worktree query params required"))
		return
	}
	resp, err := s.opStatus(r.Context(), repo, worktree, service)
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
	rwSvc := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":     map[string]any{"type": "string", "description": "Repository name"},
			"worktree": map[string]any{"type": "string", "description": "Worktree name"},
			"services": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional subset of service types to act on. Defaults to server's --services list.",
			},
		},
		"required": []string{"repo", "worktree"},
	}
	rwOptionalService := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":     map[string]any{"type": "string", "description": "Repository name"},
			"worktree": map[string]any{"type": "string", "description": "Worktree name"},
			"service":  map[string]any{"type": "string", "description": "Optional single service type to filter to."},
		},
		"required": []string{"repo", "worktree"},
	}
	upSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":     map[string]any{"type": "string", "description": "Repository name"},
			"worktree": map[string]any{"type": "string", "description": "Worktree name"},
			"services": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional subset of service types to bring up. Defaults to server's --services list.",
			},
			"image": map[string]any{"type": "string", "description": "Optional postgres image override."},
		},
		"required": []string{"repo", "worktree"},
	}
	empty := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
	return []mcpTool{
		{Name: "pgpool_up", Description: "Bring up the configured services for a worktree. Returns one entry per service with its endpoints.", InputSchema: upSchema},
		{Name: "pgpool_down", Description: "Tear down services for a worktree. Defaults to all configured services.", InputSchema: rwSvc},
		{Name: "pgpool_status", Description: "Report state of services for a worktree. Optionally filter to one service.", InputSchema: rwOptionalService},
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
			Service  string `json:"service"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return nil, fmt.Errorf("parse arguments: %w", err)
			}
		}
		return s.opStatus(ctx, req.Repo, req.Worktree, req.Service)
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

// ---------- helpers ----------

func parseServicesCSV(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------- main ----------

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	servicesCSV := getenv("PGPOOL_SERVICES", "postgres")

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
	flag.StringVar(&servicesCSV, "services", servicesCSV, "comma-separated list of service types to bring up by default")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("%s %s\n", serverName, serverVersion)
		return
	}

	if cfg.PgPassword == "" {
		log.Fatal("pgpool: --pg-password (or PGPOOL_PG_PASSWORD) is required")
	}

	cfg.DefaultServices = parseServicesCSV(servicesCSV)
	if len(cfg.DefaultServices) == 0 {
		log.Fatal("pgpool: --services must be non-empty")
	}
	for _, name := range cfg.DefaultServices {
		if _, ok := serviceDefs[name]; !ok {
			log.Fatalf("pgpool: unknown service %q in --services", name)
		}
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

	log.Printf("pgpool listening on %s (advertise-host=%s, services=%s, postgres-image=%s)",
		cfg.ListenAddr, cfg.AdvertiseHost, strings.Join(cfg.DefaultServices, ","), cfg.PgImage)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("pgpool: %v", err)
	}
}
