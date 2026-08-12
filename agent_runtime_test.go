package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

type runtimeFailureClient struct {
	*runtimeRecordingClient
	resumeErr error
	runErr    error
	events    []codex.Event
}

type mismatchedResumeClient struct{ *runtimeRecordingClient }

type blockingCloseRuntimeClient struct {
	*spyCodexClient
	started chan struct{}
	release chan struct{}
	count   atomic.Int64
}

func (c *blockingCloseRuntimeClient) Close(context.Context) error {
	c.count.Add(1)
	close(c.started)
	<-c.release

	return codex.ErrProcessContainmentIncomplete
}

func (c *mismatchedResumeClient) ResumeThread(
	context.Context,
	codex.ThreadResumeRequest,
) (codex.Thread, error) {
	return codex.Thread{ID: "different-thread"}, nil
}

func (c *runtimeFailureClient) ResumeThread(ctx context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
	if c.resumeErr != nil {
		return codex.Thread{}, c.resumeErr
	}

	return c.runtimeRecordingClient.ResumeThread(ctx, req)
}

func (c *runtimeFailureClient) RunTurn(context.Context, codex.TurnStartRequest) (<-chan codex.Event, error) {
	if c.runErr != nil {
		return nil, c.runErr
	}

	events := make(chan codex.Event, len(c.events))
	for _, event := range c.events {
		events <- event
	}
	close(events)

	return events, nil
}

type runtimeRecordingClient struct {
	*spyCodexClient

	mu         sync.Mutex
	starts     []codex.ThreadStartRequest
	resumes    []codex.ThreadResumeRequest
	turns      []codex.TurnStartRequest
	order      []string
	closeCount int
}

func TestAgentSessionDefaultsToOrdinaryExecution(t *testing.T) {
	const canary = "ACP_GO_CODEX_TEST_ACTUAL_AMBIENT"
	t.Setenv(canary, "captured")

	var launched codex.Options
	agent := NewAgent(
		WithScratchDir(t.TempDir()),
		withClientFactory(func(_ context.Context, options codex.Options) (codex.Client, error) {
			launched = options

			return newRuntimeRecordingClient(), nil
		}),
	)
	t.Cleanup(func() { require.NoError(t, agent.Close()) })
	t.Setenv(canary, "mutated")

	_, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	require.Nil(t, launched.ProcessIsolation)
	require.Equal(t, "captured", launched.ImplicitEnvironment[canary])

	launched.ImplicitEnvironment[canary] = "caller mutation"
	require.Equal(t, "captured", agent.options.implicitEnvironment[canary])
}

func newRuntimeRecordingClient() *runtimeRecordingClient {
	return &runtimeRecordingClient{spyCodexClient: newSpyCodexClient()}
}

func (c *runtimeRecordingClient) StartThread(_ context.Context, req codex.ThreadStartRequest) (codex.Thread, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.starts = append(c.starts, req)
	id := "thread-" + string(rune('1'+len(c.starts)-1))

	return codex.Thread{ID: id, Cwd: req.Cwd, Model: req.Model}, nil
}

func (c *runtimeRecordingClient) ResumeThread(_ context.Context, req codex.ThreadResumeRequest) (codex.Thread, error) {
	c.mu.Lock()
	c.resumes = append(c.resumes, req)
	c.order = append(c.order, "resume:"+req.ThreadID)
	c.mu.Unlock()

	return codex.Thread{ID: req.ThreadID, Cwd: req.Cwd}, nil
}

func (c *runtimeRecordingClient) RunTurn(_ context.Context, req codex.TurnStartRequest) (<-chan codex.Event, error) {
	text, _ := req.Prompt[0]["text"].(string)
	c.mu.Lock()
	c.turns = append(c.turns, req)
	c.mu.Unlock()
	events := make(chan codex.Event, 2)
	if strings.Contains(text, "runtime_ready") {
		nonce := strings.SplitN(strings.SplitN(text, "nonce ", 2)[1], ".", 2)[0]
		c.mu.Lock()
		c.order = append(c.order, "canary:"+req.ThreadID)
		c.mu.Unlock()
		events <- codex.Event{
			Kind:     codex.EventToolCompleted,
			ThreadID: req.ThreadID,
			Tool: codex.ToolEvent{
				Title:   "runtime_ready",
				Content: nonce,
			},
		}
	}
	events <- codex.Event{Kind: codex.EventCompleted, ThreadID: req.ThreadID, StopReason: codex.StopReasonEndTurn}
	close(events)

	return events, nil
}

func TestSharedRuntimeEightThreadRaceStressPreservesCWDAndTurnRouting(t *testing.T) {
	const threadCount = 8

	client := newRuntimeRecordingClient()
	var launches atomic.Int64
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		launches.Add(1)

		return client, nil
	}))
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	type sessionResult struct {
		index int
		cwd   string
		resp  acp.NewSessionResponse
		err   error
	}
	start := make(chan struct{})
	results := make(chan sessionResult, threadCount)
	root := t.TempDir()
	for index := range threadCount {
		cwd := filepath.Join(root, fmt.Sprintf("workspace-%02d", index))
		require.NoError(t, os.Mkdir(cwd, 0o700))
		go func() {
			<-start
			resp, err := agent.NewSession(context.Background(), NewSessionRequest(cwd))
			results <- sessionResult{index: index, cwd: cwd, resp: resp, err: err}
		}()
	}
	close(start)

	sessions := make([]sessionResult, threadCount)
	threadToMarker := make(map[string]string, threadCount)
	for range threadCount {
		result := <-results
		require.NoError(t, result.err)
		sessions[result.index] = result
		bound, err := agent.session(result.resp.SessionId)
		require.NoError(t, err)
		snapshot := bound.snapshot()
		require.Equal(t, result.cwd, snapshot.cwd)
		require.NotEmpty(t, snapshot.codexThreadID)
		threadToMarker[snapshot.codexThreadID] = fmt.Sprintf("marker-%02d", result.index)
	}
	require.EqualValues(t, 1, launches.Load())
	require.Len(t, threadToMarker, threadCount)

	promptErrors := make(chan error, threadCount)
	for index, result := range sessions {
		go func() {
			_, err := agent.Prompt(context.Background(), TextPromptRequest(
				result.resp.SessionId,
				fmt.Sprintf("turn-%02d", index),
				fmt.Sprintf("marker-%02d", index),
			))
			promptErrors <- err
		}()
	}
	for range threadCount {
		require.NoError(t, <-promptErrors)
	}

	client.mu.Lock()
	starts := append([]codex.ThreadStartRequest(nil), client.starts...)
	turns := append([]codex.TurnStartRequest(nil), client.turns...)
	client.mu.Unlock()
	require.Len(t, starts, threadCount)
	require.Len(t, turns, threadCount)

	seenCWD := make(map[string]struct{}, threadCount)
	for _, request := range starts {
		seenCWD[request.Cwd] = struct{}{}
	}
	for _, result := range sessions {
		require.Contains(t, seenCWD, result.cwd)
	}
	for _, request := range turns {
		text, ok := request.Prompt[0]["text"].(string)
		require.True(t, ok)
		require.Equal(t, threadToMarker[request.ThreadID], text)
	}
}

func (c *runtimeRecordingClient) Close(context.Context) error {
	c.mu.Lock()
	c.closeCount++
	c.mu.Unlock()

	return nil
}

func TestReadySharedRuntimeNewSessionDeterministicProviderFreePerformanceGate(t *testing.T) {
	const (
		repetitions = 5
		budget      = 500 * time.Millisecond
	)

	client := newRuntimeRecordingClient()
	var launches atomic.Int64
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		launches.Add(1)

		return client, nil
	}))
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	root := t.TempDir()
	warmCWD := filepath.Join(root, "warm")
	require.NoError(t, os.Mkdir(warmCWD, 0o700))
	_, err := agent.NewSession(context.Background(), NewSessionRequest(warmCWD))
	require.NoError(t, err)
	require.EqualValues(t, 1, launches.Load())

	cwds := make([]string, repetitions)
	for index := range repetitions {
		cwds[index] = filepath.Join(root, fmt.Sprintf("ready-%d", index))
		require.NoError(t, os.Mkdir(cwds[index], 0o700))
	}

	durations := make([]time.Duration, 0, repetitions)
	for _, cwd := range cwds {
		started := time.Now()
		_, newSessionErr := agent.NewSession(context.Background(), NewSessionRequest(cwd))
		elapsed := time.Since(started)

		require.NoError(t, newSessionErr)
		durations = append(durations, elapsed)
	}

	sort.Slice(durations, func(left, right int) bool { return durations[left] < durations[right] })
	p95Index := (95*len(durations)+99)/100 - 1
	p95 := durations[p95Index]
	t.Logf("deterministic provider-free ready shared runtime NewSession p95=%s samples=%v", p95, durations)

	client.mu.Lock()
	startCount := len(client.starts)
	turnCount := len(client.turns)
	client.mu.Unlock()
	require.Equal(t, repetitions+1, startCount)
	require.Zero(t, turnCount, "performance gate crossed the prompt/inference boundary")
	require.EqualValues(t, 1, launches.Load(), "ready-session path launched another native runtime")
	require.Less(t, p95, budget)
}

func TestAgentSharesOneRuntimeAcrossThreadsAndReleasesItAtAgentClose(t *testing.T) {
	client := newRuntimeRecordingClient()
	var launches atomic.Int64
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		launches.Add(1)

		return client, nil
	}))

	serverA := HTTPMCPServer("marker", "https://a.example/mcp", map[string]string{"Authorization": "Bearer A"})
	serverB := HTTPMCPServer("marker", "https://b.example/mcp", map[string]string{"Authorization": "Bearer B"})
	first, err := agent.NewSession(context.Background(), NewSessionRequest("/work/a", WithSessionMCPServers(serverA)))
	require.NoError(t, err)
	second, err := agent.NewSession(context.Background(), NewSessionRequest("/work/b", WithSessionMCPServers(serverB)))
	require.NoError(t, err)
	require.EqualValues(t, 1, launches.Load())
	require.Len(t, client.starts, 2)

	firstServers, ok := client.starts[0].Config["mcp_servers"].(map[string]any)
	require.True(t, ok)
	secondServers, ok := client.starts[1].Config["mcp_servers"].(map[string]any)
	require.True(t, ok)
	firstMarker, ok := firstServers["marker"].(map[string]any)
	require.True(t, ok)
	secondMarker, ok := secondServers["marker"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "https://a.example/mcp", firstMarker["url"])
	require.Equal(t, "https://b.example/mcp", secondMarker["url"])

	_, err = agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: first.SessionId})
	require.NoError(t, err)
	require.Zero(t, client.closeCount, "logical release closed the shared runtime")
	_, err = agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: second.SessionId})
	require.NoError(t, err)
	require.Zero(t, client.closeCount)
	require.NoError(t, agent.Close())
	require.Equal(t, 1, client.closeCount)
}

func TestAgentCloseConcurrentCallersJoinMemoizedContainmentResult(t *testing.T) {
	client := &blockingCloseRuntimeClient{
		spyCodexClient: newSpyCodexClient(),
		started:        make(chan struct{}),
		release:        make(chan struct{}),
	}
	agent := NewAgent()
	agent.runtimeClient = client

	results := make(chan error, 2)
	go func() { results <- agent.Close() }()
	<-client.started
	go func() { results <- agent.Close() }()

	select {
	case result := <-results:
		t.Fatalf("concurrent Close returned before the owned cleanup completed: %v", result)
	case <-time.After(25 * time.Millisecond):
	}

	close(client.release)
	require.ErrorIs(t, <-results, codex.ErrProcessContainmentIncomplete)
	require.ErrorIs(t, <-results, codex.ErrProcessContainmentIncomplete)
	require.ErrorIs(t, agent.Close(), codex.ErrProcessContainmentIncomplete)
	require.EqualValues(t, 1, client.count.Load())
}

func TestAgentCloseJoinsIncompleteRuntimeLaunchBeforeMemoizing(t *testing.T) {
	factoryStarted := make(chan struct{})
	releaseFactory := make(chan struct{})
	client := &errorCodexClient{
		spyCodexClient: newSpyCodexClient(),
		closeErr:       codex.ErrProcessContainmentIncomplete,
	}
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		close(factoryStarted)
		<-releaseFactory

		return client, nil
	}))

	runtimeErr := make(chan error, 1)
	go func() {
		_, err := agent.sharedRuntime(context.Background())
		runtimeErr <- err
	}()
	<-factoryStarted

	closeErr := make(chan error, 1)
	go func() { closeErr <- agent.Close() }()
	require.Eventually(t, func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()

		return agent.closed
	}, time.Second, time.Millisecond)

	select {
	case err := <-closeErr:
		t.Fatalf("Close returned before the admitted runtime launch cleaned up: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseFactory)
	require.ErrorIs(t, <-runtimeErr, codex.ErrProcessContainmentIncomplete)
	require.ErrorIs(t, <-closeErr, codex.ErrProcessContainmentIncomplete)
	require.ErrorIs(t, agent.Close(), codex.ErrProcessContainmentIncomplete)
}

func TestCloseSharedRuntimeFencesNewConstructionAndConcurrentClosers(t *testing.T) {
	factoryStarted := make(chan struct{})
	factoryCancelled := make(chan struct{})
	releaseFactory := make(chan struct{})
	var factoryCalls atomic.Int64
	agent := NewAgent(withClientFactory(func(ctx context.Context, _ codex.Options) (codex.Client, error) {
		if factoryCalls.Add(1) != 1 {
			return newSpyCodexClient(), nil
		}

		close(factoryStarted)
		<-ctx.Done()
		close(factoryCancelled)
		<-releaseFactory

		return nil, ctx.Err()
	}))

	runtimeErr := make(chan error, 1)
	go func() {
		_, err := agent.sharedRuntime(context.Background())
		runtimeErr <- err
	}()
	<-factoryStarted

	firstClose := make(chan error, 1)
	go func() { firstClose <- agent.closeSharedRuntime(context.Background()) }()
	<-factoryCancelled
	quiesceErr := make(chan error, 1)
	go func() {
		quiesceErr <- agent.quiesceRuntimeAfterCancel(context.Background(), newSpyCodexClient())
	}()

	waitCtx, cancelWait := context.WithCancel(context.Background())
	waitingRuntime := make(chan error, 1)
	go func() {
		_, err := agent.sharedRuntime(waitCtx)
		waitingRuntime <- err
	}()
	cancelWait()
	require.ErrorIs(t, <-waitingRuntime, context.Canceled)
	require.EqualValues(t, 1, factoryCalls.Load())
	survivingRuntime := make(chan error, 1)
	go func() {
		_, err := agent.sharedRuntime(context.Background())
		survivingRuntime <- err
	}()
	require.Never(t, func() bool { return factoryCalls.Load() != 1 }, 25*time.Millisecond, time.Millisecond)

	secondClose := make(chan error, 1)
	go func() { secondClose <- agent.closeSharedRuntime(context.Background()) }()

	close(releaseFactory)
	require.ErrorIs(t, <-runtimeErr, context.Canceled)
	require.NoError(t, <-firstClose)
	require.NoError(t, <-quiesceErr)
	require.NoError(t, <-secondClose)
	require.NoError(t, <-survivingRuntime)
	require.EqualValues(t, 2, factoryCalls.Load())
	require.NoError(t, agent.Close())
}

func TestRuntimeVersionProbeOwnsDiscoveryAdmissionsAndGeneration(t *testing.T) {
	originalProbe := runtimeProbeCodexVersion
	t.Cleanup(func() { runtimeProbeCodexVersion = originalProbe })

	parent := t.TempDir()
	var acquired []RuntimeResourceKind
	var reserved []RuntimeResourceKind
	var releasedNative atomic.Int64
	var releasedScratch atomic.Int64
	var probeOptions codex.VersionProbeOptions
	runtimeProbeCodexVersion = func(_ context.Context, options codex.VersionProbeOptions) (string, error) {
		probeOptions = options

		return minSupportedCodexVersion, nil
	}

	agent := NewAgent(
		WithScratchDir(parent),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			AcquireNativeRoot: func(_ context.Context, kind RuntimeResourceKind) (func(), error) {
				acquired = append(acquired, kind)

				return func() { releasedNative.Add(1) }, nil
			},
			ReserveScratchRoot: func(_ context.Context, kind RuntimeResourceKind) (func(), error) {
				reserved = append(reserved, kind)

				return func() { releasedScratch.Add(1) }, nil
			},
		}),
	)

	version, err := agent.probeRuntimeVersion(context.Background())
	require.NoError(t, err)
	require.Equal(t, minSupportedCodexVersion, version)
	require.Equal(t, []RuntimeResourceKind{RuntimeResourceDiscovery}, acquired)
	require.Equal(t, []RuntimeResourceKind{RuntimeResourceDiscovery}, reserved)
	require.EqualValues(t, 1, releasedNative.Load())
	require.EqualValues(t, 1, releasedScratch.Load())
	require.Equal(t, parent, probeOptions.ScratchParent)
	require.Contains(t, filepath.Base(probeOptions.Scratch), "acp-go-codex-runtime-discovery-")
	require.NoDirExists(t, probeOptions.Scratch)
}

// The version probe launches a native process under the configured identity, so
// it must refuse an unproven writable home before it takes any admission slot.
func TestRuntimeVersionProbeRefusesUnprovenNativeHome(t *testing.T) {
	var reserved atomic.Int64
	agent := NewAgent(
		WithHome(t.TempDir()),
		WithProcessIsolation(foreignNativeIdentity()),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
				reserved.Add(1)

				return func() {}, nil
			},
		}),
	)

	_, err := agent.probeRuntimeVersion(t.Context())
	require.Error(t, err)
	require.EqualValues(t, 0, reserved.Load())
}

// Both native-launch refusals must fire before the app-server is spawned.
func TestRuntimeLaunchRefusesSeedFilesAndUnprovenNativeHome(t *testing.T) {
	var launches atomic.Int64
	factory := withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		launches.Add(1)

		return newSpyCodexClient(), nil
	})

	seeded := NewAgent(
		WithHome(t.TempDir()),
		WithProcessIsolation(ProcessIsolation{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}),
		WithSeedFiles(map[string]string{"config.toml": "model = \"gpt-5\""}),
		factory,
	)
	_, err := seeded.launchRuntimeClient(t.Context(), 1, t.TempDir(), minSupportedCodexVersion)
	require.ErrorContains(t, err, "seed files are unsupported with process isolation")

	unproven := NewAgent(WithHome(t.TempDir()), WithProcessIsolation(foreignNativeIdentity()), factory)
	_, err = unproven.launchRuntimeClient(t.Context(), 1, t.TempDir(), minSupportedCodexVersion)
	require.Error(t, err)

	require.EqualValues(t, 0, launches.Load())
}

// foreignNativeIdentity is a native identity the calling process cannot prove
// ownership for on any platform.
func foreignNativeIdentity() ProcessIsolation {
	return ProcessIsolation{UID: uint32(os.Geteuid()) + 1, GID: uint32(os.Getegid()) + 1}
}

// Without a caller-supplied client factory the adapter owns the native launch,
// so it probes the CLI version and hands it to the app-server options.
func TestSharedRuntimeProbesNativeVersionForAdapterOwnedLaunch(t *testing.T) {
	originalProbe := runtimeProbeCodexVersion
	t.Cleanup(func() { runtimeProbeCodexVersion = originalProbe })
	runtimeProbeCodexVersion = func(context.Context, codex.VersionProbeOptions) (string, error) {
		return "0.199.0", nil
	}

	agent := NewAgent(WithScratchDir(t.TempDir()))
	var launched codex.Options
	agent.options.clientFactory = func(_ context.Context, options codex.Options) (codex.Client, error) {
		launched = options

		return newSpyCodexClient(), nil
	}
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	client, err := agent.sharedRuntime(t.Context())
	require.NoError(t, err)
	require.NotNil(t, client)
	require.Equal(t, "0.199.0", launched.NativeVersion)
}

func TestRuntimeVersionProbeAdmissionFailureBranches(t *testing.T) {
	t.Run("scratch admission", func(t *testing.T) {
		agent := NewAgent(WithRuntimeResourceHooks(RuntimeResourceHooks{
			ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
				return nil, errors.New("scratch admission")
			},
		}))
		_, err := agent.probeRuntimeVersion(context.Background())
		require.ErrorContains(t, err, "scratch admission")
	})

	t.Run("scratch generation", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(parent, nil, 0o600))
		var released atomic.Int64
		agent := NewAgent(
			WithScratchDir(parent),
			WithRuntimeResourceHooks(RuntimeResourceHooks{
				ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
					return func() { released.Add(1) }, nil
				},
			}),
		)
		_, err := agent.probeRuntimeVersion(context.Background())
		require.Error(t, err)
		require.EqualValues(t, 1, released.Load())
	})

	t.Run("native admission", func(t *testing.T) {
		var released atomic.Int64
		agent := NewAgent(WithRuntimeResourceHooks(RuntimeResourceHooks{
			ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
				return func() { released.Add(1) }, nil
			},
			AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
				return nil, errors.New("native admission")
			},
		}))
		_, err := agent.probeRuntimeVersion(context.Background())
		require.ErrorContains(t, err, "native admission")
		require.EqualValues(t, 1, released.Load())
	})
}

func TestRuntimeReplacementResumesEachSessionLazilyWithItsOwnConfig(t *testing.T) {
	first := newRuntimeRecordingClient()
	replacement := newRuntimeRecordingClient()
	clients := []codex.Client{first, replacement}
	launch := 0
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		client := clients[launch]
		launch++

		return client, nil
	}))
	agent.setAgentClient(newRecordingAgentClient())

	serverA := HTTPMCPServer("marker", "https://a.example/mcp", map[string]string{"Authorization": "Bearer A"})
	serverB := HTTPMCPServer("marker", "https://b.example/mcp", map[string]string{"Authorization": "Bearer B"})
	a, err := agent.NewSession(context.Background(), NewSessionRequest("/work/a", WithSessionMCPServers(serverA)))
	require.NoError(t, err)
	b, err := agent.NewSession(context.Background(), NewSessionRequest("/work/b", WithSessionMCPServers(serverB)))
	require.NoError(t, err)

	agent.markRuntimeDead(first)
	_, err = agent.Prompt(context.Background(), TextPromptRequest(a.SessionId, "turn-a", "hello"))
	require.NoError(t, err)
	require.Equal(t, 2, launch)
	require.Len(t, replacement.resumes, 1)
	require.Len(t, replacement.order, 2)
	require.True(t, strings.HasPrefix(replacement.order[0], "resume:"), replacement.order)
	require.True(t, strings.HasPrefix(replacement.order[1], "canary:"), replacement.order)
	bSession := agent.activeSession(b.SessionId)
	require.NotNil(t, bSession)
	require.True(t, bSession.clientDead)

	_, err = agent.Prompt(context.Background(), TextPromptRequest(b.SessionId, "turn-b", "hello"))
	require.NoError(t, err)
	require.Equal(t, 2, launch)
	require.Len(t, replacement.resumes, 2)
	require.Len(t, replacement.order, 4)
	require.Equal(t, []string{
		"resume:thread-1",
		"canary:thread-1",
		"resume:thread-2",
		"canary:thread-2",
	}, replacement.order)

	configs := map[string]map[string]any{}
	for _, resume := range replacement.resumes {
		servers, ok := resume.Config["mcp_servers"].(map[string]any)
		require.True(t, ok)
		marker, ok := servers["marker"].(map[string]any)
		require.True(t, ok)
		configs[resume.ThreadID] = marker
	}
	require.Equal(t, "https://a.example/mcp", configs["thread-1"]["url"])
	require.Equal(t, "https://b.example/mcp", configs["thread-2"]["url"])
	require.Equal(t, map[string]string{"Authorization": "Bearer A"}, configs["thread-1"]["http_headers"])
	require.Equal(t, map[string]string{"Authorization": "Bearer B"}, configs["thread-2"]["http_headers"])
}

func TestRuntimeResourceHooksRejectAndReleaseAtExactBoundaries(t *testing.T) {
	root := t.TempDir()
	var scratchAcquired, scratchReleased, nativeAcquired, nativeReleased atomic.Int64
	factoryCalled := false
	agent := NewAgent(
		WithScratchDir(root),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			ReserveScratchRoot: func(_ context.Context, kind RuntimeResourceKind) (func(), error) {
				require.Equal(t, RuntimeResourceRuntime, kind)
				scratchAcquired.Add(1)

				return func() { scratchReleased.Add(1) }, nil
			},
			AcquireNativeRoot: func(_ context.Context, kind RuntimeResourceKind) (func(), error) {
				require.Equal(t, RuntimeResourceRuntime, kind)
				nativeAcquired.Add(1)

				return func() { nativeReleased.Add(1) }, errors.New("native root limit")
			},
		}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			factoryCalled = true

			return newSpyCodexClient(), nil
		}),
	)

	_, err := agent.NewSession(context.Background(), NewSessionRequest("/work"))
	require.ErrorContains(t, err, "native root limit")
	require.False(t, factoryCalled)
	require.EqualValues(t, 1, scratchAcquired.Load())
	require.EqualValues(t, 1, scratchReleased.Load())
	require.EqualValues(t, 1, nativeAcquired.Load())
	require.Zero(t, nativeReleased.Load())
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestRuntimeResourceHooksReleaseOnlyAfterRuntimeCloseAndScratchDeletion(t *testing.T) {
	root := t.TempDir()
	client := newRuntimeRecordingClient()
	var scratchReleased, nativeReleased atomic.Int64
	agent := NewAgent(
		WithScratchDir(root),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
				return func() { scratchReleased.Add(1) }, nil
			},
			AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
				return func() { nativeReleased.Add(1) }, nil
			},
		}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)

	resp, err := agent.NewSession(context.Background(), NewSessionRequest("/work"))
	require.NoError(t, err)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Zero(t, scratchReleased.Load())
	require.Zero(t, nativeReleased.Load())

	_, err = agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: resp.SessionId})
	require.NoError(t, err)
	require.Zero(t, scratchReleased.Load())
	require.Zero(t, nativeReleased.Load())

	require.NoError(t, agent.Close())
	require.EqualValues(t, 1, scratchReleased.Load())
	require.EqualValues(t, 1, nativeReleased.Load())
	entries, err = os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestRuntimeResourceCleanupReleaseProofBoundaries(t *testing.T) {
	originalRemoveAll := runtimeRemoveAll
	t.Cleanup(func() { runtimeRemoveAll = originalRemoveAll })

	t.Run("ordinary close error releases both after deletion", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "runtime")
		require.NoError(t, os.Mkdir(root, 0o700))
		var nativeReleased, scratchReleased atomic.Int64
		closeErr := errors.New("ordinary close failure")

		err := closeRuntimeResources(
			context.Background(),
			&errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: closeErr},
			func() { nativeReleased.Add(1) },
			root,
			func() { scratchReleased.Add(1) },
		)

		require.ErrorIs(t, err, closeErr)
		require.EqualValues(t, 1, nativeReleased.Load())
		require.EqualValues(t, 1, scratchReleased.Load())
		_, statErr := os.Stat(root)
		require.ErrorIs(t, statErr, os.ErrNotExist)
	})

	t.Run("package stage cleanup failure retains scratch reservation", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "runtime")
		require.NoError(t, os.Mkdir(root, 0o700))
		var nativeReleased, scratchReleased atomic.Int64
		stageErr := errors.Join(codex.ErrPackageStageCleanupIncomplete, errors.New("delete package stage"))

		err := closeRuntimeResources(
			context.Background(),
			&errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: stageErr},
			func() { nativeReleased.Add(1) },
			root,
			func() { scratchReleased.Add(1) },
		)

		require.ErrorIs(t, err, codex.ErrPackageStageCleanupIncomplete)
		require.EqualValues(t, 1, nativeReleased.Load())
		require.Zero(t, scratchReleased.Load())
		_, statErr := os.Stat(root)
		require.ErrorIs(t, statErr, os.ErrNotExist)
	})

	t.Run("unproven process tree retains both reservations", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "runtime")
		require.NoError(t, os.Mkdir(root, 0o700))
		var nativeReleased, scratchReleased atomic.Int64
		removeCalled := false
		runtimeRemoveAll = func(string) error {
			removeCalled = true

			return nil
		}
		t.Cleanup(func() { runtimeRemoveAll = originalRemoveAll })

		err := closeRuntimeResources(
			context.Background(),
			&errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: codex.ErrProcessContainmentIncomplete},
			func() { nativeReleased.Add(1) },
			root,
			func() { scratchReleased.Add(1) },
		)

		require.ErrorIs(t, err, codex.ErrProcessContainmentIncomplete)
		require.Zero(t, nativeReleased.Load())
		require.Zero(t, scratchReleased.Load())
		require.False(t, removeCalled)
		require.DirExists(t, root)
	})

	t.Run("scratch deletion failure retains only scratch reservation", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "runtime")
		require.NoError(t, os.Mkdir(root, 0o700))
		var nativeReleased, scratchReleased atomic.Int64
		deleteErr := errors.New("delete runtime root")
		runtimeRemoveAll = func(path string) error {
			require.Equal(t, root, path)

			return deleteErr
		}
		t.Cleanup(func() { runtimeRemoveAll = originalRemoveAll })

		err := finalizeRuntimeResources(
			nil,
			func() { nativeReleased.Add(1) },
			root,
			func() { scratchReleased.Add(1) },
		)

		require.ErrorIs(t, err, deleteErr)
		require.EqualValues(t, 1, nativeReleased.Load())
		require.Zero(t, scratchReleased.Load())
		require.DirExists(t, root)
	})
}

func TestSharedRuntimeLatchesUnprovenTreeFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime")
	require.NoError(t, os.Mkdir(root, 0o700))
	var nativeReleased, scratchReleased atomic.Int64
	agent := NewAgent()
	agent.runtimeClient = &errorCodexClient{
		spyCodexClient: newSpyCodexClient(),
		closeErr:       codex.ErrProcessContainmentIncomplete,
	}
	agent.runtimeNativeRelease = func() { nativeReleased.Add(1) }
	agent.runtimeScratchRoot = root
	agent.runtimeScratchRelease = func() { scratchReleased.Add(1) }

	err := agent.closeSharedRuntime(context.Background())
	require.ErrorIs(t, err, codex.ErrProcessContainmentIncomplete)
	require.ErrorIs(t, agent.runtimeCleanupErr, codex.ErrProcessContainmentIncomplete)
	require.Zero(t, nativeReleased.Load())
	require.Zero(t, scratchReleased.Load())
	require.DirExists(t, root)
	_, err = agent.sharedRuntime(context.Background())
	require.ErrorIs(t, err, codex.ErrProcessContainmentIncomplete)
}

func TestSharedRuntimeLatchesPackageStageCleanupFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime")
	require.NoError(t, os.Mkdir(root, 0o700))
	var nativeReleased, scratchReleased atomic.Int64
	stageErr := errors.Join(codex.ErrPackageStageCleanupIncomplete, errors.New("delete package stage"))
	agent := NewAgent()
	agent.runtimeClient = &errorCodexClient{
		spyCodexClient: newSpyCodexClient(),
		closeErr:       stageErr,
	}
	agent.runtimeNativeRelease = func() { nativeReleased.Add(1) }
	agent.runtimeScratchRoot = root
	agent.runtimeScratchRelease = func() { scratchReleased.Add(1) }

	err := agent.closeSharedRuntime(context.Background())
	require.ErrorIs(t, err, codex.ErrPackageStageCleanupIncomplete)
	require.ErrorIs(t, agent.runtimeCleanupErr, codex.ErrPackageStageCleanupIncomplete)
	require.EqualValues(t, 1, nativeReleased.Load())
	require.Zero(t, scratchReleased.Load())
	require.NoDirExists(t, root)
	_, err = agent.sharedRuntime(context.Background())
	require.ErrorIs(t, err, codex.ErrPackageStageCleanupIncomplete)
}

func TestCancelRuntimeQuiescenceWaitsForExistingTransitionAndLatchesUnprovenTree(t *testing.T) {
	t.Run("wait for owner", func(t *testing.T) {
		agent := NewAgent()
		transition := make(chan struct{})
		close(transition)
		agent.runtimeStarting = transition

		require.NoError(t, agent.quiesceRuntimeAfterCancel(context.Background(), newSpyCodexClient()))

		agent.runtimeCleanupErr = codex.ErrProcessContainmentIncomplete
		require.ErrorIs(
			t,
			agent.quiesceRuntimeAfterCancel(context.Background(), newSpyCodexClient()),
			codex.ErrProcessContainmentIncomplete,
		)
	})

	t.Run("latch unproven tree", func(t *testing.T) {
		client := &errorCodexClient{
			spyCodexClient: newSpyCodexClient(),
			closeErr:       codex.ErrProcessContainmentIncomplete,
		}
		agent := NewAgent()
		session := &session{agent: agent, id: "session", client: client}
		agent.sessions[session.id] = session
		agent.runtimeClient = client

		err := agent.quiesceRuntimeAfterCancel(context.Background(), client)
		require.ErrorIs(t, err, codex.ErrProcessContainmentIncomplete)
		require.ErrorIs(t, agent.runtimeCleanupErr, codex.ErrProcessContainmentIncomplete)
		require.True(t, session.clientDead)
		require.Nil(t, agent.runtimeStarting)
	})
}

func TestRuntimeRecoverySkipsSessionAfterCloseAdmission(t *testing.T) {
	oldClient := newSpyCodexClient()
	newClient := &blockingLifecycleCodexClient{spyCodexClient: newSpyCodexClient()}
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newClient, nil
	}))
	active := newSession(agent, "closing", "/tmp/project", nil, codex.Thread{ID: "thread-closing"}, oldClient, sessionMeta{}, nil)
	agent.sessions[active.id] = active
	agent.runtimeClient = oldClient
	agent.runtimeDead = true
	active.setClientDead(true)

	active.lifecycle.Lock()
	active.mu.Lock()
	active.closing = true
	active.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := agent.sharedRuntime(ctx)
	require.NoError(t, err)
	require.Same(t, newClient, client)
	require.Zero(t, newClient.resumeCallCount(), "an admitted close must never be rebound")

	active.lifecycle.Unlock()
	require.NoError(t, agent.Close())
}

func TestSharedRuntimeGenerationCleanupFailureBranches(t *testing.T) {
	ordinaryCloseErr := errors.New("ordinary prior generation close failure")
	ordinary := NewAgent()
	ordinary.runtimeClient = &errorCodexClient{
		spyCodexClient: newSpyCodexClient(),
		closeErr:       ordinaryCloseErr,
	}
	ordinary.runtimeDead = true

	_, err := ordinary.sharedRuntime(context.Background())
	require.ErrorIs(t, err, ordinaryCloseErr)
	require.NoError(t, ordinary.runtimeCleanupErr)
	require.Nil(t, ordinary.runtimeStarting)

	unproven := NewAgent()
	unproven.runtimeClient = &errorCodexClient{
		spyCodexClient: newSpyCodexClient(),
		closeErr:       codex.ErrProcessContainmentIncomplete,
	}
	unproven.runtimeDead = true

	_, err = unproven.sharedRuntime(context.Background())
	require.ErrorIs(t, err, codex.ErrProcessContainmentIncomplete)
	require.ErrorIs(t, unproven.runtimeCleanupErr, codex.ErrProcessContainmentIncomplete)
	require.Nil(t, unproven.runtimeStarting)

	launchFailure := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return nil, codex.ErrProcessContainmentIncomplete
	}))
	_, err = launchFailure.sharedRuntime(context.Background())
	require.ErrorIs(t, err, codex.ErrProcessContainmentIncomplete)
	require.ErrorIs(t, launchFailure.runtimeCleanupErr, codex.ErrProcessContainmentIncomplete)
}

func TestRuntimeFailureAndHelperBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()

	_, err := agent.handleCodexServerRequestForEpoch(ctx, 99, codex.ServerRequest{})
	require.Error(t, err)

	release, err := agent.acquireNativeRoot(ctx, RuntimeResourceDiscovery)
	require.NoError(t, err)
	release()
	scratchRelease, err := agent.reserveScratchRoot(ctx, RuntimeResourcePrompt)
	require.NoError(t, err)
	scratchRelease()

	agent.options.RuntimeResourceHooks.AcquireNativeRoot = func(context.Context, RuntimeResourceKind) (func(), error) {
		//nolint:nilnil // Deliberately exercise rejection of a nil release callback.
		return nil, nil
	}
	_, err = agent.acquireNativeRoot(ctx, RuntimeResourceRuntime)
	require.Error(t, err)
	agent.options.RuntimeResourceHooks.ReserveScratchRoot = func(context.Context, RuntimeResourceKind) (func(), error) {
		return nil, errors.New("scratch")
	}
	_, err = agent.reserveScratchRoot(ctx, RuntimeResourceRuntime)
	require.Error(t, err)
	agent.options.RuntimeResourceHooks.ReserveScratchRoot = func(context.Context, RuntimeResourceKind) (func(), error) {
		//nolint:nilnil // Deliberately exercise rejection of a nil release callback.
		return nil, nil
	}
	_, err = agent.reserveScratchRoot(ctx, RuntimeResourceRuntime)
	require.Error(t, err)

	closed := NewAgent()
	require.NoError(t, closed.Close())
	_, err = closed.sharedRuntime(ctx)
	require.Error(t, err)

	waiting := NewAgent()
	waiting.runtimeStarting = make(chan struct{})
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = waiting.sharedRuntime(canceled)
	require.ErrorIs(t, err, context.Canceled)

	badScratchParent := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(badScratchParent, []byte("x"), 0o600))
	badScratch := NewAgent(WithScratchDir(badScratchParent), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newSpyCodexClient(), nil
	}))
	_, err = badScratch.sharedRuntime(ctx)
	require.Error(t, err)

	lost := NewAgent()
	lost.options.customClientFactory = true
	lost.options.clientFactory = func(context.Context, codex.Options) (codex.Client, error) {
		lost.mu.Lock()
		lost.closed = true
		lost.mu.Unlock()

		return newSpyCodexClient(), nil
	}
	_, err = lost.sharedRuntime(ctx)
	require.Error(t, err)

	invalidHome := NewAgent(WithEnv(map[string]string{"CODEX_HOME": "/env-home"}))
	_, err = invalidHome.Initialize(t.Context(), acp.InitializeRequest{})
	require.ErrorContains(t, err, "reserved")
	t.Setenv("CODEX_HOME", "/process-home")
	require.Equal(t, filepath.Clean("/process-home"), NewAgent().resolvedCodexHome())
}

// The writable home a native launch is proven against resolves from the
// explicit option first, then the static runtime environment, then the
// construction-time environment and its construction-time home fallback.
func TestResolvedCodexHomePrecedence(t *testing.T) {
	t.Setenv("CODEX_HOME", "/process-home")

	configured := NewAgent(WithHome("/opt/codex-home/"))
	require.Equal(t, filepath.Clean("/opt/codex-home"), configured.resolvedCodexHomeForEnv(map[string]string{"CODEX_HOME": "/env-home"}))

	agent := NewAgent()
	require.Equal(t, filepath.Clean("/env-home"), agent.resolvedCodexHomeForEnv(map[string]string{"CODEX_HOME": "/env-home/"}))
	require.Equal(t, filepath.Clean("/process-home"), agent.resolvedCodexHomeForEnv(map[string]string{"CODEX_HOME": ""}))

	home, err := runtimeUserHomeDir()
	require.NoError(t, err)
	t.Setenv("CODEX_HOME", "")
	require.Equal(t, filepath.Clean("/process-home"), agent.resolvedCodexHomeForEnv(nil), "the agent must retain its captured environment")
	require.Equal(t, filepath.Join(home, ".codex"), NewAgent().resolvedCodexHomeForEnv(nil))
}

func TestRuntimeEnvironmentRejectsReservedSessionKeys(t *testing.T) {
	for _, key := range []string{
		"acp_go_codex_internal_spoof",
		"acp_go_codex_runtime_id",
		"ACP_GO_CODEX_SCRATCH_ROOT",
		"CODEX_HOME",
		"HOME",
		"XDG_CONFIG_HOME",
	} {
		_, err := sessionMetaFromLifecycle(CodexOptions{Env: map[string]string{key: "1"}}.Meta())
		require.Error(t, err)
		require.Contains(t, err.Error(), "_meta.codex.options.env")
	}
}

func TestManagedHomeSessionEnvironmentFailsBeforeNativeCreation(t *testing.T) {
	var launches atomic.Int64
	agent := NewAgent(
		WithHome(t.TempDir()),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			launches.Add(1)

			return newSpyCodexClient(), nil
		}),
	)

	_, err := agent.NewSession(context.Background(), NewSessionRequest(t.TempDir(), WithSessionCodexOptions(
		NewCodexOptions(WithCodexEnv(map[string]string{"CODEX_HOME": t.TempDir()})),
	)))
	require.ErrorContains(t, err, "_meta.codex.options.env")
	require.Zero(t, launches.Load())
}

func TestSessionEnvironmentDoesNotPinSharedRuntime(t *testing.T) {
	client := newRuntimeRecordingClient()
	var launches atomic.Int64
	var gotOptions codex.Options
	agent := NewAgent(
		WithEnv(map[string]string{"BASE": "agent"}),
		withClientFactory(func(_ context.Context, options codex.Options) (codex.Client, error) {
			launches.Add(1)
			gotOptions = options

			return client, nil
		}),
	)
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	root := t.TempDir()
	start := make(chan struct{})
	errs := make(chan error, 2)
	for index, value := range []string{"one", "two"} {
		go func() {
			<-start
			_, err := agent.NewSession(context.Background(), NewSessionRequest(
				filepath.Join(root, fmt.Sprintf("workspace-%d", index)),
				WithSessionCodexOptions(NewCodexOptions(
					WithCodexEnv(map[string]string{"SESSION": value}),
					WithCodexExtraPathDirs(filepath.Join(root, fmt.Sprintf("bin-%d", index))),
				)),
			))
			errs <- err
		}()
	}
	close(start)
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	require.EqualValues(t, 1, launches.Load())
	require.Equal(t, map[string]string{"BASE": "agent"}, gotOptions.Env)

	client.mu.Lock()
	starts := append([]codex.ThreadStartRequest(nil), client.starts...)
	client.mu.Unlock()
	require.Len(t, starts, 2)
	seen := map[string]string{}
	for _, request := range starts {
		require.Len(t, request.ExtraPathDirs, 1)
		seen[request.Environment["SESSION"]] = request.ExtraPathDirs[0]
	}
	require.Equal(t, filepath.Join(root, "bin-0"), seen["one"])
	require.Equal(t, filepath.Join(root, "bin-1"), seen["two"])
}

func TestLifecycleFingerprintIncludesOrderedExtraPathDirs(t *testing.T) {
	base := codexSessionStart{Cwd: "/tmp/project", Meta: sessionMeta{
		Env:           map[string]string{"SESSION": "one"},
		ExtraPathDirs: []string{"/operation/first", "/operation/second"},
	}}

	changedEnv := base
	changedEnv.Meta.Env = map[string]string{"SESSION": "two"}
	require.NotEqual(t, codexSessionStartFingerprint(base), codexSessionStartFingerprint(changedEnv))

	reordered := base
	reordered.Meta.ExtraPathDirs = []string{"/operation/second", "/operation/first"}
	require.NotEqual(t, codexSessionStartFingerprint(base), codexSessionStartFingerprint(reordered))
}

func TestRuntimeResumeAndCanaryFailureBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	invalid := &session{agent: agent, mcpApprovalMode: "invalid"}
	_, err := invalid.resumeRequest()
	require.Error(t, err)
	_, err = agent.resumeRuntimeSession(ctx, newSpyCodexClient(), invalid)
	require.Error(t, err)

	valid := &session{agent: agent, id: "s", codexThreadID: "thread", cwd: "/work"}
	resumeFailure := &runtimeFailureClient{runtimeRecordingClient: newRuntimeRecordingClient(), resumeErr: errors.New("resume")}
	_, err = agent.resumeRuntimeSession(ctx, resumeFailure, valid)
	require.Error(t, err)

	mismatch := &mismatchedResumeClient{runtimeRecordingClient: newRuntimeRecordingClient()}
	_, err = agent.resumeRuntimeSession(ctx, mismatch, valid)
	require.ErrorContains(t, err, "different thread")

	require.NoError(t, agent.runtimeReadyCanary(ctx, newSpyCodexClient(), valid))
	valid.mcpServers = []acp.McpServer{HTTPMCPServer("marker", "https://example/mcp", nil)}

	oldRead := runtimeRandRead
	t.Cleanup(func() { runtimeRandRead = oldRead })
	runtimeRandRead = func([]byte) (int, error) { return 0, errors.New("rand") }
	require.Error(t, agent.runtimeReadyCanary(ctx, newSpyCodexClient(), valid))
	runtimeRandRead = oldRead

	failure := &runtimeFailureClient{runtimeRecordingClient: newRuntimeRecordingClient(), runErr: errors.New("turn")}
	require.Error(t, agent.runtimeReadyCanary(ctx, failure, valid))

	noMarker := &runtimeFailureClient{runtimeRecordingClient: newRuntimeRecordingClient(), events: []codex.Event{{Kind: codex.EventCompleted}}}
	require.Error(t, agent.runtimeReadyCanary(ctx, noMarker, valid))

	oldDeadline := runtimeReadyDeadline
	t.Cleanup(func() { runtimeReadyDeadline = oldDeadline })
	runtimeReadyDeadline = time.Nanosecond
	require.Error(t, agent.runtimeReadyCanary(ctx, failure, valid))
}

func TestRuntimeReadyEventBranches(t *testing.T) {
	nonce := "nonce"
	require.False(t, runtimeReadyEvent(codex.Event{Kind: codex.EventRaw}, nonce))
	require.False(t, runtimeReadyEvent(codex.Event{Kind: codex.EventToolStarted, Tool: codex.ToolEvent{Title: "other"}}, nonce))
	require.True(t, runtimeReadyEvent(codex.Event{Kind: codex.EventToolDelta, Tool: codex.ToolEvent{Kind: "runtime_ready", Content: nonce}}, nonce))
	require.True(t, runtimeReadyEvent(codex.Event{Kind: codex.EventToolCompleted, Tool: codex.ToolEvent{Title: "runtime_ready", Raw: map[string]any{"nonce": nonce}}}, nonce))
	require.False(t, runtimeReadyEvent(codex.Event{Kind: codex.EventToolCompleted, Tool: codex.ToolEvent{Title: "runtime_ready", Raw: map[string]any{"invalid": make(chan int)}}}, nonce))
}

func TestRuntimeRemainingResourceAndEpochBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	agent.options.RuntimeResourceHooks.AcquireNativeRoot = nil
	agent.options.RuntimeResourceHooks.ReserveScratchRoot = nil
	release, err := agent.acquireNativeRoot(ctx, RuntimeResourceRuntime)
	require.NoError(t, err)
	release()
	release, err = agent.reserveScratchRoot(ctx, RuntimeResourceRuntime)
	require.NoError(t, err)
	release()

	agent.options.RuntimeResourceHooks.AcquireNativeRoot = func(context.Context, RuntimeResourceKind) (func(), error) {
		return nil, errors.New("native admission failed")
	}
	_, err = agent.acquireNativeRoot(ctx, RuntimeResourceRuntime)
	require.ErrorContains(t, err, "native admission failed")

	agent.runtimeEpoch = 7
	_, err = agent.handleCodexServerRequestForEpoch(ctx, 7, codex.ServerRequest{Method: codex.RequestAuthTokenRefresh})
	require.ErrorContains(t, err, "not configured")

	waiting := NewAgent()
	waiting.runtimeStarting = make(chan struct{})
	waiting.runtimeClient = newSpyCodexClient()
	waiting.runtimeDead = true
	ready := make(chan struct{})
	go func() {
		time.Sleep(time.Millisecond)
		waiting.mu.Lock()
		waiting.runtimeDead = false
		close(waiting.runtimeStarting)
		waiting.runtimeStarting = nil
		waiting.mu.Unlock()
		close(ready)
	}()
	client, err := waiting.sharedRuntime(ctx)
	require.NoError(t, err)
	require.NotNil(t, client)
	<-ready

	oldHome := runtimeUserHomeDir
	t.Cleanup(func() { runtimeUserHomeDir = oldHome })
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", "")
	runtimeUserHomeDir = func() (string, error) { return "/captured-home", nil }
	require.Equal(t, "/captured-home/.codex", NewAgent().resolvedCodexHome())
	runtimeUserHomeDir = func() (string, error) { return "", errors.New("home failed") }
	require.Empty(t, NewAgent().resolvedCodexHome())
	runtimeUserHomeDir = func() (string, error) { return "", nil }
	require.Empty(t, NewAgent().resolvedCodexHome())
}

func TestRuntimeRemainingCanaryAndObserverBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	invalid := &session{agent: agent, id: "invalid", codexThreadID: "thread", mcpApprovalMode: "invalid"}
	require.Error(t, agent.runtimeReadyCanary(ctx, newSpyCodexClient(), invalid))

	valid := &session{
		agent: agent, id: "valid", codexThreadID: "thread", cwd: "/work",
		mcpServers: []acp.McpServer{HTTPMCPServer("marker", "https://example.test/mcp", nil)},
	}
	noMarker := &runtimeFailureClient{
		runtimeRecordingClient: newRuntimeRecordingClient(),
		events:                 []codex.Event{{Kind: codex.EventError}},
	}
	_, err := agent.resumeRuntimeSession(ctx, noMarker, valid)
	require.Error(t, err)
	oldDeadline := runtimeReadyDeadline
	t.Cleanup(func() { runtimeReadyDeadline = oldDeadline })
	runtimeReadyDeadline = time.Nanosecond
	require.Error(t, agent.runtimeReadyCanary(ctx, deadlineEventClient{runtimeRecordingClient: newRuntimeRecordingClient()}, valid))

	var gotOptions codex.Options
	var processObserved, startupObserved bool
	launcher := NewAgent(
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			ObserveProcess: func(context.Context, RuntimeProcessKind, int64) { processObserved = true },
			ObserveStartupStage: func(context.Context, RuntimeResourceKind, RuntimeStartupStage, time.Duration, error) {
				startupObserved = true
			},
		}),
		withClientFactory(func(_ context.Context, options codex.Options) (codex.Client, error) {
			gotOptions = options

			return newSpyCodexClient(), nil
		}),
	)
	_, err = launcher.launchRuntimeClient(ctx, 1, "", minSupportedCodexVersion)
	require.NoError(t, err)
	gotOptions.ObserveProcess(ctx, string(RuntimeProcessProviderDescendant), 1)
	gotOptions.ObserveStartupStage(ctx, string(RuntimeResourceRuntime), string(RuntimeStartupSpawn), time.Millisecond, nil)
	require.True(t, processObserved)
	require.True(t, startupObserved)

	stale := &codexClientEventSink{agent: launcher, epoch: 99}
	stale.Handle(ctx, codex.Event{Kind: codex.EventError})
	launcher.applyCodexClientEvent(ctx, newSpyCodexClient(), codex.Event{Kind: codex.EventError})
}

// A provider turn failure is terminal for that turn, not for the shared
// app-server. In particular, quota exhaustion must not force the next
// session/new through generation recovery and stale-thread resume.
func TestProviderErrorEventDoesNotPoisonSharedRuntime(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	runtimeClient := newSpyCodexClient()
	agent := NewAgent()
	loaded := &session{
		agent: agent, id: "session-1", codexThreadID: "thread-1", client: runtimeClient,
	}
	agent.sessions[loaded.id] = loaded
	agent.runtimeClient = runtimeClient

	agent.applyCodexClientEvent(ctx, runtimeClient, codex.Event{
		Kind: codex.EventError,
		// The app-server's raw `error` notification carries only text; the
		// provider classification happens later at the ACP prompt boundary.
		Err: errors.New("usage limit reached"),
	})

	require.False(t, agent.runtimeDead)
	require.False(t, loaded.clientDead)
	got, err := agent.sharedRuntime(ctx)
	require.NoError(t, err)
	require.Same(t, runtimeClient, got)

	transportErr := fmt.Errorf("%w: peer closed", codex.ErrConnectionClosed)
	agent.applyCodexClientEvent(ctx, runtimeClient, codex.Event{Kind: codex.EventError, Err: transportErr})
	require.True(t, agent.runtimeDead)
	require.True(t, loaded.clientDead)
	require.True(t, codexRuntimeDied(&codex.ProcessExitError{Err: errors.New("exit")}))
}

func TestRemainingRouteConnectionAndRequestBranches(t *testing.T) {
	ctx := context.Background()
	meta, err := stampElicitationRoute(nil, elicitationScope{SessionID: "session", TurnNonce: "turn", ToolCallID: "tool"})
	require.NoError(t, err)
	require.Contains(t, meta, routeMetaKey)
	require.NoError(t, routeInvalidParams(nil))

	agent := NewAgent()
	connection := &localAgentConnection{agent: agent}
	_, err = connection.UnstableCreateElicitation(ctx, acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{Mode: valueForm},
	})
	require.Error(t, err)
	agent.clientCalls = make(chan struct{})
	_, err = connection.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{Mode: valueForm},
	}, elicitationScope{SessionID: "session", TurnNonce: "turn", ToolCallID: "tool"})
	require.Error(t, err)

	for _, call := range []func() (any, error){
		func() (any, error) {
			return agent.handleCodexApproval(ctx, codex.ServerRequest{Method: codex.RequestCommandApproval}, acp.ToolKindExecute)
		},
		func() (any, error) {
			return agent.handleCodexPermissionsApproval(ctx, codex.ServerRequest{Method: codex.RequestPermissionsApproval})
		},
		func() (any, error) {
			return agent.handleCodexToolUserInput(ctx, codex.ServerRequest{Method: codex.RequestToolUserInput})
		},
		func() (any, error) {
			return agent.handleCodexMCPToolApproval(ctx, codex.ServerRequest{Method: codex.RequestMCPElicitation}, map[string]any{})
		},
		func() (any, error) {
			return agent.handleCodexMCPUserElicitation(ctx, codex.ServerRequest{Method: codex.RequestMCPElicitation}, map[string]any{})
		},
	} {
		_, callErr := call()
		require.NoError(t, callErr)
	}

	agent.sessions["session"] = &session{agent: agent, id: "session", codexThreadID: "thread"}
	_, err = agent.handleCodexServerRequest(ctx, codex.ServerRequest{
		Method: "unsupported",
		Params: json.RawMessage(`{"threadId":"thread"}`),
	})
	require.Error(t, err)

	promptSession := &session{agent: agent}
	_, err = promptSession.Prompt(ctx, acp.PromptRequest{})
	require.Error(t, err)
	require.Nil(t, agent.sessionByCodexThread(" "))
}

type deadlineEventClient struct{ *runtimeRecordingClient }

func (c deadlineEventClient) RunTurn(ctx context.Context, _ codex.TurnStartRequest) (<-chan codex.Event, error) {
	<-ctx.Done()
	events := make(chan codex.Event)
	close(events)

	return events, nil
}

// A stored rollout is generated by the trusted process and then handed to the
// native identity. An unprovable handoff must leave no rollout behind and must
// return the scratch admission it took.
func TestMaterializeStoredRolloutDiscardsRolloutOnFailedOwnershipHandoff(t *testing.T) {
	scratch := t.TempDir()
	var released atomic.Int64
	agent := NewAgent(
		WithScratchDir(scratch),
		WithProcessIsolation(foreignNativeIdentity()),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
				return func() { released.Add(1) }, nil
			},
		}),
	)

	path, release, err := agent.materializeStoredRollout(
		t.Context(),
		"session",
		[]SessionStoreEntry{json.RawMessage(`{"type":"session_meta"}`)},
	)
	require.Error(t, err)
	require.Empty(t, path)
	require.Nil(t, release)
	require.EqualValues(t, 1, released.Load())

	remaining, err := os.ReadDir(scratch)
	require.NoError(t, err)
	require.Empty(t, remaining)
}

func TestRemainingMaterializationCloneAuthAndStoreBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	path, release, err := agent.materializeStoredRollout(ctx, "", nil)
	require.NoError(t, err)
	require.Empty(t, path)
	release()
	agent.options.RuntimeResourceHooks.ReserveScratchRoot = func(context.Context, RuntimeResourceKind) (func(), error) {
		return nil, errors.New("scratch admission failed")
	}
	_, _, err = agent.materializeStoredRollout(ctx, "", []SessionStoreEntry{json.RawMessage(`{}`)})
	require.ErrorContains(t, err, "scratch admission failed")

	require.Equal(t, map[string]string{"a": "b"}, cloneAny(map[string]string{"a": "b"}))

	authMeta := map[string]any{codexMetaKey: map[string]any{authMetaAuthKey: map[string]any{
		authChatGPTAuthTokensMetaPath: map[string]any{jsonFieldAccessToken: "access"},
	}}}
	active := NewAgent()
	active.sessions["session"] = &session{agent: active}
	_, err = active.Authenticate(ctx, acp.AuthenticateRequest{MethodId: authMethodChatGPTAuthTokens, Meta: authMeta})
	require.ErrorContains(t, err, "quiescent")
	closing := NewAgent()
	closing.runtimeClient = &errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: errors.New("runtime close failed")}
	_, err = closing.Authenticate(ctx, acp.AuthenticateRequest{MethodId: authMethodChatGPTAuthTokens, Meta: authMeta})
	require.ErrorContains(t, err, "runtime close failed")

	oldRemove := removeMaterializedRolloutFile
	t.Cleanup(func() { removeMaterializedRolloutFile = oldRemove })
	removeMaterializedRolloutFile = func(string) error { return errors.New("remove failed") }
	closedAgent := NewAgent()
	require.NoError(t, closedAgent.Close())
	for _, target := range []struct {
		agent   *Agent
		id      acp.SessionId
		current bool
	}{
		{agent: closedAgent, id: "closed"},
		{agent: NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1})), id: "overflow"},
		{agent: NewAgent(), id: "same", current: true},
	} {
		if target.id == "overflow" {
			target.agent.sessions["occupied"] = &session{agent: target.agent}
		}
		if target.current {
			target.agent.sessions[target.id] = &session{agent: target.agent, id: target.id, materializedPath: "old"}
		}
		candidate := &session{agent: target.agent, id: target.id, materializedPath: "candidate"}
		storeErr := target.agent.storeStartedSession(candidate)
		if target.current {
			require.NoError(t, storeErr)
		} else {
			require.Error(t, storeErr)
		}
	}

	deleteAgent := NewAgent()
	deleteAgent.runtimeClient = newSpyCodexClient()
	deleteAgent.runtimeDead = false
	deleteAgent.sessions["delete"] = &session{agent: deleteAgent, id: "delete", materializedPath: "delete-path"}
	_, err = deleteAgent.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{SessionId: "delete"})
	require.ErrorContains(t, err, "remove failed")
}

func TestRemainingSessionCanaryCleanupBranches(t *testing.T) {
	ctx := context.Background()
	server := HTTPMCPServer("marker", "https://example.test/mcp", nil)
	entries := []SessionStoreEntry{json.RawMessage(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[]}}`)}

	newFailureClient := func() *runtimeFailureClient {
		return &runtimeFailureClient{
			runtimeRecordingClient: newRuntimeRecordingClient(),
			events:                 []codex.Event{{Kind: codex.EventCompleted}},
		}
	}

	resumeFailure := newFailureClient()
	resumeStore := NewInMemorySessionStore()
	require.NoError(t, resumeStore.Replace(ctx, SessionKey{SessionID: "native"}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: "native"},
		Entries: entries,
	}}))
	resumeAgent := NewAgent(WithSessionStore(resumeStore), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return resumeFailure, nil
	}))
	_, err := resumeAgent.ResumeSession(ctx, ResumeSessionRequest("native", "/work", WithSessionMCPServers(server)))
	require.ErrorContains(t, err, "runtime_ready")

	materializedFailure := newFailureClient()
	materializedAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return materializedFailure, nil
	}))
	_, err = materializedAgent.resumeMaterializedSession(ctx, ResumeSessionRequest("stored", "/work", WithSessionMCPServers(server)), entries)
	require.ErrorContains(t, err, "runtime_ready")
	_, err = materializedAgent.loadMaterializedSession(ctx, LoadSessionRequest("stored", "/work", WithSessionMCPServers(server)), entries)
	require.ErrorContains(t, err, "runtime_ready")

	resumeErrClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), resumeErr: errors.New("fork resume failed")}
	resumeErrAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return resumeErrClient, nil
	}))
	parent, err := resumeErrAgent.NewSession(ctx, NewSessionRequest("/work"))
	require.NoError(t, err)
	_, err = resumeErrAgent.forkSession(ctx, ForkSessionRequest(parent.SessionId, "/work"))
	require.ErrorContains(t, err, "fork resume failed")

	forkFailure := newFailureClient()
	forkAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return forkFailure, nil
	}))
	parent, err = forkAgent.NewSession(ctx, NewSessionRequest("/work"))
	require.NoError(t, err)
	_, err = forkAgent.forkSession(ctx, ForkSessionRequest(parent.SessionId, "/work", WithSessionMCPServers(server)))
	require.ErrorContains(t, err, "runtime_ready")
}

func TestRetainedRuntimeOwnershipBranches(t *testing.T) {
	ctx := context.Background()
	entries := []SessionStoreEntry{json.RawMessage(`{"type":"session_meta","payload":{"id":"thread"}}`)}

	newRetained := func(agent *Agent, client codex.Client, id acp.SessionId) *retainedRuntimeThread {
		agent.runtimeClient = client
		agent.runtimeEpoch = 7
		agent.runtimeDead = false
		retained := &retainedRuntimeThread{
			sessionID: id,
			threadID:  "thread",
			path:      "/native/rollout.jsonl",
			client:    client,
			epoch:     7,
		}
		agent.retainedThreads[id] = retained

		return retained
	}

	t.Run("logical close transfer", func(t *testing.T) {
		agent := NewAgent()
		client := newSpyCodexClient()
		agent.runtimeClient = client
		agent.runtimeEpoch = 3
		active := newSession(agent, "session", "/work", nil, codex.Thread{
			ID:   "thread",
			Path: "/native/rollout.jsonl",
		}, client, sessionMeta{}, nil)
		materialized, err := materializeRollout(t.TempDir(), entries)
		require.NoError(t, err)
		released := false
		active.materializedPath = materialized
		active.materializedRelease = func() { released = true }
		agent.sessions[active.id] = active
		agent.retainedThreads = nil

		removed, retainedOwnership := agent.finishSessionCloseRetainingThread(active.id, &session{})
		require.False(t, removed)
		require.False(t, retainedOwnership)
		active.closing = true
		removed, retainedOwnership = agent.finishSessionCloseRetainingThread(active.id, active)
		require.True(t, removed)
		require.True(t, retainedOwnership)
		retained := agent.retainedThreads[active.id]
		require.NotNil(t, retained)
		require.Equal(t, materialized, retained.materializedPath)
		require.Empty(t, active.materializedPath)
		require.NoError(t, cleanupRetainedRuntimeThread(retained))
		require.True(t, released)
	})

	t.Run("logical close without live runtime", func(t *testing.T) {
		agent := NewAgent()
		client := newSpyCodexClient()
		active := newSession(agent, "session", "/work", nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)
		agent.sessions[active.id] = active
		agent.runtimeClient = client
		agent.runtimeDead = true
		active.closing = true
		removed, retainedOwnership := agent.finishSessionCloseRetainingThread(active.id, active)
		require.True(t, removed)
		require.False(t, retainedOwnership)
		require.Empty(t, agent.retainedThreads)
	})

	t.Run("lookup active peer", func(t *testing.T) {
		agent := NewAgent()
		client := newSpyCodexClient()
		agent.runtimeClient = client
		agent.runtimeEpoch = 1
		agent.sessions["requested"] = newSession(agent, "requested", "/work", nil, codex.Thread{ID: "self"}, client, sessionMeta{}, nil)
		agent.sessions["unrelated"] = newSession(agent, "unrelated", "/work", nil, codex.Thread{ID: "other"}, client, sessionMeta{}, nil)
		retained, err := agent.claimRetainedRuntimeThreadForStore("requested", "unclaimed")
		require.ErrorIs(t, err, errNoRetainedRuntimeThread)
		require.Nil(t, retained)

		agent.sessions["peer"] = newSession(agent, "peer", "/work", nil, codex.Thread{ID: "claimed"}, client, sessionMeta{}, nil)
		_, err = agent.claimRetainedRuntimeThreadForStore("requested", "claimed")
		require.ErrorContains(t, err, "active in another session")
		retained, err = agent.claimRetainedRuntimeThreadForStore("missing", "")
		require.ErrorIs(t, err, errNoRetainedRuntimeThread)
		require.Nil(t, retained)
	})

	t.Run("store guards", func(t *testing.T) {
		fixture := func() (*Agent, *retainedRuntimeThread, *session) {
			agent := NewAgent()
			client := newSpyCodexClient()
			retained := newRetained(agent, client, "session")
			retained.claimed = true
			candidate := newSession(agent, "session", "/work", nil, codex.Thread{ID: "thread"}, client, sessionMeta{}, nil)

			return agent, retained, candidate
		}

		agent, retained, candidate := fixture()
		agent.closed = true
		require.Error(t, agent.storeRetainedRuntimeSession(candidate, retained))

		agent, _, candidate = fixture()
		require.ErrorContains(t, agent.storeRetainedRuntimeSession(candidate, nil), "ownership changed")

		agent, retained, candidate = fixture()
		retained.nativeEnded = true
		require.ErrorContains(t, agent.storeRetainedRuntimeSession(candidate, retained), "ownership changed")

		agent, retained, candidate = fixture()
		agent.runtimeDead = true
		require.ErrorContains(t, agent.storeRetainedRuntimeSession(candidate, retained), "runtime ownership changed")

		agent, retained, candidate = fixture()
		agent.sessions[candidate.id] = &session{id: candidate.id}
		require.ErrorContains(t, agent.storeRetainedRuntimeSession(candidate, retained), "became active")

		agent, retained, candidate = fixture()
		agent.options.ConcurrencyLimits.MaxActiveSessions = 1
		agent.sessions["occupied"] = &session{id: "occupied"}
		require.ErrorContains(t, agent.storeRetainedRuntimeSession(candidate, retained), valueBackpressure)

		agent, retained, candidate = fixture()
		materialized, err := materializeRollout(t.TempDir(), entries)
		require.NoError(t, err)
		released := false
		retained.materializedPath = materialized
		retained.materializedRelease = func() { released = true }
		require.NoError(t, agent.storeRetainedRuntimeSession(candidate, retained))
		require.Equal(t, materialized, candidate.materializedPath)
		require.Nil(t, agent.retainedThreads[candidate.id])
		require.NoError(t, candidate.Close(ctx))
		require.True(t, released)
	})

	t.Run("cleanup and end", func(t *testing.T) {
		require.NoError(t, cleanupRetainedRuntimeThread(nil))
		require.NoError(t, NewAgent().endRetainedRuntimeThread(nil))

		agent := NewAgent()
		client := newSpyCodexClient()
		retained := newRetained(agent, client, "session")
		agent.retainedThreads[retained.sessionID] = &retainedRuntimeThread{sessionID: retained.sessionID}
		require.NoError(t, agent.endRetainedRuntimeThread(retained))

		retained = newRetained(agent, client, "release-race")
		other := &retainedRuntimeThread{sessionID: retained.sessionID}
		retained.materializedRelease = func() {
			agent.mu.Lock()
			agent.retainedThreads[retained.sessionID] = other
			agent.mu.Unlock()
		}
		require.NoError(t, agent.endRetainedRuntimeThread(retained))
		require.Same(t, other, agent.retainedThreads[retained.sessionID])

		retained = newRetained(agent, client, "ended")
		released := false
		retained.materializedRelease = func() { released = true }
		require.NoError(t, agent.endRetainedRuntimeThread(retained))
		require.True(t, released)
		require.Nil(t, agent.retainedThreads[retained.sessionID])
	})

	t.Run("cleanup failure and runtime release", func(t *testing.T) {
		oldRemove := removeMaterializedRolloutFile
		t.Cleanup(func() { removeMaterializedRolloutFile = oldRemove })
		materialized, err := materializeRollout(t.TempDir(), entries)
		require.NoError(t, err)

		agent := NewAgent()
		client := newSpyCodexClient()
		retained := newRetained(agent, client, "failed")
		retained.materializedPath = materialized
		removeMaterializedRolloutFile = func(string) error { return errors.New("remove retained failed") }
		require.ErrorContains(t, cleanupRetainedRuntimeThread(retained), "remove retained failed")
		require.ErrorContains(t, agent.endRetainedRuntimeThread(retained), "remove retained failed")
		require.ErrorContains(t, agent.releaseRetainedRuntimeThreads(client, 7), "remove retained failed")

		removeMaterializedRolloutFile = oldRemove
		require.NoError(t, agent.releaseRetainedRuntimeThreads(client, 7))
		require.Nil(t, agent.retainedThreads[retained.sessionID])

		unrelated := newRetained(agent, client, "unrelated")
		unrelated.epoch = 8
		require.NoError(t, agent.releaseRetainedRuntimeThreads(client, 7))
		require.Same(t, unrelated, agent.retainedThreads[unrelated.sessionID])
	})
}
