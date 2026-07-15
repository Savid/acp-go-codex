//go:build linux || darwin || freebsd || openbsd

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/savid/acp-go-codex/internal/homelock"
)

func TestTerminalAuthContainsDescendantsAndHoldsHomeUntilQuiescence(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	ready := filepath.Join(root, "ready")
	pidPath := filepath.Join(root, "child.pid")
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
echo ready > "$AUTH_READY"
wait
`)
	t.Setenv("AUTH_READY", ready)
	t.Setenv("AUTH_CHILD_PID", pidPath)
	t.Setenv("AUTH_WRITES", writes)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	var commandStderr bytes.Buffer
	scratch := t.TempDir()
	go func() {
		errCh <- runCodexCLI(ctx, script, home, scratch, loginCommand, false, bytes.NewReader(nil), bytes.NewBuffer(nil), &commandStderr)
	}()

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
		t.Fatalf("auth descendant %d survived contained shutdown: %v", pid, probeErr)
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

func waitUntilWithFailure(t *testing.T, ready func() bool, failure func() string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
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
