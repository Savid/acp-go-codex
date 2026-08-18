package codexacp

import (
	"context"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/lifecycle"
)

// promptIncarnation is one prompt's lifecycle stream. This configuration
// delivers nothing between prompts, so a session opens one incarnation per
// prompt: a fresh identity with its own sequence space and its own entities,
// fenced when the prompt ends.
//
// Emission is serialized because more than the prompt goroutine reaches it: a
// permission or elicitation arrives on an app-server request goroutine and
// announces its action on the same stream. The sequence is claimed and the
// notification delivered under one lock, so the order a host reduces is the
// order the sequence numbers state.
type promptIncarnation struct {
	session *session
	stream  *lifecycle.Stream
	cycleID string
	turnID  string

	mu       sync.Mutex
	accepted bool
	settled  bool
}

// lifecycleActive reports whether this prompt speaks the extension at all. An
// unnegotiated connection carries no incarnation, and every lifecycle call on it
// is a no-op rather than a silent half-stream.
func (in *promptIncarnation) lifecycleActive() bool { return in != nil }

// openIncarnation mints the stream and states the truthful opening snapshot. The
// snapshot is the first notification inside the prompt and precedes acceptance,
// and its quiescence member is whatever the last boundary actually proved, which
// starts and stays negative while no boundary proves vacancy.
func (s *session) openIncarnation(
	ctx context.Context,
	negotiated lifecycle.Negotiated,
) (*promptIncarnation, error) {
	if !negotiated.Present() {
		return nil, nil //nolint:nilnil // An unnegotiated connection has no incarnation.
	}

	streamID, err := newSessionID()
	if err != nil {
		return nil, err
	}

	cycleID, err := newSessionID()
	if err != nil {
		return nil, err
	}

	turnID, err := newSessionID()
	if err != nil {
		return nil, err
	}

	incarnation := &promptIncarnation{
		session: s,
		stream:  lifecycle.NewStream(streamID, negotiated),
		cycleID: cycleID,
		turnID:  turnID,
	}

	if err := incarnation.emit(ctx, lifecycle.SnapshotEvent(cycleID, s.provenQuiescence())); err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.incarnation = incarnation
	s.mu.Unlock()

	return incarnation, nil
}

// provenQuiescence states what this session's last completed boundary proved.
// Nothing this adapter can do proves one logical session's descendants gone
// while the app-server generation those descendants live in keeps serving every
// peer, so the fact is negative and the opening snapshot says so.
func (s *session) provenQuiescence() lifecycle.QuiescenceFact {
	return lifecycle.QuiescenceFact{}
}

// liveIncarnation reports the incarnation a server-request goroutine may
// announce an action on.
func (s *session) liveIncarnation() *promptIncarnation {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.incarnation
}

func (s *session) clearIncarnation(incarnation *promptIncarnation) {
	if incarnation == nil {
		return
	}

	incarnation.mu.Lock()
	incarnation.stream.Fence()
	incarnation.mu.Unlock()

	s.mu.Lock()
	if s.incarnation == incarnation {
		s.incarnation = nil
	}
	s.mu.Unlock()
}

// emit claims the next sequence, reduces the event through the same reducer the
// family battery drives, and delivers it on its own identity-only carrier. An
// event this adapter cannot state truthfully is never sent, and its sequence
// stays consumed so the loss is a detectable gap.
func (in *promptIncarnation) emit(ctx context.Context, event lifecycle.Event) error {
	if !in.lifecycleActive() {
		return nil
	}

	in.mu.Lock()
	defer in.mu.Unlock()

	return in.emitLocked(ctx, event)
}

func (in *promptIncarnation) emitLocked(ctx context.Context, event lifecycle.Event) error {
	envelope, err := in.stream.Emit(event)
	if err != nil {
		return err
	}

	conn := in.session.agent.connection()
	if conn == nil {
		return nil
	}

	return conn.SessionUpdate(ctx, acp.SessionNotification{
		Meta:      map[string]any{lifecycle.MetaKey: envelope},
		SessionId: in.session.id,
		Update:    acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{}},
	})
}

// accept records that the native dispatcher took durable ownership of the frame
// and opens the foreground cycle the submission caused.
func (in *promptIncarnation) accept(ctx context.Context, submission lifecycle.Submission) error {
	if !in.lifecycleActive() {
		return nil
	}

	in.mu.Lock()
	defer in.mu.Unlock()

	if err := in.emitLocked(ctx, lifecycle.AcceptedEvent(submission, in.turnID)); err != nil {
		return err
	}

	in.accepted = true

	return in.emitLocked(ctx, lifecycle.TransitionEvent(lifecycle.ForegroundRunning, in.cycleID, in.turnID))
}

// announceAction registers one pending permission or elicitation on the stream
// and, when it blocks the foreground, the transition its cycle owes. The action
// is minted and registered against its inbound request before this runs, so a
// host never sees an action id it cannot yet answer.
func (in *promptIncarnation) announceAction(
	ctx context.Context,
	actionID string,
	kind lifecycle.ActionKind,
	blocksForeground bool,
) error {
	if !in.lifecycleActive() {
		return nil
	}

	in.mu.Lock()
	defer in.mu.Unlock()

	if !in.accepted || in.settled {
		return nil
	}

	owner := lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: in.turnID}
	if err := in.emitLocked(ctx, lifecycle.ActionEvent(
		lifecycle.PendingAction(actionID, kind, owner, blocksForeground),
	)); err != nil {
		return err
	}

	if !blocksForeground {
		return nil
	}

	return in.emitLocked(ctx, lifecycle.TransitionEvent(lifecycle.ForegroundRequiresAction, in.cycleID, in.turnID))
}

// resolveAction terminalizes one action and, when it was the blocker, returns
// the cycle to running. The resolution is ordered before the transition it
// unblocks, because the resolution is the reason the foreground may move.
func (in *promptIncarnation) resolveAction(ctx context.Context, actionID string, state lifecycle.ActionState) error {
	if !in.lifecycleActive() {
		return nil
	}

	in.mu.Lock()
	defer in.mu.Unlock()

	return in.resolveActionLocked(ctx, actionID, state)
}

func (in *promptIncarnation) resolveActionLocked(
	ctx context.Context,
	actionID string,
	state lifecycle.ActionState,
) error {
	record, known := in.stream.State().Action(actionID)
	if !known || record.State.Terminal() {
		return nil
	}

	if err := in.emitLocked(ctx, lifecycle.ActionEvent(lifecycle.ResolvedAction(actionID, state))); err != nil {
		return err
	}

	if !record.BlocksForeground || in.stream.State().Foreground.State != lifecycle.ForegroundRequiresAction {
		return nil
	}

	return in.emitLocked(ctx, lifecycle.TransitionEvent(lifecycle.ForegroundRunning, in.cycleID, in.turnID))
}

// settle terminalizes every action this incarnation still holds and ends the
// cycle. Cancellation, connection loss, out-of-turn refusal, and close all reach
// the ordered terminal action state before the foreground transition that
// depends on it, because a cycle may not end while an action blocking it is
// still nonterminal.
func (in *promptIncarnation) settle(
	ctx context.Context,
	stopReason acp.StopReason,
	outcome lifecycle.Outcome,
) error {
	if !in.lifecycleActive() {
		return nil
	}

	in.mu.Lock()
	defer in.mu.Unlock()

	if in.settled {
		return nil
	}

	in.settled = true

	for _, action := range in.stream.State().Actions {
		if action.State.Terminal() {
			continue
		}

		if err := in.resolveActionLocked(ctx, action.ActionID, lifecycle.ActionCancelled); err != nil {
			return err
		}
	}

	if !in.accepted {
		return nil
	}

	reason := string(stopReason)
	if outcome == lifecycle.OutcomeFailed {
		reason = ""
	}

	return in.emitLocked(ctx, lifecycle.IdleEvent(in.cycleID, in.turnID, reason, outcome))
}

// fenceSession ends the addressed logical session's stream. A closed session
// admits no further incarnation of itself: reuse of the stored conversation
// happens through a new load or resume, which is a different logical session
// with its own stream and its own reduction, and never by reopening this handle.
func (s *session) fenceSession() {
	s.clearIncarnation(s.liveIncarnation())
}
