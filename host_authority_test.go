package codexacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

type unavailableAuthority struct{}

func (*unavailableAuthority) NativeEnvironment() map[string]string {
	return map[string]string{"PATH": "/bin"}
}
func (*unavailableAuthority) PrepareNativeTree(context.Context, string) error { return nil }
func (*unavailableAuthority) ReadNativeAppendLog(context.Context, string, uint64) ([][]byte, error) {
	return nil, nil
}
func (*unavailableAuthority) ReclaimNativeTree(context.Context, string) error { return nil }
func (*unavailableAuthority) StartNative(context.Context, NativeRequest) (NativeProcess, error) {
	return nil, ErrHostAuthorityUnavailable
}

type invalidEnvironmentAuthority struct{ starts *int }

func (a invalidEnvironmentAuthority) NativeEnvironment() map[string]string { return nil }
func (a invalidEnvironmentAuthority) PrepareNativeTree(context.Context, string) error {
	return errors.New("unexpected prepare")
}
func (a invalidEnvironmentAuthority) ReadNativeAppendLog(context.Context, string, uint64) ([][]byte, error) {
	return nil, errors.New("unexpected read")
}
func (a invalidEnvironmentAuthority) ReclaimNativeTree(context.Context, string) error { return nil }
func (a invalidEnvironmentAuthority) StartNative(context.Context, NativeRequest) (NativeProcess, error) {
	(*a.starts)++

	return nil, errors.New("unexpected launch")
}

func TestHostAuthoritySurfaceAndConstructionGate(t *testing.T) {
	require.EqualError(t, ErrHostAuthorityUnavailable, "host authority unavailable")
	require.EqualError(t, ErrContainmentIncomplete, "native containment incomplete")
	require.EqualError(t, ErrNativeTreeBusy, "native tree has live lease processes")

	var typedNil *unavailableAuthority
	for name, authority := range map[string]HostAuthority{
		"typed nil":           typedNil,
		"missing environment": invalidEnvironmentAuthority{starts: new(int)},
	} {
		t.Run(name, func(t *testing.T) {
			agent := NewAgent(WithHostAuthority(authority))
			_, err := agent.Initialize(t.Context(), acp.InitializeRequest{})
			require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
		})
	}
}

type publicNativeProcess struct{}

func (publicNativeProcess) Stdin() io.WriteCloser { return nil }
func (publicNativeProcess) Stdout() io.ReadCloser { return nil }
func (publicNativeProcess) Stderr() io.ReadCloser { return nil }
func (publicNativeProcess) Wait(context.Context) (NativeResult, error) {
	return NativeResult{}, nil
}
func (publicNativeProcess) Revoke(context.Context) error { return nil }

var _ HostAuthority = (*unavailableAuthority)(nil)
var _ NativeProcess = publicNativeProcess{}

type orderingAuthority struct{ calls *[]string }

func (*orderingAuthority) NativeEnvironment() map[string]string {
	return map[string]string{"PATH": "/bin", "HOME": "/tmp"}
}
func (*orderingAuthority) PrepareNativeTree(context.Context, string) error { return nil }
func (*orderingAuthority) ReadNativeAppendLog(context.Context, string, uint64) ([][]byte, error) {
	return nil, nil
}
func (a *orderingAuthority) ReclaimNativeTree(context.Context, string) error {
	*a.calls = append(*a.calls, "reclaim")

	return nil
}
func (*orderingAuthority) StartNative(context.Context, NativeRequest) (NativeProcess, error) {
	return nil, ErrHostAuthorityUnavailable
}

type orderingRuntimeClient struct {
	*spyCodexClient
	calls *[]string
}

func (c *orderingRuntimeClient) Close(context.Context) error {
	*c.calls = append(*c.calls, "close")

	return nil
}

func TestRuntimeRetirementWaitsBeforeResidenceReclaim(t *testing.T) {
	calls := []string{}
	authority := &orderingAuthority{calls: &calls}
	agent := NewAgent(WithHostAuthority(authority))
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	agent.retiredResidences = []retiredNativeResidence{{
		epoch: 1,
		tree:  filepath.Dir(path),
		path:  path,
		remove: func(string) error {
			calls = append(calls, "remove")

			return nil
		},
		release: func() { calls = append(calls, "release-residence") },
	}}
	client := &orderingRuntimeClient{spyCodexClient: newSpyCodexClient(), calls: &calls}
	err := agent.closeRuntimeGeneration(t.Context(), client, func() error {
		calls = append(calls, "release-home")

		return nil
	}, "", nil, 1)
	require.NoError(t, err)
	require.Equal(t, []string{"close", "reclaim", "remove", "release-residence", "release-home"}, calls)
}

func TestRetiredResidenceBoundsForceRuntimeRetirementBeforeAdmission(t *testing.T) {
	for name, residences := range map[string][]retiredNativeResidence{
		"count": make([]retiredNativeResidence, retiredResidenceCountLimit),
		"bytes": {{epoch: 1, path: "residence", tree: "tree", bytes: retiredResidenceByteLimit}},
	} {
		t.Run(name, func(t *testing.T) {
			calls := []string{}
			authority := &orderingAuthority{calls: &calls}
			client := &orderingRuntimeClient{spyCodexClient: newSpyCodexClient(), calls: &calls}
			agent := NewAgent(WithHostAuthority(authority))
			agent.runtimeClient = client
			agent.runtimeEpoch = 1
			agent.runtimeDead = false

			for index := range residences {
				bytes := residences[index].bytes
				residences[index].epoch = 1
				residences[index].path = filepath.Join(t.TempDir(), "residence")
				residences[index].tree = filepath.Dir(residences[index].path)
				residences[index].remove = func(string) error { return nil }
				residences[index].release = func() {
					agent.mu.Lock()
					agent.nativeResidenceCount--
					agent.retiredResidenceBytes -= bytes
					agent.mu.Unlock()
				}
				agent.retiredResidenceBytes += residences[index].bytes
			}
			agent.retiredResidences = residences
			agent.nativeResidenceCount = len(residences)

			release, err := agent.reserveNativeResidenceCapacity(t.Context(), 1)
			require.NoError(t, err)
			release()
			require.Nil(t, agent.runtimeClient)
			require.Empty(t, agent.retiredResidences)
			require.Equal(t, "close", calls[0])
		})
	}
}

var _ codex.Client = (*orderingRuntimeClient)(nil)

type traceWriteCloser struct{ bytes.Buffer }

func (*traceWriteCloser) Close() error { return nil }

type traceNativeProcess struct {
	authority *traceAuthority
	stdin     traceWriteCloser
	stdout    io.ReadCloser
	stderr    io.ReadCloser
}

func (p *traceNativeProcess) Stdin() io.WriteCloser { return &p.stdin }
func (p *traceNativeProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *traceNativeProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *traceNativeProcess) Wait(context.Context) (NativeResult, error) {
	p.authority.record("wait:")

	return NativeResult{}, nil
}
func (*traceNativeProcess) Revoke(context.Context) error { return nil }

type traceAuthority struct {
	mu          sync.Mutex
	trace       []string
	prepared    map[string]string
	start       func(NativeRequest) (NativeProcess, error)
	startErr    error
	prepareErr  error
	reclaimBusy int
	environment map[string]string
}

func newTraceAuthority(home string) *traceAuthority {
	return &traceAuthority{
		prepared:    make(map[string]string),
		environment: map[string]string{"HOME": home, "PATH": "/usr/bin:/bin"},
	}
}

func (a *traceAuthority) record(event string) {
	a.mu.Lock()
	a.trace = append(a.trace, event)
	a.mu.Unlock()
}

func (a *traceAuthority) events() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]string(nil), a.trace...)
}

func (a *traceAuthority) NativeEnvironment() map[string]string {
	return cloneStringMap(a.environment)
}

func (a *traceAuthority) PrepareNativeTree(_ context.Context, path string) error {
	a.record("prepare:" + path)
	hidden := path + ".authority"
	if err := os.Rename(path, hidden); err != nil {
		return err
	}

	a.mu.Lock()
	a.prepared[path] = hidden
	a.mu.Unlock()
	if a.prepareErr != nil {
		return a.prepareErr
	}

	return nil
}

func (a *traceAuthority) ReadNativeAppendLog(_ context.Context, path string, after uint64) ([][]byte, error) {
	a.record("read:" + path)

	a.mu.Lock()
	defer a.mu.Unlock()

	for root, hidden := range a.prepared {
		if path != root && !strings.HasPrefix(path, root+string(os.PathSeparator)) {
			continue
		}

		nativePath := hidden + strings.TrimPrefix(path, root)
		records, readErr := readOrdinaryNativeAppendLog(nativePath, after)
		if readErr != nil {
			return nil, readErr
		}

		result := make([][]byte, len(records))
		for index, record := range records {
			result[index] = record
		}

		return result, nil
	}

	return nil, errors.New("native append log is outside a prepared tree")
}

func (a *traceAuthority) ReclaimNativeTree(_ context.Context, path string) error {
	a.record("reclaim:" + path)

	a.mu.Lock()
	if a.reclaimBusy > 0 {
		a.reclaimBusy--
		a.mu.Unlock()

		return ErrNativeTreeBusy
	}
	hidden := a.prepared[path]
	a.mu.Unlock()

	if hidden == "" {
		return nil
	}
	if err := os.Rename(hidden, path); err != nil {
		return err
	}

	a.mu.Lock()
	delete(a.prepared, path)
	a.mu.Unlock()

	return nil
}

func (a *traceAuthority) StartNative(_ context.Context, request NativeRequest) (NativeProcess, error) {
	a.record("start:" + request.Executable)
	if a.start != nil {
		return a.start(request)
	}

	if a.startErr != nil {
		return nil, a.startErr
	}

	return &traceNativeProcess{
		authority: a,
		stdout:    io.NopCloser(strings.NewReader("codex-cli 0.146.0\n")),
		stderr:    io.NopCloser(strings.NewReader("")),
	}, nil
}

func TestHostAuthorityManagedLaunchTrace(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	authority := newTraceAuthority(root)
	agent := NewAgent(WithHostAuthority(authority), WithHome(home), WithScratchDir(root))

	reclaim, err := agent.prepareRuntimeHome(t.Context())
	require.NoError(t, err)
	_, err = agent.probeRuntimeVersion(t.Context())
	require.NoError(t, err)
	require.NoError(t, reclaim())

	require.Equal(t, []string{
		"prepare:" + home,
		"start:" + "codex",
		"wait:",
		"reclaim:" + home,
	}, authority.events())
}

func TestHostAuthorityPreparedTreeExclusivity(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	authority := newTraceAuthority(root)
	agent := NewAgent(WithHostAuthority(authority), WithHome(home))

	reclaim, err := agent.prepareRuntimeHome(t.Context())
	require.NoError(t, err)
	_, err = os.Stat(home)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, reclaim())
	_, err = os.Stat(home)
	require.NoError(t, err)
}

func TestHostAuthorityReclaimPrecedesRemoval(t *testing.T) {
	root := t.TempDir()
	authority := newTraceAuthority(root)
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(root))

	release, err := agent.reserveNativeResidenceCapacity(t.Context(), 3)
	require.NoError(t, err)
	path, release, bytes, err := agent.materializeStoredRollout(t.Context(), []SessionStoreEntry{[]byte(`{}`)}, release)
	require.NoError(t, err)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, agent.retireMaterializedRolloutAtEpoch(path, bytes, release, 0))

	originalRemove := removeMaterializedRolloutFile
	removeMaterializedRolloutFile = func(path string) error {
		authority.record("remove:" + path)

		return originalRemove(path)
	}
	t.Cleanup(func() { removeMaterializedRolloutFile = originalRemove })

	require.NoError(t, agent.reclaimRetiredResidences(t.Context(), 0))
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	events := authority.events()
	require.Less(t, slices.Index(events, "reclaim:"+filepath.Dir(path)), slices.Index(events, "remove:"+path))
}

func TestHostAuthorityNoOrdinaryFallback(t *testing.T) {
	injected := errors.New("managed launch refused")
	root := t.TempDir()
	home := filepath.Join(root, "home")
	executable := filepath.Join(root, "ordinary-fallback-must-not-run")
	authority := newTraceAuthority(root)
	authority.startErr = injected
	agent := NewAgent(
		WithHostAuthority(authority),
		WithExecutablePath(executable),
		WithHome(home),
		WithScratchDir(root),
	)

	_, err := agent.sharedRuntime(t.Context())
	require.ErrorIs(t, err, injected)
	require.NotErrorIs(t, err, ErrHostAuthorityUnavailable)
	require.NotErrorIs(t, err, ErrContainmentIncomplete)
	require.Equal(t, []string{"prepare:" + home, "start:" + executable, "reclaim:" + home}, authority.events())
	require.DirExists(t, home)
}

func TestHostAuthorityAmbiguousStartRetainsPreparedTree(t *testing.T) {
	var typedNil *traceNativeProcess

	for name, start := range map[string]func(NativeRequest) (NativeProcess, error){
		"panic": func(NativeRequest) (NativeProcess, error) {
			panic("start outcome unknown")
		},
		"nil success": func(NativeRequest) (NativeProcess, error) {
			//nolint:nilnil // Exercises an invalid successful host result.
			return nil, nil
		},
		"typed nil success": func(NativeRequest) (NativeProcess, error) {
			return typedNil, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			const callers = 4

			root := t.TempDir()
			home := filepath.Join(root, "home")
			authority := newTraceAuthority(root)
			startEntered := make(chan struct{})
			releaseStart := make(chan struct{})
			authority.start = func(request NativeRequest) (NativeProcess, error) {
				close(startEntered)
				<-releaseStart

				return start(request)
			}
			agent := NewAgent(WithHostAuthority(authority), WithHome(home), WithScratchDir(root))

			originalRemove := runtimeRemoveAll
			removeCalls := 0
			runtimeRemoveAll = func(string) error {
				removeCalls++

				return nil
			}
			t.Cleanup(func() { runtimeRemoveAll = originalRemove })

			results := make(chan error, callers)
			go func() {
				_, err := agent.sharedRuntime(t.Context())
				results <- err
			}()
			<-startEntered
			for range callers - 1 {
				go func() {
					_, err := agent.sharedRuntime(t.Context())
					results <- err
				}()
			}
			close(releaseStart)
			for range callers {
				err := <-results
				require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
				require.ErrorIs(t, err, ErrContainmentIncomplete)
			}

			require.ErrorIs(t, agent.runtimeCleanupErr, ErrHostAuthorityUnavailable)
			require.ErrorIs(t, agent.runtimeCleanupErr, ErrContainmentIncomplete)
			require.NotNil(t, agent.runtimeNativeRelease)
			require.NotEmpty(t, agent.runtimeScratchRoot)
			require.Equal(t, []string{"prepare:" + home, "start:codex"}, authority.events())
			require.Zero(t, removeCalls)

			authority.mu.Lock()
			prepared := authority.prepared[home]
			authority.mu.Unlock()
			require.NotEmpty(t, prepared)

			_, err := agent.sharedRuntime(t.Context())
			require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
			require.ErrorIs(t, err, ErrContainmentIncomplete)
			require.Equal(t, []string{"prepare:" + home, "start:codex"}, authority.events())
			require.Zero(t, removeCalls)

			_, err = agent.reserveNativeResidenceCapacity(t.Context(), 1)
			require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
			require.ErrorIs(t, err, ErrContainmentIncomplete)
			require.Zero(t, agent.nativeResidenceCount)
			require.Zero(t, removeCalls)

			closeErr := agent.Close()
			require.ErrorIs(t, closeErr, ErrHostAuthorityUnavailable)
			require.ErrorIs(t, closeErr, ErrContainmentIncomplete)
			require.ErrorIs(t, agent.Close(), ErrContainmentIncomplete)
			require.Equal(t, []string{"prepare:" + home, "start:codex"}, authority.events())
			require.Zero(t, removeCalls)
			authority.mu.Lock()
			require.Equal(t, prepared, authority.prepared[home])
			authority.mu.Unlock()
		})
	}
}

func TestHostAuthorityExplicitNilIsUnavailable(t *testing.T) {
	agent := NewAgent(WithHostAuthority(nil))
	_, err := agent.Initialize(t.Context(), acp.InitializeRequest{})
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
}

func TestHostAuthorityWithholdsProviderAuthSurface(t *testing.T) {
	root := t.TempDir()
	ledger := filepath.Join(root, "provider-auth")
	agent := NewAgent(WithHostAuthority(newTraceAuthority(root)), WithProviderAuthRoot(ledger))

	response, err := agent.Initialize(t.Context(), acp.InitializeRequest{})
	require.NoError(t, err)
	codexMeta, ok := response.AgentCapabilities.Meta[codexMetaKey].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, codexMeta, providerAuthCapabilityKey)
	_, err = os.Stat(ledger)
	require.ErrorIs(t, err, os.ErrNotExist)

	_, err = agent.HandleExtensionMethod(t.Context(), AuthMethodsMethod, json.RawMessage(`{}`))
	var requestErr *acp.RequestError
	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, -32601, requestErr.Code)
}

func TestHostAuthorityRejectsRelativeNodeModulesSelectorBeforeMutation(t *testing.T) {
	root := t.TempDir()
	authority := newTraceAuthority(root)
	agent := NewAgent(
		WithHostAuthority(authority),
		WithExecutablePath(filepath.Join("packages", "node_modules", "@openai", "codex")),
	)

	_, err := agent.Initialize(t.Context(), acp.InitializeRequest{})
	require.Error(t, err)
	require.Empty(t, authority.events())
}

func TestHostAuthorityPrepareFailureRemainsOpaque(t *testing.T) {
	root := t.TempDir()
	authority := newTraceAuthority(root)
	injected := errors.New("prepare transferred ownership then failed")
	authority.prepareErr = injected
	agent := NewAgent(WithHostAuthority(authority), WithHome(filepath.Join(root, "home")))

	_, err := agent.sharedRuntime(t.Context())
	require.ErrorIs(t, err, injected)
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.ErrorIs(t, agent.runtimeCleanupErr, ErrContainmentIncomplete)
	_, err = os.Stat(filepath.Join(root, "home"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Nil(t, agent.runtimeNativeRelease)
	require.Empty(t, agent.runtimeScratchRoot)

	authority.prepareErr = nil
	_, err = agent.sharedRuntime(t.Context())
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.Equal(t, []string{"prepare:" + filepath.Join(root, "home")}, authority.events())
}

func TestHostAuthorityRolloutPrepareFailureRemainsOpaque(t *testing.T) {
	root := t.TempDir()
	authority := newTraceAuthority(root)
	injected := errors.New("rollout prepare failed after transfer")
	authority.prepareErr = injected
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(root))
	originalRemove := removeMaterializedRolloutFile
	removeCalls := 0
	removeMaterializedRolloutFile = func(path string) error {
		removeCalls++

		return originalRemove(path)
	}
	t.Cleanup(func() { removeMaterializedRolloutFile = originalRemove })

	release, err := agent.reserveNativeResidenceCapacity(t.Context(), 3)
	require.NoError(t, err)
	path, returnedRelease, _, err := agent.materializeStoredRollout(
		t.Context(), []SessionStoreEntry{[]byte(`{}`)}, release,
	)
	require.Empty(t, path)
	require.Nil(t, returnedRelease)
	require.ErrorIs(t, err, injected)
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.Equal(t, 1, agent.nativeResidenceCount)
	require.Len(t, authority.events(), 1)
	require.True(t, strings.HasPrefix(authority.events()[0], "prepare:"))
	require.NotContains(t, strings.Join(authority.events(), "\n"), "reclaim:")
	require.Zero(t, removeCalls)
	_, err = agent.reserveNativeResidenceCapacity(t.Context(), 1)
	require.ErrorIs(t, err, ErrContainmentIncomplete)
}

func TestHostAuthorityPromptImagePrepareFailureRemainsOpaque(t *testing.T) {
	root := t.TempDir()
	authority := newTraceAuthority(root)
	injected := errors.New("image prepare failed after transfer")
	authority.prepareErr = injected
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(root))
	active := &session{agent: agent}
	originalRemove := removePromptImageDir
	removeCalls := 0
	removePromptImageDir = func(path string) error {
		removeCalls++

		return originalRemove(path)
	}
	t.Cleanup(func() { removePromptImageDir = originalRemove })

	_, err := active.preparePromptImages(t.Context(), []decodedPromptImage{{
		data: make([]byte, codexInlineImageEnvelopeSize), mimeType: mimeImagePNG,
	}})
	require.ErrorIs(t, err, injected)
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.Equal(t, 1, agent.nativeResidenceCount)
	require.Len(t, authority.events(), 1)
	require.True(t, strings.HasPrefix(authority.events()[0], "prepare:"))
	require.NotContains(t, strings.Join(authority.events(), "\n"), "reclaim:")
	require.Zero(t, removeCalls)
}

func TestHostAuthorityPromptImageResidenceRelease(t *testing.T) {
	root := t.TempDir()
	authority := newTraceAuthority(root)
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(root))
	active := &session{agent: agent}

	prepared, err := active.preparePromptImages(t.Context(), []decodedPromptImage{{
		data: make([]byte, codexInlineImageEnvelopeSize), mimeType: mimeImagePNG,
	}})
	require.NoError(t, err)
	prepared.release()
	prepared.release()
	require.NoError(t, agent.reclaimRetiredResidences(t.Context(), agent.runtimeEpoch))
	require.Zero(t, agent.nativeResidenceCount)

	blocked := NewAgent(WithHostAuthority(authority), WithScratchDir(root))
	blocked.runtimeCleanupErr = ErrContainmentIncomplete
	_, err = (&session{agent: blocked}).preparePromptImages(t.Context(), []decodedPromptImage{{
		data: make([]byte, codexInlineImageEnvelopeSize), mimeType: mimeImagePNG,
	}})
	require.ErrorIs(t, err, ErrContainmentIncomplete)
}

func TestNativeTreeBusyBlocksAdmissionUntilReclaimRetry(t *testing.T) {
	root := t.TempDir()
	authority := newTraceAuthority(root)
	authority.reclaimBusy = 1
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(root))
	client := &orderingRuntimeClient{spyCodexClient: newSpyCodexClient(), calls: new([]string)}
	agent.runtimeClient = client
	agent.runtimeEpoch = 1
	agent.runtimeDead = false
	agent.nativeResidenceCount = retiredResidenceCountLimit

	path, err := materializeRollout(root, []SessionStoreEntry{[]byte(`{}`)})
	require.NoError(t, err)
	require.NoError(t, authority.PrepareNativeTree(t.Context(), filepath.Dir(path)))
	agent.retiredResidences = []retiredNativeResidence{{
		epoch: 1, tree: filepath.Dir(path), path: path, release: func() {
			agent.mu.Lock()
			agent.nativeResidenceCount--
			agent.mu.Unlock()
		}, remove: removeMaterializedRollout,
	}}

	_, err = agent.reserveNativeResidenceCapacity(t.Context(), 1)
	require.ErrorIs(t, err, ErrNativeTreeBusy)
	require.NotErrorIs(t, err, ErrContainmentIncomplete)
	require.Same(t, client, agent.runtimeClient)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)

	release, err := agent.reserveNativeResidenceCapacity(t.Context(), 1)
	require.NoError(t, err)
	release()
	require.Nil(t, agent.runtimeClient)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

type authorityCoverageHost struct {
	environment func() map[string]string
	prepare     func() error
	read        func() ([][]byte, error)
	reclaim     func() error
	start       func() (NativeProcess, error)
}

func (h authorityCoverageHost) NativeEnvironment() map[string]string {
	return h.environment()
}

func (h authorityCoverageHost) PrepareNativeTree(context.Context, string) error {
	return h.prepare()
}

func (h authorityCoverageHost) ReadNativeAppendLog(context.Context, string, uint64) ([][]byte, error) {
	return h.read()
}

func (h authorityCoverageHost) ReclaimNativeTree(context.Context, string) error {
	return h.reclaim()
}

func (h authorityCoverageHost) StartNative(context.Context, NativeRequest) (NativeProcess, error) {
	return h.start()
}

type authorityCoverageProcess struct {
	stdin  func() io.WriteCloser
	stdout func() io.ReadCloser
	stderr func() io.ReadCloser
	wait   func() (NativeResult, error)
	revoke func() error
}

func (p authorityCoverageProcess) Stdin() io.WriteCloser                      { return p.stdin() }
func (p authorityCoverageProcess) Stdout() io.ReadCloser                      { return p.stdout() }
func (p authorityCoverageProcess) Stderr() io.ReadCloser                      { return p.stderr() }
func (p authorityCoverageProcess) Wait(context.Context) (NativeResult, error) { return p.wait() }
func (p authorityCoverageProcess) Revoke(context.Context) error               { return p.revoke() }

func TestHostAuthorityDefensiveWrappers(t *testing.T) {
	panicNow := func() { panic("host panic") }
	process := authorityCoverageProcess{
		stdin: func() io.WriteCloser {
			panicNow()

			return nil
		},
		stdout: func() io.ReadCloser {
			panicNow()

			return nil
		},
		stderr: func() io.ReadCloser {
			panicNow()

			return nil
		},
		wait: func() (NativeResult, error) {
			panicNow()

			return NativeResult{}, nil
		},
		revoke: func() error {
			panicNow()

			return nil
		},
	}
	host := authorityCoverageHost{
		environment: func() map[string]string { return map[string]string{"SAFE": "value"} },
		prepare: func() error {
			panicNow()

			return nil
		},
		read: func() ([][]byte, error) {
			panicNow()

			return nil, nil
		},
		reclaim: func() error {
			panicNow()

			return nil
		},
		start: func() (NativeProcess, error) { return process, nil },
	}

	normalized, err := normalizeHostAuthority(host, true)
	require.NoError(t, err)
	guarded, ok := normalized.(*guardedHostAuthority)
	require.True(t, ok)
	require.Equal(t, map[string]string{"SAFE": "value"}, guarded.NativeEnvironment())
	require.ErrorIs(t, guarded.PrepareNativeTree(t.Context(), "tree"), ErrHostAuthorityUnavailable)
	_, err = guarded.ReadNativeAppendLog(t.Context(), "tree/log", 0)
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	require.ErrorIs(t, guarded.ReclaimNativeTree(t.Context(), "tree"), ErrHostAuthorityUnavailable)

	started, err := guarded.StartNative(t.Context(), NativeRequest{})
	require.NoError(t, err)
	require.Nil(t, started.Stdin())
	require.Nil(t, started.Stdout())
	require.Nil(t, started.Stderr())
	_, err = started.Wait(t.Context())
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	require.ErrorIs(t, started.Revoke(t.Context()), ErrHostAuthorityUnavailable)

	panicHost := host
	panicHost.environment = func() map[string]string {
		panicNow()

		return nil
	}
	_, err = normalizeHostAuthority(panicHost, true)
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)

	panicHost = host
	panicHost.start = func() (NativeProcess, error) {
		panicNow()

		return nil, nil //nolint:nilnil // The panic prevents a return.
	}
	panicGuarded, err := normalizeHostAuthority(panicHost, true)
	require.NoError(t, err)
	_, err = panicGuarded.StartNative(t.Context(), NativeRequest{})
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	require.ErrorIs(t, err, ErrContainmentIncomplete)
}

func TestHostAuthorityValidationAndAdapters(t *testing.T) {
	require.False(t, nilable(reflect.Int))
	require.True(t, interfaceNil(nil))
	require.Nil(t, adaptHostAuthority(nil))

	base := authorityCoverageHost{
		environment: func() map[string]string { return map[string]string{"SAFE": "value"} },
		prepare:     func() error { return ErrContainmentIncomplete },
		read:        func() ([][]byte, error) { return nil, ErrHostAuthorityUnavailable },
		reclaim:     func() error { return ErrNativeTreeBusy },
		start:       func() (NativeProcess, error) { return nil, ErrHostAuthorityUnavailable },
	}
	adapter := adaptHostAuthority(base)
	require.ErrorIs(t, adapter.PrepareNativeTree(t.Context(), "tree"), codex.ErrContainmentIncomplete)
	_, err := adapter.ReadNativeAppendLog(t.Context(), "tree/log", 0)
	require.ErrorIs(t, err, codex.ErrHostAuthorityUnavailable)
	require.ErrorIs(t, adapter.ReclaimNativeTree(t.Context(), "tree"), codex.ErrNativeTreeBusy)
	_, err = adapter.StartNative(t.Context(), codex.NativeRequest{})
	require.ErrorIs(t, err, codex.ErrHostAuthorityUnavailable)

	base.start = func() (NativeProcess, error) {
		return nil, nil //nolint:nilnil // Exercises an invalid host result.
	}
	adapter = adaptHostAuthority(base)
	_, err = adapter.StartNative(t.Context(), codex.NativeRequest{})
	require.ErrorIs(t, err, codex.ErrHostAuthorityUnavailable)

	want := NativeResult{ExitCode: 7}
	base.start = func() (NativeProcess, error) {
		return authorityCoverageProcess{
			stdin: func() io.WriteCloser { return nil }, stdout: func() io.ReadCloser { return nil },
			stderr: func() io.ReadCloser { return nil },
			wait:   func() (NativeResult, error) { return want, ErrNativeTreeBusy },
			revoke: func() error { return nil },
		}, nil
	}
	adapter = adaptHostAuthority(base)
	internalProcess, err := adapter.StartNative(t.Context(), codex.NativeRequest{})
	require.NoError(t, err)
	result, err := internalProcess.Wait(t.Context())
	require.Equal(t, codex.NativeResult(want), result)
	require.ErrorIs(t, err, codex.ErrNativeTreeBusy)

	for _, pair := range []struct {
		internal error
		public   error
	}{
		{codex.ErrHostAuthorityUnavailable, ErrHostAuthorityUnavailable},
		{codex.ErrContainmentIncomplete, ErrContainmentIncomplete},
		{codex.ErrNativeTreeBusy, ErrNativeTreeBusy},
	} {
		require.ErrorIs(t, toPublicAuthorityError(pair.internal), pair.public)
		require.ErrorIs(t, toPublicAuthorityError(pair.public), pair.public)
	}
	injected := errors.New("injected")
	require.Same(t, injected, toInternalAuthorityError(injected))
	require.Same(t, injected, toPublicAuthorityError(injected))

	for _, environment := range []map[string]string{
		{"": "value"}, {"BAD=KEY": "value"}, {"BAD\x00KEY": "value"}, {"KEY": "bad\x00value"},
		{"acp_go_codex_internal_test": "value"}, {"xdg_cache_home": "/tmp"},
	} {
		require.Error(t, validateRuntimeEnvironment(environment))
	}
	require.NoError(t, validateRuntimeEnvironment(map[string]string{"SAFE": "value"}))
	require.True(t, reservedCodexEnvKey("codex_home"))
	require.False(t, reservedCodexEnvKey("safe"))

	for _, environment := range []map[string]string{
		{"": "value"}, {"BAD=KEY": "value"}, {"KEY": "bad\x00value"},
	} {
		invalid := base
		invalid.environment = func() map[string]string { return environment }
		_, err := normalizeHostAuthority(invalid, true)
		require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	}
}
