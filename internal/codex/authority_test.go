package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type authorityTestWriteCloser struct{ bytes.Buffer }

func (*authorityTestWriteCloser) Close() error { return nil }

type blockingAuthorityReadCloser struct {
	closed chan struct{}
	done   chan struct{}
	once   sync.Once
}

func newBlockingAuthorityReadCloser() *blockingAuthorityReadCloser {
	return &blockingAuthorityReadCloser{closed: make(chan struct{}), done: make(chan struct{})}
}

func (r *blockingAuthorityReadCloser) Read([]byte) (int, error) {
	<-r.closed
	r.once.Do(func() { close(r.done) })

	return 0, io.EOF
}

func (r *blockingAuthorityReadCloser) Close() error {
	r.release()

	return nil
}

func (r *blockingAuthorityReadCloser) release() {
	select {
	case <-r.closed:
	default:
		close(r.closed)
	}
}

type authorityTestProcess struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	mu      sync.Mutex
	waits   int
	revokes int
}

func newAuthorityTestProcess(stdout string) *authorityTestProcess {
	return &authorityTestProcess{
		stdin:  &authorityTestWriteCloser{},
		stdout: io.NopCloser(bytes.NewBufferString(stdout)),
		stderr: io.NopCloser(bytes.NewReader(nil)),
	}
}

func (p *authorityTestProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *authorityTestProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *authorityTestProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *authorityTestProcess) Wait(context.Context) (NativeResult, error) {
	p.mu.Lock()
	p.waits++
	p.mu.Unlock()

	return NativeResult{}, nil
}
func (p *authorityTestProcess) Revoke(context.Context) error {
	p.mu.Lock()
	p.revokes++
	p.mu.Unlock()

	return nil
}

type authorityTestHost struct {
	environment map[string]string
	process     NativeProcess
	requests    []NativeRequest
	prepared    []string
	prepareErrs []error
	reclaimed   []string
	reclaimErrs []error
}

func (h *authorityTestHost) NativeEnvironment() map[string]string { return h.environment }
func (h *authorityTestHost) PrepareNativeTree(_ context.Context, path string) error {
	h.prepared = append(h.prepared, path)
	if len(h.prepareErrs) != 0 {
		err := h.prepareErrs[0]
		h.prepareErrs = h.prepareErrs[1:]

		return err
	}

	return nil
}
func (h *authorityTestHost) ReclaimNativeTree(_ context.Context, path string) error {
	h.reclaimed = append(h.reclaimed, path)
	if len(h.reclaimErrs) != 0 {
		err := h.reclaimErrs[0]
		h.reclaimErrs = h.reclaimErrs[1:]

		return err
	}

	return nil
}
func (h *authorityTestHost) StartNative(_ context.Context, request NativeRequest) (NativeProcess, error) {
	h.requests = append(h.requests, request)

	return h.process, nil
}

func TestManagedVersionProbeUsesOnlyHostAuthority(t *testing.T) {
	process := newAuthorityTestProcess("codex-cli 0.144.1\n")
	host := &authorityTestHost{environment: map[string]string{"PATH": "/host/bin", "HOME": "/host/home"}, process: process}

	version, err := ProbeVersion(t.Context(), VersionProbeOptions{CLIPath: "host-pinned-codex", HostAuthority: host})
	require.NoError(t, err)
	require.Equal(t, "0.144.1", version)
	require.Equal(t, []NativeRequest{{
		Executable: "host-pinned-codex",
		Arguments:  []string{"--version"},
		Environment: []string{
			"HOME=/host/home",
			"PATH=/host/bin",
		},
	}}, host.requests)
	require.Equal(t, 1, process.waits)
	require.Zero(t, process.revokes)
}

func TestManagedSelectorRefusesSessionPackageCopyBeforeLaunch(t *testing.T) {
	host := &authorityTestHost{environment: map[string]string{"PATH": "/host/bin"}, process: newAuthorityTestProcess("")}
	_, err := ProbeVersion(t.Context(), VersionProbeOptions{
		CLIPath:       "/tmp/session/node_modules/@openai/codex/bin/codex.js",
		HostAuthority: host,
	})
	require.ErrorContains(t, err, "staged and pinned by the host before adapter initialization")
	require.Empty(t, host.requests)
}

func TestIncompleteManagedStdioIsRevokedAndWaited(t *testing.T) {
	process := newAuthorityTestProcess("")
	stdin := &recordingCloser{}
	stdout := &recordingCloser{}
	process.stdin = stdin
	process.stdout = stdout
	process.stderr = nil
	host := &authorityTestHost{environment: map[string]string{"PATH": "/host/bin"}, process: process}

	transport, version, nativePath, err := launchAppServer(t.Context(), Options{
		CLIPath: "host-pinned-codex", NativeVersion: "0.144.1", HostAuthority: host,
	})
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	require.Nil(t, transport)
	require.Empty(t, version)
	require.Empty(t, nativePath)
	require.Equal(t, 1, process.revokes)
	require.Equal(t, 1, process.waits)
	require.True(t, stdin.closed)
	require.True(t, stdout.closed)
}

type orderedAuthorityProcess struct {
	stdin  *orderedWriteCloser
	done   chan struct{}
	calls  *[]string
	result NativeResult
}

type deadlineAuthorityProcess struct {
	*authorityTestProcess
}

func (p *deadlineAuthorityProcess) Wait(ctx context.Context) (NativeResult, error) {
	p.mu.Lock()
	p.waits++
	p.mu.Unlock()
	<-ctx.Done()

	return NativeResult{}, ctx.Err()
}

func (p *deadlineAuthorityProcess) Revoke(ctx context.Context) error {
	p.mu.Lock()
	p.revokes++
	p.mu.Unlock()
	<-ctx.Done()

	return ctx.Err()
}

type orderedWriteCloser struct{ calls *[]string }

func (w *orderedWriteCloser) Write(value []byte) (int, error) { return len(value), nil }
func (w *orderedWriteCloser) Close() error {
	*w.calls = append(*w.calls, "stdin-close")

	return nil
}

func (p *orderedAuthorityProcess) Stdin() io.WriteCloser { return p.stdin }
func (*orderedAuthorityProcess) Stdout() io.ReadCloser   { return io.NopCloser(bytes.NewReader(nil)) }
func (*orderedAuthorityProcess) Stderr() io.ReadCloser   { return io.NopCloser(bytes.NewReader(nil)) }
func (p *orderedAuthorityProcess) Wait(ctx context.Context) (NativeResult, error) {
	select {
	case <-ctx.Done():
		return NativeResult{}, ctx.Err()
	case <-p.done:
		*p.calls = append(*p.calls, "wait")

		return p.result, nil
	}
}
func (p *orderedAuthorityProcess) Revoke(context.Context) error {
	*p.calls = append(*p.calls, "revoke")
	close(p.done)

	return nil
}

func TestManagedProcessCloseGracefullyClosesThenRevokesAndWaits(t *testing.T) {
	originalGrace := processCloseGrace
	processCloseGrace = 0
	t.Cleanup(func() { processCloseGrace = originalGrace })

	calls := []string{}
	native := &orderedAuthorityProcess{done: make(chan struct{}), calls: &calls}
	native.stdin = &orderedWriteCloser{calls: &calls}
	managed := &process{native: native, stdin: native.stdin, stdout: native.Stdout()}
	require.NoError(t, managed.Close())
	require.Equal(t, []string{"stdin-close", "revoke", "wait"}, calls)
}

func TestManagedProcessCloseBoundsAuthorityCalls(t *testing.T) {
	originalGrace, originalContainment := processCloseGrace, processContainmentTimeout
	processCloseGrace, processContainmentTimeout = 0, 0
	t.Cleanup(func() {
		processCloseGrace, processContainmentTimeout = originalGrace, originalContainment
	})

	native := &deadlineAuthorityProcess{authorityTestProcess: newAuthorityTestProcess("")}
	managed := &process{native: native, stdin: native.stdin, stdout: native.stdout}
	err := managed.Close()
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 2, native.waits)
	require.Equal(t, 1, native.revokes)
}

type retryableAuthorityProcess struct {
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   io.ReadCloser
	terminal atomic.Bool
	waits    atomic.Int32
	revokes  atomic.Int32
}

type independentlyFailingWaitProcess struct {
	*authorityTestProcess
	err   error
	waits atomic.Int32
}

func (p *independentlyFailingWaitProcess) Wait(context.Context) (NativeResult, error) {
	if p.waits.Add(1) == 1 {
		return NativeResult{}, p.err
	}

	return NativeResult{}, nil
}

func (p *retryableAuthorityProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *retryableAuthorityProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *retryableAuthorityProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *retryableAuthorityProcess) Wait(ctx context.Context) (NativeResult, error) {
	p.waits.Add(1)
	_, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		panic("process wait did not have a deadline")
	}
	if p.terminal.Load() {
		return NativeResult{Revoked: true}, nil
	}

	<-ctx.Done()

	return NativeResult{}, ctx.Err()
}
func (p *retryableAuthorityProcess) Revoke(ctx context.Context) error {
	p.revokes.Add(1)
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		panic("process revoke did not have a deadline")
	}

	return nil
}

func TestManagedProcessCloseJoinsPipesAndRetriesTerminalProof(t *testing.T) {
	originalGrace, originalContainment := processCloseGrace, processContainmentTimeout
	processCloseGrace, processContainmentTimeout = 20*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() {
		processCloseGrace, processContainmentTimeout = originalGrace, originalContainment
	})

	stdout := newBlockingAuthorityReadCloser()
	stderr := newBlockingAuthorityReadCloser()
	native := &retryableAuthorityProcess{
		stdin: &authorityTestWriteCloser{}, stdout: stdout, stderr: stderr,
	}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)

		_, _ = io.Copy(io.Discard, stderr)
	}()

	cleanupCalls := atomic.Int32{}
	managed := &process{
		native: native, stdin: native.stdin, stdout: stdout, stderr: stderr, stderrDone: stderrDone,
		packageCleanup: func() error {
			cleanupCalls.Add(1)

			return nil
		},
	}
	require.ErrorIs(t, managed.Close(), ErrContainmentIncomplete)
	select {
	case <-stdout.closed:
	default:
		t.Fatal("incomplete close did not release stdout")
	}
	select {
	case <-stderrDone:
	default:
		t.Fatal("incomplete close did not join stderr drain")
	}
	require.Zero(t, cleanupCalls.Load(), "uncertain terminal wait released package resources")

	native.terminal.Store(true)
	require.NoError(t, managed.Close())
	require.EqualValues(t, 1, cleanupCalls.Load())
	require.EqualValues(t, 3, native.waits.Load())
	require.EqualValues(t, 1, native.revokes.Load())
}

func TestManagedProcessClosePreservesFirstWaitCauseWithoutRewaitingTerminalProcess(t *testing.T) {
	waitErr := errors.New("independent host wait failure")
	native := &independentlyFailingWaitProcess{authorityTestProcess: newAuthorityTestProcess(""), err: waitErr}
	cleanupCalls := atomic.Int32{}
	managed := &process{native: native, packageCleanup: func() error {
		cleanupCalls.Add(1)

		return nil
	}}

	require.ErrorIs(t, managed.Close(), waitErr)
	require.NoError(t, managed.Close())
	require.EqualValues(t, 2, native.waits.Load())
	require.EqualValues(t, 1, cleanupCalls.Load())
}

func TestWithoutExactErrorLeavesDropsWrappedOwnedContextOnly(t *testing.T) {
	independent := errors.New("independent")
	owned := context.DeadlineExceeded
	err := errors.Join(independent, fmt.Errorf("owned wait context: %w", owned))

	filtered := withoutExactErrorLeaves(err, owned)
	require.ErrorIs(t, filtered, independent)
	require.NotErrorIs(t, filtered, owned)
	require.NoError(t, withoutExactErrorLeaves(fmt.Errorf("owned wait context: %w", owned), owned))
}
