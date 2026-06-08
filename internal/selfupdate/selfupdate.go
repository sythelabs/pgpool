// Package selfupdate refreshes the installed pgpool binaries by reusing the
// published install.sh. That script resolves the latest release (or the pinned
// PGPOOL_VERSION), downloads the matching archive, and installs both pgpool and
// pgpoolcli - so a single update from either binary refreshes the whole
// install. Keeping this here means the server and CLI share one source of truth
// for download/extract/replace instead of duplicating it.
package selfupdate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// installerURL is the published installer this package drives.
const installerURL = "https://raw.githubusercontent.com/sythelabs/pgpool/main/install.sh"

// Runner runs name with args under env. Injected so Run is testable without
// touching the network; pass ExecRun in production.
type Runner func(env []string, name string, args ...string) error

// InstallDirFor returns the directory that holds the running binary, which is
// where the installer should drop the refreshed binaries. exePath is normally
// the symlink-resolved result of os.Executable().
func InstallDirFor(exePath string) string {
	return filepath.Dir(exePath)
}

// BuildEnv layers the install dir and optional version pin onto a base
// environment for the installer process. An empty version means "latest".
func BuildEnv(base []string, version, installDir string) []string {
	env := append([]string{}, base...)
	env = append(env, "INSTALL_DIR="+installDir)
	if version != "" {
		env = append(env, "PGPOOL_VERSION="+version)
	}
	return env
}

// Run resolves the install dir (when dir is empty) from the running binary,
// then invokes the installer through run.
func Run(version, dir string, run Runner) error {
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate running binary: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		dir = InstallDirFor(exe)
	}
	env := BuildEnv(os.Environ(), version, dir)
	return run(env, "sh", "-c", "curl -fsSL "+installerURL+" | sh")
}

// ExecRun is the production Runner. It inherits stdio so installer progress is
// visible and runs with the supplied environment.
func ExecRun(env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("installer failed (%s %s): %w", name, strings.Join(args, " "), err)
	}
	return nil
}
