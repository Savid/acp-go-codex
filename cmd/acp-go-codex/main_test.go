package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	codexacp "github.com/savid/acp-go-codex"
)

func TestRunPassesOptions(t *testing.T) {
	originalServe := serve
	originalAgentVersion := agentVersion
	t.Cleanup(func() {
		serve = originalServe
		agentVersion = originalAgentVersion
	})

	var got codexacp.Options
	serve = func(_ context.Context, _ io.Reader, _ io.Writer, opts ...codexacp.Option) error {
		for _, opt := range opts {
			opt(&got)
		}

		return nil
	}
	agentVersion = func() string { return "v9.8.7" }

	code := run(
		context.Background(),
		[]string{
			"-codex", "/bin/codex",
			"-codex-home", "/tmp/codex",
			"-model", "gpt-5.5",
			"-debug",
		},
		bytes.NewBuffer(nil),
		bytes.NewBuffer(nil),
		bytes.NewBuffer(nil),
	)

	if code != 0 {
		t.Fatalf("run code = %d, want 0", code)
	}
	if got.AgentVersion != "v9.8.7" {
		t.Fatalf("agent version = %q, want v9.8.7", got.AgentVersion)
	}
	if got.CodexPath != "/bin/codex" {
		t.Fatalf("codex path = %q", got.CodexPath)
	}
	if got.CodexHome != "/tmp/codex" {
		t.Fatalf("codex home = %q", got.CodexHome)
	}
	if got.DefaultModel != "gpt-5.5" {
		t.Fatalf("default model = %q", got.DefaultModel)
	}
	if got.Logger == nil {
		t.Fatal("logger was not set")
	}
	if _, ok := any(got.Logger).(*slog.Logger); !ok {
		t.Fatalf("logger type = %T", got.Logger)
	}
}

func TestRunErrorPathsAndVersion(t *testing.T) {
	originalServe := serve
	originalCLI := runCodexCLICommand
	t.Cleanup(func() {
		serve = originalServe
		runCodexCLICommand = originalCLI
	})

	serve = func(context.Context, io.Reader, io.Writer, ...codexacp.Option) error {
		return context.Canceled
	}
	if code := run(context.Background(), []string{"-bad"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); code != 2 {
		t.Fatalf("bad flags code = %d", code)
	}

	serve = func(context.Context, io.Reader, io.Writer, ...codexacp.Option) error {
		return assertError("serve failed")
	}
	var stderr bytes.Buffer
	if code := run(context.Background(), nil, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr); code != 1 || !strings.Contains(stderr.String(), "serve failed") {
		t.Fatalf("serve error code/stderr = %d %q", code, stderr.String())
	}

	runCodexCLICommand = func(context.Context, string, string, string, bool, io.Reader, io.Writer, io.Writer) error {
		return assertError("cli failed")
	}
	stderr.Reset()
	if code := run(context.Background(), []string{"-cli", "login"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr); code != 1 || !strings.Contains(stderr.String(), "cli failed") {
		t.Fatalf("cli error code/stderr = %d %q", code, stderr.String())
	}

	if version() == "" {
		t.Fatal("version returned empty string")
	}
}

func TestRunSuccessBranches(t *testing.T) {
	originalCLI := runCodexCLICommand
	originalServe := serve
	t.Cleanup(func() {
		runCodexCLICommand = originalCLI
		serve = originalServe
	})
	runCodexCLICommand = func(context.Context, string, string, string, bool, io.Reader, io.Writer, io.Writer) error {
		return nil
	}
	if code := run(context.Background(), []string{"-cli", "login"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); code != 0 {
		t.Fatalf("successful cli run code = %d", code)
	}
	serve = func(ctx context.Context, _ io.Reader, _ io.Writer, _ ...codexacp.Option) error {
		proc, err := os.FindProcess(os.Getpid())
		if err != nil {
			return err
		}
		if err := proc.Signal(syscall.SIGHUP); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
	if code := run(context.Background(), nil, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); code != 128+int(syscall.SIGHUP) {
		t.Fatalf("signal-canceled run code = %d", code)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("token"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	t.Setenv(codexacp.MCPProxyTokenFileEnv, tokenFile)
	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		_ = conn.Close()
		done <- nil
	}()
	if code := run(context.Background(), []string{"mcp-proxy", "-address", ln.Addr().String(), "-acp-id", "acp"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); code != 0 {
		t.Fatalf("successful mcp-proxy run code = %d", code)
	}
	if err := <-done; err != nil {
		t.Fatalf("mcp proxy accept: %v", err)
	}
}

func TestMainUsesRunAndExitOnlyOnFailure(t *testing.T) {
	originalServe := serve
	originalExit := exit
	originalArgs := os.Args
	t.Cleanup(func() {
		serve = originalServe
		exit = originalExit
		os.Args = originalArgs
	})

	serve = func(context.Context, io.Reader, io.Writer, ...codexacp.Option) error {
		return nil
	}
	exitCalled := false
	exit = func(code int) {
		exitCalled = true
		t.Fatalf("exit called with code %d", code)
	}
	os.Args = []string{"acp-go-codex"}
	main()
	if exitCalled {
		t.Fatal("main exited on success")
	}

	serve = func(context.Context, io.Reader, io.Writer, ...codexacp.Option) error {
		return assertError("serve failed")
	}
	gotCode := 0
	exit = func(code int) { gotCode = code }
	main()
	if gotCode != 1 {
		t.Fatalf("exit code = %d, want 1", gotCode)
	}
}

func TestRunMCPProxyReadsTokenAndRuns(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("token\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	t.Setenv(codexacp.MCPProxyTokenFileEnv, tokenFile)

	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		var hello map[string]any
		if err := json.NewDecoder(conn).Decode(&hello); err != nil {
			done <- err
			return
		}
		if hello["token"] != "token" || hello["acpId"] != "acp" {
			done <- assertError("bad hello")
			return
		}
		done <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = runMCPProxy(ctx, []string{"-address", ln.Addr().String(), "-acp-id", "acp"}, strings.NewReader(""), bytes.NewBuffer(nil), bytes.NewBuffer(nil))
	if err != nil {
		t.Fatalf("runMCPProxy returned error: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("proxy server error: %v", err)
	}
}

func TestVersionDefaultBranch(t *testing.T) {
	original := buildVersion
	t.Cleanup(func() { buildVersion = original })
	buildVersion = ""
	if version() != "dev" {
		t.Fatalf("empty build version returned %q", version())
	}
}

func TestRunCodexCLI(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "codex")
	logPath := filepath.Join(dir, "log")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$@:$CODEX_HOME\" > \"$TEST_LOG\"\n"), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	t.Setenv("TEST_LOG", logPath)

	if err := runCodexCLI(context.Background(), script, "/tmp/codex-home", "login", true, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); err != nil {
		t.Fatalf("runCodexCLI login returned error: %v", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(raw), "login --device-auth:/tmp/codex-home") {
		t.Fatalf("log = %q", string(raw))
	}
	if err := runCodexCLI(context.Background(), script, "", "logout", false, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); err != nil {
		t.Fatalf("runCodexCLI logout returned error: %v", err)
	}
	if err := runCodexCLI(context.Background(), script, "", "bad", false, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); err == nil {
		t.Fatal("unsupported CLI command succeeded")
	}

	fail := filepath.Join(dir, "codex-fail")
	if err := os.WriteFile(fail, []byte("#!/bin/sh\nexit 7\n"), 0o700); err != nil {
		t.Fatalf("write failing script: %v", err)
	}
	if err := runCodexCLI(context.Background(), fail, "", "login", false, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); err == nil || commandExitCode(err) != 7 {
		t.Fatalf("failing cli err=%v code=%d", err, commandExitCode(err))
	}
	pathDir := t.TempDir()
	pathScript := filepath.Join(pathDir, "codex")
	if err := os.WriteFile(pathScript, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write PATH codex: %v", err)
	}
	t.Setenv("PATH", pathDir)
	if err := runCodexCLI(context.Background(), "", "", "logout", false, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); err != nil {
		t.Fatalf("runCodexCLI default path returned error: %v", err)
	}
	if err := runCodexCLI(context.Background(), filepath.Join(pathDir, "missing"), "", "logout", false, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)); err == nil {
		t.Fatal("runCodexCLI accepted missing executable")
	}

	signalScript := filepath.Join(dir, "codex-signal")
	readyPath := filepath.Join(dir, "ready")
	signalPath := filepath.Join(dir, "signal")
	if err := os.WriteFile(signalScript, []byte(`#!/bin/sh
if [ "$1" = "logout" ]; then
  trap 'echo hup > "$TEST_SIGNAL_LOG"; exit 0' HUP
  echo ready > "$TEST_READY_LOG"
  while :; do sleep 1; done
fi
exit 2
`), 0o700); err != nil {
		t.Fatalf("write signal script: %v", err)
	}
	t.Setenv("TEST_READY_LOG", readyPath)
	t.Setenv("TEST_SIGNAL_LOG", signalPath)
	errCh := make(chan error, 1)
	go func() {
		errCh <- runCodexCLI(context.Background(), signalScript, "", "logout", false, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil))
	}()
	waitUntil(t, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	})
	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("find self: %v", err)
	}
	if err := proc.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("signal self: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("runCodexCLI signal forwarding returned error: %v", err)
	}
	if raw, err := os.ReadFile(signalPath); err != nil || strings.TrimSpace(string(raw)) != "hup" {
		t.Fatalf("signal forwarding log=%q err=%v", raw, err)
	}
	if commandExitCode(assertError("plain")) != 1 {
		t.Fatal("plain command error did not map to exit code 1")
	}
	signals := make(chan os.Signal, 1)
	if pendingSignal(signals) != nil {
		t.Fatal("empty signal channel returned a signal")
	}
	signals <- os.Interrupt
	if pendingSignal(signals) != os.Interrupt {
		t.Fatal("pendingSignal missed interrupt")
	}
	if signalCode(os.Interrupt) <= 0 || signalCode(testSignal("custom")) != 1 {
		t.Fatal("signalCode returned unexpected value")
	}
	cmd := exec.Command("/bin/sh", "-c", "kill -TERM $$")
	err = cmd.Run()
	if err != nil && commandExitCode(err) <= 0 {
		t.Fatalf("signal exit did not map to exit code: %v", err)
	}
	exitCmd := exec.Command("/bin/sh", "-c", "exit 3")
	if err := exitCmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && signalExitCode(exitErr) != 0 {
			t.Fatalf("non-signal exit mapped to signal code")
		}
	}
}

func TestRunMCPProxyValidation(t *testing.T) {
	var stderr bytes.Buffer
	if err := runMCPProxy(context.Background(), []string{"-bad"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr); err == nil {
		t.Fatal("runMCPProxy accepted bad flag")
	}
	if err := runMCPProxy(context.Background(), nil, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr); err == nil {
		t.Fatal("runMCPProxy without address succeeded")
	}
	if err := runMCPProxy(context.Background(), []string{"-address", "127.0.0.1:1"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr); err == nil {
		t.Fatal("runMCPProxy without acp id succeeded")
	}
	if err := runMCPProxy(context.Background(), []string{"-address", "127.0.0.1:1", "-acp-id", "mcp"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr); err == nil {
		t.Fatal("runMCPProxy without token env succeeded")
	}
	t.Setenv(codexacp.MCPProxyTokenFileEnv, filepath.Join(t.TempDir(), "missing"))
	if err := runMCPProxy(context.Background(), []string{"-address", "127.0.0.1:1", "-acp-id", "mcp"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr); err == nil {
		t.Fatal("runMCPProxy accepted missing token file")
	}
	if code := run(context.Background(), []string{"mcp-proxy"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr); code != 1 {
		t.Fatalf("run mcp-proxy validation code = %d", code)
	}
}

type assertError string

func (e assertError) Error() string { return string(e) }

type testSignal string

func (s testSignal) String() string { return string(s) }
func (s testSignal) Signal()        {}

func waitUntil(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met")
}
