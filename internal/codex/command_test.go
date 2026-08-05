package codex

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAppServerArgs(t *testing.T) {
	base := []string{"app-server", "--listen", "stdio://", "--disable", "plugins"}

	tests := []struct {
		name    string
		options Options
		want    []string
	}{
		{
			name:    "no config",
			options: Options{},
			want:    base,
		},
		{
			name: "config emitted in sorted key order",
			options: Options{Config: map[string]any{
				"model_provider":                   "litellm",
				"model_providers.litellm.base_url": "https://litellm.example/v1",
				"model_providers.litellm.wire_api": "responses",
			}},
			want: append(append([]string(nil), base...),
				"-c", `model_provider="litellm"`,
				"-c", `model_providers.litellm.base_url="https://litellm.example/v1"`,
				"-c", `model_providers.litellm.wire_api="responses"`,
			),
		},
		{
			name: "config before extra args",
			options: Options{
				Config:    map[string]any{"model_provider": "litellm"},
				ExtraArgs: []string{"--extra"},
			},
			want: append(append([]string(nil), base...),
				"-c", `model_provider="litellm"`, "--extra"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appServerArgs(tt.options)
			if strings.Join(got, " ") != strings.Join(tt.want, " ") {
				t.Fatalf("appServerArgs = %q, want %q", got, tt.want)
			}
			for _, arg := range got {
				if arg == "--profile" {
					t.Fatalf("appServerArgs emitted --profile: %q", got)
				}
			}
		})
	}
}

func TestCommandHelpers(t *testing.T) {
	path, err := resolveCodexPath("/bin/sh", []string{"PATH=/usr/bin:/bin"})
	if err != nil || path != "/bin/sh" {
		t.Fatalf("explicit path=%q err=%v", path, err)
	}

	env, err := buildMergedEnv(Options{
		CodexHome: "/home/codex",
		Env: map[string]string{
			"A":               "B",
			"CODEX_HOME":      "/hostile/codex",
			"HOME":            "/hostile/home",
			"XDG_CONFIG_HOME": "/hostile/xdg-config",
		},
		ProcessIsolation: &ProcessIsolation{UID: 1, GID: 2, BaseEnvironment: map[string]string{
			"HOME": "/managed/home", "PATH": "/usr/bin:/bin", "XDG_CONFIG_HOME": "/managed/xdg-config",
		}, StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !envContains(env, envCodexHome+"=/home/codex") || !envContains(env, "A=B") ||
		!envContains(env, "HOME=/managed/home") || !envContains(env, "XDG_CONFIG_HOME=/managed/xdg-config") ||
		envContains(env, envCodexHome+"=/hostile/codex") || envContains(env, "HOME=/hostile/home") ||
		envContains(env, "XDG_CONFIG_HOME=/hostile/xdg-config") {
		t.Fatalf("merged env missing values: %v", env)
	}
	updated := upsertEnv([]string{"A=1"}, "A", "2")
	if len(updated) != 1 || updated[0] != "A=2" {
		t.Fatalf("updated env = %v", updated)
	}
	added := upsertEnv([]string{"A=1"}, "B", "2")
	if !envContains(added, "B=2") {
		t.Fatalf("added env = %v", added)
	}

	if shellValue("x y") != `"x y"` || shellValue(true) != "true" || shellValue(false) != "false" || shellValue(7) != "7" {
		t.Fatalf("shellValue returned unexpected values")
	}
	if n, err := codexStderrWriter(nil).Write(nil); n != 0 || err != nil {
		t.Fatalf("empty stderr write returned n=%d err=%v", n, err)
	}
	if parseCodexVersion("codex-cli 0.129.0") != "0.129.0" || parseCodexVersion("codex 1.2.3-beta") != "1.2.3" || parseCodexVersion("none") != "" {
		t.Fatal("parseCodexVersion failed")
	}
	if compareSemver("0.144.2", minCodexVersion) <= 0 || compareSemver("0.144.0", minCodexVersion) >= 0 || compareSemver(minCodexVersion, minCodexVersion) != 0 {
		t.Fatal("compareSemver failed")
	}
	if _, err := resolveCodexPath("", []string{"PATH=/missing"}); err == nil {
		t.Fatal("resolveCodexPath without codex succeeded")
	}
}

func TestValidateCodexVersion(t *testing.T) {
	version, err := validateCodexVersionOutput("codex-cli 0.144.1")
	if err != nil {
		t.Fatalf("validateCodexVersion returned error: %v", err)
	}
	if version != "0.144.1" {
		t.Fatalf("validateCodexVersion returned %q, want 0.144.1", version)
	}

	if _, err := validateCodexVersionOutput("codex-cli 0.1.0"); err == nil {
		t.Fatal("old codex version succeeded")
	}

	if _, err := validateCodexVersionOutput("nope"); err == nil {
		t.Fatal("bad codex version output succeeded")
	}
}

func TestProcessCloserNil(t *testing.T) {
	if err := (&process{}).Close(); err != nil {
		t.Fatalf("nil process closer returned error: %v", err)
	}
	if err := killProcess(nil); err != nil {
		t.Fatalf("killProcess nil returned error: %v", err)
	}
}

func envContains(env []string, want string) bool {
	for _, entry := range env {
		if strings.TrimSpace(entry) == want {
			return true
		}
	}

	return false
}

func TestCommandLaunchAndProcessErrors(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	dir := testTraversableTempDir(t)
	codexPath := filepath.Join(dir, "codex")
	if err := os.WriteFile(codexPath, []byte("#!/bin/sh\necho codex-cli 0.144.1\n"), 0o700); err != nil {
		t.Fatalf("write codex: %v", err)
	}
	if resolved, err := resolveCodexPath("", []string{"PATH=" + dir}); err != nil || resolved != codexPath {
		t.Fatalf("resolve PATH = %q err=%v", resolved, err)
	}

	logPath := filepath.Join(dir, "args")
	script := filepath.Join(dir, "codex-app")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
if [ "$1" = "--version" ]; then
  echo codex-cli 0.144.1
  exit 0
fi
printf '%s\n' "$*" > "$TEST_ARGS"
echo app-server-stderr >&2
read line || exit 0
echo '{"jsonrpc":"2.0","id":1,"result":{}}'
read line || true
while read line; do :; done
`), 0o700); err != nil {
		t.Fatalf("write app script: %v", err)
	}
	t.Setenv("TEST_ARGS", logPath)
	var logBuffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var processSnapshotsQuiescent int
	client, err := NewAppServerClient(context.Background(), Options{
		CLIPath:          script,
		CodexHome:        testNativeOwnedTempDir(t),
		SupervisorRoot:   testTraversableTempDir(t),
		SupervisorParent: os.TempDir(),
		DarwinBestEffort: true,
		NativeVersion:    minCodexVersion,
		Config:           map[string]any{"feature.enabled": true, "name": "x y"},
		ExtraArgs:        []string{"--extra"},
		Logger:           logger,
		LaunchTimeout:    5 * time.Second,
		ProcessIsolation: testProcessIsolation(),
		NewProcessSnapshotObserver: func(context.Context) ProcessSnapshotObserver {
			return ProcessSnapshotObserver{Quiescent: func(context.Context) { processSnapshotsQuiescent++ }}
		},
	})
	if err != nil {
		t.Fatalf("NewAppServerClient with config returned error: %v", err)
	}
	_ = client.Close(context.Background())
	if processSnapshotsQuiescent != 1 {
		t.Fatalf("process snapshot quiescence callbacks = %d, want 1", processSnapshotsQuiescent)
	}
	rawArgs, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	args := string(rawArgs)
	if !strings.Contains(args, "-c feature.enabled=true") || !strings.Contains(args, "-c name=\"x y\"") || !strings.Contains(args, "--extra") {
		t.Fatalf("args = %q", args)
	}
	if !strings.Contains(logBuffer.String(), "app-server-stderr") {
		t.Fatalf("stderr log = %q", logBuffer.String())
	}
}

func TestCommandLaunchAppServerErrors(t *testing.T) {
	if parseCodexVersion("codex 1.2.3+meta") != "1.2.3" {
		t.Fatal("parseCodexVersion did not trim build metadata")
	}
	origExec := execCommandContext
	t.Cleanup(func() { execCommandContext = origExec })
	versionCmd := func(context.Context, string, ...string) *exec.Cmd {
		return exec.Command("/bin/sh", "-c", "echo codex-cli 0.144.1")
	}
	execCommandContext = func(ctx context.Context, path string, args ...string) *exec.Cmd {
		if len(args) == 1 && args[0] == codexVersionArgument {
			return versionCmd(ctx, path, args...)
		}
		cmd := exec.Command("/bin/sh", "-c", "cat")
		cmd.Stdin = strings.NewReader("")

		return cmd
	}
	shell := sleepCommandPath(t)
	if _, _, _, err := launchAppServer(context.Background(), context.Background(), Options{
		CLIPath: shell, NativeVersion: minCodexVersion, skipSupervisor: true, ProcessIsolation: testProcessIsolation(),
	}); err == nil {
		t.Fatal("launchAppServer ignored StdinPipe error")
	}
	execCommandContext = func(ctx context.Context, path string, args ...string) *exec.Cmd {
		if len(args) == 1 && args[0] == codexVersionArgument {
			return versionCmd(ctx, path, args...)
		}
		cmd := exec.Command("/bin/sh", "-c", "cat")
		cmd.Stdout = io.Discard

		return cmd
	}
	if _, _, _, err := launchAppServer(context.Background(), context.Background(), Options{
		CLIPath: shell, NativeVersion: minCodexVersion, skipSupervisor: true, ProcessIsolation: testProcessIsolation(),
	}); err == nil {
		t.Fatal("launchAppServer ignored StdoutPipe error")
	}
	execCommandContext = func(ctx context.Context, path string, args ...string) *exec.Cmd {
		if len(args) == 1 && args[0] == codexVersionArgument {
			return versionCmd(ctx, path, args...)
		}

		return exec.Command(filepath.Join(t.TempDir(), "missing"))
	}
	if _, _, _, err := launchAppServer(context.Background(), context.Background(), Options{
		CLIPath: shell, NativeVersion: minCodexVersion, skipSupervisor: true, ProcessIsolation: testProcessIsolation(),
	}); err == nil {
		t.Fatal("launchAppServer ignored start error")
	}
	execCommandContext = origExec
	if _, _, _, err := launchAppServer(context.Background(), context.Background(), Options{
		CLIPath: shell, NativeVersion: "0.1.0", skipSupervisor: true, ProcessIsolation: testProcessIsolation(),
	}); err == nil {
		t.Fatal("launchAppServer ignored version error")
	}
	if _, _, _, err := launchAppServer(context.Background(), context.Background(), Options{
		CLIPath: shell, NativeVersion: minCodexVersion, ProcessIsolation: testProcessIsolation(),
	}); err == nil {
		t.Fatal("launchAppServer ignored home-lock configuration error")
	}
	originalExecutable := supervisorExecutable
	supervisorExecutable = func() (string, error) { return "", errors.New("no embedded supervisor") }
	if _, _, _, err := launchAppServer(context.Background(), context.Background(), Options{
		CLIPath: shell, NativeVersion: minCodexVersion, SupervisorParent: os.TempDir(),
		WritableHome: t.TempDir(), SupervisorRoot: t.TempDir(), ProcessIsolation: testProcessIsolation(),
	}); err == nil {
		t.Fatal("launchAppServer ignored supervisor configuration error")
	}
	supervisorExecutable = originalExecutable
	pathless := testProcessIsolation()
	delete(pathless.BaseEnvironment, "PATH")
	if _, _, _, err := launchAppServer(context.Background(), context.Background(), Options{
		NativeVersion: minCodexVersion, skipSupervisor: true, ProcessIsolation: pathless,
	}); err == nil {
		t.Fatal("launchAppServer ignored missing codex path")
	}
}

func sleepCommandPath(t *testing.T) string {
	t.Helper()

	return sleepCommand(t, "10").Path
}

func TestCommandWaitJoinsSupervisorCompletion(t *testing.T) {
	cmd := exec.Command("/usr/bin/false")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start failing command: %v", err)
	}
	waiter := newSupervisorWaiter(cmd, false)

	completion := filepath.Join(t.TempDir(), "complete")
	if err := writeSupervisorMarker(completion); err != nil {
		t.Fatalf("write supervisor completion: %v", err)
	}

	proc := &process{cmd: cmd, supervisor: &supervisorProof{completion: completion}, processWaiter: waiter}
	proc.beginWait()
	<-proc.waitDone
	if proc.waitErr == nil {
		t.Fatal("failing supervised command returned nil wait error")
	}
}

func TestCommandWaitRequiresCompletionAfterSuccessfulGuardianExit(t *testing.T) {
	cmd := exec.Command("/usr/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start successful command: %v", err)
	}
	waiter := newSupervisorWaiter(cmd, false)

	root := t.TempDir()
	started := filepath.Join(root, "started")
	if err := writeSupervisorMarker(started); err != nil {
		t.Fatalf("write supervisor start marker: %v", err)
	}

	proc := &process{cmd: cmd, supervisor: &supervisorProof{
		started:        started,
		completion:     filepath.Join(root, "missing-completion"),
		completionWait: 20 * time.Millisecond,
	}, processWaiter: waiter}
	proc.beginWait()
	<-proc.waitDone
	if !errors.Is(proc.waitErr, ErrProcessContainmentIncomplete) {
		t.Fatalf("successful guardian without completion error = %v", proc.waitErr)
	}
}

func TestCommandProcessKillBranches(t *testing.T) {
	exited := exec.Command("/bin/sh", "-c", "exit 0")
	if err := exited.Run(); err != nil {
		t.Fatalf("run exited cmd: %v", err)
	}
	if err := killProcess(exited); err != nil {
		t.Fatalf("kill exited process: %v", err)
	}
	running := exec.Command("/bin/sh", "-c", "sleep 10")
	runningWaiter, runningStartErr := startProcess(running)
	if runningStartErr != nil {
		t.Fatalf("start running process: %v", runningStartErr)
	}
	if err := killProcess(running); err != nil {
		t.Fatalf("kill running process: %v", err)
	}
	runningWaiter.start()
	<-runningWaiter.result()
	origGetPGID := getProcessGroupID
	origKillPID := killProcessID
	t.Cleanup(func() {
		getProcessGroupID = origGetPGID
		killProcessID = origKillPID
	})
	getProcessGroupID = func(int) (int, error) { return 0, errors.New("pgid failed") }
	fallback := exec.Command("/bin/sh", "-c", "sleep 10")
	fallbackWaiter, fallbackStartErr := startProcess(fallback)
	if fallbackStartErr != nil {
		t.Fatalf("start fallback process: %v", fallbackStartErr)
	}
	if err := killProcess(fallback); err != nil {
		t.Fatalf("fallback process kill: %v", err)
	}
	fallbackWaiter.start()
	<-fallbackWaiter.result()
	getProcessGroupID = origGetPGID
	signaled := exec.Command("/bin/sh", "-c", "kill -KILL $$")
	signaledWaiter, signaledStartErr := startProcess(signaled)
	if signaledStartErr != nil {
		t.Fatalf("start signaled process: %v", signaledStartErr)
	}
	if err := (&process{cmd: signaled, processWaiter: signaledWaiter}).Close(); err != nil {
		t.Fatalf("signaled process close returned error: %v", err)
	}
	getProcessGroupID = func(int) (int, error) { return 123, nil }
	killProcessID = func(int, syscall.Signal) error { return errors.New("kill failed") }
	if err := killProcess(&exec.Cmd{Process: &os.Process{Pid: 123}}); err == nil {
		t.Fatal("killProcess ignored process-group kill error")
	}
	getProcessGroupID = origGetPGID
	killProcessID = origKillPID
	killFail := sleepCommand(t, "10")
	killFailWaiter, killFailStartErr := startProcess(killFail)
	if killFailStartErr != nil {
		t.Fatalf("start kill-fail process: %v", killFailStartErr)
	}
	getProcessGroupID = func(int) (int, error) { return 123, nil }
	killProcessID = func(int, syscall.Signal) error { return errors.New("kill failed") }
	origGrace := processCloseGrace
	processCloseGrace = 0
	if err := (&process{cmd: killFail, processWaiter: killFailWaiter}).Close(); err == nil {
		t.Fatal("processCloser ignored kill error")
	}
	getProcessGroupID = origGetPGID
	killProcessID = origKillPID
	_ = killProcess(killFail)
	processCloseGrace = origGrace

	finalKillFail := sleepCommand(t, "10")
	finalKillFailWaiter, finalKillFailStartErr := startProcess(finalKillFail)
	if finalKillFailStartErr != nil {
		t.Fatalf("start final-kill-fail process: %v", finalKillFailStartErr)
	}
	signalCount := 0
	getProcessGroupID = func(int) (int, error) { return 123, nil }
	killProcessID = func(int, syscall.Signal) error {
		signalCount++
		if signalCount == 1 {
			return nil
		}

		return errors.New("final kill failed")
	}
	processCloseGrace = 0
	if err := (&process{cmd: finalKillFail, processWaiter: finalKillFailWaiter}).Close(); err == nil {
		t.Fatal("processCloser ignored final kill error")
	}
	getProcessGroupID = origGetPGID
	killProcessID = origKillPID
	_ = killProcess(finalKillFail)
	processCloseGrace = origGrace

	ready := filepath.Join(t.TempDir(), "stubborn-ready")
	stubborn := exec.Command("/bin/sh", "-c", `trap '' TERM; : > "$STUBBORN_READY"; exec /bin/sleep 10`)
	stubborn.Env = append(os.Environ(), "STUBBORN_READY="+ready)
	stubbornWaiter, stubbornStartErr := startProcess(stubborn)
	if stubbornStartErr != nil {
		t.Fatalf("start stubborn process: %v", stubbornStartErr)
	}
	for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = killProcess(stubborn)
			stubbornWaiter.start()
			<-stubbornWaiter.result()
			t.Fatal("stubborn process did not become ready")
		}
	}
	processCloseGrace = 0
	t.Cleanup(func() { processCloseGrace = origGrace })
	if err := (&process{cmd: stubborn, processWaiter: stubbornWaiter}).Close(); err != nil {
		t.Fatalf("processCloser timeout kill returned error: %v", err)
	}
	processCloseGrace = origGrace
}

func TestProcessCloserSendsTermBeforeKill(t *testing.T) {
	origGrace := processCloseGrace
	processCloseGrace = 200 * time.Millisecond
	t.Cleanup(func() { processCloseGrace = origGrace })

	dir := t.TempDir()
	marker := filepath.Join(dir, "term")
	ready := filepath.Join(dir, "ready")
	script := filepath.Join(dir, "codex-trap-term")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
trap 'printf term > "$TERM_MARK"; exit 0' TERM
printf ready > "$READY_MARK"
read release
`), 0o700); err != nil {
		t.Fatalf("write trap script: %v", err)
	}

	// The script waits for the signal in a blocking read, so it burns no CPU
	// and cannot outlive the pipe this process holds.
	holdRead, holdWrite, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatalf("open trap process stdin hold: %v", pipeErr)
	}

	cmd := exec.Command(script)
	cmd.Env = append(os.Environ(), "TERM_MARK="+marker, "READY_MARK="+ready)
	cmd.Stdin = holdRead
	waiter, startErr := startProcess(cmd)
	if startErr != nil {
		t.Fatalf("start trap process: %v", startErr)
	}
	t.Cleanup(func() {
		_ = holdWrite.Close()
		_ = holdRead.Close()

		if cmd.ProcessState == nil {
			_ = killProcess(cmd)
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("trap script never signalled readiness")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := (&process{cmd: cmd, processWaiter: waiter}).Close(); err != nil {
		t.Fatalf("close trap process: %v", err)
	}

	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read term marker: %v", err)
	}
	if string(data) != "term" {
		t.Fatalf("term marker = %q", string(data))
	}
}

func TestSupervisedProcessCloseHasLocalDeadline(t *testing.T) {
	originalWait := processSupervisorCloseWait
	processSupervisorCloseWait = 20 * time.Millisecond
	t.Cleanup(func() { processSupervisorCloseWait = originalWait })

	proc := &process{
		cmd:        &exec.Cmd{Process: &os.Process{Pid: 123}},
		supervisor: &supervisorProof{},
		waitDone:   make(chan struct{}),
	}
	proc.waitOnce.Do(func() {})

	started := time.Now()
	err := proc.Close()
	if !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("supervised close timeout = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("supervised close timeout elapsed = %v", elapsed)
	}
}

func sleepCommand(t *testing.T, seconds string) *exec.Cmd {
	t.Helper()

	for _, path := range []string{"/bin/sleep", "/usr/bin/sleep"} {
		if _, err := os.Stat(path); err == nil {
			return exec.Command(path, seconds)
		}
	}
	if path, err := exec.LookPath("sleep"); err == nil {
		return exec.Command(path, seconds)
	}
	t.Fatal("find sleep")

	return nil
}
