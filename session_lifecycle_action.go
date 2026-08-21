package codexacp

import (
	"context"
	"errors"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/lifecycle"
)

type lifecycleActionTurnKey struct{}

// liveAction is one permission or elicitation held open on the lifecycle
// stream. Minting precedes the ACP request, but publication waits for the
// connection registration barrier so a host can answer every pending action it
// observes.
type liveAction struct {
	incarnation *promptIncarnation
	id          string
	kind        lifecycle.ActionKind
	blocks      bool
	registered  bool
}

func withLifecycleActionTurn(ctx context.Context, in *promptIncarnation) context.Context {
	return context.WithValue(ctx, lifecycleActionTurnKey{}, in)
}

// beginAction mints one action and returns the correlation value to stamp on the
// outbound request. register publishes it only after the host request is known
// to the ACP connection. An unnegotiated connection carries no action at all.
func (s *session) beginAction(
	ctx context.Context,
	kind lifecycle.ActionKind,
	blocksForeground bool,
) (*liveAction, map[string]any, error) {
	incarnation, _ := ctx.Value(lifecycleActionTurnKey{}).(*promptIncarnation)
	if incarnation == nil {
		s.lifecycleMu.Lock()
		negotiated := s.lifecycleStream != nil
		s.lifecycleMu.Unlock()

		if incarnation == nil && negotiated {
			return nil, nil, errors.New("lifecycle action omitted its exact native turn")
		}

		if incarnation == nil {
			return nil, nil, nil
		}
	}

	if !incarnation.lifecycleActive() {
		return nil, nil, nil
	}

	actionID, err := newSessionID()
	if err != nil {
		return nil, nil, err
	}

	action := &liveAction{
		incarnation: incarnation,
		id:          actionID,
		kind:        kind,
		blocks:      blocksForeground,
	}

	correlation := lifecycle.ActionCorrelation{
		StreamID: incarnation.stream.ID(),
		ActionID: actionID,
		Owner:    lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: incarnation.turnID},
	}

	return action, correlation.Value(), nil
}

func (a *liveAction) register(ctx context.Context) error {
	if a == nil {
		return nil
	}

	if a.registered {
		return errors.New("lifecycle action was registered more than once")
	}

	if err := a.incarnation.announceAction(ctx, a.id, a.kind, a.blocks); err != nil {
		return err
	}

	a.registered = true

	return nil
}

func requestPermissionWithAction(
	ctx context.Context,
	conn agentClient,
	request acp.RequestPermissionRequest,
	action *liveAction,
	registeredHook func(),
) (acp.RequestPermissionResponse, error) {
	if action == nil {
		if registeredHook != nil {
			registeredHook()
		}

		return conn.RequestPermission(ctx, request)
	}

	registered, ok := conn.(registeredActionClient)
	if !ok {
		return acp.RequestPermissionResponse{}, errors.New("ACP client cannot prove permission request registration")
	}

	return registered.RequestPermissionRegistered(ctx, request, action.id, func() error {
		if err := action.register(ctx); err != nil {
			return err
		}

		if registeredHook != nil {
			registeredHook()
		}

		return nil
	})
}

func createElicitationWithAction(
	ctx context.Context,
	conn agentClient,
	request acp.UnstableCreateElicitationRequest,
	scope elicitationScope,
	action *liveAction,
	registeredHook func(),
) (acp.UnstableCreateElicitationResponse, error) {
	if action == nil {
		if registeredHook != nil {
			registeredHook()
		}

		return conn.CreateElicitation(ctx, request, scope)
	}

	registered, ok := conn.(registeredActionClient)
	if !ok {
		return acp.UnstableCreateElicitationResponse{}, errors.New("ACP client cannot prove elicitation request registration")
	}

	return registered.CreateElicitationRegistered(ctx, request, scope, action.id, func() error {
		if err := action.register(ctx); err != nil {
			return err
		}

		if registeredHook != nil {
			registeredHook()
		}

		return nil
	})
}

// resolve terminalizes the action with the state its answer reached. It is safe
// to call for an action that never existed, which is what an unnegotiated
// connection has.
//
// A teardown answers the pending request by cancelling the context it was issued
// on, and two things follow from that. A cancel or close teardown terminalizes
// `cancelled`: incarnation loss is the path that terminalizes `failed`, and the
// two never share a terminal state, so a host can tell a contained end from a
// lost one. And the terminal patch is emitted on a context that outlives the
// teardown, because the sequence it claims is spent whether or not the
// notification is delivered — an undelivered resolution leaves the host holding
// a gap and an action with no terminal at all.
func (a *liveAction) resolve(ctx context.Context, state lifecycle.ActionState) error {
	if a == nil || !a.registered {
		return nil
	}

	if ctx.Err() == nil {
		return a.incarnation.resolveAction(ctx, a.id, state)
	}

	if state == lifecycle.ActionFailed && (a.incarnation.cancelled || a.incarnation.session.wasTurnCancelled()) {
		state = lifecycle.ActionCancelled
	}

	emitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), promptSettlementTimeout)
	defer cancel()

	return a.incarnation.resolveAction(emitCtx, a.id, state)
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
