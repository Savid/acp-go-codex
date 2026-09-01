package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/lifecycle"
	"github.com/savid/acp-go-codex/internal/observer"
)

const (
	codexUsageMetaKey         = "usage"
	codexThreadUsageMetaKey   = "threadUsage"
	usageCachedReadTokensKey  = "cachedReadTokens"
	usageCachedWriteTokensKey = "cachedWriteTokens"
	usageInputTokensKey       = "inputTokens"
	usageOutputTokensKey      = "outputTokens"
	usageReasoningOutputKey   = "reasoningOutputTokens"
	usageTotalTokensKey       = "totalTokens"

	sandboxModeWorkspaceWrite   = "workspace-write"
	sandboxModeReadOnly         = "read-only"
	sandboxModeDangerFullAccess = "danger-full-access"
	toolKindCommandExecution    = "commandExecution"
	toolKindFileChange          = "fileChange"
	toolKindMcpToolCall         = "mcpToolCall"
	toolKindDynamicToolCall     = "dynamicToolCall"
	toolKindShell               = "shell"
	toolKindPatch               = "patch"

	sandboxTypeDangerFullAccess = "dangerFullAccess"
	sandboxTypeReadOnly         = "readOnly"
	sandboxTypeWorkspaceWrite   = "workspaceWrite"
)

// promptToCodex maps validated ACP prompt content into native Codex input.
func promptToCodex(blocks []acp.ContentBlock, images []codex.PromptImage) ([]codex.UserInput, error) {
	if len(blocks) == 0 {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: jsonFieldPrompt})
	}

	input, err := codex.PromptToUserInput(blocks, images)
	if err == nil {
		return input, nil
	}

	return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: jsonFieldPrompt})
}

func (s *session) preparePromptInput(ctx context.Context, blocks []acp.ContentBlock) (
	[]codex.UserInput,
	func(),
	error,
) {
	images, imageErr, abortErr := validatePromptImages(
		ctx,
		blocks,
		s.agent.options.ImageLimits,
		s.agent.options.InputHandoffRoot,
	)
	if abortErr != nil {
		return nil, nil, abortErr
	}

	if imageErr != nil {
		return nil, nil, imageErr.invalidParams()
	}

	if err := s.ensureLiveClient(ctx); err != nil {
		return nil, nil, s.mapTurnFailure(fmt.Errorf("%w: %w", codex.ErrConnectionClosed, err))
	}

	if len(images) > 0 &&
		selectedModelImageSupport(modelList(ctx, s.client), s.currentModel()) == imageInputUnsupported {
		imageErr = &promptImageError{
			code:  imageErrorUnsupportedByModel,
			field: images[0].field,
			index: images[0].index,
		}

		return nil, nil, imageErr.invalidParams()
	}

	prepared, err := s.preparePromptImages(ctx, images)
	if err != nil {
		// The real failure stays in the chain for the caller to inspect; the
		// client-visible text does not, because it names the adapter's own
		// scratch directory.
		return nil, nil, s.mapTurnFailure(fmt.Errorf("%w: %w", &codex.TurnFailedError{
			Cause:   codex.CauseTransport,
			Message: promptImageScratchFailure,
		}, err))
	}

	input, err := promptToCodex(blocks, prepared.images)
	if err != nil {
		prepared.release()

		return nil, nil, err
	}

	return input, prepared.release, nil
}

func (s *session) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	negotiated := s.agent.negotiatedLifecycle()
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()

	if closing {
		return acp.PromptResponse{}, newSessionCloseInProgress()
	}

	if err := s.awaitLifecycleEstablishment(ctx); err != nil {
		return acp.PromptResponse{}, err
	}

	if err := s.attachNativeEvents(); err != nil {
		return acp.PromptResponse{}, err
	}

	if !negotiated.Present() {
		defer func() {
			if err := s.prepareNativeEventRebind(); err != nil {
				s.fenceSession()
			}
		}()
	}

	// Route validation runs first, so a prompt never reports two rejections and
	// the order of two failures is never implementation-defined.
	route, err := parseInboundRoute(params.Meta)
	if err != nil {
		return acp.PromptResponse{}, routeInvalidParams(err)
	}

	submission, refusal := lifecycle.DecodePromptCorrelation(params.Meta, negotiated)
	if refusal != nil {
		return acp.PromptResponse{}, lifecycleInvalidParams(refusal)
	}

	// A prompt that never took the session's single turn slot opened no turn, so
	// it has no terminal to report: the refusal is the answer. Backpressure is
	// the fixed `session_prompt` invalid request, and a caller context that was
	// already done is that context's own error. Answering either with a
	// successful `cancelled` response would invent a terminal for a turn that
	// never existed and hide the one signal a host can act on.
	releaseTurn, err := s.acquireTurn(ctx)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	defer releaseTurn()

	// One prompt's whole settlement order runs under this gate, so close and
	// delete wait for the durable commit and the terminal lifecycle event rather
	// than for the native loop alone.
	s.settleGate.Lock()
	defer s.settleGate.Unlock()

	turnCtx, err := s.beginPromptTurn(ctx, route.TurnNonce)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	defer s.finishTurn()

	if err = s.ensureMirrorSynced(turnCtx); err != nil { //nolint:gocritic // Reuses the function's error variable.
		return acp.PromptResponse{}, err
	}

	input, releaseImages, err := s.preparePromptInput(turnCtx, params.Prompt)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	defer releaseImages()

	incarnation, err := s.openIncarnation(turnCtx, negotiated)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	defer s.clearIncarnation(incarnation)

	result := s.runPromptTurn(ctx, turnCtx, promptRun{
		incarnation: incarnation,
		submission:  submission,
		input:       input,
	})

	return s.settlePrompt(ctx, turnCtx, incarnation, result, params.MessageId)
}

// promptRun is what one native turn needs to start: the incarnation that reports
// it, the submission identity it echoes at acceptance, and the native input.
type promptRun struct {
	incarnation *promptIncarnation
	submission  lifecycle.Submission
	input       []codex.UserInput
}

// promptTurnResult is what one native turn produced. Every exit of the loop
// returns one, so settlement happens once and on every path rather than at the
// nine places the loop could have returned from.
type promptTurnResult struct {
	state    *promptEventState
	snapshot sessionSnapshot
	// failure is the native cause this turn ended on, already mapped onto the
	// uniform wire error. A failure is never a stop reason.
	failure error
	// accepted records that the native dispatcher took durable ownership of the
	// frame. A failure before that point creates neither submission nor turn.
	accepted bool
}

// runPromptTurn drives one native turn to its terminal and returns what it
// produced. It never returns a response and never commits: settlement is one
// step, and it is the caller's.
func (s *session) runPromptTurn(ctx context.Context, turnCtx context.Context, run promptRun) promptTurnResult {
	snapshot := s.snapshot()
	model, effort, tier, personality, collaborationMode := s.turnSettings()

	var agentText strings.Builder

	result := promptTurnResult{
		snapshot: snapshot,
		state: &promptEventState{
			snapshot:            snapshot,
			agentDeltaItems:     map[string]struct{}{},
			reasoningDeltaItems: map[string]struct{}{},
			toolContents:        make(map[acp.ToolCallId][]acp.ToolCallContent),
			agentText:           &agentText,
			stopReason:          acp.StopReasonEndTurn,
			imageTools:          newImageToolState(),
		},
	}

	s.markTurnDispatched()

	turn, err := s.client.RunTurn(turnCtx, codex.TurnStartRequest{
		ThreadID:          snapshot.codexThreadID,
		Prompt:            run.input,
		Model:             model,
		ServiceTier:       tier,
		ReasoningEffort:   effort,
		Personality:       personality,
		CollaborationMode: collaborationMode,
		ApprovalPolicy:    snapshot.approvalPolicy,
		SandboxPolicy: firstNonNil(
			sandboxPolicy(snapshot.sandboxPolicy),
			sandboxPolicyForAdditionalDirectories(snapshot.additionalDirectories),
		),
		OutputSchema: snapshot.outputSchema,
	})
	if err == nil && turn.ID == "" {
		err = errors.New("codex accepted a turn without naming it")
	}

	if err != nil {
		nativeID, buffered, nativeIDs, bindErr := run.incarnation.acceptBufferedNative(turnCtx, run.submission)
		if nativeID != "" {
			s.stageTurnID(nativeID)
			s.acceptTurnBinding()

			result.accepted = true

			for index := range buffered {
				event := buffered[index]
				if eventErr := s.handlePromptEvent(turnCtx, event, result.state); eventErr != nil {
					bindErr = errors.Join(bindErr, eventErr)
				}
			}

			bindErr = errors.Join(bindErr, s.shutdownActiveTurn(ctx))
		} else {
			s.stageTurnID(turn.ID)
			s.rejectTurnBinding()

			for _, turnID := range nativeIDs {
				interruptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
				bindErr = errors.Join(bindErr, s.client.CancelTurn(interruptCtx, snapshot.codexThreadID, turnID))

				cancel()
			}

			if len(nativeIDs) > 0 {
				s.failNativeIncarnation(bindErr)
			}
		}

		if errors.Is(err, codex.ErrTurnEventOverflow) && turn.ID != "" {
			bindErr = errors.Join(bindErr, s.shutdownActiveTurn(ctx))
		}

		result.failure = s.mapTurnFailure(errors.Join(err, bindErr))

		return result
	}

	s.stageTurnID(turn.ID)
	s.acceptTurnBinding()

	// The ack is the dispatch linearization point: the app-server has taken
	// durable ownership of the frame and named the turn it opened.
	if err := run.incarnation.acceptNative(turnCtx, run.submission, turn.ID); err != nil {
		result.accepted = true
		containmentErr := s.shutdownActiveTurn(ctx)
		result.failure = s.mapTurnFailure(errors.Join(err, containmentErr))

		return result
	}

	result.accepted = true

	return s.drivePromptTurn(ctx, turnCtx, s.nativePromptEvents(run.incarnation), result)
}

// drivePromptTurn consumes the native stream until its terminal and records how
// the turn ended.
func (s *session) drivePromptTurn(
	ctx context.Context,
	turnCtx context.Context,
	events <-chan codex.Event,
	result promptTurnResult,
) promptTurnResult {
	state := result.state

	var turnDeadline <-chan time.Time

	if timeout := s.agent.options.TurnTimeout; timeout > 0 {
		deadlineTimer := time.NewTimer(timeout)
		defer deadlineTimer.Stop()

		turnDeadline = deadlineTimer.C
	}

	timedOut := false
	overflowed := false
	handled := error(nil)

	eventsOpen := true
	for eventsOpen {
		select {
		case event, ok := <-events:
			if !ok {
				eventsOpen = false

				continue
			}

			overflowed = errors.Is(event.Err, codex.ErrTurnEventOverflow)
			if handled = s.handlePromptEvent(turnCtx, event, state); handled != nil {
				eventsOpen = false
			}
		case <-turnCtx.Done():
			eventsOpen = false
		case <-turnDeadline:
			timedOut = true
			eventsOpen = false
		}
	}

	turnCancelled := turnCtx.Err() != nil || s.wasTurnCancelled()
	if turnCancelled {
		state.stopReason = acp.StopReasonCancelled
		state.nativeFailure = false
		handled = nil
	}

	switch {
	case overflowed && !turnCancelled:
		containmentErr := s.shutdownActiveTurn(ctx)
		result.failure = s.mapTurnFailure(errors.Join(codex.ErrTurnEventOverflow, containmentErr))
	case handled != nil:
		var deliveryFailure *hostDeliveryError

		var nativeFailure *acp.RequestError
		switch {
		case errors.As(handled, &deliveryFailure):
			containmentErr := s.shutdownActiveTurn(ctx)
			result.failure = errors.Join(handled, containmentErr)
		case errors.As(handled, &nativeFailure):
			result.failure = handled
		default:
			containmentErr := s.shutdownActiveTurn(ctx)
			result.failure = s.mapTurnFailure(errors.Join(handled, containmentErr))
		}
	case state.stopReason == acp.StopReasonCancelled:
		// Cancellation wins a coincident deadline. The native interrupt was
		// already issued by the cancel path, so timeout must not send it again.
	case timedOut:
		result.failure = s.failTimedOutTurn(ctx)
	case state.nativeFailure:
		result.failure = s.mapTurnFailure(&codex.TurnFailedError{
			Cause:   codex.CauseProvider,
			Message: "codex reported a turn status this adapter cannot state as a clean stop",
		})
	case !state.completed && state.stopReason != acp.StopReasonCancelled:
		result.failure = s.mapTurnFailure(codex.ErrConnectionClosed)
	}

	return result
}

// failTimedOutTurn aborts the native turn a deadline expired on. A timeout is a
// failure rather than a cancel, so it never reports a stop reason.
func (s *session) failTimedOutTurn(ctx context.Context) error {
	abortErr := s.abortTurnAfterTimeout(ctx)

	message := fmt.Sprintf("codex turn exceeded %s deadline", s.agent.options.TurnTimeout)
	if abortErr != nil {
		message = fmt.Sprintf("%s: %v", message, abortErr)
	}

	return s.mapTurnFailure(&codex.TurnFailedError{Cause: codex.CauseTimeout, Message: message})
}

// settlePrompt is the one settlement point every prompt exit reaches. The order
// is the contract's: the native foreground terminal, then the durable
// foreground-prefix commit, then the terminal idle that boundary earns, then the
// v1 result.
//
// Store work runs on a context detached from the request, because a host that
// cancelled its call is not a reason to leave the frames it was shown
// uncommitted. A failed commit fails the prompt and emits no terminal idle:
// durability outranks the terminal event, so the incarnation ends unsettled and
// the next one opens with a truthful snapshot.
func (s *session) settlePrompt(
	ctx context.Context,
	turnCtx context.Context,
	incarnation *promptIncarnation,
	result promptTurnResult,
	messageID *string,
) (acp.PromptResponse, error) {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), promptSettlementTimeout)
	defer cancel()

	// The terminal identity update is still mandatory delivery. If it fails,
	// contain the exact accepted native turn before durability or idle can move.
	if deliveryErr := s.finalizePromptNativeIdentity(context.WithoutCancel(turnCtx), result.state); deliveryErr != nil {
		containmentErr := s.shutdownActiveTurn(ctx)
		result.failure = errors.Join(deliveryErr, containmentErr, result.failure)
	}

	if err := s.awaitTurnContainment(settleCtx); err != nil {
		return acp.PromptResponse{}, err
	}

	stopReason, outcome := promptSettlement(result)

	if err := s.commitForegroundPrefix(settleCtx); err != nil {
		s.poisonForegroundSettlement(incarnation, err)

		return acp.PromptResponse{}, settlementFailure(result.failure, err)
	}

	if err := incarnation.settle(settleCtx, stopReason, outcome); err != nil {
		return acp.PromptResponse{}, settlementFailure(result.failure, err)
	}

	if result.failure != nil {
		return acp.PromptResponse{}, result.failure
	}

	return acp.PromptResponse{
		StopReason:    stopReason,
		Usage:         result.state.usage,
		UserMessageId: messageID,
		Meta: mergePromptResponseMeta(
			structuredOutputMeta(result.state.agentText.String(), result.snapshot.outputSchema),
			result.state.nativeIdentity,
		),
	}, nil
}

func (s *session) poisonForegroundSettlement(incarnation *promptIncarnation, cause error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.incarnation != incarnation || incarnation == nil || incarnation.settled {
		return
	}

	_ = s.latchLifecycleFailureLocked(errors.Join(errors.New("codex foreground settlement has a pending durable commit"), cause))
}

// settlementFailure keeps a failed native turn's own cause on the wire when the
// settlement behind it fails too. The turn-failure envelope names the cause the
// host has to classify and decide a retry against, and an adapter's own
// settlement fault renames nothing about the turn that already failed; reporting
// the settlement instead would tell a host its store broke when what broke was
// the provider. The settlement failure is not lost by staying off the wire — it
// keeps the effect it always had, which is the terminal idle this exit no longer
// emits, and a capture the store refused stays latched for the close boundary to
// place.
func settlementFailure(nativeFailure error, settlementErr error) error {
	if nativeFailure != nil {
		return nativeFailure
	}

	return settlementErr
}

// commitForegroundPrefix places the largest prefix of this turn's native state
// the store can hold. A failed or cancelled turn commits exactly what it
// streamed, so the commit runs on every exit rather than on success alone.
func (s *session) commitForegroundPrefix(ctx context.Context) error {
	err := s.mirrorAndEmitRollout(ctx)
	if err == nil {
		return nil
	}

	var imageErr *imageOutputError
	if errors.As(err, &imageErr) {
		return s.mapTurnFailure(imageErr)
	}

	return err
}

// promptSettlement derives what the turn itself produced, before the v1 layer
// masks anything. A failure records outcome `failed` and states no stop reason,
// because no ACP v1 stop reason names a failure and the v1 error carries it.
func promptSettlement(result promptTurnResult) (acp.StopReason, lifecycle.Outcome) {
	if result.failure != nil {
		return "", lifecycle.OutcomeFailed
	}

	switch result.state.stopReason {
	case acp.StopReasonCancelled:
		return acp.StopReasonCancelled, lifecycle.OutcomeCancelled
	case acp.StopReasonRefusal:
		return acp.StopReasonRefusal, lifecycle.OutcomeRefused
	case acp.StopReasonMaxTokens, acp.StopReasonMaxTurnRequests:
		return result.state.stopReason, lifecycle.OutcomeLimit
	default:
		return acp.StopReasonEndTurn, lifecycle.OutcomeSuccess
	}
}

type promptEventState struct {
	snapshot            sessionSnapshot
	agentDeltaItems     map[string]struct{}
	reasoningDeltaItems map[string]struct{}
	agentText           *strings.Builder

	streamedUsage              codex.Usage
	streamedThreadUsage        codex.Usage
	streamedUsageContextWindow int64

	usage      *acp.Usage
	stopReason acp.StopReason
	completed  bool
	// nativeFailure records a native completion this adapter cannot state as a
	// clean stop. A failure is not a stop reason, so it is carried separately
	// rather than folded into one.
	nativeFailure bool

	nativeIdentity        nativeTurnIdentity
	emittedNativeIdentity nativeTurnIdentity
	imageTools            imageToolState
	toolContents          map[acp.ToolCallId][]acp.ToolCallContent
}

func (s *session) handlePromptEvent(turnCtx context.Context, event codex.Event, state *promptEventState) error {
	if event.TurnID != "" {
		s.setTurnID(event.TurnID)
	}

	if event.Kind == codex.EventAccountUpdated {
		s.setAccount(redactedAccountMeta(event.Account))
	}

	state.nativeIdentity = nativeIdentityFromEvent(event, state.nativeIdentity)

	if err := s.emitRawCodexEvent(turnCtx, event); err != nil {
		s.recordRawEmitFailure(turnCtx, err)

		return err
	}

	visibleEvent := dedupeCompletedTextEvent(event, state.agentDeltaItems, state.reasoningDeltaItems)
	if visibleEvent.Kind == codex.EventAgentMessageDelta {
		visibleEvent = dedupeCompletedAggregateTextEvent(visibleEvent, state.agentText.String())
	}

	if err := s.emitPromptUpdates(turnCtx, event, visibleEvent, state); err != nil {
		return err
	}

	s.applyPromptUsage(event, state)

	if visibleEvent.Kind == codex.EventAgentMessageDelta && visibleEvent.Text != "" {
		state.agentText.WriteString(visibleEvent.Text)
	}

	if failErr := turnFailureFromEvent(event); failErr != nil && !s.wasTurnCancelled() {
		return s.mapTurnFailure(failErr)
	}

	return nil
}

// abortTurnAfterTimeout interrupts the in-flight native Codex turn, then
// contains only background terminals owned by this thread. Peer sessions keep
// using the shared app-server generation.
func (s *session) abortTurnAfterTimeout(ctx context.Context) error {
	client, threadID, turnID := s.activeTurnTarget()

	interruptCtx, cancelInterrupt := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	interruptErr := client.CancelTurn(interruptCtx, threadID, turnID)

	cancelInterrupt()

	return s.containCancelledTurn(ctx, client, threadID, interruptErr)
}

func (s *session) recordRawEmitFailure(ctx context.Context, err error) {
	if err == nil {
		return
	}

	s.mu.Lock()
	s.rawEmitFailures++
	s.mu.Unlock()

	if logger := agentLogger(s.agent); logger != nil {
		logger.WarnContext(ctx, "codex raw event delivery failed")
	}
}

func (s *session) emitPromptUpdates(turnCtx context.Context, event codex.Event, visibleEvent codex.Event, state *promptEventState) (err error) {
	if event.Kind == codex.EventImageStarted || event.Kind == codex.EventImageCompleted {
		updates, imageErr := s.imageEventUpdates(turnCtx, event, &state.imageTools)
		if imageErr != nil {
			if emitErr := s.emitUpdates(turnCtx, updates...); emitErr != nil {
				return emitErr
			}

			return s.mapTurnFailure(imageErr)
		}

		return s.emitUpdates(turnCtx, updates...)
	}

	toolPublication, err := s.preparePermissionToolEvent(turnCtx, visibleEvent)
	if err != nil {
		return err
	}
	defer func() { toolPublication.finish(s, err) }()

	updates := toolPublication.updates(state.toolContents)
	updates = append(updates, usageUpdatesForEvent(event, &state.streamedUsage, &state.streamedThreadUsage, &state.streamedUsageContextWindow)...)

	checkpointIdentity := nativeTurnIdentity{turnID: state.nativeIdentity.turnID}
	if nativeIdentityChanged(checkpointIdentity, state.emittedNativeIdentity) {
		if len(updates) == 0 {
			updates = []acp.SessionUpdate{{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{}}}
		}

		emitErr := s.emitUpdatesWithNativeIdentity(turnCtx, checkpointIdentity, updates...)
		if emitErr != nil {
			return emitErr
		}

		state.emittedNativeIdentity = checkpointIdentity

		return nil
	}

	err = s.emitUpdates(turnCtx, updates...)

	return err
}

func (s *session) applyPromptUsage(event codex.Event, state *promptEventState) {
	if event.Kind == codex.EventUsageUpdated {
		if tokenUsage := usageFromCodex(event.TokenUsage.Last); tokenUsage != nil {
			state.usage = tokenUsage
		}
	}

	if event.Kind == codex.EventCompleted {
		state.completed = true

		if stop, clean := promptStopReason(event.StopReason); clean {
			state.stopReason = stop
		} else {
			state.nativeFailure = true
		}

		if completedUsage := usageFromCodex(event.Usage); completedUsage != nil {
			state.usage = completedUsage
		}
	}
}

func dedupeCompletedTextEvent(event codex.Event, agentDeltaItems map[string]struct{}, reasoningDeltaItems map[string]struct{}) codex.Event {
	switch event.Kind {
	case codex.EventAgentMessageDelta:
		return dedupeCompletedTextKind(event, agentDeltaItems)
	case codex.EventReasoningDelta:
		return dedupeCompletedTextKind(event, reasoningDeltaItems)
	default:
		return event
	}
}

func dedupeCompletedTextKind(event codex.Event, deltaItems map[string]struct{}) codex.Event {
	if event.ItemID == "" {
		return event
	}

	if event.Completed {
		if _, ok := deltaItems[event.ItemID]; ok {
			event.Text = ""
		}

		return event
	}

	if event.Text != "" {
		deltaItems[event.ItemID] = struct{}{}
	}

	return event
}

func dedupeCompletedAggregateTextEvent(event codex.Event, priorText string) codex.Event {
	if !event.Completed || event.ItemID != "" || event.Text == "" {
		return event
	}

	if event.Text == priorText {
		event.Text = ""
	}

	return event
}

func (s *session) turnSettings() (model string, effort string, serviceTier string, personality any, collaborationMode any) {
	s.mu.Lock()
	model = s.model
	effort = s.reasoningEffort
	serviceTier = s.serviceTier
	personalityValue := s.personality
	mode := s.mode
	s.mu.Unlock()

	if personalityValue != "" {
		personality = personalityValue
	}

	if mode != modeDefault {
		collaborationMode = map[string]any{
			jsonFieldMode: string(mode),
			"settings": map[string]any{
				metaModelKey:             firstNonEmpty(model, valueDefault),
				"developer_instructions": nil,
				"reasoning_effort":       nullableString(effort),
			},
		}
	}

	return model, effort, serviceTier, personality, collaborationMode
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}

	return value
}

func (s *session) emitUpdates(ctx context.Context, updates ...acp.SessionUpdate) error {
	if len(updates) > 0 {
		s.agent.observe.ObserveFirstPromptUpdate(ctx)
	}

	for _, update := range updates {
		if err := s.agent.emitUpdate(ctx, s.id, update); err != nil {
			return wrapHostDeliveryError(err)
		}
	}

	return nil
}

func (s *session) emitRawCodexEvent(ctx context.Context, event codex.Event) error {
	if !s.rawMessages.Enabled() || len(event.RawParams) == 0 {
		return nil
	}

	raw := decodedRawEvent(event.RawParams)
	if !s.rawMessages.ShouldEmit(raw) {
		return nil
	}

	raw[jsonFieldMethod] = event.RawMethod

	conn := s.agent.connection()
	if conn == nil {
		return nil
	}

	s.rawEventMu.Lock()
	defer s.rawEventMu.Unlock()

	sequence := s.rawEventSequence + 1

	payload := map[string]any{
		jsonFieldSessionID: s.id,
		jsonFieldSequence:  sequence,
		jsonFieldSource:    rawEventSource,
		jsonFieldEvent:     raw,
	}
	if meta := turnRouteMetaFromContext(ctx); meta != nil {
		payload["_meta"] = meta
	}

	capped, err := capRawEventPayload(payload)
	if err != nil {
		return err
	}

	if err := conn.NotifyExtension(ctx, RawEventMethod, capped); err != nil {
		return err
	}

	s.rawEventSequence = sequence

	return nil
}

func eventUpdates(event codex.Event) []acp.SessionUpdate {
	return eventUpdatesWithToolSnapshots(event, make(map[acp.ToolCallId][]acp.ToolCallContent))
}

func eventUpdatesWithToolSnapshots(
	event codex.Event,
	snapshots map[acp.ToolCallId][]acp.ToolCallContent,
) []acp.SessionUpdate {
	switch event.Kind {
	case codex.EventAgentMessageDelta:
		if event.Text == "" {
			return nil
		}

		return []acp.SessionUpdate{acp.UpdateAgentMessageText(event.Text)}
	case codex.EventReasoningDelta:
		if event.Text == "" {
			return nil
		}

		return []acp.SessionUpdate{acp.UpdateAgentThoughtText(event.Text)}
	case codex.EventPlanUpdated:
		return planUpdate(event.Plan)
	case codex.EventToolStarted:
		return []acp.SessionUpdate{startToolUpdate(event.Tool)}
	case codex.EventToolDelta:
		return toolDeltaUpdate(event.Tool, event.Text, snapshots)
	case codex.EventToolCompleted:
		return []acp.SessionUpdate{completeToolUpdate(event.Tool, snapshots)}
	case codex.EventDiffUpdated:
		if event.Diff == "" {
			return nil
		}

		return []acp.SessionUpdate{acp.UpdateToolCall(acp.ToolCallId("codex-diff"), acp.WithUpdateContent([]acp.ToolCallContent{diffContent("", event.Diff)}))}
	case codex.EventWarning:
		if event.Text == "" {
			return nil
		}

		return []acp.SessionUpdate{acp.UpdateAgentThoughtText(event.Text)}
	default:
		return nil
	}
}

func usageUpdatesForEvent(
	event codex.Event,
	streamedUsage *codex.Usage,
	streamedThreadUsage *codex.Usage,
	streamedUsageContextWindow *int64,
) []acp.SessionUpdate {
	switch event.Kind {
	case codex.EventUsageUpdated:
		if event.TokenUsage.Last == *streamedUsage &&
			event.TokenUsage.Total == *streamedThreadUsage &&
			event.TokenUsage.ModelContextWindow == *streamedUsageContextWindow {
			return nil
		}

		updates := tokenUsageUpdateFromCodex(event.TokenUsage)
		if len(updates) > 0 {
			*streamedUsage = event.TokenUsage.Last
			*streamedThreadUsage = event.TokenUsage.Total
			*streamedUsageContextWindow = event.TokenUsage.ModelContextWindow
		}

		return updates
	case codex.EventCompleted:
		if event.Usage == *streamedUsage {
			return nil
		}

		updates := usageUpdateFromCodex(event.Usage)
		if len(updates) > 0 {
			*streamedUsage = event.Usage
			*streamedThreadUsage = codex.Usage{}
			*streamedUsageContextWindow = 0
		}

		return updates
	default:
		return nil
	}
}

func planUpdate(steps []codex.PlanStep) []acp.SessionUpdate {
	if len(steps) == 0 {
		return nil
	}

	entries := make([]acp.PlanEntry, 0, len(steps))
	for _, step := range steps {
		entries = append(entries, acp.PlanEntry{
			Content:  step.Text,
			Priority: acp.PlanEntryPriorityMedium,
			Status:   planStatus(step.Status),
		})
	}

	return []acp.SessionUpdate{acp.UpdatePlan(entries...)}
}

func startToolUpdate(tool codex.ToolEvent) acp.SessionUpdate {
	id := acp.ToolCallId(firstNonEmpty(tool.ID, "codex-tool"))
	title := firstNonEmpty(tool.Title, tool.Kind, string(id))

	return acp.StartToolCall(
		id,
		title,
		acp.WithStartKind(toolKind(tool)),
		acp.WithStartStatus(acp.ToolCallStatusInProgress),
		acp.WithStartRawInput(tool.Raw),
	)
}

func toolDeltaUpdate(
	tool codex.ToolEvent,
	text string,
	snapshotArgs ...map[acp.ToolCallId][]acp.ToolCallContent,
) []acp.SessionUpdate {
	if text == "" {
		text = tool.Content
	}

	if text == "" {
		return nil
	}

	id := acp.ToolCallId(firstNonEmpty(tool.ID, "codex-tool"))
	snapshots := toolSnapshotMap(snapshotArgs)
	snapshots[id] = append(snapshots[id], textToolContent(text))

	return []acp.SessionUpdate{acp.UpdateToolCall(id, acp.WithUpdateContent(append([]acp.ToolCallContent(nil), snapshots[id]...)))}
}

func completeToolUpdate(
	tool codex.ToolEvent,
	snapshotArgs ...map[acp.ToolCallId][]acp.ToolCallContent,
) acp.SessionUpdate {
	id := acp.ToolCallId(firstNonEmpty(tool.ID, "codex-tool"))
	snapshots := toolSnapshotMap(snapshotArgs)

	opts := []acp.ToolCallUpdateOpt{
		acp.WithUpdateStatus(acp.ToolCallStatusCompleted),
		acp.WithUpdateKind(toolKind(tool)),
		acp.WithUpdateRawOutput(tool.Raw),
	}
	if tool.Title != "" {
		opts = append(opts, acp.WithUpdateTitle(tool.Title))
	}

	if tool.Content != "" {
		snapshots[id] = append(snapshots[id], textToolContent(tool.Content))
		opts = append(opts, acp.WithUpdateContent(append([]acp.ToolCallContent(nil), snapshots[id]...)))
	}

	return acp.UpdateToolCall(id, opts...)
}

func toolSnapshotMap(
	snapshotArgs []map[acp.ToolCallId][]acp.ToolCallContent,
) map[acp.ToolCallId][]acp.ToolCallContent {
	if len(snapshotArgs) > 0 && snapshotArgs[0] != nil {
		return snapshotArgs[0]
	}

	return make(map[acp.ToolCallId][]acp.ToolCallContent)
}

func textToolContent(text string) acp.ToolCallContent {
	return acp.ToolCallContent{
		Content: &acp.ToolCallContentContent{
			Content: acp.TextBlock(text),
		},
	}
}

func diffContent(path string, diff string) acp.ToolCallContent {
	return acp.ToolCallContent{
		Diff: &acp.ToolCallContentDiff{
			Path:    path,
			NewText: diff,
		},
	}
}

func toolKind(tool codex.ToolEvent) acp.ToolKind {
	switch tool.Kind {
	case toolKindCommandExecution, valueCommand, "exec", toolKindShell:
		return acp.ToolKindExecute
	case toolKindFileChange, "edit", toolKindPatch:
		return acp.ToolKindEdit
	case toolKindMcpToolCall, toolKindDynamicToolCall:
		return acp.ToolKindOther
	default:
		return acp.ToolKindOther
	}
}

func planStatus(status codex.PlanStepStatus) acp.PlanEntryStatus {
	switch status {
	case codex.PlanStepInProgress:
		return acp.PlanEntryStatusInProgress
	case codex.PlanStepCompleted:
		return acp.PlanEntryStatusCompleted
	default:
		return acp.PlanEntryStatusPending
	}
}

// promptStopReason maps one native completion onto the ACP v1 stop reason it
// actually reports. A native failure is not a stop reason at all — the v1 error
// carries it — so the second result is false and the turn fails rather than
// reporting a clean end it did not reach.
func promptStopReason(reason codex.StopReason) (acp.StopReason, bool) {
	switch reason {
	case codex.StopReasonCancelled:
		return acp.StopReasonCancelled, true
	case codex.StopReasonError:
		return "", false
	default:
		return acp.StopReasonEndTurn, true
	}
}

func sandboxPolicyForAdditionalDirectories(dirs []string) any {
	if len(dirs) == 0 {
		return nil
	}

	return workspaceWriteSandboxPolicy(dirs)
}

func sandboxMode(policy any) any {
	switch p := policy.(type) {
	case string:
		return p
	case map[string]any:
		switch p[jsonFieldType] {
		case sandboxTypeDangerFullAccess, sandboxModeDangerFullAccess, "fullAccess":
			return sandboxModeDangerFullAccess
		case sandboxTypeReadOnly, sandboxModeReadOnly:
			return sandboxModeReadOnly
		case sandboxTypeWorkspaceWrite, sandboxModeWorkspaceWrite:
			return sandboxModeWorkspaceWrite
		}
	}

	return nil
}

func sandboxPolicy(policy any) any {
	switch p := policy.(type) {
	case string:
		switch p {
		case sandboxModeDangerFullAccess:
			return map[string]any{jsonFieldType: sandboxTypeDangerFullAccess}
		case sandboxModeReadOnly:
			return map[string]any{
				jsonFieldType:          sandboxTypeReadOnly,
				jsonFieldNetworkAccess: false,
			}
		case sandboxModeWorkspaceWrite:
			return workspaceWriteSandboxPolicy(nil)
		default:
			return p
		}
	case map[string]any:
		cloned, _ := cloneAny(p).(map[string]any)
		if cloned == nil {
			return p
		}

		if cloned[jsonFieldType] == sandboxTypeWorkspaceWrite || cloned[jsonFieldType] == sandboxModeWorkspaceWrite {
			cloned[jsonFieldType] = sandboxTypeWorkspaceWrite
			if _, ok := cloned["writableRoots"]; !ok {
				cloned["writableRoots"] = []string{}
			}

			if _, ok := cloned[jsonFieldNetworkAccess]; !ok {
				cloned[jsonFieldNetworkAccess] = false
			}

			if _, ok := cloned["excludeTmpdirEnvVar"]; !ok {
				cloned["excludeTmpdirEnvVar"] = false
			}

			if _, ok := cloned["excludeSlashTmp"]; !ok {
				cloned["excludeSlashTmp"] = false
			}
		}

		return cloned
	default:
		return p
	}
}

func workspaceWriteSandboxPolicy(dirs []string) map[string]any {
	return map[string]any{
		jsonFieldType:          sandboxTypeWorkspaceWrite,
		"writableRoots":        append([]string{}, dirs...),
		jsonFieldNetworkAccess: false,
		"excludeTmpdirEnvVar":  false,
		"excludeSlashTmp":      false,
	}
}

func usageFromCodex(usage codex.Usage) *acp.Usage {
	if usage.CachedReadTokens == 0 &&
		usage.CachedWriteTokens == 0 &&
		usage.InputTokens == 0 &&
		usage.OutputTokens == 0 &&
		usage.ReasoningOutputTokens == 0 &&
		usage.TotalTokens == 0 {
		return nil
	}

	result := &acp.Usage{
		InputTokens:  int(usage.InputTokens),
		OutputTokens: int(usage.OutputTokens),
		TotalTokens:  int(usage.TotalTokens),
	}
	if result.TotalTokens == 0 {
		result.TotalTokens = result.InputTokens + result.OutputTokens
	}

	if usage.CachedReadTokens > 0 {
		result.CachedReadTokens = acp.Ptr(int(usage.CachedReadTokens))
	}

	if usage.CachedWriteTokens > 0 {
		result.CachedWriteTokens = acp.Ptr(int(usage.CachedWriteTokens))
	}

	if usage.ReasoningOutputTokens > 0 {
		result.ThoughtTokens = acp.Ptr(int(usage.ReasoningOutputTokens))
	}

	return result
}

func usageUpdateFromCodex(usage codex.Usage) []acp.SessionUpdate {
	return usageUpdateFromCodexContext(usage, codex.Usage{}, 0)
}

func tokenUsageUpdateFromCodex(usage codex.TokenUsage) []acp.SessionUpdate {
	return usageUpdateFromCodexContext(usage.Last, usage.Total, usage.ModelContextWindow)
}

func usageUpdateFromCodexContext(usage codex.Usage, threadUsage codex.Usage, contextWindow int64) []acp.SessionUpdate {
	acpUsage := usageFromCodex(usage)
	if acpUsage == nil {
		return nil
	}

	used := acpUsage.TotalTokens
	meta := map[string]any{
		codexUsageMetaKey: usageMetaFromCodex(acpUsage),
	}

	if threadACPUsage := usageFromCodex(threadUsage); threadACPUsage != nil {
		used = threadACPUsage.TotalTokens
		meta[codexThreadUsageMetaKey] = usageMetaFromCodex(threadACPUsage)
	}

	return []acp.SessionUpdate{{
		UsageUpdate: &acp.SessionUsageUpdate{
			Used: used,
			Size: int(contextWindow),
			Meta: map[string]any{
				codexMetaKey: meta,
			},
		},
	}}
}

func usageMetaFromCodex(usage *acp.Usage) map[string]any {
	meta := map[string]any{
		usageInputTokensKey:       usage.InputTokens,
		usageCachedReadTokensKey:  0,
		usageCachedWriteTokensKey: 0,
		usageOutputTokensKey:      usage.OutputTokens,
		usageReasoningOutputKey:   0,
		usageTotalTokensKey:       usage.TotalTokens,
	}
	if usage.CachedReadTokens != nil {
		meta[usageCachedReadTokensKey] = *usage.CachedReadTokens
	}

	if usage.CachedWriteTokens != nil {
		meta[usageCachedWriteTokensKey] = *usage.CachedWriteTokens
	}

	if usage.ThoughtTokens != nil {
		meta[usageReasoningOutputKey] = *usage.ThoughtTokens
	}

	return meta
}

func promptResultForObserver(resp acp.PromptResponse, err error, model string) observer.PromptResult {
	result := observer.PromptResult{
		Err:        err,
		Model:      model,
		StopReason: string(resp.StopReason),
	}
	if resp.Usage == nil {
		return result
	}

	result.InputTokens = resp.Usage.InputTokens
	result.OutputTokens = resp.Usage.OutputTokens

	result.TotalTokens = resp.Usage.TotalTokens
	if resp.Usage.CachedReadTokens != nil {
		result.CachedReadTokens = *resp.Usage.CachedReadTokens
	}

	if resp.Usage.CachedWriteTokens != nil {
		result.CachedWriteTokens = *resp.Usage.CachedWriteTokens
	}

	if resp.Usage.ThoughtTokens != nil {
		result.ThoughtTokens = *resp.Usage.ThoughtTokens
	}

	return result
}

func structuredOutputMeta(text string, schema any) map[string]any {
	if schema == nil || strings.TrimSpace(text) == "" {
		return nil
	}

	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return nil
	}

	return map[string]any{codexMetaKey: map[string]any{structuredOutputMetaKey: value}}
}
