package codexacp

import (
	"context"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/lifecycle"
)

// liveAction is one pending permission or elicitation held open on the lifecycle
// stream. It is minted and announced before the inbound request is issued, so a
// host can never see an action id it cannot yet answer, and it resolves exactly
// once.
type liveAction struct {
	incarnation *promptIncarnation
	id          string
}

// beginAction mints one action, announces it on the stream, and returns the
// correlation value to stamp on the outbound request. An unnegotiated connection
// carries no action at all, and the standard ACP permission and elicitation
// outcomes are unchanged either way.
func (s *session) beginAction(
	ctx context.Context,
	kind lifecycle.ActionKind,
	blocksForeground bool,
) (*liveAction, map[string]any, error) {
	incarnation := s.liveIncarnation()
	if incarnation == nil {
		return nil, nil, nil
	}

	actionID, err := newSessionID()
	if err != nil {
		return nil, nil, err
	}

	action := &liveAction{incarnation: incarnation, id: actionID}
	if err := incarnation.announceAction(ctx, actionID, kind, blocksForeground); err != nil {
		return nil, nil, err
	}

	correlation := lifecycle.ActionCorrelation{
		StreamID: incarnation.stream.ID(),
		ActionID: actionID,
		Owner:    lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: incarnation.turnID},
	}

	return action, correlation.Value(), nil
}

// resolve terminalizes the action with the state its answer reached. It is safe
// to call for an action that never existed, which is what an unnegotiated
// connection has.
func (a *liveAction) resolve(ctx context.Context, state lifecycle.ActionState) error {
	if a == nil {
		return nil
	}

	return a.incarnation.resolveAction(ctx, a.id, state)
}

// permissionActionState reads the standard ACP permission outcome. The outcome
// union is the only source: response `_meta` is read by nobody, so a host cannot
// change an outcome by annotating it.
func permissionActionState(resp acp.RequestPermissionResponse, err error) lifecycle.ActionState {
	switch {
	case err != nil:
		return lifecycle.ActionFailed
	case resp.Outcome.Cancelled != nil:
		return lifecycle.ActionCancelled
	case resp.Outcome.Selected != nil:
		return lifecycle.ActionAccepted
	default:
		return lifecycle.ActionDeclined
	}
}

// elicitationActionState reads the standard ACP elicitation action.
func elicitationActionState(resp acp.UnstableCreateElicitationResponse, err error) lifecycle.ActionState {
	switch {
	case err != nil:
		return lifecycle.ActionFailed
	case resp.Accept != nil:
		return lifecycle.ActionAccepted
	case resp.Cancel != nil:
		return lifecycle.ActionCancelled
	default:
		return lifecycle.ActionDeclined
	}
}

// stampActionCorrelation places the correlation value beside whatever `_meta` a
// request already carries. On an elicitation the reserved route object stays
// where it is: the two keys coexist, and neither replaces the other.
func stampActionCorrelation(meta map[string]any, correlation map[string]any) map[string]any {
	if correlation == nil {
		return meta
	}

	out := cloneAnyMap(meta)
	if out == nil {
		out = map[string]any{}
	}

	out[lifecycle.MetaKey] = correlation

	return out
}
