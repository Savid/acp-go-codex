//go:build integration

package integration

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

const (
	browserProbePath = "/usr/local/bin/browser-shim.test"

	// The two legs are opposites and each one has to be named: login must reach
	// the harness with every launcher shadowed, logout must reach it with the
	// environment untouched. A run that selected only the login leg would
	// report a pass while the logout leg was never executed.
	loginLegTest  = "TestLoginNeverExecsABrowserLauncher"
	logoutLegTest = "TestLogoutRunsWithoutABrowserShim"
)

// TestKeystoreContainerBrowserLauncherContainment runs the account-command
// browser legs on Linux, where `xdg-open`, `x-www-browser`, `www-browser`, and
// `sensible-browser` are the launchers a login would otherwise exec. macOS
// exercises only `open`, so every other name on that list is a claim no host in
// this family ever runs. The probe is the internal package's own test binary,
// built for the fixture's platform, so the code under test is the production
// launch path rather than a restatement of it.
func TestKeystoreContainerBrowserLauncherContainment(t *testing.T) {
	requireKeystoreTier(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	container := startLinuxFixture(t, ctx)
	probe := buildBrowserShimProbe(t)

	if err := container.CopyFileToContainer(ctx, probe, browserProbePath, 0o755); err != nil {
		t.Fatalf("copy browser shim probe: %v", err)
	}

	code, output, err := container.Exec(ctx, []string{
		browserProbePath, "-test.v",
		"-test.run", "^(" + loginLegTest + "|" + logoutLegTest + ")$",
	}, tcexec.Multiplexed())
	if err != nil {
		t.Fatalf("run browser shim probe: %v", err)
	}

	logs, readErr := io.ReadAll(output)
	if readErr != nil {
		t.Fatalf("read browser shim output: %v", readErr)
	}

	t.Log(string(logs))

	if code != 0 {
		t.Fatalf("browser shim probe exited %d", code)
	}

	for _, leg := range []string{loginLegTest, logoutLegTest} {
		if !strings.Contains(string(logs), "--- PASS: "+leg) {
			t.Fatalf("%s did not pass in the container", leg)
		}
	}

	if !strings.Contains(string(logs), "\nPASS\n") {
		t.Fatal("browser shim probe reported no overall pass")
	}
}

// buildBrowserShimProbe compiles the package that owns the launch path for the
// fixture's platform. The launcher legs carry no build tag beyond `!windows`,
// so the binary runs them natively against a real Linux PATH.
func buildBrowserShimProbe(t *testing.T) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "browser-shim.test")

	command := exec.Command("go", "test", "-c", "-o", out, "./internal/codex")
	command.Dir = ".."
	command.Env = append(os.Environ(), "GOWORK=off", "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")

	if buildOutput, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build browser shim probe: %v: %s", err, buildOutput)
	}

	return out
}
