//go:build !windows

package codex

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/savid/acp-go-codex/internal/homelock"
	"github.com/stretchr/testify/require"
)

type coverageNativeProcess struct {
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	stderr    io.ReadCloser
	result    NativeResult
	waitErr   error
	revokeErr error
	gate      chan struct{}
	once      sync.Once
}

func newCoverageNativeProcess() *coverageNativeProcess {
	return &coverageNativeProcess{
		stdin:  &authorityTestWriteCloser{},
		stdout: io.NopCloser(bytes.NewReader(nil)),
		stderr: io.NopCloser(bytes.NewReader(nil)),
	}
}

func (p *coverageNativeProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *coverageNativeProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *coverageNativeProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *coverageNativeProcess) Wait(ctx context.Context) (NativeResult, error) {
	if p.gate != nil {
		select {
		case <-p.gate:
		case <-ctx.Done():
			return NativeResult{}, ctx.Err()
		}
	}

	return p.result, p.waitErr
}
func (p *coverageNativeProcess) Revoke(context.Context) error {
	if p.gate != nil {
		p.once.Do(func() { close(p.gate) })
	}

	return p.revokeErr
}

type coverageHost struct {
	environment map[string]string
	process     NativeProcess
	err         error
}

func (h *coverageHost) NativeEnvironment() map[string]string          { return h.environment }
func (*coverageHost) PrepareNativeTree(context.Context, string) error { return nil }
func (*coverageHost) ReclaimNativeTree(context.Context, string) error { return nil }
func (h *coverageHost) StartNative(context.Context, NativeRequest) (NativeProcess, error) {
	return h.process, h.err
}

type deadlineCoverageHost struct{ coverageHost }

func (h *deadlineCoverageHost) StartNative(ctx context.Context, _ NativeRequest) (NativeProcess, error) {
	<-ctx.Done()

	return nil, ctx.Err()
}

type errorReadCloser struct{ err error }

func (r errorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (errorReadCloser) Close() error               { return nil }

func requireLaunchAppServerError(t *testing.T, options Options) error {
	t.Helper()

	transport, version, nativePath, err := launchAppServer(t.Context(), options)
	require.Nil(t, transport)
	require.Empty(t, version)
	require.Empty(t, nativePath)
	require.Error(t, err)

	return err
}

func TestLaunchAppServerFailureCoverage(t *testing.T) {
	err := requireLaunchAppServerError(t, Options{HostAuthority: &coverageHost{environment: map[string]string{"BAD=KEY": "x"}}})
	require.Error(t, err)
	err = requireLaunchAppServerError(t, Options{CLIPath: filepath.Join(t.TempDir(), "missing"), ImplicitEnvironment: map[string]string{"PATH": "/bin"}})
	require.ErrorContains(t, err, "find codex CLI")

	fixture := newPackagedCodexFixture(t)
	err = requireLaunchAppServerError(t, Options{CLIPath: fixture.executable, NativeVersion: minCodexVersion, ImplicitEnvironment: map[string]string{"PATH": "/bin"}})
	require.ErrorContains(t, err, "runtime scratch")

	err = requireLaunchAppServerError(t, Options{CLIPath: "node_modules/codex", NativeVersion: minCodexVersion, HostAuthority: &coverageHost{environment: map[string]string{}}})
	require.ErrorContains(t, err, "staged and pinned")
	err = requireLaunchAppServerError(t, Options{CLIPath: "managed", NativeVersion: "bad", HostAuthority: &coverageHost{environment: map[string]string{}}})
	require.ErrorContains(t, err, "could not parse")
	startErr := errors.New("start failed")
	err = requireLaunchAppServerError(t, Options{CLIPath: "managed", NativeVersion: minCodexVersion, HostAuthority: &coverageHost{environment: map[string]string{}, err: startErr}})
	require.ErrorIs(t, err, startErr)
	err = requireLaunchAppServerError(t, Options{CLIPath: "managed", NativeVersion: minCodexVersion, HostAuthority: &coverageHost{environment: map[string]string{}}})
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	err = requireLaunchAppServerError(t, Options{NativeVersion: minCodexVersion, HostAuthority: &coverageHost{environment: map[string]string{}}})
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	err = requireLaunchAppServerError(t, Options{
		CLIPath: fixture.executable, NativeVersion: "bad", Scratch: t.TempDir(),
		ImplicitEnvironment: map[string]string{"PATH": "/bin"},
	})
	require.ErrorContains(t, err, "could not parse")

	incomplete := newCoverageNativeProcess()
	incomplete.stderr = nil
	incomplete.waitErr = errors.New("wait failed")
	incomplete.revokeErr = errors.New("revoke failed")
	err = requireLaunchAppServerError(t, Options{CLIPath: "managed", NativeVersion: minCodexVersion, HostAuthority: &coverageHost{environment: map[string]string{}, process: incomplete}})
	require.ErrorIs(t, err, ErrContainmentIncomplete)

	_, err = NewAppServerClient(t.Context(), Options{
		CLIPath: "managed", NativeVersion: minCodexVersion, LaunchTimeout: time.Millisecond,
		HostAuthority: &deadlineCoverageHost{coverageHost{environment: map[string]string{}}},
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)

	environment := browserShimEnviron([]string{"MALFORMED", "KEEP=yes"}, t.TempDir())
	require.Contains(t, environment, "MALFORMED")
}

func TestCommandAndProcessCoverageEdges(t *testing.T) {
	require.Equal(t, "1.2.3", parseCodexVersion("codex 1.2.3+build"))
	require.Equal(t, "false", shellValue(false))
	require.Equal(t, "7", shellValue(7))
	var nilProcess *process
	require.NoError(t, nilProcess.cleanupPackage())

	gate := make(chan struct{})
	native := newCoverageNativeProcess()
	native.gate = gate
	p := &process{native: native}
	require.False(t, p.exited(0))
	close(gate)
	require.True(t, p.exited(time.Second))

	waitFailure := newCoverageNativeProcess()
	waitFailure.waitErr = errors.New("wait")
	p = &process{native: waitFailure}
	require.ErrorIs(t, p.waitTerminal(), ErrContainmentIncomplete)
	exitFailure := newCoverageNativeProcess()
	exitFailure.result.ExitCode = 4
	p = &process{native: exitFailure}
	require.ErrorContains(t, p.waitTerminal(), "status 4")

	cleanupCalls := 0
	p = &process{packageCleanup: func() error {
		cleanupCalls++

		return errors.New("cleanup")
	}}
	require.ErrorContains(t, p.Close(), "cleanup")
	require.ErrorContains(t, p.cleanupPackage(), "cleanup")
	require.Equal(t, 1, cleanupCalls)

	immediate := newCoverageNativeProcess()
	immediate.waitErr = errors.New("terminal")
	p = &process{native: immediate, stdin: &authorityTestWriteCloser{}}
	require.ErrorIs(t, p.Close(), ErrContainmentIncomplete)

	originalGrace := processCloseGrace
	processCloseGrace = 0
	t.Cleanup(func() { processCloseGrace = originalGrace })
	revoked := newCoverageNativeProcess()
	revoked.gate = make(chan struct{})
	revoked.waitErr = errors.New("wait")
	revoked.revokeErr = errors.New("revoke")
	p = &process{native: revoked}
	err := p.Close()
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.ErrorContains(t, err, "revoke")
}

func TestOrdinaryNativeConstructionAndWaitEdges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := startOrdinaryNative(ctx, NativeRequest{}, Options{skipHomeLock: true})
	require.ErrorIs(t, err, context.Canceled)

	originalCommand := ordinaryExecCommand
	t.Cleanup(func() { ordinaryExecCommand = originalCommand })
	for _, configure := range []func(*exec.Cmd){
		func(cmd *exec.Cmd) { cmd.Stdin = strings.NewReader("") },
		func(cmd *exec.Cmd) { cmd.Stdout = io.Discard },
		func(cmd *exec.Cmd) { cmd.Stderr = io.Discard },
	} {
		ordinaryExecCommand = func(string, ...string) *exec.Cmd {
			cmd := exec.Command("/bin/true")
			configure(cmd)

			return cmd
		}
		_, err = startOrdinaryNative(t.Context(), NativeRequest{Executable: "/bin/true"}, Options{skipHomeLock: true})
		require.Error(t, err)
	}
	ordinaryExecCommand = originalCommand

	_, err = startOrdinaryNative(t.Context(), NativeRequest{Executable: "/bin/true"}, Options{})
	require.Error(t, err)

	scratch, home := t.TempDir(), t.TempDir()
	lockRoot, err := HomeLockRoot(scratch, home)
	require.NoError(t, err)
	lock, err := homelock.Acquire(lockRoot)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Release()) })
	_, err = startOrdinaryNative(t.Context(), NativeRequest{Executable: "/bin/true"}, Options{ScratchParent: scratch, WritableHome: home})
	require.Error(t, err)

	notStarted := &ordinaryNativeProcess{cmd: exec.Command("/bin/true"), done: make(chan struct{})}
	_, err = notStarted.Wait(t.Context())
	require.Error(t, err)

	openDone := make(chan struct{})
	blocked := &ordinaryNativeProcess{cmd: &exec.Cmd{}, done: openDone}
	blocked.waitOnce.Do(func() {})
	canceled, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	_, err = blocked.Wait(canceled)
	require.ErrorIs(t, err, context.Canceled)

	closedDone := make(chan struct{})
	close(closedDone)
	doneProcess := &ordinaryNativeProcess{done: closedDone}
	doneProcess.waitOnce.Do(func() {})
	require.NoError(t, doneProcess.Revoke(t.Context()))

	originalGrace := processCloseGrace
	processCloseGrace = 0
	t.Cleanup(func() { processCloseGrace = originalGrace })
	require.ErrorIs(t, blocked.Revoke(canceled), context.Canceled)
	finishingDone := make(chan struct{})
	finishing := &ordinaryNativeProcess{cmd: &exec.Cmd{}, done: finishingDone}
	finishing.waitOnce.Do(func() {})
	go func() {
		time.Sleep(time.Millisecond)
		close(finishingDone)
	}()
	require.NoError(t, finishing.Revoke(t.Context()))
}

func TestUnixSignalProcessEdges(t *testing.T) {
	require.NoError(t, terminateProcess(nil))
	require.NoError(t, killProcess(nil))

	originalPGID, originalKill, originalSignal := getProcessGroupID, killProcessID, signalOneProcess
	t.Cleanup(func() {
		getProcessGroupID, killProcessID, signalOneProcess = originalPGID, originalKill, originalSignal
	})
	doneCommand := exec.Command("/bin/true")
	require.NoError(t, doneCommand.Run())
	require.ErrorIs(t, signalOneProcess(doneCommand.Process, syscall.SIGTERM), os.ErrProcessDone)
	cmd := &exec.Cmd{Process: &os.Process{Pid: 12345}}
	getProcessGroupID = func(int) (int, error) { return 7, nil }
	killProcessID = func(int, syscall.Signal) error { return os.ErrPermission }
	require.ErrorIs(t, signalProcess(cmd, syscall.SIGTERM), os.ErrPermission)
	killProcessID = func(int, syscall.Signal) error { return syscall.ESRCH }
	require.NoError(t, signalProcess(cmd, syscall.SIGTERM))

	getProcessGroupID = func(int) (int, error) { return 0, errors.New("no group") }
	signalOneProcess = func(*os.Process, os.Signal) error { return os.ErrProcessDone }
	require.NoError(t, signalProcess(cmd, syscall.SIGTERM))
	signalErr := errors.New("signal")
	signalOneProcess = func(*os.Process, os.Signal) error { return signalErr }
	require.ErrorIs(t, signalProcess(cmd, syscall.SIGTERM), signalErr)
}

func TestLineTransportReadErrorCoverage(t *testing.T) {
	cause := errors.New("read")
	require.ErrorIs(t, (&lineTransport{}).readError(cause), cause)
	blocked := newCoverageNativeProcess()
	blocked.gate = make(chan struct{})
	transport := &lineTransport{proc: &process{native: blocked}, grace: 0}
	require.ErrorIs(t, transport.readError(cause), cause)
	close(blocked.gate)
	transport.grace = time.Second
	var exit *ProcessExitError
	require.ErrorAs(t, transport.readError(cause), &exit)
	failed := newCoverageNativeProcess()
	failed.waitErr = errors.New("terminal proof failed")
	transport.proc = &process{native: failed}
	err := transport.readError(cause)
	require.ErrorAs(t, err, &exit)
	require.ErrorIs(t, err, ErrContainmentIncomplete)
}

type terminalErrorVersionProcess struct {
	*authorityTestProcess
	err error
}

func (p *terminalErrorVersionProcess) Wait(ctx context.Context) (NativeResult, error) {
	if ctx.Err() != nil {
		return NativeResult{}, ctx.Err()
	}

	return NativeResult{}, p.err
}

func TestVersionProbeCoverageEdges(t *testing.T) {
	_, err := ProbeVersion(t.Context(), VersionProbeOptions{HostAuthority: &coverageHost{environment: map[string]string{"BAD=KEY": "x"}}})
	require.Error(t, err)

	process := newAuthorityTestProcess("codex 0.144.1")
	_, err = ProbeVersion(t.Context(), VersionProbeOptions{HostAuthority: &coverageHost{environment: map[string]string{}, process: process}})
	require.NoError(t, err)
	startErr := errors.New("start")
	_, err = ProbeVersion(t.Context(), VersionProbeOptions{CLIPath: "managed", HostAuthority: &coverageHost{environment: map[string]string{}, err: startErr}})
	require.ErrorIs(t, err, startErr)

	incomplete := newCoverageNativeProcess()
	incomplete.stderr = nil
	incomplete.waitErr = errors.New("wait")
	incomplete.revokeErr = errors.New("revoke")
	_, err = ProbeVersion(t.Context(), VersionProbeOptions{CLIPath: "managed", HostAuthority: &coverageHost{environment: map[string]string{}, process: incomplete}})
	require.ErrorIs(t, err, ErrContainmentIncomplete)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	terminalErr := errors.New("terminal")
	cancelProcess := &terminalErrorVersionProcess{authorityTestProcess: newAuthorityTestProcess(""), err: terminalErr}
	_, err = ProbeVersion(ctx, VersionProbeOptions{CLIPath: "managed", HostAuthority: &coverageHost{environment: map[string]string{}, process: cancelProcess}})
	require.ErrorIs(t, err, terminalErr)

	copyErr := errors.New("copy")
	copyFailure := newCoverageNativeProcess()
	copyFailure.stdout = errorReadCloser{err: copyErr}
	_, err = ProbeVersion(t.Context(), VersionProbeOptions{CLIPath: "managed", HostAuthority: &coverageHost{environment: map[string]string{}, process: copyFailure}})
	require.ErrorIs(t, err, copyErr)

	exitFailure := newCoverageNativeProcess()
	exitFailure.stdout = io.NopCloser(bytes.NewBufferString("codex 0.144.1"))
	exitFailure.result = NativeResult{ExitCode: 9}
	_, err = ProbeVersion(t.Context(), VersionProbeOptions{CLIPath: "managed", HostAuthority: &coverageHost{environment: map[string]string{}, process: exitFailure}})
	require.ErrorContains(t, err, "status 9")
}

func TestRunAccountCommandCoverageEdges(t *testing.T) {
	originalScratch, originalProbe := accountScratchParent, accountProbeVersion
	originalMkdir := browserShimMkdirTemp
	t.Cleanup(func() {
		accountScratchParent, accountProbeVersion = originalScratch, originalProbe
		browserShimMkdirTemp = originalMkdir
	})
	SetScratchParentResolver(func(string) (string, error) { return t.TempDir(), nil })

	base := AccountCommandOptions{
		CLIPath: "/bin/true", CodexHome: t.TempDir(), Mode: accountCommandLogout,
		ImplicitEnvironment: map[string]string{"PATH": "/bin"},
	}
	invalidEnvironment := base
	invalidEnvironment.ImplicitEnvironment = map[string]string{"BAD=KEY": "x"}
	require.Error(t, RunAccountCommand(t.Context(), invalidEnvironment))

	missing := base
	missing.CLIPath = filepath.Join(t.TempDir(), "missing")
	require.Error(t, RunAccountCommand(t.Context(), missing))

	login := base
	login.Mode = accountCommandLogin
	browserShimMkdirTemp = func(string, string) (string, error) { return "", os.ErrPermission }
	require.ErrorIs(t, RunAccountCommand(t.Context(), login), os.ErrPermission)
	browserShimMkdirTemp = originalMkdir

	script := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	accountProbeVersion = func(context.Context, VersionProbeOptions) (string, error) {
		require.NoError(t, os.Remove(script))

		return minCodexVersion, nil
	}
	startFailure := base
	startFailure.CLIPath = script
	require.Error(t, RunAccountCommand(t.Context(), startFailure))

	defaultSelector := base
	defaultSelector.CLIPath = ""
	defaultSelector.ImplicitEnvironment = map[string]string{"PATH": t.TempDir()}
	require.Error(t, RunAccountCommand(t.Context(), defaultSelector))
}

func TestRunAccountNativeCoverageEdges(t *testing.T) {
	exitFailure := newCoverageNativeProcess()
	exitFailure.result = NativeResult{ExitCode: 9}
	require.ErrorContains(t, runAccountNative(t.Context(), AccountCommandOptions{}, exitFailure), "status 9")

	copyErr := errors.New("copy")
	copyFailure := newCoverageNativeProcess()
	copyFailure.stdout = errorReadCloser{err: copyErr}
	require.ErrorIs(t, runAccountNative(t.Context(), AccountCommandOptions{}, copyFailure), copyErr)

	for _, test := range []struct {
		name     string
		terminal bool
		signal   bool
	}{
		{"cancel terminal", true, false},
		{"cancel nonterminal", false, false},
		{"signal terminal", true, true},
		{"signal nonterminal", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			native := newCoverageNativeProcess()
			native.gate = make(chan struct{})
			native.revokeErr = errors.New("revoke")
			if !test.terminal {
				native.waitErr = errors.New("wait")
			}
			options := AccountCommandOptions{}
			ctx := context.Background()
			if test.signal {
				signals := make(chan os.Signal, 1)
				signals <- os.Interrupt
				options.Signals = signals
			} else {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			err := runAccountNative(ctx, options, native)
			if test.signal && test.terminal {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			if !test.terminal {
				require.ErrorIs(t, err, ErrContainmentIncomplete)
			}
		})
	}

	require.NoError(t, accountCopyError(os.ErrClosed))
	copyErr = errors.New("copy")
	require.ErrorIs(t, accountCopyError(copyErr), copyErr)
	var output bytes.Buffer
	require.Same(t, &output, writerOrDiscard(&output))
}
