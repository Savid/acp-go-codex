package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/image"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/wire"
)

const (
	limitSessionPrompt = "session_prompt"

	// structuredOutputKey is where a schema-bound turn's parsed final answer
	// rides on the prompt response.
	structuredOutputKey = "structuredOutput"
	// nativeCauseMaxBytes bounds the native cause text a failure carries.
	nativeCauseMaxBytes = 2048
)

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
func (s *session) rejectImagesForModel(rt *runtime, prompt nativePrompt) error {
	if prompt.firstImage == nil {
		return nil
	}

	model := s.currentModel()

	for index := range rt.models {
		candidate := &rt.models[index]
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
		ThreadID:        string(s.id),
		Input:           input,
		Model:           s.model,
		ServiceTier:     s.serviceTier,
		ReasoningEffort: s.effort,
		Personality:     s.personality,
		ApprovalPolicy:  s.options.ApprovalPolicy,
		SandboxPolicy:   s.turnSandboxPolicy(),
	}

	if s.options.OutputSchema != nil {
		req.OutputSchema = cloneAnyMap(s.options.OutputSchema)
	}

	if s.mode != modeDefault {
		var effort any
		if s.effort != "" {
			effort = s.effort
		}

		req.CollaborationMode = map[string]any{
			"mode": s.mode,
			"settings": map[string]any{
				metaModelKey: firstNonEmpty(s.model, modeDefault), "developer_instructions": nil, "reasoning_effort": effort,
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
		return acp.PromptResponse{}, invalidParam(paramErr)
	}

	if err := s.admissionError(); err != nil {
		return acp.PromptResponse{}, err
	}

	release, err := s.acquireGate(limitSessionPrompt)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	defer release()

	s.mu.Lock()
	busy := s.cycle != nil
	s.mu.Unlock()

	if busy {
		return acp.PromptResponse{}, wire.Backpressure(limitSessionPrompt)
	}

	mapped, err := s.mapPrompt(ctx, params.Prompt)
	if err != nil {
		if ctx.Err() != nil {
			return cancelledResponse(params), nil
		}

		return acp.PromptResponse{}, err
	}

	// session/cancel cancels this request's context through the SDK. A cancel
	// that lands before native dispatch creates neither submission nor turn and
	// answers cancelled.
	if ctx.Err() != nil {
		return cancelledResponse(params), nil
	}

	rt, err := s.ensureBound(ctx)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	if refusal := s.rejectImagesForModel(rt, mapped); refusal != nil {
		return acp.PromptResponse{}, refusal
	}

	t := &turn{
		cycle:      cycle{origin: lifecycle.CauseSubmission, state: cycleState{tools: make(map[string]*toolState)}},
		submission: submission,
		settled:    make(chan struct{}),
		finished:   make(chan struct{}),
	}
	defer close(t.finished)

	s.mu.Lock()
	s.turn = t
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if s.turn == t {
			s.turn = nil
		}
		s.mu.Unlock()
	}()

	if timeout := s.agent.options.TurnTimeout; timeout > 0 {
		timer := time.AfterFunc(timeout, func() { s.timeout(context.WithoutCancel(ctx), t) })
		defer timer.Stop()
	}

	nativeTurnID, err := rt.client.StartTurn(ctx, s.turnStart(mapped.input))
	if err != nil {
		if !t.accepted {
			if ctx.Err() != nil {
				return cancelledResponse(params), nil
			}

			return acp.PromptResponse{}, s.dispatchFailure(ctx, rt, err)
		}
	}

	s.mu.Lock()
	if t.nativeTurnID == "" {
		t.nativeTurnID = nativeTurnID
	}
	s.mu.Unlock()

	s.acceptTurn(ctx, t)

	select {
	case <-t.settled:
	case <-ctx.Done():
		s.cancel(ctx)

		select {
		case <-t.settled:
		case <-time.After(sessionAbortTimeout):
			t.settle(turnTransportEnded)
		}
	}

	return s.settleTurn(ctx, rt, t, params)
}

func cancelledResponse(params acp.PromptRequest) acp.PromptResponse {
	return acp.PromptResponse{StopReason: acp.StopReasonCancelled, UserMessageId: params.MessageId}
}

// dispatchFailure classifies a turn the app-server never accepted: a native
// rejection carries its message as a provider failure, a dead process is a
// process exit, and everything else is transport.
func (s *session) dispatchFailure(ctx context.Context, rt *runtime, err error) error {
	var rpcErr *codex.RPCError
	if errors.As(err, &rpcErr) {
		return turnFailure(wire.CauseProvider, rpcErr.Message)
	}

	return s.agent.transportFailure(ctx, rt, err)
}

func turnFailure(cause string, message string) *acp.RequestError {
	return wire.TurnFailed(vendor, wire.TurnFailure{Cause: cause, Message: boundNativeCause(message)})
}

// boundNativeCause is the single gate every native cause text passes through
// before it reaches a client.
func boundNativeCause(message string) string {
	if len(message) > nativeCauseMaxBytes {
		message = message[:nativeCauseMaxBytes]
	}

	return strings.TrimSpace(strings.ToValidUTF8(message, ""))
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
func judgeCycle(c *cycle, cancelled bool) cycleVerdict {
	switch {
	case cancelled:
		return cycleVerdict{outcome: lifecycle.OutcomeCancelled, stopReason: lifecycle.StopReasonCancelled}
	case c.failure != nil:
		return cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: c.failure}
	case c.state.stop == codex.StopReasonError:
		failure := wire.TurnFailure{Cause: wire.CauseProvider, Message: "codex reported a turn error"}
		if c.state.failure != nil {
			failure.Message = boundNativeCause(c.state.failure.Message)
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
func (s *session) settleTurn(ctx context.Context, rt *runtime, t *turn, params acp.PromptRequest) (acp.PromptResponse, error) {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionSettleTimeout)
	defer cancel()

	var verdict cycleVerdict

	switch {
	case t.cancelled:
		verdict = cycleVerdict{outcome: lifecycle.OutcomeCancelled, stopReason: lifecycle.StopReasonCancelled}
	case t.timedOut:
		verdict = cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: turnFailure(wire.CauseTimeout, formatDeadline(s.agent.options.TurnTimeout))}
	case t.ended == turnTransportEnded:
		verdict = cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: s.agent.transportFailure(settleCtx, rt, nil)}
	default:
		verdict = judgeCycle(&t.cycle, false)
	}

	if t.ended == turnSettled && !t.cancelled {
		s.emitSessionInfo(settleCtx, params.Prompt)
	}

	if err := s.commitMirror(settleCtx); err != nil && verdict.failure == nil {
		verdict.failure = s.mirrorFailure(&t.state, err)
		verdict.outcome = lifecycle.OutcomeFailed
	}

	if err := s.lcIdle(settleCtx, &t.cycle, verdict); err != nil && verdict.failure == nil {
		verdict.failure = err
	}

	if t.ended == turnTransportEnded {
		s.lcFence()
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

	return map[string]any{vendor: map[string]any{structuredOutputKey: value}}
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
