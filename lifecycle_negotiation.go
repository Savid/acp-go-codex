package codexacp

import (
	"encoding/json"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/lifecycle"
)

// lifecycleAdvertisement resolves the lifecycle answer for one connection from
// the containment mode that connection actually enforces.
//
// Every fact it can state is degenerate, in every configuration. A quiescence
// class needs a boundary that proves the addressed session's own descendants
// gone, and every proof this adapter holds is scoped to the shared app-server
// generation that serves every logical session at once: the Linux subreaper's
// ECHILD result and its descendant inventory prove that generation vacant, and
// ending it to prove one session settled would end every peer with it. The
// thread-scoped background-terminal sweep enumerates terminals rather than the
// process tree behind them, and a poll predicate is not a proof class. So the
// mode is the resolution path the contract requires rather than a compiled-in
// constant, and it selects the same truthful row everywhere until some boundary
// earns more.
//
// `updatesOutsidePrompt` is false for the same reason it is honest: a lifecycle
// stream belongs to one prompt incarnation and is fenced when that prompt ends,
// so there is no channel between prompts to deliver on. `activityKinds` is empty
// because the app-server publishes no structured per-instance activity event at
// all.
func lifecycleAdvertisement(RuntimeContainmentMode) lifecycle.Negotiated {
	return lifecycle.Negotiated{ActivityKinds: []lifecycle.ActivityKind{}}
}

// negotiateLifecycle reads the host's offer and resolves this connection's
// answer. An absent offer means the host asked for nothing, so the key is
// omitted from the response and no envelope, prompt correlation, or action
// correlation exists on the connection.
func (a *Agent) negotiateLifecycle(meta map[string]any) (lifecycle.Negotiated, error) {
	offer, present, refusal := lifecycle.DecodeOffer(meta)
	if refusal != nil {
		return lifecycle.Negotiated{}, lifecycleInvalidParams(refusal)
	}

	if !present {
		return lifecycle.Negotiated{}, nil
	}

	answer, common := offer.Answer(lifecycleAdvertisement(containmentMode(a.options)))
	if !common {
		return lifecycle.Negotiated{}, nil
	}

	return answer, nil
}

// negotiatedLifecycle reports the answer this connection settled on.
func (a *Agent) negotiatedLifecycle() lifecycle.Negotiated {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.lifecycle
}

// lifecycleResponseMeta places the answer on the response's own `_meta`, beside
// the other reserved family literals rather than inside the capability object.
// Capability structs move in later protocol work; `initialize` `_meta` does not.
func lifecycleResponseMeta(negotiated lifecycle.Negotiated) map[string]any {
	if !negotiated.Present() {
		return nil
	}

	return map[string]any{lifecycle.MetaKey: negotiated.Advertisement()}
}

// lifecycleInvalidParams renders one refusal as ACP invalid params naming the
// exact member path the closed decoder reached.
func lifecycleInvalidParams(refusal *lifecycle.ParamError) error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldError: errValueUnsupported,
		jsonFieldField: refusal.Field,
	})
}

// rejectLifecycleKey refuses the reserved literal on a surface that carries no
// lifecycle value. A family literal is never a foreign namespace and never a
// no-op, so it is refused by name before the surface's own side effects or its
// own refusal.
func rejectLifecycleKey(meta map[string]any) error {
	if refusal := lifecycle.RejectKey(meta); refusal != nil {
		return lifecycleInvalidParams(refusal)
	}

	return nil
}

// rejectLifecycleKeyInParams refuses the literal on an extension method, whose
// params arrive as raw JSON. Params this adapter cannot even read carry no key,
// so the surface's own decoder reports the shape problem.
func rejectLifecycleKeyInParams(params json.RawMessage) error {
	var envelope struct {
		Meta map[string]any `json:"_meta"` //nolint:tagliatelle // ACP wire name.
	}

	if err := json.Unmarshal(params, &envelope); err != nil {
		return nil //nolint:nilerr // The route-specific decoder reports malformed params.
	}

	return rejectLifecycleKey(envelope.Meta)
}
