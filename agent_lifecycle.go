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

	negotiated, answered := offer.Answer(provenLifecycleFacts(a.ContainmentMode()))
	if !answered {
		return lifecycle.Negotiated{}, nil
	}

	return negotiated, nil
}

// lifecycleFactsByContainment is this adapter's per-configuration lifecycle
// truth table. The answer describes the active configuration, so it is keyed on
// the same containment mode that selects and enforces the native process
// boundary rather than compiled in once for the package.
//
// Every row is identical, and that is itself the finding rather than an
// unfinished table: containment mode changes which identity the app-server runs
// under and which vacancy the adapter can prove about it, and none of these
// three facts turns on either. Each is negative for a reason that holds in every
// mode:
//
//   - `updatesOutsidePrompt` is false because a session opens one incarnation
//     per prompt and fences it at settlement, so no lifecycle envelope is ever
//     delivered while no prompt is in flight. That is a property of this
//     adapter's stream ownership, which no containment mode touches.
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
// A row is upgraded only when a deterministic fixture in this repository proves
// both the native source it reads and the ordering it claims for that mode.
var lifecycleFactsByContainment = map[RuntimeContainmentMode]lifecycle.Negotiated{
	RuntimeContainmentAuthoritative:  provenFacts(),
	RuntimeContainmentBestEffort:     provenFacts(),
	RuntimeContainmentSharedIdentity: provenFacts(),
	RuntimeContainmentUnavailable:    provenFacts(),
}

// provenFacts is the answer every row carries: no delivery outside a prompt, no
// quiescence class, and no activity kind. Negotiating version 1 still obligates
// the complete ordered foreground stream whatever these fields say.
func provenFacts() lifecycle.Negotiated {
	return lifecycle.Negotiated{
		UpdatesOutsidePrompt:    false,
		AuthoritativeQuiescence: false,
		ActivityKinds:           []lifecycle.ActivityKind{},
	}
}

// provenLifecycleFacts reads one configuration's row. A mode with no row proves
// nothing, which is the same answer the unavailable boundary gives.
func provenLifecycleFacts(mode RuntimeContainmentMode) lifecycle.Negotiated {
	facts, known := lifecycleFactsByContainment[mode]
	if !known {
		return provenFacts()
	}

	return facts
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
