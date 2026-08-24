package codexacp

import (
	"context"
	"errors"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

// steerTurn consumes native turn/steer only through the exact turn-route
// surface. A steer queued before turn/start acknowledgement waits for that
// exact binding; cancellation, interruption, settlement, and rebind all make
// the route fail closed instead of retargeting whichever turn is current.
func (s *session) steerTurn(ctx context.Context, turnNonce string, blocks []acp.ContentBlock) error {
	interactionCtx, finish := s.beginInteraction(ctx, "turn-steer:"+turnNonce)
	defer finish()

	if !s.turnRouteActive(turnNonce) {
		return errTurnRouteMismatch
	}

	input, release, err := s.prepareSteerInput(interactionCtx, blocks)
	if err != nil {
		return err
	}
	defer release()

	if bindingErr := s.waitForSteerBinding(interactionCtx, turnNonce); bindingErr != nil {
		return bindingErr
	}

	s.nativeControlMu.Lock()
	defer s.nativeControlMu.Unlock()

	client, threadID, turnID, err := s.exactSteerTarget(turnNonce)
	if err != nil {
		return err
	}

	return client.SteerTurn(interactionCtx, codex.TurnSteerRequest{
		ThreadID: threadID, ExpectedTurnID: turnID, Input: input,
	})
}

func (s *session) turnRouteActive(turnNonce string) bool {
	return turnNonce != "" && s.activeTurnNonce() == turnNonce
}

func (s *session) prepareSteerInput(ctx context.Context, blocks []acp.ContentBlock) ([]codex.UserInput, func(), error) {
	images, imageErr, abortErr := validatePromptImages(
		ctx, blocks, s.agent.options.ImageLimits, s.agent.options.InputHandoffRoot,
	)
	if abortErr != nil {
		return nil, nil, abortErr
	}

	if imageErr != nil {
		return nil, nil, imageErr.invalidParams()
	}

	s.mu.Lock()
	client := s.client
	model := s.model
	clientDead := s.clientDead
	s.mu.Unlock()

	if client == nil || clientDead {
		return nil, nil, codex.ErrConnectionClosed
	}

	if len(images) > 0 && selectedModelImageSupport(modelList(ctx, client), model) == imageInputUnsupported {
		imageErr = &promptImageError{code: imageErrorUnsupportedByModel, field: images[0].field, index: images[0].index}

		return nil, nil, imageErr.invalidParams()
	}

	prepared, err := s.preparePromptImages(ctx, images)
	if err != nil {
		return nil, nil, errors.New("codex steer image preparation failed")
	}

	input, err := promptToCodex(blocks, prepared.images)
	if err != nil {
		prepared.release()

		return nil, nil, err
	}

	return input, prepared.release, nil
}

func (s *session) waitForSteerBinding(ctx context.Context, turnNonce string) error {
	s.mu.Lock()
	promptOwned := s.turnNonce == turnNonce && s.turnDone != nil
	ready := s.turnReady
	s.mu.Unlock()

	if !promptOwned || ready == nil {
		return nil
	}

	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *session) exactSteerTarget(turnNonce string) (codex.Client, string, string, error) {
	s.mu.Lock()
	if s.turnNonce == turnNonce && s.turnDone != nil {
		active := s.turnAccepted && !s.turnCancelled && s.turnContainment == nil && s.turnID != ""
		if active {
			select {
			case <-s.turnDone:
				active = false
			default:
			}
		}

		client, threadID, turnID := s.client, s.codexThreadID, s.turnID
		s.mu.Unlock()

		if !active || client == nil || threadID == "" {
			return nil, "", "", errTurnRouteMismatch
		}

		return client, threadID, turnID, nil
	}
	s.mu.Unlock()

	s.mu.Lock()
	client, threadID, clientDead := s.client, s.codexThreadID, s.clientDead
	s.mu.Unlock()

	if client == nil || clientDead || threadID == "" {
		return nil, "", "", errTurnRouteMismatch
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	in := s.agentIncarnation
	if in == nil || in.turnNonce != turnNonce || in.settled || in.terminating != nil ||
		s.lifecycleClosing || s.nativeEventRebinding || in.nativeTurnID == "" {
		return nil, "", "", errTurnRouteMismatch
	}

	return client, threadID, in.nativeTurnID, nil
}
