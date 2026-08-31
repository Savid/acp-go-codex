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
	browserCanaryUID     = 4242
	browserCanaryGID     = 4242
	browserCanaryNative  = "/usr/local/bin/codex"
	browserCanaryScratch = "/canary/scratch"
	browserCanaryHome    = "/home/canary"
	browserCanaryState   = "/var/lib/acp-go-codex-browser-canary"
)

var browserCanaryLaunchers = []string{"open", "xdg-open", "x-www-browser", "www-browser", "sensible-browser"}

// TestRealNativeBrowserContainment runs the current Codex login binary through
// the same account-command function used by the production CLI. The container
// entrypoint traces the full tree and proves its real browser attempt execed a
// production-generated no-op shim rather than one of the image's decoys.
func TestRealNativeBrowserContainment(t *testing.T) {
	if os.Getenv("ACP_GO_CODEX_BROWSER_CANARY") != "1" {
		t.Fatal("real-native browser canary was selected without its required execution gate")
	}
	if os.Getuid() != 0 {
		t.Fatal("the canary must start as root so production isolation can enter uid 4242")
	}
	if _, err := os.Stat(browserCanaryNative); err != nil {
		t.Fatalf("current native Codex binary is absent: %v", err)
	}

	if err := os.MkdirAll(browserCanaryScratch, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(browserCanaryScratch, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(browserCanaryHome, browserCanaryUID, browserCanaryGID); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(browserCanaryHome, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{
		browserCanaryHome + "/.cache",
		browserCanaryHome + "/.config",
		browserCanaryHome + "/.local/share",
		browserCanaryHome + "/.local/state",
		browserCanaryHome + "/.run",
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(dir, browserCanaryUID, browserCanaryGID); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(browserCanaryState, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(browserCanaryState, browserCanaryUID, browserCanaryGID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(browserCanaryState) })

	isolation := processIsolationConfig{
		UID:                 browserCanaryUID,
		GID:                 browserCanaryGID,
		StandaloneOwnerID:   "codex-browser-canary",
		StandaloneStateRoot: browserCanaryState,
		BaseEnvironment: map[string]string{
			"BROWSER":         "/canary/decoys/open",
			"HOME":            browserCanaryHome,
			"LOGNAME":         "canary",
			"PATH":            "/canary/decoys:/usr/local/bin:/usr/bin:/bin",
			"USER":            "canary",
			"XDG_CACHE_HOME":  browserCanaryHome + "/.cache",
			"XDG_CONFIG_HOME": browserCanaryHome + "/.config",
			"XDG_DATA_HOME":   browserCanaryHome + "/.local/share",
			"XDG_RUNTIME_DIR": browserCanaryHome + "/.run",
			"XDG_STATE_HOME":  browserCanaryHome + "/.local/state",
		},
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
			loginCommand, false, &isolation, stdinRead, stdoutWrite, stdoutWrite, signals)
	}()

	seenURL := false
	for !seenURL {
		select {
		case line := <-lines:
			seenURL = strings.HasPrefix(line, "https://auth.openai.com/oauth/authorize?")
			if seenURL {
				t.Log("observed current Codex browser authorization URL")
			} else {
				t.Log(line)
			}
		case runErr := <-errCh:
			t.Fatalf("Codex login exited before presenting its browser URL: %v", runErr)
		case <-ctx.Done():
			t.Fatalf("wait for Codex browser presentation: %v", ctx.Err())
		}
	}

	shim := requireLiveCodexBrowserShim(t)
	for _, name := range browserCanaryLaunchers {
		if _, err := os.Stat(filepath.Join(shim, name)); err != nil {
			t.Fatalf("production browser shim lacks %s: %v", name, err)
		}
	}
	eventually(t, 10*time.Second, func() bool {
		for _, name := range browserCanaryLaunchers {
			info, statErr := os.Stat(filepath.Join(shim, name))
			if statErr != nil {
				continue
			}
			stat, statOK := info.Sys().(*syscall.Stat_t)
			if statOK && stat.Atim.Nano() > stat.Mtim.Nano() {
				return true
			}
		}

		return false
	}, "native login did not execute a generated browser launcher")

	signals <- syscall.SIGTERM
	select {
	case runErr := <-errCh:
		if runErr == nil {
			t.Fatal("signalled Codex login unexpectedly returned success")
		}
	case <-ctx.Done():
		t.Fatalf("wait for contained Codex login shutdown: %v", ctx.Err())
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
	}, "production account login created no unique live browser shim")

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
