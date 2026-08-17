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

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	envRunKeystore    = "ACP_GO_CODEX_RUN_KEYSTORE"
	keystoreEnvFile   = "/run/acp-go-codex-keystore/env"
	keystoreRoundTrip = "/usr/local/bin/roundtrip.sh"
	keystoreProbePath = "/usr/local/bin/residence.test"

	// The session-bus address is what reaches the Secret Service, so it is the
	// configuration itself rather than a label for one. The probe reads it
	// directly; nothing else selects which half runs.
	envSessionBus = "DBUS_SESSION_BUS_ADDRESS"
)

// requireRunKeystore gates the tier on both env vars. Below them the tier is
// not selected and nothing in it runs.
func requireRunKeystore(t *testing.T) {
	t.Helper()

	if os.Getenv(envRunIntegration) != "1" || os.Getenv(envRunKeystore) != "1" {
		t.Skipf("set %s=1 and %s=1 to run the credential-residence tier", envRunIntegration, envRunKeystore)
	}
}

// requireKeystoreRuntime adds the container runtime the fixture needs. Above
// the gate the tier fails rather than skips, because a silently green residence
// suite is worse than a red one.
func requireKeystoreRuntime(t *testing.T) {
	t.Helper()

	requireRunKeystore(t)

	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("%s=1 requires a container runtime: %v", envRunKeystore, err)
	}
}

// TestKeystoreLinuxCredentialResidence runs the credential-residence matrix in
// both Linux configurations. The matrix itself lives beside the read path it
// exercises; this test builds the fixture, proves the service answers a real
// store/lookup round trip, and runs the matrix inside it twice — once with the
// session bus that reaches the Secret Service, and once without one, which is
// the keystore-absent configuration an ungated run would never pick. The macOS
// third of the matrix needs no container and runs from the same gate on a macOS
// runner.
func TestKeystoreLinuxCredentialResidence(t *testing.T) {
	requireKeystoreRuntime(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	container := startKeystoreFixture(ctx, t)
	probe := buildResidenceProbe(t)

	if err := container.CopyFileToContainer(ctx, probe, keystoreProbePath, 0o755); err != nil {
		t.Fatalf("copy residence probe: %v", err)
	}

	runResidenceMatrix(ctx, t, container, false)
	runResidenceMatrix(ctx, t, container, true)
}

// runResidenceMatrix runs the probe in one configuration. Withholding the bus
// address is the whole difference between the two: the daemon is still running,
// and the read path simply has no service to ask.
func runResidenceMatrix(ctx context.Context, t *testing.T, container testcontainers.Container, bus bool) {
	t.Helper()

	name, prelude := "keystore-absent", "unset "+envSessionBus+"; "
	if bus {
		name, prelude = "keystore-present", ". "+keystoreEnvFile+"; export "+envSessionBus+"; "
	}

	t.Run(name, func(t *testing.T) {
		// The raw exec stream is frame-multiplexed: each read carries an
		// eight-byte header, and a caller that reads it unframed reports the
		// header as part of the matrix output.
		code, output, err := container.Exec(ctx, []string{
			"/bin/sh", "-c",
			prelude + "export " + envRunIntegration + "=1 " + envRunKeystore + "=1; exec " +
				keystoreProbePath + " -test.v -test.run '^TestKeystoreResidenceMatrix$'",
		}, tcexec.Multiplexed())
		if err != nil {
			t.Fatalf("run residence matrix: %v", err)
		}

		logs, readErr := io.ReadAll(output)
		if readErr != nil {
			t.Fatalf("read residence output: %v", readErr)
		}

		t.Log(string(logs))

		if code != 0 {
			t.Fatalf("residence matrix exited %d", code)
		}

		// A configuration that skipped exits zero, which is the silent success
		// this tier exists to prevent.
		if !strings.Contains(string(logs), "--- PASS: TestKeystoreResidenceMatrix") {
			t.Fatalf("the residence matrix did not run in the %q configuration", name)
		}
	})
}

// startKeystoreFixture builds the tier's Linux fixture and returns it running.
func startKeystoreFixture(ctx context.Context, t *testing.T) testcontainers.Container {
	t.Helper()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context:    filepath.Join(".", "keystore"),
				Dockerfile: "Dockerfile",
				KeepImage:  true,
			},
			// Readiness is a store/lookup round trip executed in the container.
			// A log line and a bus-name check both report ready against a
			// service that answers no lookup.
			WaitingFor: wait.ForExec([]string{keystoreRoundTrip}).WithStartupTimeout(3 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start keystore fixture: %v", err)
	}

	t.Cleanup(func() {
		if err := container.Terminate(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("terminate keystore fixture: %v", err)
		}
	})

	return container
}

// buildResidenceProbe compiles the package that owns the read path for the
// fixture's platform. Both halves of this tier build only under the integration
// tag: a residence test compiled into an ungated `go test ./...` is the defect
// the split exists to prevent. GOWORK=off keeps a workspace in scope from
// resolving the probe against another module's requirements.
func buildResidenceProbe(t *testing.T) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "residence.test")

	command := exec.Command("go", "test", "-c", "-tags=integration", "-o", out, ".")
	command.Dir = ".."
	command.Env = append(os.Environ(), "GOWORK=off", "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")

	if buildOutput, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build residence probe: %v: %s", err, buildOutput)
	}

	return out
}
