//go:build linux && browsercanary

package main

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	browserCanaryNative  = "/usr/local/bin/codex"
	browserCanaryScratch = "/canary/scratch"
	browserCanaryHome    = "/home/canary/.codex"
)

var browserCanaryLaunchers = []string{"open", "xdg-open", "x-www-browser", "www-browser", "sensible-browser"}

func TestRealNativeBrowserContainment(t *testing.T) {
	if os.Getenv("ACP_GO_CODEX_BROWSER_CANARY") != "1" {
		t.Fatal("real-native browser canary was selected without its execution gate")
	}
	if _, err := os.Stat(browserCanaryNative); err != nil {
		t.Fatalf("current native Codex binary is absent: %v", err)
	}
	if err := os.MkdirAll(browserCanaryHome, 0o700); err != nil {
		t.Fatal(err)
	}

	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutRead.Close()
	defer stdoutWrite.Close()

	lines := make(chan string, 32)
	readDone := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdoutRead)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		readDone <- scanner.Err()
	}()

	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdinRead.Close()
	defer stdinWrite.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	signals := make(chan os.Signal, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- runCodexCLIWithSignals(ctx, browserCanaryNative, browserCanaryHome, browserCanaryScratch,
			loginCommand, false, stdinRead, stdoutWrite, stdoutWrite, signals)
	}()

	for {
		select {
		case line := <-lines:
			if strings.HasPrefix(line, "https://auth.openai.com/oauth/authorize?") {
				goto presented
			}
			t.Log(line)
		case runErr := <-errCh:
			t.Fatalf("Codex login exited before presenting its browser URL: %v", runErr)
		case <-ctx.Done():
			t.Fatalf("wait for Codex browser presentation: %v", ctx.Err())
		}
	}

presented:
	shim := requireLiveCodexBrowserShim(t)
	for _, name := range browserCanaryLaunchers {
		if _, err := os.Stat(filepath.Join(shim, name)); err != nil {
			t.Fatalf("browser shim lacks %s: %v", name, err)
		}
	}

	signals <- syscall.SIGTERM
	select {
	case runErr := <-errCh:
		if runErr == nil {
			t.Fatal("signalled Codex login unexpectedly returned success")
		}
	case <-ctx.Done():
		t.Fatalf("wait for Codex login shutdown: %v", ctx.Err())
	}
	_ = stdoutWrite.Close()
	select {
	case scanErr := <-readDone:
		if scanErr != nil {
			t.Fatalf("read Codex login output: %v", scanErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Codex login output reader did not close")
	}

	eventually(t, 10*time.Second, func() bool {
		matches, globErr := filepath.Glob(filepath.Join(browserCanaryScratch, "acp-go-codex-browser-shim-*"))
		return globErr == nil && len(matches) == 0
	}, "account-command shutdown left its browser shim behind")
}

func requireLiveCodexBrowserShim(t *testing.T) string {
	t.Helper()

	var matches []string
	eventually(t, 10*time.Second, func() bool {
		var err error
		matches, err = filepath.Glob(filepath.Join(browserCanaryScratch, "acp-go-codex-browser-shim-*"))
		return err == nil && len(matches) == 1
	}, "account login created no unique live browser shim")

	return matches[0]
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal(message)
}
