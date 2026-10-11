package codexacp

import (
	"cmp"
	"context"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/process"
	"github.com/savid/acp-go-core/usage/gateway"
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
	done     chan struct{}
	exited   chan struct{}
	// stopped drops later records for this session on this generation; the
	// queue stays registered until retireQueue joins its worker, so no second
	// worker can start while the first still runs.
	stopped bool
}

// stopQueue fences a session's delivery on this generation.
func (runtime *runtime) stopQueue(s *session) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()

	queue := runtime.events[s]
	if queue == nil || queue.stopped {
		return
	}

	queue.stopped = true
	close(queue.done)
}

// retireQueue joins a session's worker and forgets its queue, so the
// session's next binding on this generation starts a fresh one.
func (runtime *runtime) retireQueue(s *session) {
	runtime.mu.Lock()
	queue := runtime.events[s]
	runtime.mu.Unlock()

	if queue == nil {
		return
	}

	<-queue.exited

	runtime.mu.Lock()
	defer runtime.mu.Unlock()

	if runtime.events[s] == queue {
		delete(runtime.events, s)
	}
}

// sessionMessage preserves one thread's notification and request order.
type sessionMessage struct {
	event   codex.Event
	request *codex.ServerRequest
}

func (runtime *runtime) alive() bool {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()

	return !runtime.dead
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

	runtime, err := a.startRuntime(ctx)
	if err != nil {
		a.log.ErrorContext(ctx, "codex app-server start failed", slog.String("reason", err.Error()))

		if refusal := wire.SeedFileRefusal(err); refusal != nil {
			return nil, refusal
		}

		if a.runtime != nil {
			return nil, wire.RuntimeUnavailable(vendor)
		}

		return nil, wire.InternalFailure(vendor, wire.ClassNativeStart)
	}

	a.runtime = runtime

	return runtime, nil
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

	runtime := &runtime{
		homeLock:   homeLock,
		proc:       proc,
		client:     nativeClient,
		cancel:     cancelRead,
		done:       make(chan struct{}),
		nativePath: pathFromEnvironment(env),
		home:       home,
	}

	go a.pump(context.WithoutCancel(ctx), runtime)

	handshakeCtx, cancel := context.WithTimeout(ctx, runtimeStartTimeout)
	defer cancel()

	if initErr := nativeClient.Initialize(handshakeCtx); initErr != nil {
		failure := process.LaunchFailure(proc, "codex app-server", initErr)

		a.stopGeneration(context.WithoutCancel(ctx), runtime)

		return nil, failure
	}

	models, listErr := nativeClient.ListModels(handshakeCtx)
	if listErr != nil {
		failure := process.LaunchFailure(proc, "codex app-server", listErr)

		a.stopGeneration(context.WithoutCancel(ctx), runtime)

		return nil, failure
	}

	if brokered := a.gatewayModels(handshakeCtx, home, env, models); len(brokered) > 0 {
		models = brokered
	}

	runtime.models = models
	started = true

	return runtime, nil
}

// gatewayModels is the model list of the gateway the active model provider
// routes through, when it publishes one. Codex sends a model name to that
// provider as given, so the gateway's ids, each naming its upstream, replace
// the app-server's presets. A list that cannot be read leaves the presets.
func (a *Agent) gatewayModels(ctx context.Context, home string, env []string, presets []codex.Model) []codex.Model {
	lookup := func(key string) (string, bool) { return process.Lookup(env, key) }

	route, ok, err := codex.ActiveGatewayRoute(home, a.options.CodexConfigOverrides, lookup)
	if err != nil || !ok {
		return nil
	}

	listed, err := gateway.Models(ctx, a.providerTransport, route)
	if err != nil {
		a.log.WarnContext(ctx, "codex gateway model list unavailable", slog.String("provider", route.Provider), slog.String("reason", err.Error()))

		return nil
	}

	return gatewayModelsWithPresetEfforts(listed, presets)
}

// gatewayModelsWithPresetEfforts converts a gateway's list into model entries.
// A gateway id ends in the upstream model's own name; when that name is an
// app-server preset, Codex sends the preset's reasoning efforts to the
// provider unchanged, so the entry carries the preset's ladder and default.
// Any other id carries no effort menu.
func gatewayModelsWithPresetEfforts(listed []gateway.Model, presets []codex.Model) []codex.Model {
	byName := make(map[string]codex.Model, len(presets))
	for _, preset := range presets {
		byName[preset.ID] = preset
	}

	models := make([]codex.Model, 0, len(listed))
	for _, model := range listed {
		entry := codex.Model{ID: model.ID, Name: model.Name, ContextWindow: model.ContextWindow, InputModalities: model.Inputs}
		if preset, ok := byName[model.ID[strings.LastIndex(model.ID, "/")+1:]]; ok {
			entry.ReasoningEfforts = slices.Clone(preset.ReasoningEfforts)
			entry.DefaultReasoningEffort = preset.DefaultReasoningEffort
		}

		models = append(models, entry)
	}

	return models
}

func pathFromEnvironment(env []string) string {
	value, _ := process.Lookup(env, "PATH")

	return value
}

// pump routes every app-server record of one generation to the session that
// owns its thread. It is the sole reader of the client's channels for the
// life of the process.
func (a *Agent) pump(ctx context.Context, runtime *runtime) {
	defer close(runtime.done)
	defer func() {
		runtime.mu.Lock()
		for _, queue := range runtime.events {
			close(queue.messages)
		}
		runtime.mu.Unlock()
		runtime.eventPumps.Wait()
		a.generationEnded(ctx, runtime)
	}()

	notifications := runtime.client.Notifications()
	requests := runtime.client.Requests()

	for notifications != nil || requests != nil {
		select {
		case notification, ok := <-notifications:
			if !ok {
				notifications = nil

				continue
			}

			event := codex.DecodeEvent(notification)
			if s := a.sessionByThread(event.ThreadID); s != nil {
				if !a.routeMessage(ctx, runtime, s, sessionMessage{event: event}) {
					return
				}
			}
		case request, ok := <-requests:
			if !ok {
				requests = nil

				continue
			}

			if !a.routeRequest(ctx, runtime, request) {
				return
			}
		}
	}
}

// routeMessage keeps each session's publication, callbacks and settlement off
// the shared reader while preserving their native stream order.
func (a *Agent) routeMessage(ctx context.Context, runtime *runtime, s *session, message sessionMessage) bool {
	runtime.mu.Lock()

	select {
	case <-s.closeDone:
		runtime.mu.Unlock()

		if message.request != nil {
			a.respondUnowned(runtime, *message.request)
		}

		return true
	default:
	}

	queue := runtime.events[s]
	if queue == nil {
		if runtime.events == nil {
			runtime.events = make(map[*session]*sessionQueue)
		}

		queue = &sessionQueue{messages: make(chan sessionMessage, 256), done: make(chan struct{}), exited: make(chan struct{})}
		runtime.events[s] = queue
		runtime.eventPumps.Go(func() { a.pumpSession(ctx, runtime, s, queue) })
	}

	if queue.stopped {
		runtime.mu.Unlock()

		if message.request != nil {
			a.respondUnowned(runtime, *message.request)
		}

		return true
	}

	select {
	case queue.messages <- message:
		runtime.mu.Unlock()

		return true
	default:
		queue.stopped = true
		close(queue.done)
		runtime.mu.Unlock()

		a.log.ErrorContext(ctx, "codex session event queue exceeded capacity; containing the session", slog.String("session_id", string(s.id)))

		if message.request != nil {
			a.respondUnowned(runtime, *message.request)
		}

		// Contain only this session; the shared app-server and its peers keep
		// running. Run off the shared reader so a stalled session cannot pin it.
		failure := wire.TurnFailed(vendor, wire.TurnFailure{Cause: wire.CauseTransport, Message: "the host stopped reading this session's notifications"})

		runtime.eventPumps.Go(func() { s.contain(ctx, runtime, failure) })

		return true
	}
}

func (a *Agent) pumpSession(ctx context.Context, runtime *runtime, s *session, queue *sessionQueue) {
	defer close(queue.exited)
	defer func() {
		runtime.mu.Lock()
		queue.stopped = true

		select {
		case <-s.closeDone:
			delete(runtime.events, s)
		default:
		}

		var pending []codex.ServerRequest

		for range len(queue.messages) {
			if queued := <-queue.messages; queued.request != nil {
				pending = append(pending, *queued.request)
			}
		}
		runtime.mu.Unlock()

		for _, request := range pending {
			a.respondUnowned(runtime, request)
		}
	}()

	for {
		select {
		case queued, ok := <-queue.messages:
			if !ok {
				return
			}

			if queued.request != nil {
				s.handleRequest(runtime, *queued.request, queued.request.Params)
			} else if !s.handleEvent(ctx, runtime, queue.done, queued.event) {
				return
			}
		case <-s.closeDone:
			return
		case <-queue.done:
			return
		}
	}
}

// routeRequest queues one server request for the session owning its thread.
// Requests naming no live session are answered as cancelled.
func (a *Agent) routeRequest(ctx context.Context, runtime *runtime, request codex.ServerRequest) bool {
	s := a.sessionByThread(codex.RequestThreadID(request.Params))
	if s == nil {
		a.respondUnowned(runtime, request)

		return true
	}

	return a.routeMessage(ctx, runtime, s, sessionMessage{request: &request})
}

func (a *Agent) respondUnowned(runtime *runtime, request codex.ServerRequest) {
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
		_ = runtime.client.Respond(request, nil, &codex.RPCError{Code: -32601, Message: "method not found"})

		return
	}

	_ = runtime.client.Respond(request, response, nil)
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
func (a *Agent) generationEnded(ctx context.Context, runtime *runtime) {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeShutdownTimeout)
	defer cancel()

	if err := runtime.proc.Shutdown(shutdownCtx, runtimeShutdownGrace); err != nil {
		_ = runtime.proc.Kill()

		// The lock is released only once the app-server has been reaped; a
		// replacement writing the same home beside a live child corrupts it.
		select {
		case <-runtime.proc.Done():
		case <-shutdownCtx.Done():
		}
	}

	_ = runtime.homeLock.Close()
	_ = runtime.proc.Close()

	runtime.mu.Lock()
	runtime.dead = true
	runtime.mu.Unlock()

	a.mu.Lock()
	sessions := slices.Collect(maps.Values(a.sessions))
	a.mu.Unlock()

	for _, s := range sessions {
		s.runtimeEnded(ctx, runtime)
	}

	a.observe.RecordProcessExit(ctx, "exited", runtime.client.Err())
}

// stopGeneration runs the native rungs of the shutdown ladder for one
// generation: signal the process group, wait for the root, join the pump, and
// release the pipes.
func (a *Agent) stopGeneration(ctx context.Context, runtime *runtime) {
	shutdownCtx, cancel := context.WithTimeout(ctx, runtimeShutdownTimeout)
	defer cancel()

	if err := runtime.proc.Shutdown(shutdownCtx, runtimeShutdownGrace); err != nil {
		_ = runtime.proc.Kill()
	}

	select {
	case <-runtime.done:
	case <-shutdownCtx.Done():
		runtime.cancel()
	}

	// Closing the pipes ends the read loop of a pump that outlived the
	// shutdown, and the join guarantees nothing still writes the cycle state
	// the caller settles from.
	_ = runtime.proc.Close()
	<-runtime.done
}

// stopRuntime stops the live generation, if any.
func (a *Agent) stopRuntime(ctx context.Context) {
	a.runtimeMu.Lock()
	runtime := a.runtime
	a.runtime = nil
	a.runtimeMu.Unlock()

	if runtime != nil {
		a.stopGeneration(ctx, runtime)
	}
}

// transportFailure recovers the real cause behind a lost app-server stream:
// the child's exit status and stderr tail where it died, otherwise the
// transport error.
func (a *Agent) transportFailure(ctx context.Context, runtime *runtime, err error) *acp.RequestError {
	return wire.TurnFailed(vendor, wire.TransportFailure(ctx, runtime.proc, "codex app-server", err, runtime.client.Err))
}
