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
	t.Cleanup(func() {
		versionSupervisorCommand = originalSupervisor
		versionStartProcess = originalStart
	})

	t.Setenv("PATH", "")
	if _, err := ProbeVersion(context.Background(), VersionProbeOptions{}); err == nil {
		t.Fatal("missing CLI version probe succeeded")
	}

	versionSupervisorCommand = func(context.Context, supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		return nil, nil, errors.New("supervisor failed")
	}
	if _, err := ProbeVersion(context.Background(), VersionProbeOptions{CLIPath: "/usr/bin/true"}); err == nil || !strings.Contains(err.Error(), "prepare") {
		t.Fatalf("supervisor error = %v", err)
	}

	versionSupervisorCommand = func(_ context.Context, config supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		if config.LifecycleKind != lifecycleDiscovery || !config.FramedInput {
			t.Fatalf("version supervisor config = %#v", config)
		}

		return exec.Command("/usr/bin/true"), &supervisorProof{}, nil
	}
	versionStartProcess = func(*exec.Cmd) error { return errors.New("start failed") }
	if _, err := ProbeVersion(context.Background(), VersionProbeOptions{CLIPath: "/usr/bin/true"}); err == nil || !strings.Contains(err.Error(), "start") {
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
			_, err := ProbeVersion(context.Background(), VersionProbeOptions{CLIPath: "/usr/bin/true"})
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
	if _, err := ProbeVersion(context.Background(), VersionProbeOptions{CLIPath: "/usr/bin/true"}); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("containment error = %v", err)
	}
}

func TestProbeVersionSuccessAndValidation(t *testing.T) {
	originalSupervisor := versionSupervisorCommand
	t.Cleanup(func() { versionSupervisorCommand = originalSupervisor })
	versionSupervisorCommand = func(context.Context, supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		return exec.Command("/bin/sh", "-c", "echo codex-cli 0.144.1"), &supervisorProof{}, nil
	}
	version, err := ProbeVersion(context.Background(), VersionProbeOptions{CLIPath: "/usr/bin/true"})
	if err != nil || version != minCodexVersion {
		t.Fatalf("version = %q, err = %v", version, err)
	}

	versionSupervisorCommand = func(context.Context, supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		return exec.Command("/bin/sh", "-c", "echo invalid"), &supervisorProof{}, nil
	}
	if _, err := ProbeVersion(context.Background(), VersionProbeOptions{CLIPath: "/usr/bin/true"}); err == nil {
		t.Fatal("invalid version output succeeded")
	}
}
