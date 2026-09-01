package codexacp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/lifecycle"
)

const (
	sessionNativeEventBuffer = 1024
	terminalNativeTurnLimit  = 1024
	lifecycleActionLimit     = 1024
)

// promptIncarnation is one foreground cycle on the session-owned lifecycle
// stream. The stream it references is the native app-server/thread incarnation
// and is never rotated or fenced merely because this cycle ends.
type promptIncarnation struct {
	session      *session
	stream       *lifecycle.Stream
	cycleID      string
	turnID       string
	nativeTurnID string
	turnNonce    string
	autonomous   bool
	accepted     bool
	settled      bool
	events       chan codex.Event
	eventsClosed bool
	preBind      []codex.Event
	state        *promptEventState
	cancelled    bool
	terminating  *turnContainment
}

type nativeCanary struct {
	turnID  string
	events  chan codex.Event
	closed  bool
	preBind []codex.Event
}

func (in *promptIncarnation) lifecycleActive() bool { return in != nil && in.stream != nil }

// attachNativeEvents claims the exact thread broker before the session becomes
// reachable.
func (s *session) attachNativeEvents() error {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()

	return s.attachNativeEventsFrom(client)
}

func (s *session) attachNativeEventsFrom(client codex.Client) error {
	if client == nil {
		return errors.New("codex thread event broker requires a native client")
	}

	s.lifecycleMu.Lock()
	if s.lifecycleClosing {
		s.lifecycleMu.Unlock()

		return errors.New("codex lifecycle is closing")
	}

	if s.nativeEventSource && !s.nativeEventStopping {
		s.lifecycleMu.Unlock()

		return nil
	}

	if s.nativeEventAttaching {
		s.lifecycleMu.Unlock()

		return errors.New("codex thread event broker attachment is already in progress")
	}

	s.nativeEventAttaching = true
	s.lifecycleMu.Unlock()

	feedCtx, cancel := context.WithCancel(context.Background())

	feed, err := client.SubscribeThread(feedCtx, s.codexThreadID)
	if err != nil {
		cancel()
		s.lifecycleMu.Lock()
		s.nativeEventAttaching = false
		s.signalLifecycleChangedLocked()
		s.lifecycleMu.Unlock()

		return err
	}

	done := make(chan struct{})
	barrier := make(chan chan error)

	s.lifecycleMu.Lock()
	if s.lifecycleClosing || s.nativeEventStopping {
		s.nativeEventAttaching = false
		s.lifecycleMu.Unlock()
		cancel()
		feed.Release()

		return errors.New("codex thread event broker attachment was contained")
	}

	s.nativeEventCancel = cancel
	s.nativeEventRelease = feed.Release
	s.nativeEventDone = done
	s.nativeEventBarrier = barrier
	s.nativeEventSource = true
	s.nativeEventStopping = false
	s.nativeEventPumping = true
	s.nativeEventAttaching = false
	s.signalLifecycleChangedLocked()
	s.lifecycleMu.Unlock()

	go s.runNativeEventPump(feed.Events, barrier, done)

	return nil
}

func (s *session) beginNativeCanary() (*nativeCanary, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.lifecycleFailure != nil {
		return nil, s.lifecycleFailure
	}

	if !s.nativeEventSource || s.nativeEventStopping {
		return nil, errors.New("codex runtime_ready requires a live thread event broker")
	}

	if s.lifecycleClosing || s.nativeCanary != nil || s.incarnation != nil || s.agentIncarnation != nil {
		return nil, errors.New("codex runtime_ready cannot overlap another native turn")
	}

	canary := &nativeCanary{events: make(chan codex.Event, sessionNativeEventBuffer+1)}
	s.nativeCanary = canary

	return canary, nil
}

func (s *session) bindNativeCanary(canary *nativeCanary, turnID string) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.nativeCanary != canary || canary.closed {
		return errors.New("codex runtime_ready turn is no longer current")
	}

	if turnID == "" {
		return errors.New("codex runtime_ready acknowledgement omitted its native turn identity")
	}

	canary.turnID = turnID
	for index := range canary.preBind {
		event := canary.preBind[index]
		if event.TurnID != turnID {
			if !s.enqueueNativeRebindEventLocked(event) {
				return codex.ErrTurnEventOverflow
			}

			continue
		}

		if !s.enqueueCanaryEventLocked(canary, event) {
			return codex.ErrTurnEventOverflow
		}
	}

	canary.preBind = nil

	return nil
}

func (s *session) enqueueNativeRebindEventLocked(event codex.Event) bool {
	if len(s.nativeRebindEvents) == sessionNativeEventBuffer {
		return false
	}

	s.nativeRebindEvents = append(s.nativeRebindEvents, event)

	return true
}

func (s *session) rejectNativeCanaryAck(canary *nativeCanary, cause error) (string, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.nativeCanary != canary {
		return "", cause
	}

	turnID := ""

	for index := range canary.preBind {
		event := canary.preBind[index]
		if event.TurnID == "" || (turnID != "" && turnID != event.TurnID) {
			cause = errors.Join(cause, errors.New("codex runtime_ready acknowledgement failed after ambiguous native activity"))
			_ = s.latchLifecycleFailureLocked(cause)

			return "", cause
		}

		turnID = event.TurnID
	}

	if turnID != "" {
		canary.turnID = turnID
		cause = errors.Join(cause, errors.New("codex runtime_ready acknowledgement failed after native activity"))
	}

	return turnID, cause
}

func (s *session) endNativeCanary(canary *nativeCanary) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.nativeCanary == canary {
		s.closeCanaryEventsLocked(canary)
		s.nativeCanary = nil
	}
}

func (s *session) enqueueCanaryEventLocked(canary *nativeCanary, event codex.Event) bool {
	if canary.closed {
		return false
	}

	if len(canary.events) == sessionNativeEventBuffer {
		canary.events <- codex.Event{
			Kind: codex.EventError, Scope: codex.EventScopeThread,
			ThreadID: s.codexThreadID, TurnID: canary.turnID, Err: codex.ErrTurnEventOverflow,
		}

		s.closeCanaryEventsLocked(canary)

		return false
	}

	canary.events <- event

	if event.Kind == codex.EventCompleted || event.Kind == codex.EventError {
		s.closeCanaryEventsLocked(canary)
	}

	return true
}

func (s *session) closeCanaryEventsLocked(canary *nativeCanary) {
	if canary == nil || canary.closed {
		return
	}

	canary.closed = true
	close(canary.events)
}

func (s *session) runNativeEventPump(events <-chan codex.Event, barriers <-chan chan error, done chan<- struct{}) {
	defer close(done)
	defer func() {
		s.lifecycleMu.Lock()
		s.nativeEventPumping = false
		s.lifecycleMu.Unlock()
	}()
	defer func() {
		if recover() != nil {
			s.failNativeIncarnation(errors.New("codex thread event pump panicked"))
		}
	}()

	for {
		select {
		case event, open := <-events:
			if !open {
				goto stopped
			}

			if err := s.routeNativeEvent(event); err != nil {
				s.failNativeIncarnation(err)

				return
			}
		case result := <-barriers:
			var barrierErr error

			draining := true
			for draining {
				select {
				case event, open := <-events:
					if !open {
						result <- nil

						close(result)

						goto stopped
					}

					if err := s.routeNativeEvent(event); err != nil {
						barrierErr = err
						draining = false
					}
				default:
					draining = false
				}
			}

			result <- barrierErr

			close(result)

			if barrierErr != nil {
				s.failNativeIncarnation(barrierErr)

				return
			}
		}
	}

stopped:
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	s.lifecycleMu.Lock()
	failed := s.lifecycleFailure != nil
	stopping := s.nativeEventStopping
	s.lifecycleMu.Unlock()

	if !closing && !failed && !stopping {
		s.failNativeIncarnation(codex.ErrConnectionClosed)
	}
}

func (s *session) drainNativeEvents(ctx context.Context) error {
	s.lifecycleMu.Lock()
	barrier := s.nativeEventBarrier
	live := s.nativeEventSource && !s.nativeEventStopping
	s.lifecycleMu.Unlock()

	if !live || barrier == nil {
		return nil
	}

	result := make(chan error, 1)

	select {
	case barrier <- result:
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *session) stopNativeEventsContext(ctx context.Context) error {
	s.lifecycleMu.Lock()
	s.nativeEventStopping = true
	cancel := s.nativeEventCancel
	release := s.nativeEventRelease
	done := s.nativeEventDone
	s.nativeEventCancel = nil
	s.nativeEventRelease = nil
	s.nativeEventBarrier = nil
	s.closeCanaryEventsLocked(s.nativeCanary)
	s.nativeCanary = nil
	s.lifecycleMu.Unlock()

	if cancel != nil {
		cancel()
	}

	if release != nil {
		release()
	}

	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	s.lifecycleMu.Lock()
	if s.nativeEventDone == done {
		s.nativeEventDone = nil
	}
	s.lifecycleMu.Unlock()

	return nil
}

func (s *session) prepareNativeEventRebind() error {
	s.lifecycleRouteMu.Lock()
	s.lifecycleMu.Lock()
	active := s.nativeCanary != nil || s.incarnation != nil || s.agentIncarnation != nil ||
		s.establishment != nil || len(s.preOpenEvents) != 0 || s.nativeEventRebinding || s.nativeEventReplaying || s.nativeEventAttaching

	active = active || s.lifecycleDeliveryActive || len(s.lifecycleDeliveries) != 0
	if !active {
		s.nativeEventStopping = true
		s.nativeEventRebinding = true
		s.signalLifecycleChangedLocked()
	}
	s.lifecycleMu.Unlock()
	s.lifecycleRouteMu.Unlock()

	if active {
		return acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "Codex native lifecycle is active",
			jsonFieldLimit: limitSessionPrompt,
		})
	}

	stopCtx, cancelStop := context.WithTimeout(context.TODO(), closeTimeout)
	stopErr := s.stopNativeEventsContext(stopCtx)

	cancelStop()

	if stopErr != nil {
		s.lifecycleMu.Lock()
		s.nativeEventRebinding = false
		_ = s.latchLifecycleFailureLocked(stopErr)
		s.signalLifecycleChangedLocked()
		s.lifecycleMu.Unlock()

		return stopErr
	}

	s.lifecycleRouteMu.Lock()
	defer s.lifecycleRouteMu.Unlock()

	s.lifecycleMu.Lock()
	if s.lifecycleStream != nil && !s.lifecycleStream.Fenced() {
		s.lifecycleStream.Fence()
	}

	pending := s.nativeRebindEvents
	s.nativeRebindEvents = nil
	s.lifecycleStream = nil
	s.lifecycleFailure = nil
	s.nativeEventSource = false
	s.nativeEventStopping = false
	s.nativeEventRebinding = false
	s.nativeEventOpened = false
	s.preOpenEvents = append(s.preOpenEvents, pending...)
	s.terminalNativeTurns = nil
	s.terminalNativeTurnOrder = nil
	s.terminalNativeTurnNext = 0
	s.signalLifecycleChangedLocked()
	s.lifecycleMu.Unlock()

	return nil
}

func (s *session) beginActiveNativeRebind(ctx context.Context) error {
	gateCtx, cancel := context.WithTimeout(ctx, closeTimeout)
	defer cancel()

	if err := lockLifecycleRoute(gateCtx, &s.lifecycleRouteMu); err != nil {
		return err
	}

	defer s.lifecycleRouteMu.Unlock()

	lifecycleNegotiated := s.agent != nil && s.agent.negotiatedLifecycle().Present()
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.nativeEventRebinding && s.establishment != nil {
		return nil
	}

	lifecyclePending := lifecycleNegotiated && !s.nativeEventOpened

	active := s.lifecycleClosing || s.nativeCanary != nil || s.incarnation != nil || s.agentIncarnation != nil ||
		s.establishment != nil || len(s.preOpenEvents) != 0 || s.nativeEventRebinding || s.nativeEventReplaying ||
		s.nativeEventAttaching || s.nativeEventStopping || s.lifecycleDeliveryActive || len(s.lifecycleDeliveries) != 0 || s.lifecycleFailure != nil ||
		lifecyclePending
	if active {
		return acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "Codex native lifecycle is active",
			jsonFieldLimit: limitSessionPrompt,
		})
	}

	s.nativeEventRebinding = true
	s.signalLifecycleChangedLocked()

	return nil
}

func (s *session) finishActiveNativeRebind(ctx context.Context) error {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	if deadline, ok := ctx.Deadline(); ok {
		cancel()
		finishCtx, cancel = context.WithDeadline(context.WithoutCancel(ctx), deadline)
	}
	defer cancel()

	if err := lockLifecycleRoute(finishCtx, &s.lifecycleRouteMu); err != nil {
		return err
	}

	defer s.lifecycleRouteMu.Unlock()

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if !s.nativeEventRebinding {
		return nil
	}

	s.nativeEventRebinding = false
	s.nativeEventReplaying = true
	pending := s.nativeRebindEvents
	s.nativeRebindEvents = nil

	for index := range pending {
		event := pending[index]
		if err := s.routeNativeEventLocked(finishCtx, event); err != nil {
			s.nativeEventReplaying = false
			s.signalLifecycleChangedLocked()

			return s.latchLifecycleFailureLocked(err)
		}
	}

	s.nativeEventReplaying = false
	s.signalLifecycleChangedLocked()

	return nil
}

func (s *session) completeActiveNativeRebind(ctx context.Context) error {
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	drainErr := s.drainNativeEvents(drainCtx)

	cancel()

	return errors.Join(drainErr, s.finishActiveNativeRebind(ctx))
}

func (s *session) rebindNativeEvents(client codex.Client) error {
	if err := s.prepareNativeEventRebind(); err != nil {
		return err
	}

	return s.attachNativeEventsFrom(client)
}

func (s *session) armLifecycleEstablishment(obligation *establishmentObligation) error {
	if obligation == nil || !s.agent.negotiatedLifecycle().Present() {
		return nil
	}

	if err := obligation.bind(s); err != nil {
		return err
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.lifecycleClosing {
		return errors.New("codex lifecycle establishment raced session close")
	}

	if s.establishment != nil {
		return errors.New("codex lifecycle establishment is already outstanding")
	}

	if s.establishmentErr != nil {
		return s.establishmentErr
	}

	s.establishment = obligation

	s.establishmentRebind = s.lifecycleStream != nil
	if s.establishmentRebind {
		s.nativeEventRebinding = true
	}

	s.signalLifecycleChangedLocked()

	return nil
}

func (s *session) lifecycleEstablishmentPending() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	return s.establishment != nil
}

func (s *session) abandonLifecycleEstablishment() {
	s.lifecycleMu.Lock()
	obligation := s.establishment
	s.establishment = nil
	s.establishmentRebind = false
	s.lifecycleMu.Unlock()

	if obligation != nil {
		obligation.hooks.cancel(obligation, errEstablishmentCancelled)
	}
}

func (s *session) completeLifecycleEstablishment(
	ctx context.Context,
	obligation *establishmentObligation,
	responseErr error,
) error {
	s.lifecycleRouteMu.Lock()

	s.lifecycleMu.Lock()
	if s.establishment != obligation {
		s.lifecycleMu.Unlock()
		s.lifecycleRouteMu.Unlock()

		return errEstablishmentCancelled
	}

	if s.lifecycleClosing {
		s.establishment = nil
		s.establishmentRebind = false
		s.establishmentErr = errEstablishmentCancelled
		s.signalLifecycleChangedLocked()
		s.lifecycleMu.Unlock()
		s.lifecycleRouteMu.Unlock()

		return errEstablishmentCancelled
	}

	rebind := s.establishmentRebind
	if responseErr != nil {
		s.establishment = nil
		s.establishmentRebind = false
		s.nativeEventRebinding = false
		s.establishmentErr = responseErr
		_ = s.latchLifecycleFailureLocked(responseErr)
		s.signalLifecycleChangedLocked()
		s.lifecycleMu.Unlock()
		s.lifecycleRouteMu.Unlock()

		return s.failLifecycleEstablishment(ctx, responseErr)
	}

	openErr := s.openLifecycleStreamLocked(ctx, s.agent.negotiatedLifecycle(), rebind)
	if s.establishment == obligation {
		s.establishment = nil
		s.establishmentRebind = false
	}

	s.establishmentErr = openErr
	if openErr != nil {
		s.nativeEventRebinding = false
		_ = s.latchLifecycleFailureLocked(openErr)
	}

	s.signalLifecycleChangedLocked()
	s.lifecycleMu.Unlock()
	s.lifecycleRouteMu.Unlock()

	if openErr != nil {
		return s.failLifecycleEstablishment(ctx, openErr)
	}

	return nil
}

func (s *session) awaitLifecycleEstablishment(ctx context.Context) error {
	s.lifecycleMu.Lock()
	obligation := s.establishment
	err := s.establishmentErr
	s.lifecycleMu.Unlock()

	if obligation == nil {
		return err
	}

	if waitErr := obligation.wait(ctx); waitErr != nil {
		return waitErr
	}

	s.lifecycleMu.Lock()
	err = s.establishmentErr
	s.lifecycleMu.Unlock()

	return err
}

func (s *session) openLifecycleStream(ctx context.Context, negotiated lifecycle.Negotiated) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	return s.openLifecycleStreamLocked(ctx, negotiated, false)
}

func (s *session) openLifecycleStreamLocked(
	ctx context.Context,
	negotiated lifecycle.Negotiated,
	rebind bool,
) error {
	if s.lifecycleFailure != nil {
		return s.lifecycleFailure
	}

	if s.lifecycleClosing {
		return errEstablishmentCancelled
	}

	if negotiated.Present() && (s.lifecycleStream == nil || rebind) {
		streamID, streamErr := newSessionID()
		if streamErr != nil {
			return streamErr
		}

		cycleID, cycleErr := newSessionID()
		if cycleErr != nil {
			return cycleErr
		}

		if rebind && s.lifecycleStream != nil && !s.lifecycleStream.Fenced() {
			s.lifecycleStream.Fence()
		}

		s.lifecycleStream = lifecycle.NewStream(streamID, negotiated)
		if streamErr = s.emitLifecycleLocked(ctx, lifecycle.SnapshotEvent(cycleID, s.provenQuiescence())); streamErr != nil {
			s.lifecycleFailure = streamErr
			if s.lifecycleStream != nil {
				s.lifecycleStream.Fence()
			}

			return streamErr
		}
	}

	s.nativeEventOpened = true

	pending := append([]codex.Event(nil), s.preOpenEvents...)
	s.preOpenEvents = nil

	if rebind {
		pending = append(pending, s.nativeRebindEvents...)
		s.nativeRebindEvents = nil
		s.nativeEventRebinding = false
		s.nativeEventReplaying = true
	}

	for index := range pending {
		event := pending[index]
		if err := s.routeNativeEventLocked(ctx, event); err != nil {
			s.nativeEventReplaying = false

			s.lifecycleFailure = err
			if s.lifecycleStream != nil {
				s.lifecycleStream.Fence()
			}

			return err
		}
	}

	s.nativeEventReplaying = false

	return nil
}

func (s *session) openIncarnation(
	ctx context.Context,
	negotiated lifecycle.Negotiated,
) (*promptIncarnation, error) {
	if err := s.awaitLifecycleEstablishment(ctx); err != nil {
		return nil, err
	}

	if err := s.openLifecycleStream(ctx, negotiated); err != nil {
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

	turnNonce := s.activeTurnNonce()

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.lifecycleFailure != nil {
		return nil, s.lifecycleFailure
	}

	if s.incarnation != nil {
		return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: valueBackpressure, jsonFieldLimit: limitSessionPrompt})
	}

	if s.agentIncarnation != nil {
		return nil, acp.NewInvalidRequest(map[string]any{
			jsonFieldError: valueBackpressure,
			jsonFieldLimit: limitSessionPrompt,
		})
	}

	s.permissionTools.reset()

	in := &promptIncarnation{
		session: s, stream: s.lifecycleStream, cycleID: cycleID, turnID: turnID,
		turnNonce: turnNonce, events: make(chan codex.Event, sessionNativeEventBuffer+1),
	}
	if !negotiated.Present() && !s.nativeEventSource {
		return nil, nil //nolint:nilnil // No lifecycle incarnation is needed when neither transport exists.
	}

	s.incarnation = in
	s.signalLifecycleChangedLocked()

	return in, nil
}

func (s *session) failLifecycleEstablishment(ctx context.Context, responseErr error) error {
	s.lifecycleMu.Lock()
	_ = s.latchLifecycleFailureLocked(responseErr)
	s.lifecycleMu.Unlock()

	containCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()

	return errors.Join(responseErr, s.Close(containCtx))
}

func (s *session) provenQuiescence() lifecycle.QuiescenceFact {
	return lifecycle.QuiescenceFact{}
}

func (s *session) liveIncarnation() *promptIncarnation {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	return s.liveIncarnationLocked()
}

func (s *session) liveIncarnationLocked() *promptIncarnation {
	if s.incarnation != nil {
		return s.incarnation
	}

	return s.agentIncarnation
}

func (s *session) nativePromptEvents(in *promptIncarnation) <-chan codex.Event {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if !s.nativeEventSource || in == nil {
		return nil
	}

	return in.events
}

func (s *session) clearIncarnation(in *promptIncarnation) {
	if in == nil {
		return
	}

	s.lifecycleMu.Lock()

	if s.incarnation == in {
		if in.accepted && !in.settled && s.lifecycleFailure != nil {
			s.closeCycleEventsLocked(in)
			s.signalLifecycleChangedLocked()
			s.lifecycleMu.Unlock()

			return
		}

		s.rememberTerminalNativeTurnLocked(in.nativeTurnID)
		s.closeCycleEventsLocked(in)
		s.incarnation = nil
		s.signalLifecycleChangedLocked()
	}
	s.lifecycleMu.Unlock()
}

func (s *session) signalLifecycleChangedLocked() {
	if s.lifecycleChanged != nil {
		close(s.lifecycleChanged)
	}

	s.lifecycleChanged = make(chan struct{})
}

func (s *session) emitLifecycleLocked(ctx context.Context, event lifecycle.Event) error {
	if s.lifecycleStream == nil {
		return nil
	}

	envelope, err := s.lifecycleStream.Emit(event)
	if err != nil {
		return s.latchLifecycleFailureLocked(err)
	}

	if s.agent.connection() == nil {
		return nil
	}

	result, err := s.enqueueLifecycleDeliveryLocked(ctx, acp.SessionNotification{
		Meta:      map[string]any{lifecycle.MetaKey: envelope},
		SessionId: s.id,
		Update:    acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{}},
	})
	if err != nil {
		return s.latchLifecycleFailureLocked(err)
	}

	s.lifecycleMu.Unlock()

	err = <-result

	s.lifecycleMu.Lock()
	if err != nil {
		return s.latchLifecycleFailureLocked(err)
	}

	return nil
}

func (s *session) latchLifecycleFailureLocked(err error) error {
	if err == nil {
		return nil
	}

	if s.lifecycleFailure == nil {
		s.lifecycleFailure = err
		if s.lifecycleStream != nil {
			s.lifecycleStream.Fence()
		}

		s.signalLifecycleChangedLocked()
	}

	return err
}

func (in *promptIncarnation) emit(ctx context.Context, event lifecycle.Event) error {
	if !in.lifecycleActive() {
		return nil
	}

	in.session.lifecycleMu.Lock()
	defer in.session.lifecycleMu.Unlock()

	return in.session.emitLifecycleLocked(ctx, event)
}

func (in *promptIncarnation) emitLocked(ctx context.Context, event lifecycle.Event) error {
	return in.session.emitLifecycleLocked(ctx, event)
}

func (in *promptIncarnation) accept(ctx context.Context, submission lifecycle.Submission) error {
	if in == nil {
		return nil
	}

	return in.acceptNative(ctx, submission, in.session.activeTurnID())
}

func (in *promptIncarnation) acceptNative(
	ctx context.Context,
	submission lifecycle.Submission,
	nativeTurnID string,
) error {
	if in == nil {
		return nil
	}

	s := in.session
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.incarnation != in || in.settled {
		return errors.New("codex prompt lifecycle cycle is no longer current")
	}

	if s.lifecycleFailure != nil {
		return s.lifecycleFailure
	}

	in.accepted = true
	in.nativeTurnID = nativeTurnID

	s.signalLifecycleChangedLocked()

	// Bind and enqueue the whole pre-acknowledgement prefix before lifecycle
	// delivery temporarily releases lifecycleMu. Otherwise a terminal event can
	// arrive during that release, close the queue, and strand an earlier event
	// in preBind behind it. The prompt consumer cannot observe this queue until
	// acceptNative returns, so lifecycle acceptance still precedes every ACP
	// update while native event order remains exact.
	pending := in.preBind
	in.preBind = nil

	for index := range pending {
		event := pending[index]
		if event.TurnID != nativeTurnID {
			return s.latchLifecycleFailureLocked(errors.New("codex prompt acknowledgement raced a different native foreground turn"))
		}

		if !s.enqueuePromptEventLocked(in, event) {
			return codex.ErrTurnEventOverflow
		}
	}

	if in.lifecycleActive() {
		if err := in.emitLocked(ctx, lifecycle.AcceptedEvent(submission, in.turnID)); err != nil {
			return err
		}

		if err := in.emitLocked(ctx, lifecycle.TransitionEvent(lifecycle.ForegroundRunning, in.cycleID, in.turnID)); err != nil {
			return err
		}
	}

	return nil
}

// acceptBufferedNative proves dispatch from notifications that arrived before
// a failed or malformed turn/start acknowledgement. Those notifications are
// never thrown away: a single exact native identity is bound and returned for
// ordered processing and containment; multiple identities fence the source.
func (in *promptIncarnation) acceptBufferedNative(
	ctx context.Context,
	submission lifecycle.Submission,
) (string, []codex.Event, []string, error) {
	if in == nil {
		return "", nil, nil, nil
	}

	s := in.session
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.incarnation != in || in.settled {
		return "", nil, nil, errors.New("codex prompt lifecycle cycle is no longer current")
	}

	if len(in.preBind) == 0 {
		return "", nil, nil, nil
	}

	identities := make([]string, 0, 1)
	seen := make(map[string]struct{})

	for index := range in.preBind {
		event := in.preBind[index]
		if event.TurnID == "" {
			err := errors.New("codex emitted pre-acknowledgement activity without a native turn identity")

			return "", nil, identities, s.latchLifecycleFailureLocked(err)
		}

		if _, ok := seen[event.TurnID]; !ok {
			seen[event.TurnID] = struct{}{}
			identities = append(identities, event.TurnID)
		}
	}

	if len(identities) != 1 {
		err := errors.New("codex emitted concurrent native turns before turn acknowledgement")

		return "", nil, identities, s.latchLifecycleFailureLocked(err)
	}

	in.accepted = true
	in.nativeTurnID = identities[0]
	events := append([]codex.Event(nil), in.preBind...)
	in.preBind = nil

	s.signalLifecycleChangedLocked()

	if in.lifecycleActive() {
		if err := in.emitLocked(ctx, lifecycle.AcceptedEvent(submission, in.turnID)); err != nil {
			return identities[0], events, identities, err
		}

		if err := in.emitLocked(ctx, lifecycle.TransitionEvent(lifecycle.ForegroundRunning, in.cycleID, in.turnID)); err != nil {
			return identities[0], events, identities, err
		}
	}

	return identities[0], events, identities, nil
}

func (in *promptIncarnation) announceAction(
	ctx context.Context,
	actionID string,
	kind lifecycle.ActionKind,
	blocksForeground bool,
) error {
	if !in.lifecycleActive() {
		return nil
	}

	s := in.session
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if !in.accepted || in.settled || s.lifecycleFailure != nil {
		return errors.New("codex lifecycle action has no live owning turn")
	}

	if len(in.stream.State().Actions) == lifecycleActionLimit {
		return s.latchLifecycleFailureLocked(fmt.Errorf("%w: lifecycle action registry", codex.ErrTurnEventOverflow))
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

func (in *promptIncarnation) resolveAction(ctx context.Context, actionID string, state lifecycle.ActionState) error {
	if !in.lifecycleActive() {
		return nil
	}

	s := in.session
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

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

	for _, action := range in.stream.State().Actions {
		if action.BlocksForeground && !action.State.Terminal() {
			return nil
		}
	}

	cause := lifecycle.CauseSubmission
	if in.autonomous {
		cause = lifecycle.CauseActivity
	}

	return in.emitLocked(ctx, lifecycle.TransitionEventWithCause(
		lifecycle.ForegroundRunning, in.cycleID, in.turnID, cause,
	))
}

func (in *promptIncarnation) settle(
	ctx context.Context,
	stopReason acp.StopReason,
	outcome lifecycle.Outcome,
) error {
	if !in.lifecycleActive() {
		return nil
	}

	s := in.session
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

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

	cause := lifecycle.CauseSubmission
	if in.autonomous {
		cause = lifecycle.CauseActivity
	}

	return in.emitLocked(ctx, lifecycle.IdleEventWithCause(in.cycleID, in.turnID, cause, reason, outcome))
}

func (s *session) routeNativeEvent(event codex.Event) error {
	s.lifecycleRouteMu.Lock()

	defer s.lifecycleRouteMu.Unlock()

	routeCtx, cancel := context.WithTimeout(context.TODO(), promptSettlementTimeout)
	defer cancel()

	s.lifecycleMu.Lock()

	s.nativeRouteCancel = cancel
	defer s.lifecycleMu.Unlock()
	defer func() {
		s.nativeRouteCancel = nil
	}()

	return s.routeNativeEventLocked(routeCtx, event)
}

func (s *session) routeNativeEventLocked(ctx context.Context, event codex.Event) error {
	if s.lifecycleFailure != nil {
		return s.lifecycleFailure
	}

	if event.Scope == codex.EventScopeTransportLost {
		if event.Err != nil {
			return event.Err
		}

		return codex.ErrConnectionClosed
	}

	if event.Scope != codex.EventScopeThread || event.ThreadID != s.codexThreadID {
		return fmt.Errorf("codex thread broker received an event outside its exact thread")
	}

	if event.TurnID == "" {
		return s.applyTurnlessThreadEventLocked(ctx, event)
	}

	if canary := s.nativeCanary; canary != nil {
		if canary.turnID == "" {
			if len(canary.preBind) == sessionNativeEventBuffer {
				return codex.ErrTurnEventOverflow
			}

			canary.preBind = append(canary.preBind, event)

			return nil
		}

		if canary.turnID != event.TurnID {
			if s.nativeEventRebinding {
				if s.enqueueNativeRebindEventLocked(event) {
					return nil
				}

				return codex.ErrTurnEventOverflow
			}

			return errors.New("codex runtime_ready broker received a concurrent native turn")
		}

		if !s.enqueueCanaryEventLocked(canary, event) {
			return codex.ErrTurnEventOverflow
		}

		return nil
	}

	if s.nativeEventRebinding {
		if !s.enqueueNativeRebindEventLocked(event) {
			return codex.ErrTurnEventOverflow
		}

		return nil
	}

	if !s.nativeEventOpened {
		if len(s.preOpenEvents) == sessionNativeEventBuffer {
			return codex.ErrTurnEventOverflow
		}

		s.preOpenEvents = append(s.preOpenEvents, event)

		return nil
	}

	if current := s.incarnation; current != nil {
		if current.nativeTurnID == "" {
			if len(current.preBind) == sessionNativeEventBuffer {
				return codex.ErrTurnEventOverflow
			}

			current.preBind = append(current.preBind, event)

			return nil
		}

		if current.nativeTurnID == event.TurnID {
			if !s.enqueuePromptEventLocked(current, event) {
				return codex.ErrTurnEventOverflow
			}

			return nil
		}
	}

	if s.lifecycleClosing {
		return nil
	}

	return s.routeAutonomousEventLocked(ctx, event)
}

// applyTurnlessThreadEventLocked disposes of a thread event the app-server
// attributed to no turn. Thread-scoped notices — guardian warnings, status,
// name, queue, and goal changes — carry a thread and nothing else, so only a
// kind that is itself turn evidence has to name the turn it belongs to.
func (s *session) applyTurnlessThreadEventLocked(ctx context.Context, event codex.Event) error {
	switch event.Kind {
	case codex.EventAccountUpdated:
		s.setAccount(redactedAccountMeta(event.Account))

		return nil
	case codex.EventWarning:
		return s.deliverTurnlessWarningLocked(ctx, event)
	case codex.EventRaw:
		return nil
	default:
		return errors.New("codex thread event omitted its native turn identity")
	}
}

// deliverTurnlessWarningLocked shows a guardian warning that named no turn on
// the session's live foreground turn. The warning is a security signal about
// this exact thread and is meant for the user, so it reaches the user on the
// only stream the user is watching; without a live foreground turn there is no
// stream to show it on and the warning is dropped.
func (s *session) deliverTurnlessWarningLocked(ctx context.Context, event codex.Event) error {
	updates := eventUpdates(event)

	in := s.liveIncarnationLocked()
	if len(updates) == 0 || in == nil || in.settled {
		return nil
	}

	ctx = withTurnRoute(ctx, in.turnNonce)

	s.lifecycleMu.Unlock()
	err := s.emitUpdates(ctx, updates...)
	s.lifecycleMu.Lock()

	return err
}

func (s *session) enqueuePromptEventLocked(in *promptIncarnation, event codex.Event) bool {
	if in.eventsClosed {
		return false
	}

	if len(in.events) == sessionNativeEventBuffer {
		in.events <- codex.Event{
			Kind: codex.EventError, Scope: codex.EventScopeThread,
			ThreadID: s.codexThreadID, TurnID: in.nativeTurnID, Err: codex.ErrTurnEventOverflow,
		}

		s.closeCycleEventsLocked(in)

		return false
	}

	in.events <- event

	if event.Kind == codex.EventCompleted || event.Kind == codex.EventError {
		s.closeCycleEventsLocked(in)
	}

	return true
}

func (s *session) closeCycleEventsLocked(in *promptIncarnation) {
	if in == nil || in.eventsClosed {
		return
	}

	in.eventsClosed = true
	close(in.events)
}

func (s *session) routeAutonomousEventLocked(ctx context.Context, event codex.Event) error {
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, promptSettlementTimeout)
		defer cancel()
	}

	// A settled turn keeps flushing: a late token-usage update, a completed item
	// racing its turn terminal. None of it reopens the turn, and none of it is
	// the start of a new one.
	if _, terminal := s.terminalNativeTurns[event.TurnID]; terminal {
		return nil
	}

	if s.incarnation != nil {
		return errors.New("codex opened an agent-origin foreground turn while a prompt foreground turn was live")
	}

	in := s.agentIncarnation
	if in == nil {
		var err error

		in, err = s.openAutonomousTurnLocked(ctx, event.TurnID)
		if err != nil {
			return err
		}
	} else if in.nativeTurnID != event.TurnID {
		return errors.New("codex opened concurrent agent-origin foreground turns on one thread")
	}

	if in.terminating != nil {
		return nil
	}

	routeCtx := withTurnRoute(ctx, in.turnNonce)
	s.lifecycleMu.Unlock()
	handleErr := s.handleAutonomousEvent(routeCtx, event, in.state)
	s.lifecycleMu.Lock()

	if in.terminating != nil || s.lifecycleClosing {
		return handleErr
	}

	if handleErr != nil {
		turnNonce := in.turnNonce
		s.lifecycleMu.Unlock()
		_, containmentErr := s.shutdownAgentTurnForNonce(ctx, turnNonce)
		s.lifecycleMu.Lock()

		return errors.Join(handleErr, containmentErr)
	}

	terminal := event.Kind == codex.EventCompleted || event.Kind == codex.EventError
	if !terminal {
		return nil
	}

	var stopReason acp.StopReason

	outcome := lifecycle.OutcomeSuccess

	if event.Kind == codex.EventError || in.state.nativeFailure {
		stopReason = ""
		outcome = lifecycle.OutcomeFailed
	} else {
		stopReason, _ = promptStopReason(event.StopReason)
	}

	boundary := &turnContainment{done: make(chan struct{}), started: true}
	in.terminating = boundary
	s.lifecycleMu.Unlock()
	settleErr := s.completeAutonomousSettlement(routeCtx, in, boundary, nil, lifecycle.ActionFailed, stopReason, outcome)
	s.lifecycleMu.Lock()

	return errors.Join(handleErr, settleErr)
}

func (s *session) completeAutonomousSettlement(
	ctx context.Context,
	in *promptIncarnation,
	boundary *turnContainment,
	priorErr error,
	actionState lifecycle.ActionState,
	stopReason acp.StopReason,
	outcome lifecycle.Outcome,
) error {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), promptSettlementTimeout)
	if deadline, ok := ctx.Deadline(); ok {
		cancel()
		settleCtx, cancel = context.WithDeadline(context.WithoutCancel(ctx), deadline)
	}
	defer cancel()

	mirrorErr := s.mirrorAndEmitRollout(settleCtx)
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.agentIncarnation != in || in.settled {
		boundary.err = errors.Join(priorErr, mirrorErr, errors.New("codex agent-origin lifecycle turn changed during settlement"))
		close(boundary.done)

		return boundary.err
	}

	if mirrorErr != nil {
		in.settled = true
		s.rememberTerminalNativeTurnLocked(in.nativeTurnID)
		s.agentIncarnation = nil
		boundary.err = errors.Join(priorErr, mirrorErr)
		_ = s.latchLifecycleFailureLocked(boundary.err)
		close(boundary.done)
		s.signalLifecycleChangedLocked()

		return boundary.err
	}

	settleErr := s.settleAutonomousTurnLocked(settleCtx, in, actionState, stopReason, outcome)

	boundary.err = errors.Join(priorErr, settleErr)
	if settleErr != nil && s.agentIncarnation == in {
		in.settled = true
		s.rememberTerminalNativeTurnLocked(in.nativeTurnID)
		s.agentIncarnation = nil
		s.signalLifecycleChangedLocked()
	}

	close(boundary.done)

	if boundary.err != nil {
		_ = s.latchLifecycleFailureLocked(boundary.err)
	}

	return boundary.err
}

func (s *session) settleAutonomousTurnLocked(
	ctx context.Context,
	in *promptIncarnation,
	actionState lifecycle.ActionState,
	stopReason acp.StopReason,
	outcome lifecycle.Outcome,
) error {
	if in == nil || in.settled {
		return nil
	}

	if in.stream != nil {
		for _, action := range in.stream.State().Actions {
			if action.State.Terminal() {
				continue
			}

			if err := in.resolveActionLocked(ctx, action.ActionID, actionState); err != nil {
				return err
			}
		}
	}

	in.settled = true
	if err := in.emitLocked(ctx, lifecycle.IdleEventWithCause(
		in.cycleID, in.turnID, lifecycle.CauseActivity, string(stopReason), outcome,
	)); err != nil {
		return err
	}

	s.rememberTerminalNativeTurnLocked(in.nativeTurnID)

	if s.agentIncarnation == in {
		s.agentIncarnation = nil
		s.signalLifecycleChangedLocked()
	}

	return nil
}

func (s *session) openAutonomousTurnLocked(ctx context.Context, nativeTurnID string) (*promptIncarnation, error) {
	if s.lifecycleClosing || s.nativeEventRebinding {
		return nil, errors.New("codex agent-origin lifecycle admission is closed")
	}

	if s.incarnation != nil {
		return nil, errors.New("codex agent-origin lifecycle cannot overlap a prompt foreground turn")
	}

	s.permissionTools.reset()

	cycleID, err := newSessionID()
	if err != nil {
		return nil, err
	}

	var text strings.Builder

	in := &promptIncarnation{
		session: s, stream: s.lifecycleStream, cycleID: cycleID,
		turnID: nativeTurnID, nativeTurnID: nativeTurnID, turnNonce: cycleID,
		autonomous: true, accepted: true,
		state: &promptEventState{
			snapshot: s.snapshot(), agentDeltaItems: map[string]struct{}{},
			reasoningDeltaItems: map[string]struct{}{}, agentText: &text,
			stopReason: acp.StopReasonEndTurn, imageTools: newImageToolState(),
			toolContents: make(map[acp.ToolCallId][]acp.ToolCallContent),
		},
	}
	ctx = withTurnRoute(ctx, in.turnNonce)
	s.agentIncarnation = in
	s.signalLifecycleChangedLocked()

	if err = in.emitLocked(ctx, lifecycle.TransitionEventWithCause(
		lifecycle.ForegroundRunning, cycleID, in.turnID, lifecycle.CauseActivity,
	)); err != nil {
		if s.agentIncarnation == in && in.terminating == nil && !s.lifecycleClosing {
			s.agentIncarnation = nil
			s.signalLifecycleChangedLocked()
		}

		return nil, err
	}

	if s.agentIncarnation != in || in.terminating != nil || s.lifecycleClosing {
		return nil, context.Canceled
	}

	return in, nil
}

// claimServerRequestTurn binds an app-server request to the foreground turn it
// names, returning the routing context the answer is delivered under. An MCP
// elicitation is the one request whose turn identity is optional: an MCP server
// can elicit outside any turn, and such a request belongs to no turn rather
// than to a missing one. Belonging to no turn, it has no turn stream to carry
// the question or the answer, so it is answered with a typed cancel — never a
// refusal the user did not make, and never a failed session.
func (s *session) claimServerRequestTurn(
	ctx context.Context,
	method string,
	params map[string]any,
) (context.Context, error) {
	nativeTurnID := codex.RequestTurnID(params)
	if nativeTurnID == "" && method == codex.RequestMCPElicitation {
		return ctx, nil
	}

	turn, lifecycleOwned, err := s.claimLifecycleTurn(ctx, nativeTurnID)
	if err != nil {
		return ctx, err
	}

	if !lifecycleOwned {
		return ctx, s.waitForTurnBinding(ctx)
	}

	return withLifecycleActionTurn(withTurnRoute(ctx, turn.turnNonce), turn), nil
}

// claimLifecycleTurn binds an app-server request to its exact foreground turn.
// The request itself is native proof of activity, so it can open an
// agent-origin turn before any notification for that turn arrives.
func (s *session) claimLifecycleTurn(ctx context.Context, nativeTurnID string) (*promptIncarnation, bool, error) {
	if err := s.awaitLifecycleEstablishment(ctx); err != nil {
		return nil, true, err
	}

	for {
		s.lifecycleMu.Lock()
		if s.lifecycleClosing {
			s.lifecycleMu.Unlock()

			return nil, true, context.Canceled
		}

		if !s.nativeEventRebinding && !s.nativeEventReplaying {
			break
		}

		if s.lifecycleChanged == nil {
			s.lifecycleChanged = make(chan struct{})
		}

		changed := s.lifecycleChanged
		s.lifecycleMu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return nil, true, ctx.Err()
		}
	}

	if s.lifecycleStream == nil {
		s.lifecycleMu.Unlock()

		return nil, false, nil
	}

	if nativeTurnID == "" {
		s.lifecycleMu.Unlock()

		return nil, true, errors.New("codex server request omitted its native turn identity")
	}

	if s.lifecycleFailure != nil {
		err := s.lifecycleFailure
		s.lifecycleMu.Unlock()

		return nil, true, err
	}

	if current := s.incarnation; current != nil {
		if current.accepted && current.nativeTurnID == nativeTurnID {
			s.lifecycleMu.Unlock()

			return current, true, nil
		}

		if current.nativeTurnID != "" {
			s.lifecycleMu.Unlock()

			return nil, true, errors.New("codex server request belongs to a concurrent native turn")
		}
		s.lifecycleMu.Unlock()
		current, err := s.waitForLifecycleTurn(ctx, nativeTurnID)

		return current, true, err
	}

	if current := s.agentIncarnation; current != nil {
		if current.nativeTurnID == nativeTurnID {
			s.lifecycleMu.Unlock()

			return current, true, nil
		}
		s.lifecycleMu.Unlock()

		return nil, true, errors.New("codex server request belongs to a concurrent agent-origin turn")
	}

	if _, terminal := s.terminalNativeTurns[nativeTurnID]; terminal {
		s.lifecycleMu.Unlock()

		return nil, true, errors.New("codex server request belongs to a terminal native turn")
	}

	in, err := s.openAutonomousTurnLocked(ctx, nativeTurnID)
	s.lifecycleMu.Unlock()

	return in, true, err
}

func (s *session) beginLifecycleClose(ctx context.Context) error {
	s.lifecycleMu.Lock()
	s.lifecycleClosing = true
	cancelRoute := s.nativeRouteCancel
	obligation := s.establishment

	var cancelBlockedDelivery context.CancelFunc
	if s.lifecycleDeliveryActive {
		cancelBlockedDelivery = s.lifecycleDeliveryCancel
	}

	s.establishment = nil
	s.establishmentRebind = false
	s.signalLifecycleChangedLocked()
	s.lifecycleMu.Unlock()

	if cancelRoute != nil {
		cancelRoute()
	}

	if cancelBlockedDelivery != nil {
		cancelBlockedDelivery()
	}

	if obligation != nil {
		obligation.hooks.cancel(obligation, errEstablishmentCancelled)
	}

	gateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()

	if deadline, ok := ctx.Deadline(); ok {
		cancel()
		gateCtx, cancel = context.WithDeadline(context.WithoutCancel(ctx), deadline)

		defer cancel()
	}

	if obligation != nil {
		if err := obligation.wait(gateCtx); err != nil {
			return err
		}
	}

	if err := lockLifecycleRoute(gateCtx, &s.lifecycleRouteMu); err != nil {
		return err
	}
	s.lifecycleRouteMu.Unlock()

	return nil
}

type lifecycleRouteGate struct {
	once  sync.Once
	token chan struct{}
}

func (g *lifecycleRouteGate) channel() chan struct{} {
	g.once.Do(func() {
		g.token = make(chan struct{}, 1)
		g.token <- struct{}{}
	})

	return g.token
}

func (g *lifecycleRouteGate) Lock() {
	<-g.channel()
}

func (g *lifecycleRouteGate) Unlock() {
	g.channel() <- struct{}{}
}

func lockLifecycleRoute(ctx context.Context, gate *lifecycleRouteGate) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	select {
	case <-gate.channel():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *session) handleAutonomousEvent(ctx context.Context, event codex.Event, state *promptEventState) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if event.Kind == codex.EventAccountUpdated {
		s.setAccount(redactedAccountMeta(event.Account))
	}

	state.nativeIdentity = nativeIdentityFromEvent(event, state.nativeIdentity)
	if err := s.emitRawCodexEvent(ctx, event); err != nil {
		s.recordRawEmitFailure(ctx, err)

		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	visible := dedupeCompletedTextEvent(event, state.agentDeltaItems, state.reasoningDeltaItems)
	if visible.Kind == codex.EventAgentMessageDelta {
		visible = dedupeCompletedAggregateTextEvent(visible, state.agentText.String())
	}

	if err := s.emitPromptUpdates(ctx, event, visible, state); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	s.applyPromptUsage(event, state)

	if visible.Kind == codex.EventAgentMessageDelta && visible.Text != "" {
		state.agentText.WriteString(visible.Text)
	}

	return nil
}

// rememberTerminalNativeTurnLocked tombstones a settled native turn so its
// trailing events are never read as a new one. Retention is bounded and the
// oldest tombstone is evicted, because the oldest settled turn is the one the
// app-server is least likely to still be flushing.
func (s *session) rememberTerminalNativeTurnLocked(turnID string) {
	if turnID == "" {
		return
	}

	if s.terminalNativeTurns == nil {
		s.terminalNativeTurns = make(map[string]struct{})
	}

	if _, exists := s.terminalNativeTurns[turnID]; exists {
		return
	}

	if len(s.terminalNativeTurnOrder) == terminalNativeTurnLimit {
		delete(s.terminalNativeTurns, s.terminalNativeTurnOrder[s.terminalNativeTurnNext])
		s.terminalNativeTurnOrder[s.terminalNativeTurnNext] = turnID
		s.terminalNativeTurnNext = (s.terminalNativeTurnNext + 1) % terminalNativeTurnLimit
	} else {
		s.terminalNativeTurnOrder = append(s.terminalNativeTurnOrder, turnID)
	}

	s.terminalNativeTurns[turnID] = struct{}{}
}

func (s *session) failNativeIncarnation(err error) {
	if err == nil {
		err = codex.ErrConnectionClosed
	}

	s.mu.Lock()
	interactions := s.detachInteractionsLocked()
	s.mu.Unlock()

	for _, cancel := range interactions {
		cancel()
	}

	s.lifecycleRouteMu.Lock()
	defer s.lifecycleRouteMu.Unlock()

	s.lifecycleMu.Lock()
	if current := s.agentIncarnation; current != nil && current.terminating != nil {
		s.lifecycleMu.Unlock()
		s.markClientDead()

		return
	}

	if s.lifecycleFailure == nil {
		if current := s.agentIncarnation; current != nil {
			boundary := &turnContainment{done: make(chan struct{}), started: true}
			current.terminating = boundary
			s.lifecycleMu.Unlock()
			_ = s.completeAutonomousSettlement(
				context.TODO(), current, boundary, err, lifecycle.ActionFailed, "", lifecycle.OutcomeFailed,
			)
			s.lifecycleMu.Lock()
		}

		if s.lifecycleFailure == nil {
			s.lifecycleFailure = err
		}

		if s.incarnation != nil && !s.incarnation.eventsClosed {
			s.enqueuePromptEventLocked(s.incarnation, codex.Event{
				Kind: codex.EventError, Scope: codex.EventScopeThread,
				ThreadID: s.codexThreadID, TurnID: s.incarnation.nativeTurnID, Err: err,
			})
		}

		if s.lifecycleStream != nil {
			s.lifecycleStream.Fence()
		}

		s.signalLifecycleChangedLocked()
	}
	s.lifecycleMu.Unlock()
	s.markClientDead()
}

func (s *session) waitForLifecycleTurn(ctx context.Context, nativeTurnID string) (*promptIncarnation, error) {
	for {
		s.lifecycleMu.Lock()
		if current := s.incarnation; current != nil && current.accepted && current.nativeTurnID == nativeTurnID {
			s.lifecycleMu.Unlock()

			return current, nil
		}

		if current := s.incarnation; current != nil && current.accepted && current.nativeTurnID != nativeTurnID {
			s.lifecycleMu.Unlock()

			return nil, errors.New("codex server request belongs to a concurrent native turn")
		}

		if current := s.agentIncarnation; current != nil && current.nativeTurnID == nativeTurnID {
			s.lifecycleMu.Unlock()

			return current, nil
		}

		if s.lifecycleFailure != nil {
			err := s.lifecycleFailure
			s.lifecycleMu.Unlock()

			return nil, err
		}

		if s.lifecycleChanged == nil {
			s.lifecycleChanged = make(chan struct{})
		}

		changed := s.lifecycleChanged
		s.lifecycleMu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s *session) fenceSession() {
	closeCtx, cancelClose := context.WithTimeout(context.TODO(), closeTimeout)
	_ = s.beginLifecycleClose(closeCtx)
	gateErr := lockLifecycleRoute(closeCtx, &s.lifecycleRouteMu)
	s.lifecycleMu.Lock()
	if gateErr != nil {
		if s.lifecycleStream != nil {
			s.lifecycleStream.Fence()
		}

		s.closeCycleEventsLocked(s.incarnation)

		s.incarnation = nil
		if s.agentIncarnation != nil {
			s.agentIncarnation.settled = true
			s.agentIncarnation = nil
		}

		s.signalLifecycleChangedLocked()
		s.lifecycleMu.Unlock()
		cancelClose()

		stopCtx, cancel := context.WithTimeout(context.TODO(), closeTimeout)
		_ = s.stopLifecycleDeliveries(stopCtx)
		_ = s.stopNativeEventsContext(stopCtx)

		cancel()

		return
	}

	current := s.agentIncarnation

	var boundary *turnContainment
	if current != nil && current.terminating == nil {
		boundary = &turnContainment{done: make(chan struct{}), started: true}
		current.terminating = boundary
	}
	s.lifecycleMu.Unlock()

	if boundary != nil {
		_ = s.completeAutonomousSettlement(
			context.TODO(), current, boundary, nil, lifecycle.ActionFailed, "", lifecycle.OutcomeFailed,
		)
	}

	s.lifecycleMu.Lock()
	if s.lifecycleStream != nil {
		s.lifecycleStream.Fence()
	}

	s.closeCycleEventsLocked(s.incarnation)
	s.incarnation = nil
	s.signalLifecycleChangedLocked()
	s.lifecycleMu.Unlock()
	s.lifecycleRouteMu.Unlock()
	cancelClose()

	stopCtx, cancel := context.WithTimeout(context.TODO(), closeTimeout)
	_ = s.stopLifecycleDeliveries(stopCtx)
	_ = s.stopNativeEventsContext(stopCtx)

	cancel()
}

func (s *session) shutdownAgentTurn(ctx context.Context) (bool, error) {
	return s.shutdownAgentTurnMatching(ctx, "", false)
}

func (s *session) shutdownAgentTurnForNonce(ctx context.Context, turnNonce string) (bool, error) {
	return s.shutdownAgentTurnMatching(ctx, turnNonce, true)
}

func (s *session) shutdownAgentTurnMatching(
	ctx context.Context,
	expectedNonce string,
	requireExactNonce bool,
) (bool, error) {
	s.lifecycleMu.Lock()

	in := s.agentIncarnation
	if in == nil || in.settled {
		s.lifecycleMu.Unlock()

		return false, nil
	}

	if requireExactNonce && in.turnNonce != expectedNonce {
		s.lifecycleMu.Unlock()

		return true, errTurnRouteMismatch
	}

	if in.terminating != nil {
		boundary := in.terminating
		done := boundary.done
		s.lifecycleMu.Unlock()

		select {
		case <-done:
			return true, boundary.err
		case <-ctx.Done():
			return true, ctx.Err()
		}
	}

	boundary := &turnContainment{done: make(chan struct{}), started: true}
	in.terminating = boundary
	in.cancelled = true
	nativeTurnID := in.nativeTurnID
	s.lifecycleMu.Unlock()

	s.mu.Lock()
	client := s.client
	threadID := s.codexThreadID
	interactions := s.detachInteractionsLocked()
	s.mu.Unlock()

	for _, cancel := range interactions {
		cancel()
	}

	s.nativeControlMu.Lock()
	defer s.nativeControlMu.Unlock()

	var interruptErr error
	if client == nil || threadID == "" || nativeTurnID == "" {
		interruptErr = errors.New("codex agent-origin turn has no exact native cancellation target")
	} else {
		interruptCtx, cancelInterrupt := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		interruptErr = client.CancelTurn(interruptCtx, threadID, nativeTurnID)

		cancelInterrupt()
	}

	containmentErr := s.containCancelledTurn(ctx, client, threadID, interruptErr)
	resultErr := s.completeAutonomousSettlement(
		ctx, in, boundary, containmentErr, lifecycle.ActionCancelled, acp.StopReasonCancelled, lifecycle.OutcomeCancelled,
	)

	return true, resultErr
}
