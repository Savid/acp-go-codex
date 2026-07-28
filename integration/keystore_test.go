//go:build integration

package integration

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
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

func requireKeystoreTier(t *testing.T) {
	t.Helper()

	if os.Getenv(envRunIntegration) != "1" || os.Getenv(envRunKeystore) != "1" {
		t.Skipf("set %s=1 and %s=1 to run the credential-residence tier", envRunIntegration, envRunKeystore)
	}

	// The tier fails rather than skips once its gate is set: a silently green
	// residence suite is worse than a red one.
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("%s=1 requires a container runtime: %v", envRunKeystore, err)
	}
}

// TestKeystoreProviderAuthResidence runs the credential-residence matrix in
// both Linux configurations. The matrix itself lives beside the read path it
// exercises; this test builds the fixture, proves the service answers a real
// store/lookup round trip, and runs the matrix inside it twice — once with the
// session bus that reaches the Secret Service, and once without one, which is
// the keystore-absent configuration an ungated run would never pick. The macOS
// third of the matrix needs no container and runs from the same gate on a macOS
// runner.
func TestKeystoreProviderAuthResidence(t *testing.T) {
	requireKeystoreTier(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

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

	probe := buildResidenceProbe(t)

	if err := container.CopyFileToContainer(ctx, probe, keystoreProbePath, 0o755); err != nil {
		t.Fatalf("copy residence probe: %v", err)
	}

	// Withholding the bus address is the whole difference between the two
	// configurations: the daemon is still running, and the read path simply has
	// no service to ask.
	configurations := map[string]string{
		"keystore present": ". " + keystoreEnvFile + "; export " + envSessionBus + "; ",
		"keystore absent":  "unset " + envSessionBus + "; ",
	}

	for state, prelude := range configurations {
		t.Run(state, func(t *testing.T) {
			code, output, err := container.Exec(ctx, []string{
				"/bin/sh", "-c",
				prelude + "export " + envRunIntegration + "=1 " + envRunKeystore + "=1; exec " +
					keystoreProbePath + " -test.v -test.run '^TestKeystoreResidenceMatrix$'",
			})
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
		})
	}
}

// buildResidenceProbe compiles the package that owns the read path for the
// fixture's platform. Both halves of this tier build only under the integration
// tag: a residence test compiled into an ungated `go test ./...` is the defect
// the split exists to prevent.
func buildResidenceProbe(t *testing.T) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "residence.test")

	command := exec.Command("go", "test", "-c", "-tags=integration", "-o", out, ".")
	command.Dir = ".."
	command.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")

	if buildOutput, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build residence probe: %v: %s", err, buildOutput)
	}

	return out
}
