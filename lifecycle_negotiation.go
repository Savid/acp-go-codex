package codexacp

import (
	"encoding/json"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/lifecycle"
)

// negotiateLifecycle validates the host's reserved offer but omits an answer.
// Codex has no registry-accepted captured-native evidence for the family
// extension, so advertising even a degenerate intersection would claim an
// evidence gate this adapter has not passed.
func (a *Agent) negotiateLifecycle(meta map[string]any) (lifecycle.Negotiated, error) {
	_, _, refusal := lifecycle.DecodeOffer(meta)
	if refusal != nil {
		return lifecycle.Negotiated{}, lifecycleInvalidParams(refusal)
	}

	return lifecycle.Negotiated{}, nil
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
