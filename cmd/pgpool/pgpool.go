package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sythelabs/pgpool/internal/selfupdate"
)

//go:embed index.html
var indexHTML []byte

//go:embed htmx-2.0.10.min.js
var htmxJS []byte

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

// EndpointRole names a logical port exposed by a service. Using a named
// type (not bare string) lets callers refer to known roles via the Role*
// constants below, which keeps typos reviewable rather than landing as
// silent map misses at runtime.
type EndpointRole string

const (
	RolePrimary EndpointRole = "primary"
	RoleMaster  EndpointRole = "master"
	RoleVolume  EndpointRole = "volume"
	RoleFiler   EndpointRole = "filer"
	RoleS3      EndpointRole = "s3"
	RoleStorage EndpointRole = "storage"
)

type EndpointSpec struct {
	Role          EndpointRole
	ContainerPort int
	Scheme        string // "postgresql" | "http" | ...
}

// ServiceBuildCtx is the single argument passed into per-service builder funcs.
// Adding a new input here lets future services pick it up without churning every
// existing ServiceDef.
//
// Image is the resolved image tag (per-call override already applied). It is
// passed to docker run by containerRun and is also available to builders if
// they ever need to switch flags by tag.
type ServiceBuildCtx struct {
	Cfg       Config
	Volume    string
	Image     string
	HostPorts map[EndpointRole]string // role -> pre-allocated host port (as decimal string)
}

// newServiceBuildCtx returns the base context for a service call. HostPorts is
// nil; populate it via withHostPorts once reservations or lookups have run.
func newServiceBuildCtx(cfg Config, volume, image string) ServiceBuildCtx {
	return ServiceBuildCtx{Cfg: cfg, Volume: volume, Image: image}
}

// withHostPorts returns a copy of bc with HostPorts set. The original is left
// alone so each phase of serviceUp gets an explicit, named transition rather
// than relying on a mutating field.
func (bc ServiceBuildCtx) withHostPorts(hp map[EndpointRole]string) ServiceBuildCtx {
	bc.HostPorts = hp
	return bc
}

type ServiceDef struct {
	Type            string
	ContainerPrefix string
	VolumePrefix    string
	Image           string
	DockerArgs      func(bc ServiceBuildCtx) []string // flags placed BEFORE the image
	DockerCommand   func(bc ServiceBuildCtx) []string // args placed AFTER the image (container CMD)
	Endpoints       []EndpointSpec
	Readiness       func(ctx context.Context, s *Server, container string, bc ServiceBuildCtx) error
	BuildURL        func(bc ServiceBuildCtx, role EndpointRole, hostPort string) string
}

var serviceDefs = map[string]ServiceDef{}

const (
	defaultPostgresImage          = "pgvector/pgvector:pg18"
	defaultPostgresMaxConnections = 100
	defaultSeaweedfsImage         = "chrislusf/seaweedfs:4.40"
)

func postgresMaxConnections(value int) int {
	if value == 0 {
		return defaultPostgresMaxConnections
	}
	return value
}

var postgresDef = ServiceDef{
	Type:            "postgres",
	ContainerPrefix: "pg",
	VolumePrefix:    "pgvol",
	Image:           defaultPostgresImage,
	DockerArgs: func(bc ServiceBuildCtx) []string {
		return []string{
			// Mount at /var/lib/postgresql (the parent), not .../data. Postgres
			// 18+ images relocated PGDATA to a major-version subdir and refuse to
			// boot when the volume is mounted at the legacy .../data path; older
			// tags keep their data under this same parent, so this is correct for
			// every supported image.
			"-v", bc.Volume + ":/var/lib/postgresql",
			"-e", "POSTGRES_PASSWORD=" + bc.Cfg.PgPassword,
			"-e", "POSTGRES_USER=" + bc.Cfg.PgUser,
			"-e", "POSTGRES_DB=" + bc.Cfg.PgDB,
		}
	},
	DockerCommand: func(bc ServiceBuildCtx) []string {
		return []string{"postgres", "-c", fmt.Sprintf("max_connections=%d", postgresMaxConnections(bc.Cfg.PgMaxConnections))}
	},
	Endpoints: []EndpointSpec{
		{Role: RolePrimary, ContainerPort: 5432, Scheme: "postgresql"},
	},
	Readiness: func(ctx context.Context, s *Server, container string, _ ServiceBuildCtx) error {
		return s.pgIsReady(ctx, container)
	},
	BuildURL: func(bc ServiceBuildCtx, _ EndpointRole, hostPort string) string {
		u := &url.URL{
			Scheme: "postgresql",
			User:   url.UserPassword(bc.Cfg.PgUser, bc.Cfg.PgPassword),
			Host:   bc.Cfg.AdvertiseHost + ":" + hostPort,
			Path:   bc.Cfg.PgDB,
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
	Image:           defaultSeaweedfsImage,
	DockerArgs: func(bc ServiceBuildCtx) []string {
		return []string{"-v", bc.Volume + ":/data"}
	},
	DockerCommand: func(_ ServiceBuildCtx) []string {
		return []string{"server", "-dir=/data", "-master", "-volume", "-filer", "-s3"}
	},
	Endpoints: []EndpointSpec{
		{Role: RoleMaster, ContainerPort: 9333, Scheme: "http"},
		{Role: RoleVolume, ContainerPort: 8080, Scheme: "http"},
		{Role: RoleFiler, ContainerPort: 8888, Scheme: "http"},
		{Role: RoleS3, ContainerPort: 8333, Scheme: "http"},
	},
	Readiness: func(ctx context.Context, s *Server, container string, bc ServiceBuildCtx) error {
		return s.httpReady(ctx, "http://"+bc.Cfg.AdvertiseHost+":"+bc.HostPorts[RoleMaster]+"/cluster/status")
	},
	BuildURL: func(bc ServiceBuildCtx, _ EndpointRole, hostPort string) string {
		return fmt.Sprintf("http://%s:%s", bc.Cfg.AdvertiseHost, hostPort)
	},
}

func init() {
	serviceDefs[seaweedfsDef.Type] = seaweedfsDef
}

var fakeGCSDef = ServiceDef{
	Type:            "fake-gcs",
	ContainerPrefix: "gcs",
	VolumePrefix:    "gcsvol",
	Image:           "fsouza/fake-gcs-server:1.49",
	DockerArgs: func(bc ServiceBuildCtx) []string {
		return []string{"-v", bc.Volume + ":/storage"}
	},
	DockerCommand: func(bc ServiceBuildCtx) []string {
		port := bc.HostPorts[RoleStorage]
		return []string{
			"-scheme", "http",
			"-public-host", bc.Cfg.AdvertiseHost + ":" + port,
			"-external-url", "http://" + bc.Cfg.AdvertiseHost + ":" + port,
		}
	},
	Endpoints: []EndpointSpec{
		{Role: RoleStorage, ContainerPort: 4443, Scheme: "http"},
	},
	Readiness: func(ctx context.Context, s *Server, _ string, bc ServiceBuildCtx) error {
		return s.httpReady(ctx, "http://"+bc.Cfg.AdvertiseHost+":"+bc.HostPorts[RoleStorage]+"/storage/v1/b")
	},
	BuildURL: func(bc ServiceBuildCtx, _ EndpointRole, hostPort string) string {
		return "http://" + bc.Cfg.AdvertiseHost + ":" + hostPort
	},
}

func init() {
	serviceDefs[fakeGCSDef.Type] = fakeGCSDef
}

// ---------- endpoint helpers ----------

type EndpointInfo struct {
	URL           string `json:"url"`
	HostPort      string `json:"host_port"`
	ContainerPort int    `json:"container_port"`
}

func buildEndpointInfo(bc ServiceBuildCtx, def ServiceDef, hostPorts map[EndpointRole]string) map[EndpointRole]EndpointInfo {
	out := map[EndpointRole]EndpointInfo{}
	for _, e := range def.Endpoints {
		hp, ok := hostPorts[e.Role]
		if !ok {
			continue
		}
		out[e.Role] = EndpointInfo{
			URL:           def.BuildURL(bc, e.Role, hp),
			HostPort:      hp,
			ContainerPort: e.ContainerPort,
		}
	}
	return out
}

// serverVersion is set at link time via -ldflags "-X main.serverVersion=..."
var serverVersion = "dev"

type Config struct {
	ListenAddr       string
	AdvertiseHost    string
	PgImage          string
	PgUser           string
	PgPassword       string
	PgDB             string
	PgMaxConnections int
	StartupTimeout   time.Duration
	DockerBin        string
	DefaultServices  []string
}

// dockerExec runs a docker subcommand. Production wraps exec.CommandContext;
// tests substitute a fake so reload/up/down can be exercised without docker.
type dockerExec func(ctx context.Context, args ...string) (string, string, error)

type Server struct {
	cfg    Config
	docker dockerExec
}

func newServer(cfg Config) *Server {
	s := &Server{cfg: cfg}
	s.docker = s.execDocker
	return s
}

func (s *Server) execDocker(ctx context.Context, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, s.cfg.DockerBin, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
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

func (s *Server) runDocker(ctx context.Context, args ...string) (string, string, error) {
	return s.docker(ctx, args...)
}

// isDockerNoSuchError reports whether a docker stderr blob is the well-known
// "object does not exist" failure, regardless of the noun docker used
// ("container" / "volume" / "object") and case. Centralised so remove/inspect
// paths stay in sync if docker rewords its messages.
func isDockerNoSuchError(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "no such")
}

func (s *Server) inspect(ctx context.Context, name string) (InspectState, error) {
	out, errOut, err := s.runDocker(ctx, "inspect", name)
	if err != nil {
		if isDockerNoSuchError(errOut) {
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
		if isDockerNoSuchError(errOut) {
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
		if isDockerNoSuchError(errOut) {
			return nil
		}
		return fmt.Errorf("docker rm -f %s: %w: %s", name, err, strings.TrimSpace(errOut))
	}
	return nil
}

type runOpts struct {
	def            ServiceDef
	container      string
	repo, worktree string
	bc             ServiceBuildCtx
}

func (s *Server) containerRun(ctx context.Context, o runOpts) error {
	args := []string{
		"run", "-d",
		"--name", o.container,
		"--restart", "unless-stopped",
	}
	for _, e := range o.def.Endpoints {
		hp, ok := o.bc.HostPorts[e.Role]
		if !ok || hp == "" {
			// Programmer error: reserveHostPorts must populate every role
			// declared in def.Endpoints before containerRun is called.
			panic(fmt.Sprintf("pgpool: %s: missing pre-allocated host port for role %q", o.def.Type, e.Role))
		}
		args = append(args, "-p", fmt.Sprintf("%s:%d", hp, e.ContainerPort))
	}
	args = append(args, o.def.DockerArgs(o.bc)...)
	args = append(args,
		"--label", labelPgpool+"=true",
		"--label", labelRepo+"="+o.repo,
		"--label", labelWorktree+"="+o.worktree,
		"--label", labelService+"="+o.def.Type,
	)
	args = append(args, o.bc.Image)
	if o.def.DockerCommand != nil {
		args = append(args, o.def.DockerCommand(o.bc)...)
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
	var lastStatus int
	var lastErr error
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		switch {
		case err != nil:
			lastErr = err
		default:
			resp.Body.Close()
			lastStatus = resp.StatusCode
			lastErr = nil
			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			switch {
			case lastStatus != 0:
				return fmt.Errorf("http readiness probe %s timed out after %s; last status %d", url, s.cfg.StartupTimeout, lastStatus)
			case lastErr != nil:
				return fmt.Errorf("http readiness probe %s timed out after %s; last error: %v", url, s.cfg.StartupTimeout, lastErr)
			default:
				return fmt.Errorf("http readiness probe %s timed out after %s", url, s.cfg.StartupTimeout)
			}
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
	Type      string                        `json:"type"`
	Container string                        `json:"container"`
	Volume    string                        `json:"volume"`
	State     string                        `json:"state,omitempty"`
	CreatedAt string                        `json:"created_at,omitempty"`
	Reused    bool                          `json:"reused,omitempty"`
	Endpoints map[EndpointRole]EndpointInfo `json:"endpoints,omitempty"`
}

// ---------- per-service primitives ----------

func (s *Server) collectHostPorts(ctx context.Context, container string, def ServiceDef) (map[EndpointRole]string, error) {
	out := map[EndpointRole]string{}
	for _, e := range def.Endpoints {
		hp, err := s.hostPort(ctx, container, e.ContainerPort)
		if err != nil {
			return nil, fmt.Errorf("%s: lookup %s host port: %w", def.Type, e.Role, err)
		}
		out[e.Role] = hp
	}
	return out, nil
}

// reserveHostPorts asks the kernel for one free TCP port per endpoint by
// briefly opening then closing a listener on 127.0.0.1:0. The returned map is
// role -> decimal port string. Used on the create path so DockerArgs and
// DockerCommand can reference the host port at container-start time
// (fake-gcs-server bakes its public URL into responses via -external-url).
//
// There is a small race between Close and `docker run -p PORT:CP`. If lost,
// docker fails fast with a bind error that surfaces unchanged.
func reserveHostPorts(endpoints []EndpointSpec) (map[EndpointRole]string, error) {
	out := make(map[EndpointRole]string, len(endpoints))
	listeners := make([]net.Listener, 0, len(endpoints))
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	for _, e := range endpoints {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("listen 127.0.0.1:0 for role %q: %w", e.Role, err)
		}
		listeners = append(listeners, l)
		addr := l.Addr().(*net.TCPAddr)
		out[e.Role] = strconv.Itoa(addr.Port)
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

	bc := newServiceBuildCtx(s.cfg, vname, image)
	reused := false
	switch {
	case state.Exists && state.Running:
		reused = true
	case state.Exists && !state.Running:
		if err := s.adoptExistingContainer(ctx, def, cname, bc); err != nil {
			return ServiceResult{}, err
		}
		reused = true
	default:
		if err := s.volumeCreate(ctx, vname); err != nil {
			return ServiceResult{}, err
		}
		hostPorts, err := reserveHostPorts(def.Endpoints)
		if err != nil {
			return ServiceResult{}, fmt.Errorf("%s: reserve host ports: %w", def.Type, err)
		}
		runBC := bc.withHostPorts(hostPorts)
		runErr := s.containerRun(ctx, runOpts{
			def: def, container: cname, repo: normalize(repo), worktree: normalize(worktree), bc: runBC,
		})
		switch {
		case runErr == nil:
			if err := def.Readiness(ctx, s, cname, runBC); err != nil {
				tail := s.logsTail(ctx, cname, 50)
				return ServiceResult{}, fmt.Errorf("%s: %w\nlast 50 log lines:\n%s", def.Type, err, tail)
			}
		case strings.Contains(runErr.Error(), "is already in use"):
			// Lost the race: a concurrent caller created the container between
			// our inspect and run. Treat as the "container exists" path - if it
			// is stopped, start it; either way re-probe readiness.
			if err := s.adoptExistingContainer(ctx, def, cname, bc); err != nil {
				return ServiceResult{}, err
			}
			reused = true
		default:
			return ServiceResult{}, runErr
		}
	}

	finalPorts, err := s.collectHostPorts(ctx, cname, def)
	if err != nil {
		return ServiceResult{}, err
	}
	finalBC := bc.withHostPorts(finalPorts)
	return ServiceResult{
		Type:      def.Type,
		Container: cname,
		Volume:    vname,
		Reused:    reused,
		Endpoints: buildEndpointInfo(finalBC, def, finalPorts),
	}, nil
}

// adoptExistingContainer starts an existing container if it is stopped, then
// re-runs the service's readiness probe. Shared between the Exists && !Running
// branch and the "name already in use" race-retry branch so both follow the
// same start-then-probe contract.
func (s *Server) adoptExistingContainer(ctx context.Context, def ServiceDef, cname string, bc ServiceBuildCtx) error {
	state, err := s.inspect(ctx, cname)
	if err != nil {
		return err
	}
	if !state.Exists {
		return fmt.Errorf("%s: container %s vanished during adopt", def.Type, cname)
	}
	if !state.Running {
		if err := s.containerStart(ctx, cname); err != nil {
			return err
		}
	}
	hostPorts, err := s.collectHostPorts(ctx, cname, def)
	if err != nil {
		return err
	}
	probeBC := bc.withHostPorts(hostPorts)
	if err := def.Readiness(ctx, s, cname, probeBC); err != nil {
		tail := s.logsTail(ctx, cname, 50)
		return fmt.Errorf("%s: %w\nlast 50 log lines:\n%s", def.Type, err, tail)
	}
	return nil
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
	bc := newServiceBuildCtx(s.cfg, vname, def.Image).withHostPorts(hostPorts)
	res.Endpoints = buildEndpointInfo(bc, def, hostPorts)
	return res, nil
}

// ---------- request/response types ----------

type UpRequest struct {
	Repo     string   `json:"repo"`
	Worktree string   `json:"worktree"`
	Services []string `json:"services,omitempty"`
	// Image overrides the postgres image tag for this call only. Ignored for
	// every other service type - those builders use def.Image.
	Image string `json:"image,omitempty"`
}

type UpResponse struct {
	Services []ServiceResult `json:"services"`
}

type ReloadRequest struct {
	Repo     string   `json:"repo"`
	Worktree string   `json:"worktree"`
	Services []string `json:"services,omitempty"`
	// Image overrides the postgres image tag for this call only. Ignored for
	// every other service type - those builders use def.Image.
	Image string `json:"image,omitempty"`
}

// ServiceFailure describes which service failed during a reload and at which
// phase. Phase is one of "down" or "up". A reload that aborts at "up" leaves
// the named service's volume destroyed - Services in the enclosing response
// will contain a corresponding entry with State="destroyed" so callers can
// see what was wiped without parsing Err.
type ServiceFailure struct {
	Type  string `json:"type"`
	Phase string `json:"phase"`
	Err   string `json:"error"`
}

const (
	reloadPhaseDown = "down"
	reloadPhaseUp   = "up"
)

type ReloadResponse struct {
	Services []ServiceResult `json:"services"`
	Failed   *ServiceFailure `json:"failed,omitempty"`
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
	Type      string                        `json:"type"`
	Container string                        `json:"container"`
	Volume    string                        `json:"volume,omitempty"`
	Repo      string                        `json:"repo"`
	Worktree  string                        `json:"worktree"`
	State     string                        `json:"state"`
	CreatedAt string                        `json:"created_at"`
	Endpoints map[EndpointRole]EndpointInfo `json:"endpoints,omitempty"`
}

type ServiceLogs struct {
	Type      string `json:"type"`
	Container string `json:"container"`
	State     string `json:"state"`
	Logs      string `json:"logs,omitempty"`
}

type LogsResponse struct {
	Repo     string        `json:"repo"`
	Worktree string        `json:"worktree"`
	Tail     int           `json:"tail"`
	Services []ServiceLogs `json:"services"`
}

const (
	defaultLogsTail = 100
	maxLogsTail     = 5000
)

// ---------- multi-service operations ----------

// errUnknownService is returned when a caller names a service that is not in
// the registry, and is the signal handlers use to translate the result to a
// 400 (caller's fault) rather than a 500 (server's fault).
var errUnknownService = errors.New("unknown service")

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
			return nil, fmt.Errorf("%w: %q", errUnknownService, name)
		}
		out = append(out, def)
	}
	return out, nil
}

func (s *Server) imageFor(def ServiceDef, override string) string {
	if def.Type != "postgres" {
		return def.Image
	}
	if override != "" {
		return override
	}
	if s.cfg.PgImage != "" {
		return s.cfg.PgImage
	}
	return def.Image
}

func (s *Server) opUp(ctx context.Context, req UpRequest) (*UpResponse, error) {
	defs, err := s.resolveServices(req.Services)
	if err != nil {
		return &UpResponse{}, err
	}
	results := make([]ServiceResult, 0, len(defs))
	for _, def := range defs {
		res, err := s.serviceUp(ctx, def, req.Repo, req.Worktree, s.imageFor(def, req.Image))
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
		return &DownResponse{}, err
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

// opReload is down-then-up per service.
//
// Partial-failure contract: if service N fails, services 1..N-1 are already
// reloaded (fresh containers + volumes) and included in resp.Services. The
// failing service is captured in resp.Failed with Phase distinguishing whether
// the failure happened in the down or up phase. If the up phase fails, the
// service's volume has already been destroyed; resp.Services will include a
// State="destroyed" entry for it so callers can see the data loss without
// having to parse the error string.
func (s *Server) opReload(ctx context.Context, req ReloadRequest) (*ReloadResponse, error) {
	defs, err := s.resolveServices(req.Services)
	if err != nil {
		return &ReloadResponse{}, err
	}
	results := make([]ServiceResult, 0, len(defs))
	for _, def := range defs {
		downRes, downErr := s.serviceDown(ctx, def, req.Repo, req.Worktree)
		if downErr != nil {
			wrapped := fmt.Errorf("%s: down: %w", def.Type, downErr)
			return &ReloadResponse{
				Services: results,
				Failed:   &ServiceFailure{Type: def.Type, Phase: reloadPhaseDown, Err: downErr.Error()},
			}, wrapped
		}
		res, upErr := s.serviceUp(ctx, def, req.Repo, req.Worktree, s.imageFor(def, req.Image))
		if upErr != nil {
			// serviceDown succeeded - the volume is gone. Surface that as a
			// State="destroyed" entry so callers see which service was wiped
			// without scraping the error string.
			results = append(results, ServiceResult{
				Type:      def.Type,
				Container: downRes.Container,
				Volume:    downRes.Volume,
				State:     "destroyed",
			})
			wrapped := fmt.Errorf("%s: up: %w", def.Type, upErr)
			return &ReloadResponse{
				Services: results,
				Failed:   &ServiceFailure{Type: def.Type, Phase: reloadPhaseUp, Err: upErr.Error()},
			}, wrapped
		}
		results = append(results, res)
	}
	return &ReloadResponse{Services: results}, nil
}

func (s *Server) opStatus(ctx context.Context, repo, worktree, service string) (*StatusResponse, error) {
	var defs []ServiceDef
	if service != "" {
		def, ok := serviceDefs[service]
		if !ok {
			return nil, fmt.Errorf("%w: %q", errUnknownService, service)
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

func (s *Server) opLogs(ctx context.Context, repo, worktree, service string, tail int) (*LogsResponse, error) {
	if repo == "" || worktree == "" {
		return nil, errors.New("repo and worktree must be non-empty")
	}
	var defs []ServiceDef
	if service != "" {
		def, ok := serviceDefs[service]
		if !ok {
			return nil, fmt.Errorf("%w: %q", errUnknownService, service)
		}
		defs = []ServiceDef{def}
	} else {
		var err error
		defs, err = s.resolveServices(nil)
		if err != nil {
			return nil, err
		}
	}
	results := make([]ServiceLogs, 0, len(defs))
	for _, def := range defs {
		cname, err := serviceContainerName(def.ContainerPrefix, repo, worktree)
		if err != nil {
			return nil, err
		}
		state, err := s.inspect(ctx, cname)
		if err != nil {
			return nil, err
		}
		entry := ServiceLogs{Type: def.Type, Container: cname}
		switch {
		case !state.Exists:
			entry.State = "missing"
		case state.Running:
			entry.State = "running"
			entry.Logs = s.logsTail(ctx, cname, tail)
		default:
			entry.State = "stopped"
			entry.Logs = s.logsTail(ctx, cname, tail)
		}
		results = append(results, entry)
	}
	return &LogsResponse{Repo: repo, Worktree: worktree, Tail: tail, Services: results}, nil
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
			if err != nil {
				log.Printf("pgpool: list: skipping endpoints for %s: %v", row.Names, err)
			} else {
				bc := newServiceBuildCtx(s.cfg, lc.Volume, def.Image).withHostPorts(hostPorts)
				lc.Endpoints = buildEndpointInfo(bc, def, hostPorts)
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
		status := http.StatusInternalServerError
		if errors.Is(err, errUnknownService) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]any{
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
		status := http.StatusInternalServerError
		if errors.Is(err, errUnknownService) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]any{
			"error":    err.Error(),
			"services": resp.Services,
		})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	var req ReloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("parse body: %w", err))
		return
	}
	resp, err := s.opReload(r.Context(), req)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errUnknownService) {
			status = http.StatusBadRequest
		}
		// Emit the typed response shape so callers can read resp.Services and
		// resp.Failed exactly the same way as on success. The error string is
		// also included in resp.Failed.Err.
		writeJSON(w, status, reloadErrorResponse(resp, err))
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// reloadErrorResponse returns the body for a reload failure. When opReload
// already populated resp.Failed (per-service failure) we emit that as-is.
// When the failure is pre-flight (e.g. unknown-service), there is no failing
// service yet, so a synthetic Failed entry carries the error message.
func reloadErrorResponse(resp *ReloadResponse, err error) ReloadResponse {
	if resp == nil {
		resp = &ReloadResponse{}
	}
	out := ReloadResponse{Services: resp.Services, Failed: resp.Failed}
	if out.Failed == nil {
		out.Failed = &ServiceFailure{Err: err.Error()}
	}
	return out
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
		status := http.StatusInternalServerError
		if errors.Is(err, errUnknownService) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func parseTailParam(raw string) (int, error) {
	if raw == "" {
		return defaultLogsTail, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid tail %q: %w", raw, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("tail must be positive, got %d", n)
	}
	if n > maxLogsTail {
		n = maxLogsTail
	}
	return n, nil
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	worktree := r.URL.Query().Get("worktree")
	service := r.URL.Query().Get("service")
	if repo == "" || worktree == "" {
		writeError(w, http.StatusBadRequest, errors.New("repo and worktree query params required"))
		return
	}
	tail, err := parseTailParam(r.URL.Query().Get("tail"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	resp, err := s.opLogs(r.Context(), repo, worktree, service, tail)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errUnknownService) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err)
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

type uiNotice struct {
	Kind    string
	Message string
}

type uiWorktree struct {
	Repo     string
	Worktree string
	Services []ListedContainer
}

type uiDashboardData struct {
	Notice    uiNotice
	Worktrees []uiWorktree
}

type uiEndpoint struct {
	Role EndpointRole
	URL  string
}

var uiTemplates = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"detailID":     uiDetailID,
	"endpoints":    sortedUIEndpoints,
	"httpEndpoint": isHTTPEndpoint,
}).Parse(`{{define "dashboard"}}
<main id="dashboard" hx-get="/ui/dashboard" hx-trigger="every 15s" hx-swap="outerHTML" aria-live="polite">
  <section class="card">
    {{if .Notice.Message}}<p class="notice {{.Notice.Kind}}" role="alert">{{.Notice.Message}}</p>{{end}}
    <div class="table-wrap">
      <table>
        <thead><tr><th>Worktree</th><th>Service</th><th>State</th><th>Endpoints</th><th>Actions</th></tr></thead>
        <tbody>
          {{if .Worktrees}}
            {{range $worktree := .Worktrees}}
              {{range .Services}}
                <tr>
                  <td><span class="worktree">{{$worktree.Repo}}</span><br><span class="muted">{{$worktree.Worktree}}</span></td>
                  <td><span class="service">{{.Type}}</span><br><span class="muted">{{.Container}}</span></td>
                  <td><span class="state {{.State}}">{{.State}}</span></td>
                  <td><div class="endpoints">{{range endpoints .Endpoints}}{{if httpEndpoint .URL}}<a href="{{.URL}}" target="_blank" rel="noreferrer">{{.Role}}: {{.URL}}</a>{{else}}<span>{{.Role}}: {{.URL}}</span>{{end}}{{else}}<span class="muted">No published endpoints</span>{{end}}</div></td>
                  <td><div class="actions">
                    <form action="/ui/status" method="get" hx-get="/ui/status" hx-target="#{{detailID $worktree.Repo $worktree.Worktree .Type}}" hx-swap="innerHTML">
                      <input type="hidden" name="repo" value="{{$worktree.Repo}}"><input type="hidden" name="worktree" value="{{$worktree.Worktree}}"><input type="hidden" name="service" value="{{.Type}}"><button class="secondary" type="submit">Status</button>
                    </form>
                    <form action="/ui/logs" method="get" hx-get="/ui/logs" hx-target="#{{detailID $worktree.Repo $worktree.Worktree .Type}}" hx-swap="innerHTML">
                      <input type="hidden" name="repo" value="{{$worktree.Repo}}"><input type="hidden" name="worktree" value="{{$worktree.Worktree}}"><input type="hidden" name="service" value="{{.Type}}"><button class="secondary" type="submit">Logs</button>
                    </form>
                    <form action="/ui/reload" method="post" hx-post="/ui/reload" hx-target="#dashboard" hx-swap="outerHTML" hx-confirm="This destroys the service volume. Continue?">
                      <input type="hidden" name="repo" value="{{$worktree.Repo}}"><input type="hidden" name="worktree" value="{{$worktree.Worktree}}"><input type="hidden" name="services" value="{{.Type}}"><button class="secondary" type="submit">Reload</button>
                    </form>
                    <form action="/ui/down" method="post" hx-post="/ui/down" hx-target="#dashboard" hx-swap="outerHTML" hx-confirm="This destroys the service volume. Continue?">
                      <input type="hidden" name="repo" value="{{$worktree.Repo}}"><input type="hidden" name="worktree" value="{{$worktree.Worktree}}"><input type="hidden" name="services" value="{{.Type}}"><button class="danger" type="submit">Down</button>
                    </form>
                  </div></td>
                </tr>
                <tr id="{{detailID $worktree.Repo $worktree.Worktree .Type}}" class="details"></tr>
              {{end}}
            {{end}}
          {{else}}
            <tr><td class="empty" colspan="5">No managed worktrees.</td></tr>
          {{end}}
        </tbody>
      </table>
    </div>
  </section>
</main>
{{end}}
{{define "status"}}
{{range .Services}}<td colspan="5"><strong>{{.Type}} status: {{.State}}</strong><div class="endpoints">{{range endpoints .Endpoints}}{{if httpEndpoint .URL}}<a href="{{.URL}}" target="_blank" rel="noreferrer">{{.Role}}: {{.URL}}</a>{{else}}<span>{{.Role}}: {{.URL}}</span>{{end}}{{else}}<span class="muted">No published endpoints</span>{{end}}</div></td>{{end}}
{{end}}
{{define "logs"}}
{{range .Services}}<td colspan="5"><strong>{{.Type}} logs (tail {{$.Tail}})</strong><pre>{{if .Logs}}{{.Logs}}{{else}}No logs available for {{.State}} service.{{end}}</pre></td>{{end}}
{{end}}
{{define "error"}}<main id="dashboard" hx-get="/ui/dashboard" hx-trigger="every 15s" hx-swap="outerHTML"><section class="card"><p class="notice error" role="alert">{{.}}</p></section></main>{{end}}
{{define "detail-error"}}<td colspan="5"><p class="notice error" role="alert">{{.}}</p></td>{{end}}`))

func sortedUIEndpoints(endpoints map[EndpointRole]EndpointInfo) []uiEndpoint {
	out := make([]uiEndpoint, 0, len(endpoints))
	for role, endpoint := range endpoints {
		out = append(out, uiEndpoint{Role: role, URL: endpoint.URL})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Role < out[j].Role })
	return out
}

func isHTTPEndpoint(raw string) bool {
	return strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")
}

func uiDetailID(repo, worktree, service string) string {
	sum := sha256.Sum256([]byte(repo + "\x00" + worktree + "\x00" + service))
	return "detail-" + hex.EncodeToString(sum[:8])
}

func executeUITemplate(name string, data any) ([]byte, error) {
	var out bytes.Buffer
	if err := uiTemplates.ExecuteTemplate(&out, name, data); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (s *Server) renderDashboard(ctx context.Context, notice uiNotice) ([]byte, error) {
	items, err := s.listContainers(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Repo != items[j].Repo {
			return items[i].Repo < items[j].Repo
		}
		if items[i].Worktree != items[j].Worktree {
			return items[i].Worktree < items[j].Worktree
		}
		return items[i].Type < items[j].Type
	})
	data := uiDashboardData{Notice: notice}
	for _, item := range items {
		if len(data.Worktrees) == 0 || data.Worktrees[len(data.Worktrees)-1].Repo != item.Repo || data.Worktrees[len(data.Worktrees)-1].Worktree != item.Worktree {
			data.Worktrees = append(data.Worktrees, uiWorktree{Repo: item.Repo, Worktree: item.Worktree})
		}
		last := len(data.Worktrees) - 1
		data.Worktrees[last].Services = append(data.Worktrees[last].Services, item)
	}
	return executeUITemplate("dashboard", data)
}

func writeUIHTML(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeUIError(w http.ResponseWriter, status int, err error) {
	body, templateErr := executeUITemplate("error", err.Error())
	if templateErr != nil {
		http.Error(w, "render UI error: "+templateErr.Error(), http.StatusInternalServerError)
		return
	}
	writeUIHTML(w, status, body)
}

func writeUIDetailError(w http.ResponseWriter, status int, err error) {
	body, templateErr := executeUITemplate("detail-error", err.Error())
	if templateErr != nil {
		http.Error(w, "render UI error: "+templateErr.Error(), http.StatusInternalServerError)
		return
	}
	writeUIHTML(w, status, body)
}

func (s *Server) writeDashboard(w http.ResponseWriter, r *http.Request, notice uiNotice) {
	body, err := s.renderDashboard(r.Context(), notice)
	if err != nil {
		writeUIError(w, http.StatusInternalServerError, fmt.Errorf("load managed worktrees: %w", err))
		return
	}
	writeUIHTML(w, http.StatusOK, body)
}

func uiRequestFromForm(r *http.Request) (repo, worktree, image string, services []string, err error) {
	if err := r.ParseForm(); err != nil {
		return "", "", "", nil, fmt.Errorf("parse form: %w", err)
	}
	repo = strings.TrimSpace(r.FormValue("repo"))
	worktree = strings.TrimSpace(r.FormValue("worktree"))
	image = strings.TrimSpace(r.FormValue("image"))
	if repo == "" || worktree == "" {
		return "", "", "", nil, errors.New("repo and worktree are required")
	}
	for _, value := range r.Form["services"] {
		services = append(services, parseServicesCSV(value)...)
	}
	return repo, worktree, image, services, nil
}

func uiQueryIdentity(r *http.Request) (repo, worktree, service string, err error) {
	repo = strings.TrimSpace(r.URL.Query().Get("repo"))
	worktree = strings.TrimSpace(r.URL.Query().Get("worktree"))
	service = strings.TrimSpace(r.URL.Query().Get("service"))
	if repo == "" || worktree == "" || service == "" {
		return "", "", "", errors.New("repo, worktree, and service are required")
	}
	return repo, worktree, service, nil
}

func (s *Server) handleHTMX(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	_, _ = w.Write(htmxJS)
}

func (s *Server) handleUIDashboard(w http.ResponseWriter, r *http.Request) {
	s.writeDashboard(w, r, uiNotice{})
}

func (s *Server) handleUIUp(w http.ResponseWriter, r *http.Request) {
	repo, worktree, image, services, err := uiRequestFromForm(r)
	if err != nil {
		writeUIError(w, http.StatusBadRequest, err)
		return
	}
	_, err = s.opUp(r.Context(), UpRequest{Repo: repo, Worktree: worktree, Services: services, Image: image})
	if err != nil {
		s.writeDashboard(w, r, uiNotice{Kind: "error", Message: err.Error()})
		return
	}
	message := fmt.Sprintf("Started %s/%s", repo, worktree)
	if image != "" {
		message += " using " + image
	}
	s.writeDashboard(w, r, uiNotice{Kind: "success", Message: message})
}

func (s *Server) handleUIDown(w http.ResponseWriter, r *http.Request) {
	repo, worktree, _, services, err := uiRequestFromForm(r)
	if err != nil {
		writeUIError(w, http.StatusBadRequest, err)
		return
	}
	_, err = s.opDown(r.Context(), DownRequest{Repo: repo, Worktree: worktree, Services: services})
	if err != nil {
		s.writeDashboard(w, r, uiNotice{Kind: "error", Message: err.Error()})
		return
	}
	s.writeDashboard(w, r, uiNotice{Kind: "success", Message: fmt.Sprintf("Stopped and removed %s/%s", repo, worktree)})
}

func (s *Server) handleUIReload(w http.ResponseWriter, r *http.Request) {
	repo, worktree, image, services, err := uiRequestFromForm(r)
	if err != nil {
		writeUIError(w, http.StatusBadRequest, err)
		return
	}
	_, err = s.opReload(r.Context(), ReloadRequest{Repo: repo, Worktree: worktree, Services: services, Image: image})
	if err != nil {
		s.writeDashboard(w, r, uiNotice{Kind: "error", Message: err.Error()})
		return
	}
	message := fmt.Sprintf("Reloaded %s/%s", repo, worktree)
	if image != "" {
		message += " using " + image
	}
	s.writeDashboard(w, r, uiNotice{Kind: "success", Message: message})
}

func (s *Server) handleUIStatus(w http.ResponseWriter, r *http.Request) {
	repo, worktree, service, err := uiQueryIdentity(r)
	if err != nil {
		writeUIDetailError(w, http.StatusBadRequest, err)
		return
	}
	response, err := s.opStatus(r.Context(), repo, worktree, service)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errUnknownService) {
			status = http.StatusBadRequest
		}
		writeUIDetailError(w, status, err)
		return
	}
	body, err := executeUITemplate("status", response)
	if err != nil {
		writeUIDetailError(w, http.StatusInternalServerError, err)
		return
	}
	writeUIHTML(w, http.StatusOK, body)
}

func (s *Server) handleUILogs(w http.ResponseWriter, r *http.Request) {
	repo, worktree, service, err := uiQueryIdentity(r)
	if err != nil {
		writeUIDetailError(w, http.StatusBadRequest, err)
		return
	}
	tail, err := parseTailParam(r.URL.Query().Get("tail"))
	if err != nil {
		writeUIDetailError(w, http.StatusBadRequest, err)
		return
	}
	response, err := s.opLogs(r.Context(), repo, worktree, service, tail)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errUnknownService) {
			status = http.StatusBadRequest
		}
		writeUIDetailError(w, status, err)
		return
	}
	body, err := executeUITemplate("logs", response)
	if err != nil {
		writeUIDetailError(w, http.StatusInternalServerError, err)
		return
	}
	writeUIHTML(w, http.StatusOK, body)
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
	logsSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":     map[string]any{"type": "string", "description": "Repository name"},
			"worktree": map[string]any{"type": "string", "description": "Worktree name"},
			"service":  map[string]any{"type": "string", "description": "Optional single service type to filter to."},
			"tail": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("Number of trailing log lines per service (default %d, max %d).", defaultLogsTail, maxLogsTail),
			},
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
		{Name: "pgpool_reload", Description: "Tear down then re-create services for a worktree (destroys volumes). Defaults to all configured services.", InputSchema: upSchema},
		{Name: "pgpool_status", Description: "Report state of services for a worktree. Optionally filter to one service.", InputSchema: rwOptionalService},
		{Name: "pgpool_list", Description: "List all pgpool-managed containers on this host.", InputSchema: empty},
		{Name: "pgpool_logs", Description: "Tail container logs for one or all configured services in a worktree.", InputSchema: logsSchema},
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
	case "pgpool_reload":
		var req ReloadRequest
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return nil, fmt.Errorf("parse arguments: %w", err)
			}
		}
		return s.opReload(ctx, req)
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
	case "pgpool_logs":
		var req struct {
			Repo     string `json:"repo"`
			Worktree string `json:"worktree"`
			Service  string `json:"service"`
			Tail     int    `json:"tail"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return nil, fmt.Errorf("parse arguments: %w", err)
			}
		}
		tail := req.Tail
		if tail <= 0 {
			tail = defaultLogsTail
		}
		if tail > maxLogsTail {
			tail = maxLogsTail
		}
		return s.opLogs(ctx, req.Repo, req.Worktree, req.Service, tail)
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

func parsePgMaxConnections(value string) (int, error) {
	if value == "" {
		return defaultPostgresMaxConnections, nil
	}
	maxConnections, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("must be a positive integer, got %q", value)
	}
	if err := validatePgMaxConnections(maxConnections); err != nil {
		return 0, err
	}
	return maxConnections, nil
}

func validatePgMaxConnections(value int) error {
	if value < 1 {
		return fmt.Errorf("must be a positive integer, got %d", value)
	}
	return nil
}

// ---------- self-update ----------
//
// `pgpool update` reuses the shared selfupdate package, which drives the
// published install.sh. That script installs both pgpool and pgpoolcli, so the
// server refreshes its own on-disk binary (and the CLI alongside it). Replacing
// the file does not restart the running daemon - the command says so.

func runUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	version := fs.String("version", "", "release tag to install (default: latest)")
	dir := fs.String("dir", "", "install directory (default: directory of the running binary)")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("pgpool: %v", err)
	}
	if err := selfupdate.Run(*version, *dir, selfupdate.ExecRun); err != nil {
		log.Fatalf("pgpool update: %v", err)
	}
	fmt.Println("update complete - pgpool and pgpoolcli replaced in place")
	fmt.Println("restart the running pgpool server to apply the new binary")
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "update" {
		runUpdate(os.Args[2:])
		return
	}

	servicesCSV := getenv("PGPOOL_SERVICES", "postgres")
	pgMaxConnections, err := parsePgMaxConnections(os.Getenv("PGPOOL_PG_MAX_CONNECTIONS"))
	if err != nil {
		log.Fatalf("pgpool: PGPOOL_PG_MAX_CONNECTIONS: %v", err)
	}
	cfg := Config{
		ListenAddr:       getenv("PGPOOL_LISTEN", ":8080"),
		AdvertiseHost:    getenv("PGPOOL_ADVERTISE_HOST", "localhost"),
		PgImage:          getenv("PGPOOL_IMAGE", defaultPostgresImage),
		PgUser:           getenv("PGPOOL_PG_USER", "postgres"),
		PgPassword:       os.Getenv("PGPOOL_PG_PASSWORD"),
		PgDB:             getenv("PGPOOL_PG_DB", "postgres"),
		PgMaxConnections: pgMaxConnections,
		DockerBin:        getenv("PGPOOL_DOCKER_BIN", "docker"),
		StartupTimeout:   30 * time.Second,
	}

	flag.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP listen address")
	flag.StringVar(&cfg.AdvertiseHost, "advertise-host", cfg.AdvertiseHost, "hostname to include in connection URLs returned to clients")
	flag.StringVar(&cfg.PgImage, "image", cfg.PgImage, "default postgres image tag")
	flag.IntVar(&cfg.PgMaxConnections, "pg-max-connections", cfg.PgMaxConnections, "maximum postgres connections")
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

	if err := validatePgMaxConnections(cfg.PgMaxConnections); err != nil {
		log.Fatalf("pgpool: --pg-max-connections: %v", err)
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

	srv := newServer(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleIndex)
	mux.HandleFunc("GET /assets/htmx-2.0.10.min.js", srv.handleHTMX)
	mux.HandleFunc("GET /ui/dashboard", srv.handleUIDashboard)
	mux.HandleFunc("POST /ui/up", srv.handleUIUp)
	mux.HandleFunc("POST /ui/down", srv.handleUIDown)
	mux.HandleFunc("POST /ui/reload", srv.handleUIReload)
	mux.HandleFunc("GET /ui/status", srv.handleUIStatus)
	mux.HandleFunc("GET /ui/logs", srv.handleUILogs)
	mux.HandleFunc("GET /healthz", srv.handleHealth)
	mux.HandleFunc("POST /v1/up", srv.handleUp)
	mux.HandleFunc("POST /v1/down", srv.handleDown)
	mux.HandleFunc("POST /v1/reload", srv.handleReload)
	mux.HandleFunc("GET /v1/status", srv.handleStatus)
	mux.HandleFunc("GET /v1/logs", srv.handleLogs)
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
