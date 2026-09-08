package lifecycle

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
)

// MetaPath is the request path a rejection names. Negotiation and correlation
// values are rejected as invalid params rather than as stream violations, because
// they are read before any stream exists.
const MetaPath = `_meta["` + MetaKey + `"]`

// VerdictUnsupported names a value the host sent and this adapter refuses; a
// present key on a surface that carries none, and a malformed member, are both
// that. VerdictMissing names a value the contract requires and the host left
// out. The two are distinct facts about one field path: a host reading
// `missing` adds the key, a host reading `unsupported` on the bare path stops
// sending it on that surface.
const (
	VerdictUnsupported = "unsupported"
	VerdictMissing     = "missing"
)

// ParamError refuses a negotiation or correlation value. It names the exact member
// path so a host can tell which value it got wrong, and it is the one family
// literal this adapter validates on `initialize` itself.
type ParamError struct {
	// Field is the full request path, from MetaPath down to the offending member.
	Field string
	// Verdict is VerdictUnsupported or VerdictMissing.
	Verdict string
}

// Error implements error.
func (e *ParamError) Error() string { return e.Verdict + " " + e.Field }

func paramError(members ...string) *ParamError {
	var field strings.Builder
	field.WriteString(MetaPath)

	for _, member := range members {
		field.WriteString("." + member)
	}

	return &ParamError{Field: field.String(), Verdict: VerdictUnsupported}
}

// missingParamError refuses the absent prompt correlation value on a
// negotiated connection.
func missingParamError() *ParamError {
	return &ParamError{Field: MetaPath, Verdict: VerdictMissing}
}

// RejectKey refuses the reserved literal on a surface that carries no lifecycle
// value. A family literal is never a foreign namespace and never a no-op, so every
// inbound surface outside `initialize`, `session/prompt`, and the stream refuses it
// by name.
func RejectKey(meta map[string]any) *ParamError {
	if _, present := meta[MetaKey]; !present {
		return nil
	}

	return paramError()
}

// Offer is the current initialize-offer marker returned by DecodeOffer.
type Offer struct{}

// DecodeOffer reads the offer from `InitializeRequest._meta`. An absent offer is
// reported as not present rather than as a refusal: the host asked for nothing, and
// the answer, every envelope, and every correlation read are then omitted for the
// whole connection.
func DecodeOffer(meta map[string]any) (Offer, bool, *ParamError) {
	raw, present := meta[MetaKey]
	if !present {
		return Offer{}, false, nil
	}

	fields, refusal := negotiationObject(raw)
	if refusal != nil {
		return Offer{}, false, refusal
	}

	for key := range fields {
		if key != fieldVersion {
			return Offer{}, false, paramError(key)
		}
	}

	version, ok := integerValue(fields[fieldVersion])
	if !ok || version != Version {
		return Offer{}, false, paramError(fieldVersion)
	}

	return Offer{}, true, nil
}

// Answer stamps the accepted current version onto the facts the active
// configuration proved.
func (Offer) Answer(proven Negotiated) Negotiated {
	proven.Version = Version

	return proven
}

// Submission names one accepted client prompt. The client nonce is the host's own
// input identity: it is distinct from every JSON-RPC message id and from the route
// envelope's turn nonce, and no sibling substitutes one for another.
type Submission struct {
	SubmissionID string
	ClientNonce  string
	RunID        string
}

// DecodePromptCorrelation reads the value a `session/prompt` carries while version 1
// is negotiated. The key is required when negotiated and forbidden when not, and
// either way the verdict is reached before the prompt is dispatched, so no frame is
// written to the harness.
func DecodePromptCorrelation(meta map[string]any, negotiated Negotiated) (Submission, *ParamError) {
	raw, present := meta[MetaKey]

	switch {
	case !negotiated.Present() && present:
		return Submission{}, paramError()
	case !negotiated.Present():
		return Submission{}, nil
	case !present:
		return Submission{}, missingParamError()
	}

	fields, refusal := negotiationObject(raw)
	if refusal != nil {
		return Submission{}, refusal
	}

	for key := range fields {
		if key != fieldVersion && key != fieldSubmission {
			return Submission{}, paramError(key)
		}
	}

	if refusal := checkCorrelationVersion(fields, negotiated); refusal != nil {
		return Submission{}, refusal
	}

	return decodeSubmission(fields[fieldSubmission])
}

func checkCorrelationVersion(fields map[string]any, negotiated Negotiated) *ParamError {
	version, ok := integerValue(fields[fieldVersion])
	if !ok || !negotiated.SupportsVersion(version) {
		return paramError(fieldVersion)
	}

	return nil
}

// minIntFloat and overMaxIntFloat bound the float64 values that are this
// platform's integers. MinInt is a power of two and therefore exact, and its
// negation is the first value one past MaxInt, so the half-open range holds
// exactly the float64 values an int can carry — MaxInt itself is not
// representable as a float64 on a 64-bit platform, and no bound may be spelled
// as a value the conversion would have to round.
const (
	minIntFloat     = float64(math.MinInt)
	overMaxIntFloat = -minIntFloat
)

// integerValue preserves exact wire integers and accepts integral Go values.
func integerValue(raw any) (int, bool) {
	switch value := raw.(type) {
	case float64:
		if value != math.Trunc(value) || value < minIntFloat || value >= overMaxIntFloat {
			return 0, false
		}

		return int(value), true
	case int:
		return value, true
	case json.RawMessage:
		return integerValue(json.Number(strings.TrimSpace(string(value))))
	case json.Number:
		number, err := value.Int64()
		if err != nil || number < math.MinInt || number > math.MaxInt {
			return 0, false
		}

		return int(number), true
	default:
		return 0, false
	}
}

func decodeSubmission(raw any) (Submission, *ParamError) {
	fields, refusal := negotiationObject(raw, fieldSubmission)
	if refusal != nil {
		return Submission{}, refusal
	}

	for key := range fields {
		if key != fieldSubmissionID && key != fieldClientNonce && key != fieldRunID {
			return Submission{}, paramError(fieldSubmission, key)
		}
	}

	submission := Submission{}

	for _, member := range []struct {
		key      string
		target   *string
		required bool
	}{
		{fieldSubmissionID, &submission.SubmissionID, true},
		{fieldClientNonce, &submission.ClientNonce, true},
		{fieldRunID, &submission.RunID, false},
	} {
		value, refusal := correlationIdentifier(fields, member.key, member.required)
		if refusal != nil {
			return Submission{}, refusal
		}

		*member.target = value
	}

	return submission, nil
}

// correlationIdentifier reads one opaque handle. An identifier is a correlation
// handle, not a payload: it is bounded, and it is never empty — an optional one is
// omitted rather than emptied, so a member present carrying the empty string is
// malformed rather than absent.
func correlationIdentifier(fields map[string]any, key string, required bool) (string, *ParamError) {
	raw, present := fields[key]
	if !present {
		if required {
			return "", paramError(fieldSubmission, key)
		}

		return "", nil
	}

	value, ok := raw.(string)
	if encoded, wire := raw.(json.RawMessage); wire {
		ok = json.Unmarshal(encoded, &value) == nil
	}

	if !ok || value == "" || len(value) > IdentifierBound {
		return "", paramError(fieldSubmission, key)
	}

	return value, nil
}

func negotiationObject(raw any, members ...string) (map[string]any, *ParamError) {
	if refusal, ok := raw.(*ParamError); ok {
		return nil, refusal
	}

	if fields, ok := raw.(map[string]any); ok {
		return fields, nil
	}

	encoded, ok := raw.(json.RawMessage)
	if !ok {
		return nil, paramError(members...)
	}

	decoder := json.NewDecoder(bytes.NewReader(encoded))
	opening, err := decoder.Token()

	if err != nil || opening != json.Delim('{') {
		return nil, paramError(members...)
	}

	fields := make(map[string]any)

	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, paramError(members...)
		}

		key, _ := token.(string) // Object member tokens are strings.
		path := append(append([]string(nil), members...), key)

		if _, duplicate := fields[key]; duplicate {
			return nil, paramError(path...)
		}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, paramError(path...)
		}

		fields[key] = value
	}

	if _, err := decoder.Token(); err != nil || !json.Valid(encoded) {
		return nil, paramError(members...)
	}

	return fields, nil
}

// ActionCorrelation names one pending permission or elicitation on the stream that
// announced it. It is lifecycle identity only: neither side may route or authorize a
// callback with it, and on an elicitation the reserved route object remains the
// routing and authentication envelope beside it.
type ActionCorrelation struct {
	StreamID string
	ActionID string
	Owner    Owner
	RunID    string
}

// Value renders the object the sibling stamps on every `session/request_permission`
// and every `elicitation/create` while version 1 is negotiated.
func (c ActionCorrelation) Value() map[string]any {
	action := map[string]any{
		fieldActionID: c.ActionID,
		fieldOwner: map[string]any{
			fieldType: string(c.Owner.Type),
			fieldID:   c.Owner.ID,
		},
	}
	withOptional(action, fieldRunID, c.RunID)

	return map[string]any{
		fieldVersion:  Version,
		fieldStreamID: c.StreamID,
		fieldAction:   action,
	}
}
