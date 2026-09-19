package codexacp

import (
	"cmp"
	"context"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/process"
	"github.com/savid/acp-go-core/wire"
)

const (
	codexHomeEnv      = codex.EnvCodexHome
	internalEnvPrefix = codex.InternalEnvPrefix

	// runtimeShutdownGrace is how long the app-server gets after SIGTERM
	// before its group is killed.
	runtimeShutdownGrace = 2 * time.Second
	// runtimeShutdownTimeout bounds one shutdown rung.
	runtimeShutdownTimeout = 10 * time.Second
	// runtimeStartTimeout bounds the app-server handshake.
	runtimeStartTimeout = 60 * time.Second
)

// runtime is one app-server generation: the process every session's thread
// runs on until it exits.
type runtime struct {
	homeLock *process.FileLock
	proc     *process.Process
	client   *codex.Client
	cancel   context.CancelFunc
	// done is closed when the pump has stopped routing this generation.
	done chan struct{}
	// nativePath is the PATH the app-server runs with; thread PATHs are
	// composed ahead of it.
	nativePath string
	// home is the Codex home the app-server resolves its sessions under.
	home string
	// models is the catalog snapshot read at start.
	models []codex.Model

	mu         sync.Mutex
	dead       bool
	events     map[*session]*sessionQueue
	eventPumps sync.WaitGroup
}

type sessionQueue struct {
	messages chan sessionMessage
	// stopped prevents more delivery while the shared reader drains the
	// remaining records of a generation whose worker has ended.
	stopped bool
}

// sessionMessage preserves one thread's notification and request order.
type sessionMessage struct {
	event   codex.Event
	request *codex.ServerRequest
}

func (rt *runtime) alive() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	return !rt.dead
}

// ensureExecutable resolves the codex executable against the base
// environment, so a session directory can never shadow it.
func (a *Agent) ensureExecutable(ctx context.Context) (string, error) {
	base, err := a.environment().Base()
	if err == nil {
		var executable string
		if executable, err = process.ResolveExecutable(cmp.Or(a.options.ExecutablePath, vendor), base); err == nil {
			return executable, nil
		}
	}

	a.log.ErrorContext(ctx, "codex executable resolution failed", slog.String("reason", err.Error()))

	return "", err
}

// ensureRuntime returns the live app-server generation, starting one
// replacement when the previous generation is gone. A first start that fails
// is a native start failure; a replacement that cannot start answers
// codex_runtime_unavailable.
func (a *Agent) ensureRuntime(ctx context.Context) (*runtime, error) {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()

	if a.runtime != nil && a.runtime.alive() {
		return a.runtime, nil
	}

	if err := a.ensureOpen(); err != nil {
		return nil, err
	}

	rt, err := a.startRuntime(ctx)
	if err != nil {
		a.log.ErrorContext(ctx, "codex app-server start failed", slog.String("reason", err.Error()))

		if refusal := wire.SeedFileRefusal(err); refusal != nil {
			return nil, refusal
		}

		if a.runtime != nil {
			return nil, wire.RuntimeUnavailable(vendor)
		}

		return nil, wire.InternalFailure(vendor, internalClassNativeStart)
	}

	a.runtime = rt

	return rt, nil
}

// startRuntime launches one app-server, performs its handshake, and reads the
// model catalog.
func (a *Agent) startRuntime(ctx context.Context) (*runtime, error) {
	executable, err := a.ensureExecutable(ctx)
	if err != nil {
		return nil, err
	}

	env, err := a.environment().Build()
	if err != nil {
		return nil, err
	}

	home := codex.CodexHome(a.options.Home, func(key string) (string, bool) { return process.Lookup(env, key) })

	home, err = filepath.Abs(home)
	if err != nil {
		return nil, err
	}

	homeLock, err := codex.LockHome(home)
	if err != nil {
		return nil, err
	}

	started := false
	defer func() {
		if !started {
			_ = homeLock.Close()
		}
	}()

	if seedErr := process.WriteSeedFiles(home, a.options.SeedFiles); seedErr != nil {
		return nil, seedErr
	}

	proc, err := process.Start(ctx, process.Request{
		Executable: executable,
		Args:       codex.Launch{ConfigOverrides: a.options.CodexConfigOverrides}.Args(),
		Env:        env,
	})
	if err != nil {
		return nil, err
	}

	// The read loop outlives the request that launched the app-server: the
	// shutdown ladder ends it.
	readCtx, cancelRead := context.WithCancel(context.WithoutCancel(ctx))
	nativeClient := codex.NewClient(proc.Stdin(), proc.Stdout())

	if startErr := nativeClient.Start(readCtx); startErr != nil {
		cancelRead()

		_ = proc.Kill()
		_ = proc.Close()

		return nil, startErr
	}

	rt := &runtime{
		homeLock:   homeLock,
		proc:       proc,
		client:     nativeClient,
		cancel:     cancelRead,
		done:       make(chan struct{}),
		nativePath: pathFromEnvironment(env),
		home:       home,
	}

	go a.pump(context.WithoutCancel(ctx), rt)

	handshakeCtx, cancel := context.WithTimeout(ctx, runtimeStartTimeout)
	defer cancel()

	if initErr := nativeClient.Initialize(handshakeCtx); initErr != nil {
		a.stopGeneration(context.WithoutCancel(ctx), rt)

		return nil, initErr
	}

	models, listErr := nativeClient.ListModels(handshakeCtx)
	if listErr != nil {
		a.stopGeneration(context.WithoutCancel(ctx), rt)

		return nil, listErr
	}

	rt.models = models
	started = true

	return rt, nil
}

func pathFromEnvironment(env []string) string {
	value, _ := process.Lookup(env, "PATH")

	return value
}

// pump routes every app-server record of one generation to the session that
// owns its thread. It is the sole reader of the client's channels for the
// life of the process.
func (a *Agent) pump(ctx context.Context, rt *runtime) {
	defer close(rt.done)
	defer func() {
		rt.mu.Lock()
		for _, queue := range rt.events {
			close(queue.messages)
		}
		rt.mu.Unlock()
		rt.eventPumps.Wait()
		a.generationEnded(ctx, rt)
	}()

	notifications := rt.client.Notifications()
	requests := rt.client.Requests()

	for notifications != nil || requests != nil {
		select {
		case notification, ok := <-notifications:
			if !ok {
				notifications = nil

				continue
			}

			event := codex.DecodeEvent(notification)
			if s := a.sessionByThread(event.ThreadID); s != nil {
				if !a.routeMessage(ctx, rt, s, sessionMessage{event: event}) {
					return
				}
			}
		case request, ok := <-requests:
			if !ok {
				requests = nil

				continue
			}

			if !a.routeRequest(ctx, rt, request) {
				return
			}
		}
	}
}

// routeMessage keeps each session's publication, callbacks and settlement off
// the shared reader while preserving their native stream order.
func (a *Agent) routeMessage(ctx context.Context, rt *runtime, s *session, message sessionMessage) bool {
	rt.mu.Lock()

	select {
	case <-s.closeDone:
		rt.mu.Unlock()

		if message.request != nil {
			a.respondUnowned(rt, *message.request)
		}

		return true
	default:
	}

	queue := rt.events[s]
	if queue == nil {
		if rt.events == nil {
			rt.events = make(map[*session]*sessionQueue)
		}

		queue = &sessionQueue{messages: make(chan sessionMessage, 256)}
		rt.events[s] = queue
		rt.eventPumps.Go(func() { a.pumpSession(ctx, rt, s, queue) })
	}

	if queue.stopped {
		rt.mu.Unlock()

		if message.request != nil {
			a.respondUnowned(rt, *message.request)
		}

		return true
	}

	select {
	case queue.messages <- message:
		rt.mu.Unlock()

		return true
	default:
		rt.mu.Unlock()
		a.log.ErrorContext(ctx, "codex session event queue exceeded capacity", slog.String("session_id", string(s.id)))

		_ = rt.proc.Kill()

		return false
	}
}

func (a *Agent) pumpSession(ctx context.Context, rt *runtime, s *session, queue *sessionQueue) {
	defer func() {
		rt.mu.Lock()
		queue.stopped = true

		select {
		case <-s.closeDone:
			delete(rt.events, s)
		default:
		}

		var pending []codex.ServerRequest

		for range len(queue.messages) {
			if queued := <-queue.messages; queued.request != nil {
				pending = append(pending, *queued.request)
			}
		}
		rt.mu.Unlock()

		for _, request := range pending {
			a.respondUnowned(rt, request)
		}
	}()

	for {
		select {
		case queued, ok := <-queue.messages:
			if !ok {
				return
			}

			if queued.request != nil {
				s.handleRequest(rt, *queued.request, codex.RequestParams(*queued.request))
			} else if !s.handleEvent(ctx, rt, queued.event) {
				return
			}
		case <-s.closeDone:
			return
		}
	}
}

// routeRequest queues one server request for the session owning its thread.
// Requests naming no live session are answered as cancelled.
func (a *Agent) routeRequest(ctx context.Context, rt *runtime, request codex.ServerRequest) bool {
	params := codex.RequestParams(request)

	s := a.sessionByThread(codex.RequestThreadID(params))
	if s == nil {
		a.respondUnowned(rt, request)

		return true
	}

	return a.routeMessage(ctx, rt, s, sessionMessage{request: &request})
}

func (a *Agent) respondUnowned(rt *runtime, request codex.ServerRequest) {
	var response any

	switch request.Method {
	case codex.RequestCommandApproval, codex.RequestFileChangeApproval:
		response = codex.ApprovalResponse(nil, nil)
	case codex.RequestPermissionsApproval:
		response = codex.PermissionsResponse(nil, nil)
	case codex.RequestToolUserInput:
		response = codex.ToolUserInputResponse(nil)
	case codex.RequestMCPElicitation:
		response = codex.ElicitationCancelResponse()
	default:
		_ = rt.client.Respond(request, nil, &codex.RPCError{Code: -32601, Message: "method not found"})

		return
	}

	_ = rt.client.Respond(request, response, nil)
}

// sessionByThread resolves a native thread id to its live session.
func (a *Agent) sessionByThread(threadID string) *session {
	if threadID == "" {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	for _, s := range a.sessions {
		if s.nativeID == threadID && !a.deleted[s.id] {
			return s
		}
	}

	return nil
}

// generationEnded records that an app-server generation stopped producing
// records: every session bound to it learns its incarnation is over.
func (a *Agent) generationEnded(ctx context.Context, rt *runtime) {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeShutdownTimeout)
	defer cancel()

	if err := rt.proc.Shutdown(shutdownCtx, runtimeShutdownGrace); err != nil {
		_ = rt.proc.Kill()

		// The lock is released only once the app-server has been reaped; a
		// replacement writing the same home beside a live child corrupts it.
		select {
		case <-rt.proc.Done():
		case <-shutdownCtx.Done():
		}
	}

	_ = rt.homeLock.Close()
	_ = rt.proc.Close()

	rt.mu.Lock()
	rt.dead = true
	rt.mu.Unlock()

	a.mu.Lock()
	sessions := slices.Collect(maps.Values(a.sessions))
	a.mu.Unlock()

	for _, s := range sessions {
		s.runtimeEnded(ctx, rt)
	}

	a.observe.RecordProcessExit(ctx, "exited", rt.client.Err())
}

// stopGeneration runs the native rungs of the shutdown ladder for one
// generation: signal the process group, wait for the root, join the pump, and
// release the pipes.
func (a *Agent) stopGeneration(ctx context.Context, rt *runtime) {
	shutdownCtx, cancel := context.WithTimeout(ctx, runtimeShutdownTimeout)
	defer cancel()

	if err := rt.proc.Shutdown(shutdownCtx, runtimeShutdownGrace); err != nil {
		_ = rt.proc.Kill()
	}

	select {
	case <-rt.done:
	case <-shutdownCtx.Done():
		rt.cancel()
	}

	// Closing the pipes ends the read loop of a pump that outlived the
	// shutdown, and the join guarantees nothing still writes the cycle state
	// the caller settles from.
	_ = rt.proc.Close()
	<-rt.done
}

// stopRuntime stops the live generation, if any.
func (a *Agent) stopRuntime(ctx context.Context) {
	a.runtimeMu.Lock()
	rt := a.runtime
	a.runtime = nil
	a.runtimeMu.Unlock()

	if rt != nil {
		a.stopGeneration(ctx, rt)
	}
}

// transportFailure recovers the real cause behind a lost app-server stream:
// the child's exit status and stderr tail where it died, otherwise the
// transport error.
func (a *Agent) transportFailure(ctx context.Context, rt *runtime, err error) *acp.RequestError {
	return wire.TurnFailed(vendor, wire.TransportFailure(ctx, rt.proc, "codex app-server", err, rt.client.Err))
}
