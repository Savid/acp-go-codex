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

	a.mu.Lock()
	carrier, ok := a.conn.(lifecycleCarrier)
	a.mu.Unlock()

	if ok && !carrier.LifecycleDeliverySupported() {
		return lifecycle.Negotiated{}, nil
	}

	negotiated, answered := offer.Answer(a.provenLifecycleFacts())
	if !answered {
		return lifecycle.Negotiated{}, nil
	}

	return negotiated, nil
}

// provenLifecycleFacts states what the active configuration can actually prove,
// resolved on the agent that enforces containment rather than compiled in once
// for the package.
//
// These facts do not turn on the containment mode:
// the mode changes which identity the app-server runs under and which vacancy
// the adapter can prove about it, and neither reaches these answers. Each
// answer holds under every mode this adapter can select:
//
//   - `updatesOutsidePrompt` is true because a session claims one exact native
//     thread broker and keeps its lifecycle stream across prompt settlement.
//   - `authoritativeQuiescence` is false because one logical session's
//     descendants cannot be proved gone while the app-server generation they
//     live in keeps serving every peer session. Even the authoritative boundary
//     proves vacancy for the whole shared generation, never for one session
//     inside it, so no configuration reaches a per-session proof class and none
//     names a source.
//   - `activityKinds` is empty because Codex activity reaches ACP as ordinary
//     tool-call updates and this adapter reads no structured native activity
//     registry. That is a native-surface fact, identical under every mode.
//
// A fact is upgraded — and the branch on `a.ContainmentMode()` that upgrading it
// requires appears here — only when a deterministic fixture in this repository
// proves both the native source it reads and the ordering it claims. Until then
// stating one answer is the honest shape; a table of identical rows would
// advertise a differentiation this adapter does not have. Negotiating version 1
// still obligates the complete ordered foreground stream whatever these fields
// say.
func (a *Agent) provenLifecycleFacts() lifecycle.Negotiated {
	return lifecycle.Negotiated{
		UpdatesOutsidePrompt:    true,
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
