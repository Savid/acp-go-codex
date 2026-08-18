package codexacp

import (
	"encoding/json"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/lifecycle"
)

// negotiateLifecycle answers the host's reserved offer with the facts this
// configuration proves. An absent offer and an empty version intersection are
// both answered by omitting the key; a non-empty intersection is answered, even
// though every fact in it is negative, because the answer is what makes the
// version-1 foreground stream reachable at all.
func (a *Agent) negotiateLifecycle(meta map[string]any) (lifecycle.Negotiated, error) {
	offer, present, refusal := lifecycle.DecodeOffer(meta)
	if refusal != nil {
		return lifecycle.Negotiated{}, lifecycleInvalidParams(refusal)
	}

	if !present {
		return lifecycle.Negotiated{}, nil
	}

	negotiated, answered := offer.Answer(a.provenLifecycleFacts())
	if !answered {
		return lifecycle.Negotiated{}, nil
	}

	return negotiated, nil
}

// provenLifecycleFacts states what this adapter can prove. The answer is a
// constant, and deliberately so: every codex configuration this adapter can be
// built with drives one app-server generation shared by every session, so no
// option, model, or runtime state moves any of these facts. There is nothing
// configuration-dependent to read, and reading a boundary at answer time would
// only dress a fixed answer up as a measurement.
//
//   - A session opens one incarnation per prompt and fences it at settlement,
//     so no lifecycle envelope is ever delivered while no prompt is in flight.
//   - Nothing here proves one logical session's descendants gone while the
//     shared app-server generation they live in keeps serving every peer, so no
//     quiescence proof class is claimed and no source is named.
//   - Codex activity reaches ACP as ordinary tool-call updates; this adapter
//     reads no structured native activity registry, so it emits no
//     activity_update and advertises no kind.
//
// Every one of those is a negative fact, which is the truthful answer for a
// prompt-contained configuration rather than a gap papered over. The receiver
// is unused for the same reason, and is kept so the facts stay addressable as
// the adapter's own answer if a future configuration ever splits generations.
func (a *Agent) provenLifecycleFacts() lifecycle.Negotiated {
	return lifecycle.Negotiated{
		UpdatesOutsidePrompt:    false,
		AuthoritativeQuiescence: false,
		ActivityKinds:           []lifecycle.ActivityKind{},
	}
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
