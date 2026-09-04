package codex

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type prefixBlockingReadCloser struct {
	prefix  *bytes.Reader
	blocked *blockingAuthorityReadCloser
}

func newPrefixBlockingReadCloser(prefix string) *prefixBlockingReadCloser {
	return &prefixBlockingReadCloser{
		prefix: bytes.NewReader([]byte(prefix)), blocked: newBlockingAuthorityReadCloser(),
	}
}

func (r *prefixBlockingReadCloser) Read(value []byte) (int, error) {
	if r.prefix.Len() > 0 {
		return r.prefix.Read(value)
	}

	return r.blocked.Read(value)
}

func (r *prefixBlockingReadCloser) Close() error { return r.blocked.Close() }

type snapshotVersionProcess struct {
	stdin  io.WriteCloser
	stdout *prefixBlockingReadCloser
	stderr *blockingAuthorityReadCloser

	settleOnRevoke bool
	terminal       atomic.Bool
	stdinCalls     atomic.Int32
	stdoutCalls    atomic.Int32
	stderrCalls    atomic.Int32
	waits          atomic.Int32
	revokes        atomic.Int32
}

func (p *snapshotVersionProcess) Stdin() io.WriteCloser {
	p.stdinCalls.Add(1)

	return p.stdin
}
func (p *snapshotVersionProcess) Stdout() io.ReadCloser {
	p.stdoutCalls.Add(1)

	return p.stdout
}
func (p *snapshotVersionProcess) Stderr() io.ReadCloser {
	p.stderrCalls.Add(1)

	return p.stderr
}
func (p *snapshotVersionProcess) Wait(ctx context.Context) (NativeResult, error) {
	p.waits.Add(1)
	if p.terminal.Load() {
		return NativeResult{Revoked: true}, nil
	}

	<-ctx.Done()

	return NativeResult{}, ctx.Err()
}
func (p *snapshotVersionProcess) Revoke(ctx context.Context) error {
	p.revokes.Add(1)
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		panic("version revoke did not have a deadline")
	}
	if p.settleOnRevoke {
		p.terminal.Store(true)
		p.stdout.blocked.release()
		p.stderr.release()
	}

	return nil
}

type orderedVersionReadCloser struct {
	name  string
	calls *[]string
}

type errorVersionWriteCloser struct{ err error }

func (*errorVersionWriteCloser) Write(value []byte) (int, error) { return len(value), nil }
func (w *errorVersionWriteCloser) Close() error                  { return w.err }

func (*orderedVersionReadCloser) Read([]byte) (int, error) { return 0, io.EOF }
func (r *orderedVersionReadCloser) Close() error {
	*r.calls = append(*r.calls, r.name+"-close")

	return nil
}

func TestTerminalVersionProbeDrainsBufferedOutputBeforeClosingReaders(t *testing.T) {
	calls := []string{}
	stdoutDone := make(chan error)
	stderrDone := make(chan error)
	go func() {
		calls = append(calls, "stdout-buffer-drained")
		stdoutDone <- nil
		calls = append(calls, "stderr-buffer-drained")
		stderrDone <- nil
	}()

	err := settleVersionProbePipes(
		true,
		&orderedVersionReadCloser{name: "stdout", calls: &calls},
		&orderedVersionReadCloser{name: "stderr", calls: &calls},
		stdoutDone,
		stderrDone,
	)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"stdout-buffer-drained", "stderr-buffer-drained"}, calls[:2])
	require.Equal(t, []string{"stdout-close", "stderr-close"}, calls[2:])
}

func TestProbeVersionOrdinaryBackend(t *testing.T) {
	script := writeFakeCLI(t, t.TempDir(), "codex", fakeCLIVersionOnly)
	version, err := ProbeVersion(t.Context(), VersionProbeOptions{
		CLIPath: script, ScratchParent: t.TempDir(), ImplicitEnvironment: map[string]string{"PATH": "/bin"},
	})
	require.NoError(t, err)
	require.Equal(t, "0.144.1", version)
}

func TestProbeVersionFailureBranches(t *testing.T) {
	_, err := ProbeVersion(t.Context(), VersionProbeOptions{
		CLIPath: filepath.Join(t.TempDir(), "missing"), ImplicitEnvironment: map[string]string{"PATH": "/bin"},
	})
	require.Error(t, err)

	host := &authorityTestHost{environment: map[string]string{"PATH": "/host/bin"}, process: newAuthorityTestProcess("bad output")}
	_, err = ProbeVersion(t.Context(), VersionProbeOptions{CLIPath: "host-codex", HostAuthority: host})
	require.Error(t, err)

	host.process = nil
	_, err = ProbeVersion(t.Context(), VersionProbeOptions{CLIPath: "host-codex", HostAuthority: host})
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
}

type cancelledVersionProcess struct{ *authorityTestProcess }

func (p *cancelledVersionProcess) Wait(ctx context.Context) (NativeResult, error) {
	if ctx.Err() != nil {
		return NativeResult{}, ctx.Err()
	}

	return p.authorityTestProcess.Wait(ctx)
}

func TestProbeVersionCancellationRevokesAndWaits(t *testing.T) {
	base := newAuthorityTestProcess("")
	process := &cancelledVersionProcess{authorityTestProcess: base}
	host := &authorityTestHost{environment: map[string]string{"PATH": "/host/bin"}, process: process}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ProbeVersion(ctx, VersionProbeOptions{CLIPath: "host-codex", HostAuthority: host})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, base.revokes)
}

func TestProbeVersionWaitFailure(t *testing.T) {
	waitErr := errors.New("wait failed")
	process := &errorWaitVersionProcess{authorityTestProcess: newAuthorityTestProcess(""), err: waitErr}
	host := &authorityTestHost{environment: map[string]string{"PATH": "/host/bin"}, process: process}
	_, err := ProbeVersion(t.Context(), VersionProbeOptions{CLIPath: "host-codex", HostAuthority: host})
	require.ErrorIs(t, err, waitErr)
}

func TestProbeVersionSnapshotsAndJoinsPipesOnCancellation(t *testing.T) {
	originalTimeout := processContainmentTimeout
	processContainmentTimeout = 20 * time.Millisecond
	t.Cleanup(func() { processContainmentTimeout = originalTimeout })

	for _, settle := range []bool{true, false} {
		t.Run(map[bool]string{true: "terminal retry", false: "incomplete retry"}[settle], func(t *testing.T) {
			process := &snapshotVersionProcess{
				stdin: &authorityTestWriteCloser{}, stdout: newPrefixBlockingReadCloser("codex-cli 0.144.1\n"),
				stderr: newBlockingAuthorityReadCloser(), settleOnRevoke: settle,
			}
			host := &authorityTestHost{environment: map[string]string{"PATH": "/host/bin"}, process: process}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			_, err := ProbeVersion(ctx, VersionProbeOptions{CLIPath: "host-codex", HostAuthority: host})
			require.ErrorIs(t, err, context.Canceled)
			if settle {
				require.NotErrorIs(t, err, ErrContainmentIncomplete)
			} else {
				require.ErrorIs(t, err, ErrContainmentIncomplete)
			}
			require.EqualValues(t, 1, process.stdinCalls.Load())
			require.EqualValues(t, 1, process.stdoutCalls.Load())
			require.EqualValues(t, 1, process.stderrCalls.Load())
			require.EqualValues(t, 2, process.waits.Load())
			require.EqualValues(t, 1, process.revokes.Load())

			select {
			case <-process.stdout.blocked.done:
			default:
				t.Fatal("version close did not join stdout reader")
			}
			select {
			case <-process.stderr.done:
			default:
				t.Fatal("version close did not join stderr reader")
			}
		})
	}
}

type errorWaitVersionProcess struct {
	*authorityTestProcess
	err error
}

type errorThenTerminalVersionProcess struct {
	*authorityTestProcess
	err   error
	waits atomic.Int32
}

func (p *errorThenTerminalVersionProcess) Wait(context.Context) (NativeResult, error) {
	if p.waits.Add(1) == 1 {
		return NativeResult{}, p.err
	}

	return NativeResult{}, nil
}

func TestProbeVersionPreservesInitialWaitErrorAfterTerminalRetry(t *testing.T) {
	waitErr := errors.New("independent wait failure")
	process := &errorThenTerminalVersionProcess{
		authorityTestProcess: newAuthorityTestProcess("codex-cli 0.144.1\n"), err: waitErr,
	}
	host := &authorityTestHost{environment: map[string]string{"PATH": "/host/bin"}, process: process}

	_, err := ProbeVersion(t.Context(), VersionProbeOptions{CLIPath: "host-codex", HostAuthority: host})
	require.ErrorIs(t, err, waitErr)
	require.NotErrorIs(t, err, ErrContainmentIncomplete)
	require.EqualValues(t, 2, process.waits.Load())
}

func TestProbeVersionPreservesStdinCloseFailure(t *testing.T) {
	closeErr := errors.New("close version stdin")
	hostProcess := &snapshotVersionProcess{
		stdin: &errorVersionWriteCloser{err: closeErr}, stdout: newPrefixBlockingReadCloser("codex-cli 0.144.1\n"),
		stderr: newBlockingAuthorityReadCloser(),
	}
	hostProcess.terminal.Store(true)
	hostProcess.stdout.blocked.release()
	hostProcess.stderr.release()
	host := &authorityTestHost{environment: map[string]string{"PATH": "/host/bin"}, process: hostProcess}

	_, err := ProbeVersion(t.Context(), VersionProbeOptions{CLIPath: "host-codex", HostAuthority: host})
	require.ErrorIs(t, err, closeErr)
}

func (p *errorWaitVersionProcess) Wait(context.Context) (NativeResult, error) {
	if p.err == nil {
		p.err = errors.New("wait failed")
	}

	return NativeResult{}, p.err
}
