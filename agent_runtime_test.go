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

func TestRuntimeReplacementResumesEveryConfigBeforeAnyCanary(t *testing.T) {
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
	_, err = agent.NewSession(context.Background(), NewSessionRequest("/work/b", WithSessionMCPServers(serverB)))
	require.NoError(t, err)

	agent.markRuntimeDead(first)
	_, err = agent.Prompt(context.Background(), TextPromptRequest(a.SessionId, "turn-a", "hello"))
	require.NoError(t, err)
	require.Equal(t, 2, launch)
	require.Len(t, replacement.resumes, 2)
	require.Len(t, replacement.order, 4)
	require.True(t, strings.HasPrefix(replacement.order[0], "resume:"), replacement.order)
	require.True(t, strings.HasPrefix(replacement.order[1], "resume:"), replacement.order)
	require.True(t, strings.HasPrefix(replacement.order[2], "canary:"), replacement.order)
	require.True(t, strings.HasPrefix(replacement.order[3], "canary:"), replacement.order)

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
			&errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: codex.ErrProcessTreeUnproven},
			func() { nativeReleased.Add(1) },
			root,
			func() { scratchReleased.Add(1) },
		)

		require.ErrorIs(t, err, codex.ErrProcessTreeUnproven)
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
		closeErr:       codex.ErrProcessTreeUnproven,
	}
	agent.runtimeNativeRelease = func() { nativeReleased.Add(1) }
	agent.runtimeScratchRoot = root
	agent.runtimeScratchRelease = func() { scratchReleased.Add(1) }

	err := agent.closeSharedRuntime(context.Background())
	require.ErrorIs(t, err, codex.ErrProcessTreeUnproven)
	require.ErrorIs(t, agent.runtimeCleanupErr, codex.ErrProcessTreeUnproven)
	require.Zero(t, nativeReleased.Load())
	require.Zero(t, scratchReleased.Load())
	require.DirExists(t, root)
	_, err = agent.sharedRuntime(context.Background())
	require.ErrorIs(t, err, codex.ErrProcessTreeUnproven)
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
		closeErr:       codex.ErrProcessTreeUnproven,
	}
	unproven.runtimeDead = true

	_, err = unproven.sharedRuntime(context.Background())
	require.ErrorIs(t, err, codex.ErrProcessTreeUnproven)
	require.ErrorIs(t, unproven.runtimeCleanupErr, codex.ErrProcessTreeUnproven)
	require.Nil(t, unproven.runtimeStarting)

	launchFailure := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return nil, codex.ErrProcessTreeUnproven
	}))
	_, err = launchFailure.sharedRuntime(context.Background())
	require.ErrorIs(t, err, codex.ErrProcessTreeUnproven)
	require.ErrorIs(t, launchFailure.runtimeCleanupErr, codex.ErrProcessTreeUnproven)
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
	lost.options.clientFactory = func(context.Context, codex.Options) (codex.Client, error) {
		lost.mu.Lock()
		lost.closed = true
		lost.mu.Unlock()

		return newSpyCodexClient(), nil
	}
	_, err = lost.sharedRuntime(ctx)
	require.Error(t, err)

	require.Equal(t, filepath.Clean("/env-home"), NewAgent(WithEnv(map[string]string{"CODEX_HOME": "/env-home"})).resolvedCodexHome())
	t.Setenv("CODEX_HOME", "/process-home")
	require.Equal(t, filepath.Clean("/process-home"), NewAgent().resolvedCodexHome())
}

func TestRuntimeEnvironmentPinIsAtomicAndImmutable(t *testing.T) {
	agent := NewAgent(WithEnv(map[string]string{"BASE": "agent"}))
	start := make(chan struct{})

	type result struct {
		env map[string]string
		err error
	}
	results := make(chan result, 2)
	for _, value := range []string{"first", "second"} {
		go func() {
			<-start
			env, err := agent.pinRuntimeEnvironment(map[string]string{"SESSION": value})
			results <- result{env: env, err: err}
		}()
	}
	close(start)

	var winner map[string]string
	failures := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			failures++

			continue
		}
		winner = result.env
	}
	if failures != 1 || winner["BASE"] != "agent" || (winner["SESSION"] != "first" && winner["SESSION"] != "second") {
		t.Fatalf("environment race winner=%#v failures=%d", winner, failures)
	}

	inherited, err := agent.pinRuntimeEnvironment(nil)
	require.NoError(t, err)
	require.Equal(t, winner, inherited)
	matching, err := agent.pinRuntimeEnvironment(map[string]string{"SESSION": winner["SESSION"]})
	require.NoError(t, err)
	require.Equal(t, winner, matching)
}

func TestConflictingSessionEnvironmentFailsBeforeNativeCreation(t *testing.T) {
	var launches atomic.Int64
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		launches.Add(1)

		return newSpyCodexClient(), nil
	}))
	_, err := agent.pinRuntimeEnvironment(map[string]string{"SESSION": "pinned"})
	require.NoError(t, err)

	_, err = agent.NewSession(context.Background(), NewSessionRequest("/tmp/project", WithSessionCodexOptions(
		NewCodexOptions(WithCodexEnv(map[string]string{"SESSION": "conflict"})),
	)))
	require.Error(t, err)
	require.Zero(t, launches.Load())
}

func TestPinnedSessionEnvironmentLaunchAndFingerprint(t *testing.T) {
	var gotOptions codex.Options
	agent := NewAgent(
		WithEnv(map[string]string{"BASE": "agent"}),
		withClientFactory(func(_ context.Context, options codex.Options) (codex.Client, error) {
			gotOptions = options

			return newSpyCodexClient(), nil
		}),
	)
	meta := sessionMeta{Env: map[string]string{"SESSION": "one"}}
	require.NoError(t, agent.canonicalizeSessionMeta(&meta))
	_, err := agent.launchRuntimeClient(context.Background(), 1, "")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"BASE": "agent", "SESSION": "one"}, gotOptions.Env)

	base := codexSessionStart{Cwd: "/tmp/project", Meta: meta}
	changed := base
	changed.Meta.Env = map[string]string{"BASE": "agent", "SESSION": "two"}
	require.NotEqual(t, codexSessionStartFingerprint(base), codexSessionStartFingerprint(changed))
}

func TestRuntimeResumeAndCanaryFailureBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	invalid := &session{agent: agent, mcpApprovalMode: "invalid"}
	_, err := invalid.resumeRequest()
	require.Error(t, err)
	require.Error(t, agent.resumeRuntimeSessions(ctx, newSpyCodexClient(), []*session{invalid}))

	valid := &session{agent: agent, id: "s", codexThreadID: "thread", cwd: "/work"}
	resumeFailure := &runtimeFailureClient{runtimeRecordingClient: newRuntimeRecordingClient(), resumeErr: errors.New("resume")}
	require.Error(t, agent.resumeRuntimeSessions(ctx, resumeFailure, []*session{valid}))

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
	require.Error(t, agent.resumeRuntimeSessions(ctx, noMarker, []*session{valid}))
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
	_, err := launcher.launchRuntimeClient(ctx, 1, "")
	require.NoError(t, err)
	gotOptions.ObserveProcess(ctx, string(RuntimeProcessProviderDescendant), 1)
	gotOptions.ObserveStartupStage(ctx, string(RuntimeResourceRuntime), string(RuntimeStartupSpawn), time.Millisecond, nil)
	require.True(t, processObserved)
	require.True(t, startupObserved)

	stale := &codexClientEventSink{agent: launcher, epoch: 99}
	stale.Handle(ctx, codex.Event{Kind: codex.EventError})
	launcher.applyCodexClientEvent(ctx, newSpyCodexClient(), codex.Event{Kind: codex.EventError})
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

func TestRemainingMaterializationCloneAuthAndStoreBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	path, release, err := agent.materializeStoredRollout(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, path)
	release()
	agent.options.RuntimeResourceHooks.ReserveScratchRoot = func(context.Context, RuntimeResourceKind) (func(), error) {
		return nil, errors.New("scratch admission failed")
	}
	_, _, err = agent.materializeStoredRollout(ctx, []SessionStoreEntry{json.RawMessage(`{}`)})
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
	resumeAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
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
