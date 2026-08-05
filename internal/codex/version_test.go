package codex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProbeVersionFailureBranches(t *testing.T) {
	originalSupervisor := versionSupervisorCommand
	originalStart := versionStartProcess
	originalPipe := supervisorPipe
	t.Cleanup(func() {
		versionSupervisorCommand = originalSupervisor
		versionStartProcess = originalStart
		supervisorPipe = originalPipe
	})

	t.Setenv("PATH", "")
	if _, err := ProbeVersion(context.Background(), withTestVersionIsolation(VersionProbeOptions{})); err == nil {
		t.Fatal("missing CLI version probe succeeded")
	}

	versionSupervisorCommand = func(context.Context, supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		return nil, nil, errors.New("supervisor failed")
	}
	if _, err := ProbeVersion(context.Background(), withTestVersionIsolation(VersionProbeOptions{CLIPath: "/usr/bin/true"})); err == nil || !strings.Contains(err.Error(), "prepare") {
		t.Fatalf("supervisor error = %v", err)
	}

	versionSupervisorCommand = func(_ context.Context, config supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		if config.LifecycleKind != lifecycleDiscovery || !config.FramedInput {
			t.Fatalf("version supervisor config = %#v", config)
		}

		return exec.Command("/usr/bin/true"), &supervisorProof{}, nil
	}
	supervisorPipe = func() (*os.File, *os.File, error) { return nil, nil, errors.New("pipe exhausted") }
	if _, err := ProbeVersion(context.Background(), withTestVersionIsolation(VersionProbeOptions{CLIPath: "/usr/bin/true"})); err == nil || !strings.Contains(err.Error(), "control input") {
		t.Fatalf("control input error = %v", err)
	}

	supervisorPipe = originalPipe
	versionStartProcess = func(*exec.Cmd) (*supervisorWaiter, error) {
		return nil, errors.New("start failed")
	}
	if _, err := ProbeVersion(context.Background(), withTestVersionIsolation(VersionProbeOptions{CLIPath: "/usr/bin/true"})); err == nil || !strings.Contains(err.Error(), "start") {
		t.Fatalf("start error = %v", err)
	}

	versionStartProcess = startProcess
	for name, script := range map[string]string{
		"without stderr": "exit 7",
		"with stderr":    "echo probe-failed >&2; exit 7",
	} {
		t.Run(name, func(t *testing.T) {
			versionSupervisorCommand = func(context.Context, supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
				return exec.Command("/bin/sh", "-c", script), &supervisorProof{}, nil
			}
			_, err := ProbeVersion(context.Background(), withTestVersionIsolation(VersionProbeOptions{CLIPath: "/usr/bin/true"}))
			if err == nil || (name == "with stderr" && !strings.Contains(err.Error(), "probe-failed")) {
				t.Fatalf("wait error = %v", err)
			}
		})
	}

	started := filepath.Join(t.TempDir(), "started")
	if err := os.WriteFile(started, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	versionSupervisorCommand = func(context.Context, supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		return exec.Command("/usr/bin/true"), &supervisorProof{
			started: started, completion: filepath.Join(t.TempDir(), "missing"), completionWait: time.Millisecond,
		}, nil
	}
	if _, err := ProbeVersion(context.Background(), withTestVersionIsolation(VersionProbeOptions{CLIPath: "/usr/bin/true"})); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("containment error = %v", err)
	}
}

// TestProbeVersionHoldsSupervisorControlInputOpen pins the caller-liveness
// contract the guardian relies on: a hangup on its control input means the
// caller is gone, and the guardian then abandons agent identity acquisition.
func TestProbeVersionHoldsSupervisorControlInputOpen(t *testing.T) {
	originalSupervisor := versionSupervisorCommand
	originalStart := versionStartProcess
	t.Cleanup(func() {
		versionSupervisorCommand = originalSupervisor
		versionStartProcess = originalStart
	})

	versionSupervisorCommand = func(context.Context, supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		return exec.Command("/bin/sh", "-c", "echo codex-cli 0.144.1"), &supervisorProof{}, nil
	}
	versionStartProcess = func(cmd *exec.Cmd) (*supervisorWaiter, error) {
		control, ok := cmd.Stdin.(*os.File)
		if !ok {
			t.Fatalf("version probe control input = %T, want a file the supervisor can poll", cmd.Stdin)
		}
		if err := control.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}

		var probe [1]byte
		if _, err := control.Read(probe[:]); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("version probe control input was not held open: %v", err)
		}
		if err := control.SetReadDeadline(time.Time{}); err != nil {
			t.Fatal(err)
		}

		return startProcess(cmd)
	}

	version, err := ProbeVersion(context.Background(), withTestVersionIsolation(VersionProbeOptions{CLIPath: "/usr/bin/true"}))
	if err != nil || version != minCodexVersion {
		t.Fatalf("version = %q, err = %v", version, err)
	}
}

func TestProbeVersionSuccessAndValidation(t *testing.T) {
	originalSupervisor := versionSupervisorCommand
	t.Cleanup(func() { versionSupervisorCommand = originalSupervisor })
	versionSupervisorCommand = func(context.Context, supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		return exec.Command("/bin/sh", "-c", "echo codex-cli 0.144.1"), &supervisorProof{}, nil
	}
	version, err := ProbeVersion(context.Background(), withTestVersionIsolation(VersionProbeOptions{CLIPath: "/usr/bin/true"}))
	if err != nil || version != minCodexVersion {
		t.Fatalf("version = %q, err = %v", version, err)
	}

	versionSupervisorCommand = func(context.Context, supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		return exec.Command("/bin/sh", "-c", "echo invalid"), &supervisorProof{}, nil
	}
	if _, err := ProbeVersion(context.Background(), withTestVersionIsolation(VersionProbeOptions{CLIPath: "/usr/bin/true"})); err == nil {
		t.Fatal("invalid version output succeeded")
	}
}
