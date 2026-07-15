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
// resource is gone. An unproven native tree retains both reservations because
// it may still be using the private runtime root. Scratch deletion failure
// likewise retains the scratch reservation so the worker-wide bound remains
// truthful.
func finalizeRuntimeResources(
	runtimeErr error,
	nativeRelease func(),
	scratchRoot string,
	scratchRelease func(),
) error {
	if errors.Is(runtimeErr, codex.ErrProcessTreeUnproven) {
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

// sharedRuntime returns the one app-server owned by this Agent. A dead runtime
// is replaced as one atomic generation: every loaded thread is resumed with
// its own MCP config before any thread becomes visible again.
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
		a.runtimeStarting = wait
		a.runtimeEpoch++
		epoch := a.runtimeEpoch
		oldClient := a.runtimeClient
		oldRelease := a.runtimeNativeRelease
		oldScratchRoot := a.runtimeScratchRoot
		oldScratchRelease := a.runtimeScratchRelease
		recovering := oldClient != nil || a.runtimeDead

		sessions := make([]*session, 0, len(a.sessions))
		for _, session := range a.sessions {
			sessions = append(sessions, session)
		}

		a.runtimeClient = nil
		a.runtimeNativeRelease = nil
		a.runtimeScratchRoot = ""
		a.runtimeScratchRelease = nil
		a.runtimeDead = true
		a.mu.Unlock()

		cleanupErr := closeRuntimeResources(
			context.Background(), oldClient, oldRelease, oldScratchRoot, oldScratchRelease,
		)
		if cleanupErr != nil {
			a.mu.Lock()
			if errors.Is(cleanupErr, codex.ErrProcessTreeUnproven) {
				a.runtimeCleanupErr = cleanupErr
			}

			a.runtimeStarting = nil

			close(wait)
			a.mu.Unlock()

			return nil, cleanupErr
		}

		scratchRelease, err := a.reserveScratchRoot(ctx, RuntimeResourceRuntime)

		var scratchRoot string
		if err == nil {
			scratchRoot, err = createPrivateTempDir(a.options.ScratchDir, "acp-go-codex-runtime-")
			if err != nil {
				scratchRelease()
			}
		}

		var release func()
		if err == nil {
			release, err = a.acquireNativeRoot(ctx, RuntimeResourceRuntime)
			if err != nil {
				err = finalizeRuntimeResources(err, nil, scratchRoot, scratchRelease)
			}
		}

		var client codex.Client
		if err == nil {
			client, err = a.launchRuntimeClient(ctx, epoch, scratchRoot)
			if err != nil {
				err = finalizeRuntimeResources(err, release, scratchRoot, scratchRelease)
			}
		}

		if err == nil && recovering && len(sessions) > 0 {
			err = a.resumeRuntimeSessions(ctx, client, sessions)
			if err != nil {
				closeErr := client.Close(context.Background())
				err = finalizeRuntimeResources(
					errors.Join(err, closeErr), release, scratchRoot, scratchRelease,
				)
			}
		}

		a.mu.Lock()
		if err == nil && !a.closed && a.runtimeEpoch == epoch {
			a.runtimeClient = client
			a.runtimeNativeRelease = release
			a.runtimeScratchRoot = scratchRoot
			a.runtimeScratchRelease = scratchRelease
			a.runtimeDead = false

			for _, session := range sessions {
				session.setClient(client, false)
			}
		} else if err == nil {
			closeErr := client.Close(context.Background())
			cleanupErr := finalizeRuntimeResources(closeErr, release, scratchRoot, scratchRelease)
			err = errors.Join(newAgentClosedError(), cleanupErr)
		}

		if errors.Is(err, codex.ErrProcessTreeUnproven) {
			a.runtimeCleanupErr = err
		}

		a.runtimeStarting = nil

		close(wait)
		a.mu.Unlock()

		if err != nil {
			return nil, err
		}

		return client, nil
	}
}

// pinRuntimeEnvironment fixes the immutable process environment for this
// Agent-owned runtime key. Empty peer session env inherits the pinned value;
// an explicit peer env must resolve to the same effective environment.
func (a *Agent) pinRuntimeEnvironment(sessionEnv map[string]string) (map[string]string, error) {
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

func (a *Agent) resumeRuntimeSessions(ctx context.Context, client codex.Client, sessions []*session) error {
	for _, session := range sessions {
		req, err := session.resumeRequest()
		if err != nil {
			return err
		}

		if _, err := client.ResumeThread(ctx, req); err != nil {
			return codexThreadACPError(err, session.accountMetaSnapshot())
		}
	}

	for _, session := range sessions {
		if err := a.runtimeReadyCanary(ctx, client, session); err != nil {
			return err
		}
	}

	return nil
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
	a.mu.Lock()
	client := a.runtimeClient
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
		closeRuntimeResources(ctx, client, release, scratchRoot, scratchRelease),
	)
	if errors.Is(cleanupErr, codex.ErrProcessTreeUnproven) {
		a.mu.Lock()
		a.runtimeCleanupErr = cleanupErr
		a.mu.Unlock()
	}

	return cleanupErr
}
