//go:build linux || darwin || freebsd || openbsd

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/savid/acp-go-codex/internal/homelock"
)

func TestTerminalAuthContainsDescendantsAndHoldsHomeUntilQuiescence(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("requires a privileged two-principal fixture to clear supplementary groups")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	ready := filepath.Join(root, "ready")
	pidPath := filepath.Join(root, "child.pid")
	rootPIDPath := filepath.Join(root, "root.pid")
	writes := filepath.Join(root, "writes")
	script := filepath.Join(root, "codex")
	requireWriteFile(t, script, `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo codex-cli 0.144.1
  exit 0
fi
(
  while :; do
    printf x >> "$AUTH_WRITES"
    sleep 0.01
  done
) &
echo $! > "$AUTH_CHILD_PID"
echo $$ > "$AUTH_ROOT_PID"
echo ready > "$AUTH_READY"
wait
`)
	t.Setenv("AUTH_READY", ready)
	t.Setenv("AUTH_CHILD_PID", pidPath)
	t.Setenv("AUTH_ROOT_PID", rootPIDPath)
	t.Setenv("AUTH_WRITES", writes)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	runDone := make(chan struct{})
	var commandStderr bytes.Buffer
	scratch := t.TempDir()
	go func() {
		defer close(runDone)

		errCh <- runCodexCLI(ctx, script, home, scratch, loginCommand, false, true, bytes.NewReader(nil), bytes.NewBuffer(nil), &commandStderr)
	}()
	t.Cleanup(func() { reapAuthFixture(t, cancel, runDone, rootPIDPath, pidPath) })

	waitUntilWithFailure(t, func() bool {
		_, err := os.Stat(ready)

		return err == nil
	}, func() string { return commandStderr.String() })
	if _, err := homelock.Acquire(home); err == nil {
		t.Fatal("second claimant acquired home while terminal auth tree was live")
	}

	rawPID, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil {
		t.Fatalf("parse descendant pid: %v", err)
	}
	if probeErr := syscall.Kill(pid, 0); probeErr != nil {
		t.Fatalf("auth descendant was not live before cancellation: %v", probeErr)
	}

	cancel()
	if runErr := <-errCh; runErr == nil {
		t.Fatal("cancelled terminal auth command returned nil")
	}
	if probeErr := syscall.Kill(pid, 0); probeErr == nil || !errors.Is(probeErr, syscall.ESRCH) {
		if runtime.GOOS != "darwin" {
			t.Fatalf("auth descendant %d survived contained shutdown: %v", pid, probeErr)
		}
		// Darwin explicitly accepts only the original process group. A shell may
		// move background work out of that group, so the test reaps the accepted
		// residual risk rather than claiming authoritative containment.
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}

	before, err := os.Stat(writes)
	if err != nil {
		t.Fatalf("stat descendant writes: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	after, err := os.Stat(writes)
	if err != nil {
		t.Fatalf("restat descendant writes: %v", err)
	}
	if before.Size() != after.Size() {
		t.Fatalf("write-capable auth descendant remained active: size %d -> %d", before.Size(), after.Size())
	}

	lock, err := homelock.Acquire(home)
	if err != nil {
		t.Fatalf("home did not reacquire after auth tree quiescence: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("release home lock: %v", err)
	}
}

// reapAuthFixture cancels the terminal-auth command and, when the auth tree
// does not collapse with it, kills the fixture's process group and the
// write-capable descendant that leaves it. It runs on every exit path,
// including t.Fatal, so no fixture process survives the test.
func reapAuthFixture(t *testing.T, cancel context.CancelFunc, done <-chan struct{}, rootPIDPath string, descendantPIDPath string) {
	t.Helper()

	cancel()

	collapsed := awaitAuthTree(done)
	if !collapsed {
		if pid := fixturePID(rootPIDPath); pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}

	if pid := fixturePID(descendantPIDPath); pid > 0 && syscall.Kill(pid, 0) == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}

	if !collapsed && !awaitAuthTree(done) {
		t.Error("terminal auth fixture outlived the test")
	}
}

func awaitAuthTree(done <-chan struct{}) bool {
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()

	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func fixturePID(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}

	return pid
}

func waitUntilWithFailure(t *testing.T, ready func() bool, failure func() string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition was not met; stderr: %s", failure())
}

func requireWriteFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatalf("write helper: %v", err)
	}
}
