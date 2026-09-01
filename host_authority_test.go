package codexacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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
func (*unavailableAuthority) ReclaimNativeTree(context.Context, string) error { return nil }
func (*unavailableAuthority) StartNative(context.Context, NativeRequest) (NativeProcess, error) {
	return nil, ErrHostAuthorityUnavailable
}

type invalidEnvironmentAuthority struct{ starts *int }

func (a invalidEnvironmentAuthority) NativeEnvironment() map[string]string { return nil }
func (a invalidEnvironmentAuthority) PrepareNativeTree(context.Context, string) error {
	return errors.New("unexpected prepare")
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
	authority := newTraceAuthority(t.TempDir())
	authority.startErr = injected
	agent := NewAgent(
		WithHostAuthority(authority),
		WithExecutablePath(filepath.Join(t.TempDir(), "ordinary-fallback-must-not-run")),
		WithScratchDir(t.TempDir()),
	)

	_, err := agent.probeRuntimeVersion(t.Context())
	require.ErrorIs(t, err, injected)
	require.Len(t, authority.events(), 1)
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

	_, err := active.preparePromptImages(t.Context(), []decodedPromptImage{{
		data: make([]byte, codexInlineImageEnvelopeSize), mimeType: mimeImagePNG,
	}})
	require.ErrorIs(t, err, injected)
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.Equal(t, 1, agent.nativeResidenceCount)
	require.Len(t, authority.events(), 1)
	require.True(t, strings.HasPrefix(authority.events()[0], "prepare:"))
	require.NotContains(t, strings.Join(authority.events(), "\n"), "reclaim:")
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
