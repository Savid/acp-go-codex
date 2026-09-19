package codexacp

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/wire"
)

const (
	sandboxDangerFullAccess = "danger-full-access"
	sandboxReadOnly         = "read-only"
	sandboxWorkspaceWrite   = "workspace-write"

	// sessionAbortTimeout bounds the native interrupt a cancel or close sends.
	sessionAbortTimeout = 5 * time.Second
	// sessionSettleTimeout bounds one turn's settlement after the native run
	// ended: the mirror commit and the terminal lifecycle event.
	sessionSettleTimeout = 60 * time.Second
)

// session is one ACP session: one Codex thread on the shared app-server.
// The native thread ID addresses the app-server conversation.
type session struct {
	callbacks             sync.WaitGroup
	agent                 *Agent
	id                    acp.SessionId
	nativeID              string
	cwd                   string
	additionalDirectories []string
	options               CodexOptions
	rawEvents             *wire.RawEvents
	// gate admits one foreground operation at a time: a prompt, a config
	// change, or a restore.
	gate chan struct{}

	mu sync.Mutex
	// rt is the generation the thread is bound on; nil once that generation
	// is gone, until the next explicit operation rebinds it.
	rt          *runtime
	rolloutPath string
	images      []storedImage
	mirrored    int
	model       string
	mode        string
	effort      string
	serviceTier string
	personality string
	// contextWindow is the selected model's context window from the last
	// usage report or the catalog.
	contextWindow int64
	title         string
	updatedAt     string
	// lastTerminalTurn is the native turn id of the last cycle this session
	// terminalized. Records naming it are a native tail, not new work.
	lastTerminalTurn string
	// generationLost records that the generation the session was bound on
	// ended, so the incarnation is fenced once its owed terminal event is out.
	generationLost bool
	// promptCancel ends the in-flight prompt's own cancellation scope. The
	// scope outlives the request context, so a superseded handler context
	// never ends a turn the session still owns.
	promptCancel context.CancelFunc
	// installed records that the agent published the session under its id,
	// so close owes the store its final generation.
	installed bool
	closing   bool
	closeDone chan struct{}
	closeErr  error
	turn      *turn
	cycle     *cycle
	dialogs   map[string]*dialog

	mirrorMu sync.Mutex
	lcMu     sync.Mutex
	lc       lifecycle.Publisher
}

// cycle is one foreground run: the work of one accepted prompt, or one
// agent-origin run the thread started between prompts.
type cycle struct {
	lifecycle.Cycle
	// nativeTurnID is the turn id the app-server named.
	nativeTurnID string
	state        cycleState
	// failure records a native failure observed during the run.
	failure error
	// terminal records that the cycle's one terminal event is claimed.
	terminal bool
}

type turnEnd int

const (
	turnRunning turnEnd = iota
	turnSettled
	turnTransportEnded
)

// turn is one accepted prompt.
type turn struct {
	cycle
	submission lifecycle.Submission
	accepted   bool
	cancelled  bool
	ended      turnEnd
	settled    chan struct{}
	settleOnce sync.Once
	finished   chan struct{}
}

func (t *turn) settle(end turnEnd) {
	t.settleOnce.Do(func() {
		t.ended = end
		close(t.settled)
	})
}

// dialog is one pending server request, cancellable by session/cancel and
// the shutdown ladder.
type dialog struct {
	cancel context.CancelCauseFunc
}

var errDialogCancelled = errors.New("dialog cancelled by the session")

// bind binds the session's thread on the given generation.
func (s *session) bind(rt *runtime, thread codex.Thread) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.rt = rt
	s.generationLost = false
	s.rolloutPath = thread.Path

	if s.model == "" {
		s.model = thread.Model
	}

	if s.model == "" {
		s.model = s.agent.options.DefaultModel
	}

	s.contextWindow = catalogContextWindow(rt.models, s.model)
}

func catalogContextWindow(models []codex.Model, model string) int64 {
	for index := range models {
		if models[index].ID == model {
			return models[index].ContextWindow
		}
	}

	return 0
}

// threadStart renders the thread/start call for a new session.
func (s *session) threadStart() codex.ThreadStartRequest {
	model := s.options.Model
	if model == "" {
		model = s.agent.options.DefaultModel
	}

	return codex.ThreadStartRequest{
		Cwd:                   s.cwd,
		AdditionalDirectories: s.additionalDirectories,
		Model:                 model,
		ServiceTier:           s.options.ServiceTier,
		Personality:           s.options.Personality,
		ApprovalPolicy:        s.options.ApprovalPolicy,
		SandboxMode:           sandboxMode(s.options.SandboxPolicy),
		Environment:           s.options.Env,
		ExtraPathDirs:         s.options.ExtraPathDirs,
	}
}

// threadResume renders the thread/resume call for this session.
func (s *session) threadResume() codex.ThreadResumeRequest {
	return codex.ThreadResumeRequest{
		ThreadID:      s.nativeID,
		Cwd:           s.cwd,
		Environment:   s.options.Env,
		ExtraPathDirs: s.options.ExtraPathDirs,
	}
}

// ensureBound returns the generation the thread is bound on, rebinding it on
// the live generation when its own is gone. A rebind opens a fresh
// incarnation.
func (s *session) ensureBound(ctx context.Context) (*runtime, error) {
	s.mu.Lock()
	rt := s.rt
	s.mu.Unlock()

	if rt != nil && rt.alive() {
		return rt, nil
	}

	rt, err := s.agent.ensureRuntime(ctx)
	if err != nil {
		return nil, err
	}

	thread, err := rt.client.ResumeThread(ctx, s.threadResume(), rt.nativePath)
	if err != nil {
		return nil, s.startFailure(ctx, err)
	}

	s.bind(rt, thread)

	if err := s.openStream(ctx); err != nil {
		return nil, err
	}

	return rt, nil
}

// startFailure maps a failed native thread start or resume onto the closed
// off-prompt internal-failure shape. The reason goes to the log.
func (s *session) startFailure(ctx context.Context, err error) error {
	s.agent.log.ErrorContext(ctx, "codex thread start failed",
		slog.String("session_id", string(s.id)),
		slog.String("reason", err.Error()),
	)

	return wire.InternalFailure(vendor, internalClassNativeStart)
}

// handleEvent attributes one native event of the session's thread to the
// foreground that owns it: the in-flight prompt, the open agent-origin
// cycle, or, for work with neither, a new agent-origin cycle.
func (s *session) handleEvent(ctx context.Context, rt *runtime, event codex.Event) {
	s.emitRawEvent(ctx, event)

	s.mu.Lock()
	t := s.turn
	c := s.cycle
	closing := s.closing
	bound := s.rt == rt
	terminalTurn := s.lastTerminalTurn
	s.mu.Unlock()

	if !bound {
		return
	}

	// A record naming the turn this session already terminalized is that
	// turn's native tail, never work of whatever runs now. Adopting its id
	// would stamp the live cycle with an id no later record of that cycle can
	// match, and the cycle would never reach its own terminal.
	if event.TurnID != "" && event.TurnID == terminalTurn {
		return
	}

	switch {
	case t != nil:
		if bearsWork(event) {
			s.acceptTurn(ctx, t)
		}

		settled, err := s.projectEvent(ctx, &t.cycle, event)
		s.recordFailure(&t.cycle, err)

		if settled {
			t.settle(turnSettled)

			select {
			case <-t.finished:
			case <-rt.proc.Done():
			}
		}
	case c != nil:
		settled, err := s.projectEvent(ctx, c, event)
		s.recordFailure(c, err)

		if settled {
			s.settleAgentCycle(ctx, c)
		}
	default:
		if bearsWork(event) && !closing {
			s.openAgentCycle(ctx, event)
		}
	}
}

// bearsWork reports whether a record says the thread is running work a cycle
// owns. Usage reports and unmodelled records are session-scoped and open
// nothing.
func bearsWork(event codex.Event) bool {
	switch event.Kind {
	case codex.EventTurnStarted, codex.EventAgentMessageDelta, codex.EventReasoningDelta, codex.EventPlanUpdated,
		codex.EventToolStarted, codex.EventToolDelta, codex.EventToolCompleted,
		codex.EventImageStarted, codex.EventImageCompleted, codex.EventDiffUpdated:
		return true
	default:
		return false
	}
}

// openAgentCycle opens the foreground for work the thread began with no
// prompt in flight.
func (s *session) openAgentCycle(ctx context.Context, event codex.Event) {
	c := &cycle{Cycle: lifecycle.Cycle{Origin: lifecycle.CauseActivity}, nativeTurnID: event.TurnID}
	c.state.tools = make(map[string]*toolState)

	// The check, the open event, and the install share one critical section,
	// so a prompt cannot install a turn between them and leave a turn and a
	// cycle live at once.
	s.mu.Lock()
	if s.turn != nil || s.cycle != nil || s.closing {
		s.mu.Unlock()

		return
	}

	if err := s.lc.OpenAgentCycle(ctx, &c.Cycle); err != nil {
		s.mu.Unlock()
		s.agent.log.ErrorContext(ctx, "open agent-origin cycle failed",
			slog.String("session_id", string(s.id)), slog.String("reason", err.Error()))

		return
	}

	s.cycle = c
	s.mu.Unlock()

	_, err := s.projectEvent(ctx, c, event)
	s.recordFailure(c, err)
}

// settleAgentCycle runs the agent-origin settlement on the pump: the mirror
// commit, then the terminal idle.
func (s *session) settleAgentCycle(ctx context.Context, c *cycle) {
	settleCtx, cancel := context.WithTimeout(ctx, sessionSettleTimeout)
	defer cancel()

	if err := s.commitMirror(settleCtx); err != nil {
		// The incarnation ends with the failed commit; dropping the binding
		// lets the next operation relaunch and publish a fresh one.
		s.mu.Lock()
		s.rt = nil
		s.mu.Unlock()
		s.lc.Fence()
		s.agent.log.ErrorContext(settleCtx, "mirror commit after agent-origin cycle failed",
			slog.String("session_id", string(s.id)), slog.String("reason", err.Error()))
	}

	verdict := s.judgeCycle(c, false)

	if s.claimTerminal(c) {
		if err := s.lcIdle(settleCtx, c, verdict); err != nil {
			s.agent.log.ErrorContext(settleCtx, "terminal idle for agent-origin cycle failed",
				slog.String("session_id", string(s.id)), slog.String("reason", err.Error()))
		}
	}

	s.mu.Lock()
	if c.nativeTurnID != "" {
		s.lastTerminalTurn = c.nativeTurnID
	}

	if s.cycle == c {
		s.cycle = nil
	}
	s.mu.Unlock()
}

// claimTerminal reports whether the caller owns the cycle's one terminal
// event. The pump and the shutdown ladder both reach an open agent-origin
// cycle, and a second idle for one cycle is refused, fences the incarnation,
// and surfaces as a failure of whichever operation published it.
func (s *session) claimTerminal(c *cycle) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if c.terminal {
		return false
	}

	c.terminal = true

	return true
}

// runtimeEnded records that the generation the thread was bound on stopped
// producing events. An in-flight prompt learns its transport ended; an open
// agent-origin cycle ends failed; the incarnation's stream is fenced once its
// last terminal event is out.
func (s *session) runtimeEnded(ctx context.Context, rt *runtime) {
	s.mu.Lock()
	if s.rt != rt {
		s.mu.Unlock()

		return
	}

	s.rt = nil
	s.generationLost = true
	t := s.turn
	c := s.cycle
	closing := s.closing

	// While closing, the shutdown ladder is the one site that terminalizes
	// the open cycle.
	if !closing {
		s.cycle = nil
	}
	s.mu.Unlock()
	s.cancelDialogs()
	s.callbacks.Wait()

	if c != nil && !closing && s.claimTerminal(c) {
		_ = s.lcIdle(ctx, c, cycleVerdict{outcome: lifecycle.OutcomeFailed})
	}

	if t != nil {
		// The prompt settles the turn and fences the stream after its idle.
		t.settle(turnTransportEnded)

		return
	}

	if !closing {
		s.lc.Fence()
	}
}

// interrupt sends turn/interrupt for the in-flight native turn under a
// bounded context detached from the caller's cancellation.
func (s *session) interrupt(ctx context.Context, rt *runtime, nativeTurnID string) {
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionAbortTimeout)
	defer cancel()

	if err := rt.client.InterruptTurn(abortCtx, s.nativeID, nativeTurnID); err != nil {
		s.agent.log.DebugContext(abortCtx, "codex turn interrupt failed", slog.String("session_id", string(s.id)))
	}
}

// cancel implements session/cancel: it ends the in-flight prompt's own
// cancellation scope, resolves its pending dialogs, and interrupts the native
// turn. It is a silent no-op with no prompt in flight.
func (s *session) cancel(ctx context.Context) {
	s.mu.Lock()
	t := s.turn
	rt := s.rt
	cancelPrompt := s.promptCancel

	if t != nil && t.cancelled {
		s.mu.Unlock()

		return
	}

	nativeTurnID := ""

	if t != nil {
		t.cancelled = true
		nativeTurnID = t.nativeTurnID
	}
	s.mu.Unlock()

	if cancelPrompt != nil {
		cancelPrompt()
	}

	if t == nil {
		return
	}

	s.cancelDialogs()

	if rt != nil && rt.alive() {
		s.interrupt(ctx, rt, nativeTurnID)
	}
}

func (s *session) registerDialog(id string, cancel context.CancelCauseFunc) func() {
	s.mu.Lock()
	if s.closing || s.rt == nil || (s.turn != nil && s.turn.cancelled) {
		s.mu.Unlock()
		cancel(errDialogCancelled)

		return func() {}
	}

	if s.dialogs == nil {
		s.dialogs = make(map[string]*dialog)
	}

	s.callbacks.Add(1)

	entry := &dialog{cancel: cancel}
	s.dialogs[id] = entry
	s.mu.Unlock()

	return sync.OnceFunc(func() {
		defer s.callbacks.Done()

		s.mu.Lock()
		if s.dialogs[id] == entry {
			delete(s.dialogs, id)
		}
		s.mu.Unlock()
	})
}

func (s *session) cancelDialogs() {
	s.mu.Lock()

	dialogs := make([]*dialog, 0, len(s.dialogs))
	for _, d := range s.dialogs {
		dialogs = append(dialogs, d)
	}
	s.mu.Unlock()

	for _, d := range dialogs {
		d.cancel(errDialogCancelled)
	}
}

// admissionError reports why a session admits no further work: it is
// closing.
func (s *session) admissionError() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closing {
		return wire.UnknownSession()
	}

	return nil
}

// acquireGate admits one foreground operation. limit names the backpressure
// token a refusal carries.
func (s *session) acquireGate(limit string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closing {
		return nil, wire.UnknownSession()
	}

	return wire.AcquireSessionGate(s.gate, limit)
}

// close runs the shutdown ladder: mark closed, resolve pending dialogs and the
// in-flight turn, release the thread, commit the owed rows, terminalize what
// the stream still owns, and fence it. The shared app-server stays up for
// its peers.
func (s *session) close(ctx context.Context) error {
	s.mu.Lock()
	if s.closing {
		done := s.closeDone
		s.mu.Unlock()
		<-done

		return s.closeErr
	}

	s.closing = true
	if s.closeDone == nil {
		s.closeDone = make(chan struct{})
	}

	installed := s.installed
	t := s.turn
	rt := s.rt
	cancelPrompt := s.promptCancel

	if t != nil {
		t.cancelled = true
	}
	s.mu.Unlock()

	if cancelPrompt != nil {
		cancelPrompt()
	}

	s.cancelDialogs()
	s.callbacks.Wait()

	if t != nil {
		if rt != nil && rt.alive() {
			s.mu.Lock()
			nativeTurnID := t.nativeTurnID
			s.mu.Unlock()
			s.interrupt(ctx, rt, nativeTurnID)
		}

		select {
		case <-t.finished:
		case <-time.After(sessionSettleTimeout):
		}
	}

	if rt != nil && rt.alive() {
		unsubscribeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionAbortTimeout)

		if err := rt.client.UnsubscribeThread(unsubscribeCtx, s.nativeID); err != nil {
			s.agent.log.DebugContext(unsubscribeCtx, "codex thread unsubscribe failed", slog.String("session_id", string(s.id)))
		}

		cancel()
	}

	var errs []error

	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionSettleTimeout)
	defer cancel()

	if installed {
		if err := s.commitMirror(commitCtx); err != nil {
			errs = append(errs, err)
		}
	}

	s.mu.Lock()
	c := s.cycle
	s.cycle = nil
	s.rt = nil
	s.mu.Unlock()

	if len(errs) == 0 && c != nil && s.claimTerminal(c) {
		if err := s.lcIdle(commitCtx, c, cycleVerdict{outcome: lifecycle.OutcomeCancelled, stopReason: lifecycle.StopReasonCancelled}); err != nil {
			errs = append(errs, err)
		}
	}

	s.lc.Fence()

	s.mu.Lock()
	s.closeErr = errors.Join(errs...)
	close(s.closeDone)
	s.mu.Unlock()

	return s.closeErr
}

// sandboxMode reduces a sandbox policy to the mode string thread/start
// accepts; an object policy names its mode by type.
func sandboxMode(policy any) any {
	switch typed := policy.(type) {
	case string:
		return typed
	case map[string]any:
		switch typed[fieldType] {
		case "dangerFullAccess", sandboxDangerFullAccess:
			return sandboxDangerFullAccess
		case "readOnly", sandboxReadOnly:
			return sandboxReadOnly
		case "workspaceWrite", sandboxWorkspaceWrite:
			return sandboxWorkspaceWrite
		}
	}

	return nil
}

// turnSandboxPolicy renders the sandbox policy turn/start accepts: the
// session's own policy when set, else workspace-write over the additional
// directories.
func (s *session) turnSandboxPolicy() any {
	switch typed := s.options.SandboxPolicy.(type) {
	case string:
		switch typed {
		case sandboxDangerFullAccess:
			return map[string]any{fieldType: "dangerFullAccess"}
		case sandboxReadOnly:
			return map[string]any{fieldType: "readOnly", "networkAccess": false}
		case sandboxWorkspaceWrite:
			return workspaceWritePolicy(nil)
		default:
			return typed
		}
	case map[string]any:
		return wire.CloneMap(typed)
	}

	if len(s.additionalDirectories) == 0 {
		return nil
	}

	return workspaceWritePolicy(s.additionalDirectories)
}

func workspaceWritePolicy(dirs []string) map[string]any {
	return map[string]any{
		fieldType:             "workspaceWrite",
		"writableRoots":       append([]string{}, dirs...),
		"networkAccess":       false,
		"excludeTmpdirEnvVar": false,
		"excludeSlashTmp":     false,
	}
}

func (s *session) sessionInfo() acp.SessionInfo {
	s.mu.Lock()
	title := s.title
	updatedAt := s.updatedAt
	s.mu.Unlock()

	if title == "" {
		title = string(s.id)
	}

	info := acp.SessionInfo{
		Meta:                  wire.NativeSessionMeta(vendor, s.nativeID),
		SessionId:             s.id,
		Title:                 &title,
		Cwd:                   s.cwd,
		AdditionalDirectories: append([]string(nil), s.additionalDirectories...),
	}
	if updatedAt != "" {
		info.UpdatedAt = &updatedAt
	}

	return info
}
