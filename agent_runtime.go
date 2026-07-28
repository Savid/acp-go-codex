package codexacp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

const (
	runtimeReadyAttempts      = 5
	runtimeReadyApprovalNever = "never"
)

var runtimeReadyDeadline = 2 * time.Minute
var runtimeRandRead = rand.Read
var runtimeUserHomeDir = os.UserHomeDir
var runtimeRemoveAll = os.RemoveAll
var runtimeProbeCodexVersion = codex.ProbeVersion
var errNoRetainedRuntimeThread = errors.New("no retained Codex runtime thread")

// retainedRuntimeThread is native ownership that outlives an ACP
// session/close but not the app-server generation that still owns the thread.
// The ACP session ID and native thread ID form one inseparable identity; the
// canonical rollout path is never selected from host-provided store data.
type retainedRuntimeThread struct {
	sessionID acp.SessionId
	threadID  string
	path      string
	client    codex.Client
	epoch     uint64
	claimed   bool

	materializedPath    string
	materializedRelease func()
	nativeEnded         bool
}

func (a *Agent) runtimeEpochIsCurrent(epoch uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return !a.closed && a.runtimeEpoch == epoch
}

func (a *Agent) handleCodexServerRequestForEpoch(ctx context.Context, epoch uint64, req codex.ServerRequest) (any, error) {
	if !a.runtimeEpochIsCurrent(epoch) {
		return nil, fmt.Errorf("codex server request belongs to a fenced runtime epoch")
	}

	return a.handleCodexServerRequest(ctx, req)
}

func (a *Agent) acquireNativeRoot(ctx context.Context, kind RuntimeResourceKind) (func(), error) {
	hook := a.options.RuntimeResourceHooks.AcquireNativeRoot
	if hook == nil {
		return func() {}, nil
	}

	release, err := hook(ctx, kind)
	if err != nil {
		return nil, err
	}

	if release == nil {
		return nil, errors.New("AcquireNativeRoot returned a nil release function")
	}

	return sync.OnceFunc(release), nil
}

func (a *Agent) reserveScratchRoot(ctx context.Context, kind RuntimeResourceKind) (func(), error) {
	hook := a.options.RuntimeResourceHooks.ReserveScratchRoot
	if hook == nil {
		return func() {}, nil
	}

	release, err := hook(ctx, kind)
	if err != nil {
		return nil, err
	}

	if release == nil {
		return nil, errors.New("ReserveScratchRoot returned a nil release function")
	}

	return sync.OnceFunc(release), nil
}

// finalizeRuntimeResources releases admissions only after their corresponding
// resource is gone. An incomplete containment boundary retains both
// reservations because native work may still be using the private runtime root. Scratch deletion failure
// likewise retains the scratch reservation so the worker-wide bound remains
// truthful.
func finalizeRuntimeResources(
	runtimeErr error,
	nativeRelease func(),
	scratchRoot string,
	scratchRelease func(),
) error {
	if errors.Is(runtimeErr, codex.ErrProcessContainmentIncomplete) {
		return runtimeErr
	}

	if nativeRelease != nil {
		nativeRelease()
	}

	var removeErr error
	if scratchRoot != "" {
		removeErr = runtimeRemoveAll(scratchRoot)
	}

	if removeErr == nil && scratchRelease != nil {
		scratchRelease()
	}

	return errors.Join(runtimeErr, removeErr)
}

func closeRuntimeResources(
	ctx context.Context,
	client codex.Client,
	nativeRelease func(),
	scratchRoot string,
	scratchRelease func(),
) error {
	var closeErr error
	if client != nil {
		closeErr = client.Close(ctx)
	}

	return finalizeRuntimeResources(closeErr, nativeRelease, scratchRoot, scratchRelease)
}

func (a *Agent) claimRetainedRuntimeThreadForStore(
	sessionID acp.SessionId,
	storedThreadID string,
) (*retainedRuntimeThread, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	current := func(retained *retainedRuntimeThread) bool {
		return retained != nil &&
			!retained.nativeEnded &&
			!a.closed && !a.runtimeDead &&
			retained.client == a.runtimeClient && retained.epoch == a.runtimeEpoch
	}

	if retained := a.retainedThreads[sessionID]; current(retained) {
		switch {
		case retained.claimed:
			return nil, acp.NewInvalidRequest(map[string]any{
				jsonFieldError: "retained Codex thread lifecycle is already in progress",
			})
		case storedThreadID == "":
			return nil, acp.NewInvalidRequest(map[string]any{
				jsonFieldError: "stored Codex thread identity is required for the retained session",
			})
		case storedThreadID != retained.threadID:
			return nil, acp.NewInvalidRequest(map[string]any{
				jsonFieldError: "stored Codex thread does not match the retained session",
			})
		default:
			retained.claimed = true

			return retained, nil
		}
	}

	if storedThreadID == "" {
		return nil, errNoRetainedRuntimeThread
	}

	for otherID, retained := range a.retainedThreads {
		if otherID != sessionID && current(retained) && retained.threadID == storedThreadID {
			return nil, acp.NewInvalidRequest(map[string]any{
				jsonFieldError: "stored Codex thread is retained by another session",
			})
		}
	}

	for otherID, active := range a.sessions {
		if otherID == sessionID {
			continue
		}

		active.mu.Lock()
		ownedByPeer := active.client == a.runtimeClient && !active.clientDead && active.codexThreadID == storedThreadID
		active.mu.Unlock()

		if ownedByPeer {
			return nil, acp.NewInvalidRequest(map[string]any{
				jsonFieldError: "stored Codex thread is active in another session",
			})
		}
	}

	return nil, errNoRetainedRuntimeThread
}

func (a *Agent) storeRetainedRuntimeSession(session *session, retained *retainedRuntimeThread) error {
	a.mu.Lock()

	switch {
	case a.closed:
		a.mu.Unlock()

		return newAgentClosedError()
	case retained == nil || retained.nativeEnded || !retained.claimed || a.retainedThreads[session.id] != retained:
		a.mu.Unlock()

		return acp.NewInvalidRequest(map[string]any{jsonFieldError: "retained Codex thread ownership changed during resume"})
	case a.runtimeDead || a.runtimeClient != retained.client || a.runtimeEpoch != retained.epoch:
		a.mu.Unlock()

		return acp.NewInvalidRequest(map[string]any{jsonFieldError: "retained Codex runtime ownership changed during resume"})
	case a.sessions[session.id] != nil:
		a.mu.Unlock()

		return acp.NewInvalidRequest(map[string]any{jsonFieldError: "Codex session became active during retained resume"})
	case len(a.sessions) >= a.options.ConcurrencyLimits.MaxActiveSessions:
		a.mu.Unlock()

		return acp.NewInvalidRequest(map[string]any{jsonFieldError: valueBackpressure, jsonFieldLimit: "active_sessions"})
	}

	session.mu.Lock()
	session.materializedPath = retained.materializedPath
	session.materializedRelease = retained.materializedRelease
	session.mu.Unlock()

	retained.materializedPath = ""
	retained.materializedRelease = nil

	delete(a.retainedThreads, session.id)
	a.sessions[session.id] = session
	a.mu.Unlock()

	a.readmitProviderAuth(session.id)

	a.observe.AddActiveSession(context.Background(), 1)

	return nil
}

func (a *Agent) releaseRetainedRuntimeThreadClaim(retained *retainedRuntimeThread) {
	if retained == nil {
		return
	}

	a.mu.Lock()
	if a.retainedThreads[retained.sessionID] == retained {
		retained.claimed = false
	}
	a.mu.Unlock()
}

func (a *Agent) claimRetainedRuntimeThreadForDelete(sessionID acp.SessionId) (*retainedRuntimeThread, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	retained := a.retainedThreads[sessionID]
	if retained == nil {
		return nil, errNoRetainedRuntimeThread
	}

	if retained.claimed {
		return nil, acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "retained Codex thread lifecycle is already in progress",
		})
	}

	retained.claimed = true

	return retained, nil
}

func cleanupRetainedRuntimeThread(retained *retainedRuntimeThread) error {
	if retained == nil {
		return nil
	}

	if retained.materializedPath != "" {
		if err := removeMaterializedRollout(retained.materializedPath); err != nil {
			return err
		}
	}

	if retained.materializedRelease != nil {
		retained.materializedRelease()
	}

	retained.materializedPath = ""
	retained.materializedRelease = nil

	return nil
}

func (a *Agent) endRetainedRuntimeThread(retained *retainedRuntimeThread) error {
	if retained == nil {
		return nil
	}

	a.mu.Lock()
	if a.retainedThreads[retained.sessionID] != retained {
		a.mu.Unlock()

		return nil
	}

	retained.nativeEnded = true
	a.mu.Unlock()

	if err := cleanupRetainedRuntimeThread(retained); err != nil {
		return err
	}

	a.mu.Lock()
	if a.retainedThreads[retained.sessionID] == retained {
		delete(a.retainedThreads, retained.sessionID)
	}
	a.mu.Unlock()

	return nil
}

func (a *Agent) releaseRetainedRuntimeThreads(client codex.Client, epoch uint64) error {
	a.mu.Lock()

	retained := make([]*retainedRuntimeThread, 0, len(a.retainedThreads))
	for _, candidate := range a.retainedThreads {
		if candidate.client == client && candidate.epoch == epoch {
			retained = append(retained, candidate)
		}
	}
	a.mu.Unlock()

	var cleanupErr error

	for _, candidate := range retained {
		if err := cleanupRetainedRuntimeThread(candidate); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)

			continue
		}

		a.mu.Lock()
		if a.retainedThreads[candidate.sessionID] == candidate {
			delete(a.retainedThreads, candidate.sessionID)
		}
		a.mu.Unlock()
	}

	return cleanupErr
}

// sharedRuntime returns the one app-server owned by this Agent. A dead runtime
// is replaced as one atomic generation. Logical sessions remain marked dead
// and resume independently on first use, so one stale peer cannot prevent an
// unrelated session from loading or starting on the replacement generation.
func (a *Agent) sharedRuntime(ctx context.Context) (codex.Client, error) {
	for {
		a.mu.Lock()
		if a.closed {
			a.mu.Unlock()

			return nil, newAgentClosedError()
		}

		if a.runtimeCleanupErr != nil {
			cleanupErr := a.runtimeCleanupErr
			a.mu.Unlock()

			return nil, cleanupErr
		}

		if closing := a.runtimeClosing; closing != nil {
			a.mu.Unlock()

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-closing:
				continue
			}
		}

		if a.runtimeClient != nil && !a.runtimeDead {
			client := a.runtimeClient
			a.mu.Unlock()

			return client, nil
		}

		if wait := a.runtimeStarting; wait != nil {
			a.mu.Unlock()

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-wait:
				continue
			}
		}

		wait := make(chan struct{})
		startCtx, cancelStart := context.WithCancel(ctx)
		a.runtimeStarting = wait
		a.runtimeStartCancel = cancelStart
		oldEpoch := a.runtimeEpoch
		a.runtimeEpoch++
		epoch := a.runtimeEpoch
		oldClient := a.runtimeClient
		oldRelease := a.runtimeNativeRelease
		oldScratchRoot := a.runtimeScratchRoot
		oldScratchRelease := a.runtimeScratchRelease
		a.runtimeClient = nil
		a.runtimeNativeRelease = nil
		a.runtimeScratchRoot = ""
		a.runtimeScratchRelease = nil
		a.runtimeDead = true
		a.mu.Unlock()

		cleanupErr := a.closeRuntimeGeneration(
			context.Background(), oldClient, oldRelease, oldScratchRoot, oldScratchRelease,
			oldEpoch,
		)
		if cleanupErr != nil {
			cancelStart()

			a.mu.Lock()
			if errors.Is(cleanupErr, codex.ErrProcessContainmentIncomplete) {
				a.runtimeCleanupErr = cleanupErr
			}

			a.runtimeStarting = nil
			a.runtimeStartCancel = nil

			close(wait)
			a.mu.Unlock()

			return nil, cleanupErr
		}

		var (
			nativeVersion = minSupportedCodexVersion
			err           error
		)
		if !a.options.customClientFactory {
			nativeVersion, err = a.probeRuntimeVersion(startCtx)
		}

		var scratchRelease func()
		if err == nil {
			scratchRelease, err = a.reserveScratchRoot(startCtx, RuntimeResourceRuntime)
		}

		var scratchRoot string
		if err == nil {
			scratchRoot, err = createPrivateTempDir(a.options.ScratchDir, "acp-go-codex-runtime-")
			if err != nil {
				scratchRelease()
			}
		}

		var release func()
		if err == nil {
			release, err = a.acquireNativeRoot(startCtx, RuntimeResourceRuntime)
			if err != nil {
				err = finalizeRuntimeResources(err, nil, scratchRoot, scratchRelease)
			}
		}

		var client codex.Client
		if err == nil {
			client, err = a.launchRuntimeClient(startCtx, epoch, scratchRoot, nativeVersion)
			if err != nil {
				err = finalizeRuntimeResources(err, release, scratchRoot, scratchRelease)
			}
		}

		cancelStart()

		a.mu.Lock()
		if err == nil && !a.closed && a.runtimeEpoch == epoch {
			a.runtimeClient = client
			a.runtimeNativeRelease = release
			a.runtimeScratchRoot = scratchRoot
			a.runtimeScratchRelease = scratchRelease
			a.runtimeDead = false
		} else if err == nil {
			closeErr := client.Close(context.Background())
			cleanupErr := finalizeRuntimeResources(closeErr, release, scratchRoot, scratchRelease)
			err = errors.Join(newAgentClosedError(), cleanupErr)
		}

		if errors.Is(err, codex.ErrProcessContainmentIncomplete) {
			a.runtimeCleanupErr = err
		}

		a.runtimeStarting = nil
		a.runtimeStartCancel = nil

		close(wait)
		a.mu.Unlock()

		if err != nil {
			return nil, err
		}

		return client, nil
	}
}

func (a *Agent) probeRuntimeVersion(ctx context.Context) (string, error) {
	scratchRelease, err := a.reserveScratchRoot(ctx, RuntimeResourceDiscovery)
	if err != nil {
		return "", err
	}

	scratchRoot, err := createPrivateTempDir(a.options.ScratchDir, "acp-go-codex-runtime-discovery-")
	if err != nil {
		scratchRelease()

		return "", err
	}

	nativeRelease, err := a.acquireNativeRoot(ctx, RuntimeResourceDiscovery)
	if err != nil {
		return "", finalizeRuntimeResources(err, nil, scratchRoot, scratchRelease)
	}

	env, _ := a.pinRuntimeEnvironment(nil)
	version, probeErr := runtimeProbeCodexVersion(ctx, codex.VersionProbeOptions{
		CLIPath:          a.options.ExecutablePath,
		CodexHome:        a.options.Home,
		WritableHome:     a.resolvedCodexHomeForEnv(env),
		Scratch:          scratchRoot,
		ScratchParent:    filepath.Dir(scratchRoot),
		DarwinBestEffort: a.containmentMode == RuntimeContainmentBestEffort,
		Env:              env,
	})
	probeErr = finalizeRuntimeResources(probeErr, nativeRelease, scratchRoot, scratchRelease)

	return version, probeErr
}

// pinRuntimeEnvironment fixes the immutable process environment for this
// Agent-owned runtime key. Empty peer session env inherits the pinned value;
// an explicit peer env must resolve to the same effective environment.
func (a *Agent) pinRuntimeEnvironment(sessionEnv map[string]string) (map[string]string, error) {
	for key := range sessionEnv {
		if reservedCodexEnvKey(key) {
			return nil, acp.NewInvalidParams(map[string]any{
				jsonFieldError: "session env uses a reserved Codex adapter process-management key",
				jsonFieldField: "_meta.codex.options.env",
			})
		}
	}

	desired := cloneStringMap(a.options.Env)
	if desired == nil {
		desired = map[string]string{}
	}

	for key, value := range sessionEnv {
		desired[key] = value
	}

	if len(desired) == 0 {
		desired = nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.runtimeEnvSet {
		a.runtimeEnv = cloneStringMap(desired)
		a.runtimeEnvSet = true

		return cloneStringMap(a.runtimeEnv), nil
	}

	if len(sessionEnv) == 0 || maps.Equal(a.runtimeEnv, desired) {
		return cloneStringMap(a.runtimeEnv), nil
	}

	return nil, acp.NewInvalidParams(map[string]any{
		jsonFieldError: "session env conflicts with the immutable Codex runtime environment",
		jsonFieldField: "_meta.codex.options.env",
	})
}

func (a *Agent) canonicalizeSessionMeta(meta *sessionMeta) error {
	env, err := a.pinRuntimeEnvironment(meta.Env)
	if err != nil {
		return err
	}

	meta.Env = env

	return nil
}

func (a *Agent) sessionMetaForLifecycle(values map[string]any) (sessionMeta, error) {
	meta, err := sessionMetaFromLifecycle(values)
	if err != nil {
		return sessionMeta{}, err
	}

	if err := a.canonicalizeSessionMeta(&meta); err != nil {
		return sessionMeta{}, err
	}

	return meta, nil
}

func (a *Agent) resolvedCodexHome() string {
	return a.resolvedCodexHomeForEnv(a.options.Env)
}

func (a *Agent) resolvedCodexHomeForEnv(env map[string]string) string {
	if a.options.Home != "" {
		return filepath.Clean(a.options.Home)
	}

	if value := env["CODEX_HOME"]; value != "" {
		return filepath.Clean(value)
	}

	if value := os.Getenv("CODEX_HOME"); value != "" {
		return filepath.Clean(value)
	}

	home, err := runtimeUserHomeDir()
	if err != nil || home == "" {
		return ""
	}

	return filepath.Join(home, ".codex")
}

func (a *Agent) markRuntimeDead(client codex.Client) {
	a.mu.Lock()
	if a.runtimeClient == client {
		a.runtimeDead = true
		for _, session := range a.sessions {
			session.setClientDead(true)
		}
	}
	a.mu.Unlock()
}

// quiesceRuntimeAfterCancel atomically fences the shared runtime generation
// that accepted a cancelled turn, then closes it and waits for its native-tree
// completion proof. A replacement generation cannot start until cleanup has
// completed, and every logical session remains registered for lazy resume.
func (a *Agent) quiesceRuntimeAfterCancel(ctx context.Context, expected codex.Client) error {
	a.mu.Lock()
	if closing := a.runtimeClosing; closing != nil {
		a.mu.Unlock()
		<-closing

		a.mu.Lock()
		cleanupErr := a.runtimeCleanupErr
		a.mu.Unlock()

		return cleanupErr
	}

	if expected == nil || a.runtimeClient != expected {
		wait := a.runtimeStarting
		a.mu.Unlock()

		if wait != nil {
			// Another generation transition already owns cleanup. Its completion
			// is the proof that the expected tree cannot survive this return.
			<-wait
		}

		a.mu.Lock()
		cleanupErr := a.runtimeCleanupErr
		a.mu.Unlock()

		return cleanupErr
	}

	wait := make(chan struct{})
	a.runtimeStarting = wait

	client := a.runtimeClient
	epoch := a.runtimeEpoch
	release := a.runtimeNativeRelease
	scratchRoot := a.runtimeScratchRoot
	scratchRelease := a.runtimeScratchRelease

	a.runtimeClient = nil
	a.runtimeNativeRelease = nil
	a.runtimeScratchRoot = ""
	a.runtimeScratchRelease = nil
	a.runtimeDead = true
	a.runtimeEpoch++

	for _, session := range a.sessions {
		session.setClientDead(true)
	}
	a.mu.Unlock()

	cleanupErr := a.closeRuntimeGeneration(
		context.WithoutCancel(ctx), client, release, scratchRoot, scratchRelease, epoch,
	)

	a.mu.Lock()
	if errors.Is(cleanupErr, codex.ErrProcessContainmentIncomplete) {
		a.runtimeCleanupErr = cleanupErr
	}

	if a.runtimeStarting == wait {
		a.runtimeStarting = nil

		close(wait)
	}
	a.mu.Unlock()

	return cleanupErr
}

func (a *Agent) resumeRuntimeSession(ctx context.Context, client codex.Client, session *session) (codex.Thread, error) {
	request, err := session.resumeRequest()
	if err != nil {
		return codex.Thread{}, err
	}

	thread, err := client.ResumeThread(ctx, request)
	if err != nil {
		return codex.Thread{}, codexThreadACPError(err, session.accountMetaSnapshot())
	}

	if thread.ID != "" && thread.ID != request.ThreadID {
		return codex.Thread{}, acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "Codex resumed a different thread during runtime recovery",
		})
	}

	if err := a.runtimeReadyCanary(ctx, client, session); err != nil {
		return codex.Thread{}, err
	}

	return thread, nil
}

func (a *Agent) runtimeReadyCanary(parent context.Context, client codex.Client, session *session) error {
	config, err := session.threadConfig()
	if err != nil {
		return err
	}

	if len(config) == 0 {
		return nil
	}

	var nonceBytes [16]byte
	if _, err := runtimeRandRead(nonceBytes[:]); err != nil {
		return fmt.Errorf("create runtime_ready nonce: %w", err)
	}

	nonce := hex.EncodeToString(nonceBytes[:])
	threadID := session.snapshot().codexThreadID

	deadlineCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), runtimeReadyDeadline)
	defer cancel()

	for attempt := 0; attempt < runtimeReadyAttempts; attempt++ {
		// Diagnostic only. It must never decide readiness.
		_, _ = client.MCPServerStatusList(deadlineCtx, threadID)

		events, runErr := client.RunTurn(deadlineCtx, codex.TurnStartRequest{
			ThreadID: threadID,
			Prompt: []codex.UserInput{{
				jsonFieldType: jsonFieldText,
				jsonFieldText: "Call the side-effect-free MCP tool named runtime_ready exactly once with nonce " + nonce + ". Do not call any other tool. Reply only after the tool returns.",
			}},
			ApprovalPolicy: runtimeReadyApprovalNever,
		})
		if runErr != nil {
			if deadlineCtx.Err() != nil {
				break
			}

			continue
		}

		ready := false

		for event := range events {
			if runtimeReadyEvent(event, nonce) {
				ready = true
			}

			if event.Kind == codex.EventCompleted || event.Kind == codex.EventError {
				break
			}
		}

		if ready {
			return nil
		}

		if deadlineCtx.Err() != nil {
			break
		}
	}

	cause := deadlineCtx.Err()
	if cause == nil {
		cause = errors.New("marker was not observed")
	}

	return fmt.Errorf("codex thread %s failed runtime_ready after %d attempts: %w", threadID, runtimeReadyAttempts, cause)
}

func runtimeReadyEvent(event codex.Event, nonce string) bool {
	if event.Kind != codex.EventToolStarted && event.Kind != codex.EventToolDelta && event.Kind != codex.EventToolCompleted {
		return false
	}

	name := strings.ToLower(event.Tool.Title + " " + event.Tool.Kind)
	if !strings.Contains(name, "runtime_ready") {
		return false
	}

	if strings.Contains(event.Tool.Content, nonce) {
		return true
	}

	encoded, err := json.Marshal(event.Tool.Raw)

	return err == nil && strings.Contains(string(encoded), nonce)
}

func (a *Agent) closeSharedRuntime(ctx context.Context) error {
	closing := make(chan struct{})

	for {
		a.mu.Lock()
		if currentClosing := a.runtimeClosing; currentClosing != nil {
			a.mu.Unlock()
			<-currentClosing

			continue
		}

		a.runtimeClosing = closing
		starting := a.runtimeStarting
		cancelStart := a.runtimeStartCancel
		a.mu.Unlock()

		if cancelStart != nil {
			cancelStart()
		}

		if starting != nil {
			<-starting
		}

		break
	}

	a.mu.Lock()
	client := a.runtimeClient
	epoch := a.runtimeEpoch
	release := a.runtimeNativeRelease
	scratchRoot := a.runtimeScratchRoot
	scratchRelease := a.runtimeScratchRelease
	latchedCleanupErr := a.runtimeCleanupErr
	a.runtimeClient = nil
	a.runtimeNativeRelease = nil
	a.runtimeScratchRoot = ""
	a.runtimeScratchRelease = nil
	a.runtimeDead = true
	a.runtimeEpoch++
	a.mu.Unlock()

	cleanupErr := errors.Join(
		latchedCleanupErr,
		a.closeRuntimeGeneration(ctx, client, release, scratchRoot, scratchRelease, epoch),
	)

	if errors.Is(cleanupErr, codex.ErrProcessContainmentIncomplete) {
		a.mu.Lock()
		a.runtimeCleanupErr = cleanupErr
		a.mu.Unlock()
	}

	a.mu.Lock()
	if a.runtimeClosing == closing {
		a.runtimeClosing = nil

		close(closing)
	}
	a.mu.Unlock()

	return cleanupErr
}

func (a *Agent) closeRuntimeGeneration(
	ctx context.Context,
	client codex.Client,
	release func(),
	scratchRoot string,
	scratchRelease func(),
	epoch uint64,
) error {
	runtimeCloseErr := closeRuntimeResources(ctx, client, release, scratchRoot, scratchRelease)
	if errors.Is(runtimeCloseErr, codex.ErrProcessContainmentIncomplete) {
		return runtimeCloseErr
	}

	return errors.Join(runtimeCloseErr, a.releaseRetainedRuntimeThreads(client, epoch))
}
