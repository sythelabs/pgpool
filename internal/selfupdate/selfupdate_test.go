package selfupdate

import (
	"strings"
	"testing"
)

func TestInstallDirFor_ReturnsDirOfExecutable(t *testing.T) {
	if got := InstallDirFor("/usr/local/bin/pgpool"); got != "/usr/local/bin" {
		t.Errorf("InstallDirFor = %q, want /usr/local/bin", got)
	}
}

func TestBuildEnv_AddsInstallDirAndVersion(t *testing.T) {
	env := BuildEnv([]string{"PATH=/bin"}, "v1.2.3", "/opt/bin")
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "PATH=/bin") {
		t.Errorf("base env dropped: %v", env)
	}
	if !strings.Contains(joined, "INSTALL_DIR=/opt/bin") {
		t.Errorf("INSTALL_DIR missing: %v", env)
	}
	if !strings.Contains(joined, "PGPOOL_VERSION=v1.2.3") {
		t.Errorf("PGPOOL_VERSION missing: %v", env)
	}
}

func TestBuildEnv_OmitsVersionWhenEmpty(t *testing.T) {
	env := BuildEnv(nil, "", "/opt/bin")
	for _, e := range env {
		if strings.HasPrefix(e, "PGPOOL_VERSION=") {
			t.Errorf("PGPOOL_VERSION should be absent when version empty: %v", env)
		}
	}
	if !strings.Contains(strings.Join(env, "\n"), "INSTALL_DIR=/opt/bin") {
		t.Errorf("INSTALL_DIR missing: %v", env)
	}
}

// TestRun_InvokesInstallerWithEnv checks the orchestration wires the install
// dir, version pin, and installer command into the injected runner.
func TestRun_InvokesInstallerWithEnv(t *testing.T) {
	var gotEnv, gotArgs []string
	var gotName string
	run := func(env []string, name string, args ...string) error {
		gotEnv, gotName, gotArgs = env, name, args
		return nil
	}
	if err := Run("v9.9.9", "/opt/bin", run); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotName != "sh" {
		t.Errorf("runner name = %q, want sh", gotName)
	}
	joinedArgs := strings.Join(gotArgs, " ")
	if !strings.Contains(joinedArgs, "curl") || !strings.Contains(joinedArgs, "install.sh") {
		t.Errorf("installer command missing curl|install.sh: %v", gotArgs)
	}
	joinedEnv := strings.Join(gotEnv, "\n")
	if !strings.Contains(joinedEnv, "INSTALL_DIR=/opt/bin") || !strings.Contains(joinedEnv, "PGPOOL_VERSION=v9.9.9") {
		t.Errorf("env missing install dir/version: %v", gotEnv)
	}
}
