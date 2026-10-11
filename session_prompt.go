package codexacp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/image"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/sessionlog"
	"github.com/savid/acp-go-core/wire"
)

const limitSessionPrompt = "session_prompt"

// nativePrompt is one mapped prompt.
type nativePrompt struct {
	input []codex.UserInput
	// firstImage is the gated-media index and field of the first image, for
	// the selected-model refusal.
	firstImage *image.Decoded
}

// mapPrompt converts ACP prompt content to the app-server's input shape.
// Images run the core input gates and travel as data URLs.
func (s *session) mapPrompt(ctx context.Context, blocks []acp.ContentBlock) (nativePrompt, error) {
	if len(blocks) == 0 {
		return nativePrompt{}, wire.Unsupported("prompt")
	}

	decoded, refusal, err := image.ValidatePrompt(ctx, blocks, image.Options{
		Limits:      s.agent.options.ImageLimits.core(),
		HandoffRoot: s.agent.options.InputHandoffRoot,
		Blobs:       func(string) image.BlobDisposition { return image.BlobRefuse },
	})
	if err != nil {
		return nativePrompt{}, err
	}

	if refusal != nil {
		return nativePrompt{}, refusal.InvalidParams()
	}

	images := make([]codex.PromptImage, 0, len(decoded))

	var firstImage *image.Decoded

	for index := range decoded {
		if firstImage == nil {
			firstImage = &decoded[index]
		}

		images = append(images, codex.PromptImage{Data: decoded[index].Data, MIME: decoded[index].MIME})
	}

	input, err := codex.PromptToUserInput(blocks, images)
	if err != nil {
		return nativePrompt{}, wire.Unsupported("prompt")
	}

	if len(input) == 0 {
		return nativePrompt{}, wire.Unsupported("prompt")
	}

	return nativePrompt{input: input, firstImage: firstImage}, nil
}

// rejectImagesForModel refuses image input pre-turn when the selected model's
// catalog entry says it accepts no images. An absent model, field, or empty
// list forwards.
func (s *session) rejectImagesForModel(runtime *runtime, prompt nativePrompt) error {
	if prompt.firstImage == nil {
		return nil
	}

	model := s.currentModel()

	for index := range runtime.models {
		candidate := &runtime.models[index]
		if candidate.ID != model || len(candidate.InputModalities) == 0 {
			continue
		}

		for _, modality := range candidate.InputModalities {
			if strings.EqualFold(strings.TrimSpace(modality), "image") {
				return nil
			}
		}

		return image.UnsupportedByModel(prompt.firstImage.Field, prompt.firstImage.Index).InvalidParams()
	}

	return nil
}

// turnStart renders the turn/start call for the session's current settings.
func (s *session) turnStart(input []codex.UserInput) codex.TurnStartRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	req := codex.TurnStartRequest{
		ThreadID:        s.nativeID,
		Input:           input,
		Model:           s.model,
		ServiceTier:     s.serviceTier,
		ReasoningEffort: s.effort,
		Personality:     s.personality,
		ApprovalPolicy:  s.options.ApprovalPolicy,
		SandboxPolicy:   s.turnSandboxPolicy(),
	}

	if s.options.OutputSchema != nil {
		req.OutputSchema = wire.CloneMap(s.options.OutputSchema)
	}

	if s.mode != modeDefault {
		var effort any
		if s.effort != "" {
			effort = s.effort
		}

		req.CollaborationMode = map[string]any{
			"mode": s.mode,
			"settings": map[string]any{
				metaModelKey: cmp.Or(s.model, modeDefault), "developer_instructions": nil, "reasoning_effort": effort,
			},
		}
	}

	return req
}

// prompt sends one turn to the thread and streams updates until it settles.
func (s *session) prompt(ctx context.Context, params acp.PromptRequest, raw json.RawMessage) (acp.PromptResponse, error) {
	meta := lifecycle.RetainRequestMetadata(params.Meta, raw)

	submission, paramErr := lifecycle.DecodePromptCorrelation(meta, s.lifecycleNegotiated())
	if paramErr != nil {
		return acp.PromptResponse{}, wire.ParamRefusal(paramErr)
	}

	if err := s.admissionError(); err != nil {
		return acp.PromptResponse{}, err
	}

	release, err := s.acquireGate(limitSessionPrompt)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	defer release()

	// The turn is driven by a session-owned scope, not by this request's
	// context: the SDK cancels the previous prompt's context when a second
	// one arrives for the same session, and a refused peer prompt must not
	// end the turn this session still owns.
	turnCtx, cancelTurn := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelTurn()

	s.mu.Lock()
	s.promptCancel = cancelTurn
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.promptCancel = nil
		s.mu.Unlock()
	}()

	mapped, err := s.mapPrompt(turnCtx, params.Prompt)
	if err != nil {
		if turnCtx.Err() != nil {
			return wire.CancelledResponse(params), nil
		}

		return acp.PromptResponse{}, err
	}

	// A cancel that lands before native dispatch creates neither submission
	// nor turn and answers cancelled.
	if turnCtx.Err() != nil {
		return wire.CancelledResponse(params), nil
	}

	runtime, err := s.ensureBound(turnCtx)
	if err != nil {
		if turnCtx.Err() != nil {
			return wire.CancelledResponse(params), nil
		}

		return acp.PromptResponse{}, err
	}

	if refusal := s.rejectImagesForModel(runtime, mapped); refusal != nil {
		return acp.PromptResponse{}, refusal
	}

	t := &turn{
		Origin:     lifecycle.CauseSubmission,
		state:      cycleState{tools: make(map[string]*toolState)},
		submission: submission,
		ctx:        turnCtx,
		settled:    make(chan struct{}),
		finished:   make(chan struct{}),
	}

	s.mu.Lock()
	s.turn = t
	s.mu.Unlock()

	defer close(t.finished)

	defer func() {
		s.mu.Lock()
		if s.turn == t {
			s.turn = nil
		}

		if t.nativeTurnID != "" {
			s.lastTerminalTurn = t.nativeTurnID
		}

		lost := s.generationLost
		s.mu.Unlock()

		// The incarnation that ran the turn is gone: fence it here, after the
		// turn's own terminal event, so the next one publishes on a new
		// stream.
		if lost || t.ended == turnTransportEnded {
			s.fenceStream()
		}
	}()

	nativeTurnID, err := runtime.client.StartTurn(turnCtx, s.turnStart(mapped.input))
	if err != nil {
		if !s.turnAccepted(t) {
			if turnCtx.Err() != nil {
				return wire.CancelledResponse(params), nil
			}

			return acp.PromptResponse{}, s.dispatchFailure(context.WithoutCancel(ctx), runtime, err)
		}
	}

	s.mu.Lock()
	if t.nativeTurnID == "" {
		t.nativeTurnID = nativeTurnID
	}
	s.mu.Unlock()

	s.acceptTurn(turnCtx, t)

	select {
	case <-t.settled:
	case <-turnCtx.Done():
		s.cancel(context.WithoutCancel(ctx))

		select {
		case <-t.settled:
		case <-time.After(sessionAbortTimeout):
			// The native turn ignored the interrupt: contain this session so
			// nothing still writes the cycle state the response is judged
			// from, and leave the shared app-server to its peers.
			runtime.stopQueue(s)
			s.contain(context.WithoutCancel(ctx), runtime, wire.TurnFailed(vendor, wire.TurnFailure{Cause: wire.CauseProvider, Message: "codex did not end the turn after its interrupt"}))
			t.settle(turnContained)
		}
	}

	return s.settleTurn(ctx, runtime, t, params)
}

// dispatchFailure classifies a turn the app-server never accepted: a native
// rejection carries its message as a provider failure, a dead process is a
// process exit, and everything else is transport.
func (s *session) dispatchFailure(ctx context.Context, runtime *runtime, err error) error {
	if rpcErr, ok := errors.AsType[*codex.RPCError](err); ok {
		return wire.TurnFailed(vendor, wire.TurnFailure{Cause: wire.CauseProvider, Message: rpcErr.Message})
	}

	return s.agent.transportFailure(ctx, runtime, err)
}

// cycleVerdict is how one cycle ended, in the terms the lifecycle stream and
// the prompt response need.
type cycleVerdict struct {
	outcome    lifecycle.Outcome
	stopReason string
	failure    error
}

// judgeCycle records how a natively settled cycle finished. The cancel guard
// runs before every failure mapping.
func (s *session) judgeCycle(c *cycle, cancelled bool) cycleVerdict {
	observed := s.cycleFailure(c)

	switch {
	case cancelled:
		return cycleVerdict{outcome: lifecycle.OutcomeCancelled, stopReason: lifecycle.StopReasonCancelled}
	case observed != nil:
		return cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: observed}
	case c.state.stop == codex.StopReasonError:
		failure := wire.TurnFailure{Cause: wire.CauseProvider, Message: "codex reported a turn error"}
		if c.state.failure != nil {
			failure.Message = c.state.failure.Message
			failure.StatusCode = c.state.failure.StatusCode
			failure.ProviderCode = c.state.failure.ProviderCode
		}

		return cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: wire.TurnFailed(vendor, failure)}
	case c.state.stop == codex.StopReasonCancelled:
		return cycleVerdict{outcome: lifecycle.OutcomeCancelled, stopReason: lifecycle.StopReasonCancelled}
	default:
		return cycleVerdict{outcome: lifecycle.OutcomeSuccess, stopReason: lifecycle.StopReasonEndTurn}
	}
}

// settleTurn is the one settlement point every accepted prompt reaches: the
// session info, the durable mirror commit, the terminal idle, and only then
// the response or error.
func (s *session) settleTurn(ctx context.Context, runtime *runtime, t *turn, params acp.PromptRequest) (acp.PromptResponse, error) {
	s.beginSettlement(&t.cycle)

	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionSettleTimeout)
	defer cancel()

	s.mu.Lock()
	cancelled := t.cancelled
	s.mu.Unlock()

	var verdict cycleVerdict

	commitFailed := false

	switch {
	case cancelled:
		verdict = cycleVerdict{outcome: lifecycle.OutcomeCancelled, stopReason: lifecycle.StopReasonCancelled}
	case t.ended == turnContained:
		verdict = cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: s.cycleFailure(&t.cycle)}
	case t.ended == turnTransportEnded:
		verdict = cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: s.agent.transportFailure(settleCtx, runtime, nil)}
	default:
		verdict = s.judgeCycle(&t.cycle, false)
	}

	if t.ended == turnSettled && !cancelled {
		s.emitSessionInfo(settleCtx, params.Prompt)

		if refusal := invalidArgumentsRefusal(verdict.failure); refusal != nil {
			s.repairHistory(settleCtx, runtime, refusal)
		}
	}

	if err := s.commitMirror(settleCtx); err != nil {
		s.mu.Lock()
		s.runtime = nil
		s.mu.Unlock()
		s.fenceStream()

		commitFailed = true

		mirrorErr := s.mirrorFailure(&t.state, err)

		if verdict.failure == nil {
			verdict.failure = mirrorErr
		}

		verdict.outcome = lifecycle.OutcomeFailed
	}

	if s.claimCancellation(&t.cycle) && !commitFailed {
		verdict = cycleVerdict{outcome: lifecycle.OutcomeCancelled, stopReason: lifecycle.StopReasonCancelled}
	}

	if err := s.lcIdle(settleCtx, &t.cycle, verdict); err != nil && verdict.failure == nil {
		verdict.failure = err
	}

	if verdict.failure != nil {
		return acp.PromptResponse{}, verdict.failure
	}

	return acp.PromptResponse{
		StopReason:    acp.StopReason(verdict.stopReason),
		Usage:         t.state.usage,
		UserMessageId: params.MessageId,
		Meta:          s.structuredOutputMeta(t.state.agentText.String()),
	}, nil
}

// structuredOutputMeta carries the parsed final answer of a schema-bound
// turn. Output that does not parse omits the key and the turn still
// succeeds.
func (s *session) structuredOutputMeta(text string) map[string]any {
	if s.options.OutputSchema == nil || strings.TrimSpace(text) == "" {
		return nil
	}

	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return nil
	}

	return map[string]any{vendor: map[string]any{metaStructuredOutputKey: value}}
}

// mirrorFailure maps a failed mirror commit onto the turn-failure shape. A
// turn that delivered image bytes lost their durable replay representation.
func (s *session) mirrorFailure(state *cycleState, err error) error {
	s.agent.log.Error("session mirror commit failed", slog.String("session_id", string(s.id)), slog.String("reason", err.Error()))

	failure := wire.TurnFailure{Cause: wire.CauseTransport, Message: "session mirror commit failed"}
	if state.imagesEmitted {
		failure.Message = "image output is no longer available from the artifact store"
		failure.Stage = image.OutputStage
		failure.Reason = image.ReasonStorageFailed
	}

	return wire.TurnFailed(vendor, failure)
}

// invalidArgumentsRefusal returns the turn failure's data when the provider
// refused a function call in the thread's history whose arguments are not
// JSON, as a model cut off mid-call leaves one, and nil otherwise.
func invalidArgumentsRefusal(failure error) map[string]any {
	refusal, ok := errors.AsType[*acp.RequestError](failure)
	if !ok {
		return nil
	}

	data, _ := refusal.Data.(map[string]any)
	if message, _ := data[wire.FieldMessage].(string); !codex.RefusesInvalidArguments(message) {
		return nil
	}

	return data
}

// Outcomes of the history repair a refused turn's failure reports under
// fieldHistoryRepair.
const (
	fieldHistoryRepair    = "historyRepair"
	historyRepairRepaired = "repaired"
	historyRepairFailed   = "failed"
)

// threadRepairTimeout bounds unloading, repairing, and resuming a thread.
const threadRepairTimeout = 15 * time.Second

// repairHistory makes a thread usable again after the provider refused a
// function call in its history whose arguments are not JSON: every later
// request resends that call, so the thread could never complete another
// turn. It records the outcome in the refused turn's failure data. A thread
// that could not be resumed is unbound, so the next operation rebinds it.
func (s *session) repairHistory(ctx context.Context, runtime *runtime, failure map[string]any) {
	repaired, err := s.repairInvalidArguments(ctx, runtime)

	log := s.agent.log.With(slog.String("session_id", string(s.id)))

	if err != nil {
		failure[fieldHistoryRepair] = historyRepairFailed

		log.WarnContext(ctx, "codex history repair failed", slog.Int("calls", repaired), slog.String("reason", err.Error()))

		return
	}

	failure[fieldHistoryRepair] = historyRepairRepaired

	log.WarnContext(ctx, "codex history repaired", slog.Int("calls", repaired))
}

// repairInvalidArguments unloads the thread so its rollout is complete and
// no longer written, rewrites every function call's non-JSON arguments as an
// empty object, and resumes the thread from the repaired rollout. The call's
// output, which already tells the model the arguments did not parse, is
// kept. A rewritten rollout no longer extends what the store holds, so the
// mirror republishes it whole. It reports how many calls it repaired; a
// rollout with none is an error, since the refusal stands.
func (s *session) repairInvalidArguments(ctx context.Context, runtime *runtime) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, threadRepairTimeout)
	defer cancel()

	s.mu.Lock()
	nativeID := s.nativeID
	path := s.rolloutPath
	resume := s.threadResume()
	s.mu.Unlock()

	if err := runtime.client.UnloadThread(ctx, nativeID); err != nil {
		return 0, s.resumeAfterRepair(ctx, runtime, resume, fmt.Errorf("unload thread: %w", err))
	}

	repaired, err := repairRollout(path)
	if repaired > 0 {
		s.mirror.Reset()
	}

	if err == nil && repaired == 0 {
		err = errors.New("the rollout holds no function call with arguments that are not JSON")
	}

	return repaired, s.resumeAfterRepair(ctx, runtime, resume, err)
}

// resumeAfterRepair resumes the thread whatever the repair did, and joins a
// resume failure to the repair's own. A thread left unresumed is unbound.
func (s *session) resumeAfterRepair(ctx context.Context, runtime *runtime, resume codex.ThreadResumeRequest, repairErr error) error {
	thread, err := runtime.client.ResumeThread(ctx, resume, runtime.nativePath)
	if err != nil {
		// The turn's incarnation is fenced after its idle and the next
		// operation rebinds the thread on a fresh one.
		s.mu.Lock()
		if s.runtime == runtime {
			s.runtime = nil
			s.generationLost = true
		}
		s.mu.Unlock()

		return errors.Join(repairErr, fmt.Errorf("resume thread: %w", err))
	}

	s.bind(runtime, thread)

	return repairErr
}

// repairRollout rewrites the rollout at path with every function call's
// non-JSON arguments replaced, and reports how many calls it repaired.
func repairRollout(path string) (int, error) {
	native, err := sessionlog.ReadJSONL(path, sessionlog.Cursor{})
	if err != nil {
		return 0, err
	}

	rows, repaired, err := codex.RepairCallArguments(native.Rows)
	if err != nil || repaired == 0 {
		return 0, err
	}

	if _, err := sessionlog.WriteJSONL(path, rows); err != nil {
		return 0, err
	}

	return repaired, nil
}
