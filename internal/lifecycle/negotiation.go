package lifecycle

import (
	"encoding/json"
	"math"
	"slices"
)

// MetaPath is the request path a rejection names. Negotiation and correlation
// values are rejected as invalid params rather than as stream violations, because
// they are read before any stream exists.
const MetaPath = `_meta["` + MetaKey + `"]`

// ParamError refuses a negotiation or correlation value. It names the exact member
// path so a host can tell which value it got wrong, and it is the one family
// literal this adapter validates on `initialize` itself.
type ParamError struct {
	// Field is the full request path, from MetaPath down to the offending member.
	Field string
}

// Error implements error.
func (e *ParamError) Error() string { return "unsupported " + e.Field }

func paramError(members ...string) *ParamError {
	field := MetaPath
	for _, member := range members {
		field += "." + member
	}

	return &ParamError{Field: field}
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

// Offer is the host's `initialize` offer. It carries exactly one member, so a later
// version adds its own members inside its own version's shape rather than breaking
// a version-1 sibling.
type Offer struct {
	Versions []int
}

// DecodeOffer reads the offer from `InitializeRequest._meta`. An absent offer is
// reported as not present rather than as a refusal: the host asked for nothing, and
// the answer, every envelope, and every correlation read are then omitted for the
// whole connection.
func DecodeOffer(meta map[string]any) (Offer, bool, *ParamError) {
	raw, present := meta[MetaKey]
	if !present {
		return Offer{}, false, nil
	}

	fields, ok := raw.(map[string]any)
	if !ok {
		return Offer{}, false, paramError()
	}

	for key := range fields {
		if key != fieldVersions {
			return Offer{}, false, paramError(key)
		}
	}

	versions, refusal := decodeVersions(fields[fieldVersions])
	if refusal != nil {
		return Offer{}, false, refusal
	}

	return Offer{Versions: versions}, true, nil
}

// Answer intersects the offer with the versions this adapter implements and carries
// the facts the active configuration proved. The intersection is ascending by
// construction, because this adapter implements one version. An empty intersection
// returns no answer at all: the key is omitted rather than answered with an empty
// array.
func (o Offer) Answer(proven Negotiated) (Negotiated, bool) {
	common := make([]int, 0, len(o.Versions))

	for _, version := range o.Versions {
		if version == Version && !slices.Contains(common, version) {
			common = append(common, version)
		}
	}

	if len(common) == 0 {
		return Negotiated{}, false
	}

	proven.Versions = common

	return proven, true
}

// decodeVersions reads the non-empty integer array every negotiation object
// carries. It is validated on every offer whatever the version. Only the answer is
// ordered: a host is free to offer its versions in any order, and refusing an
// unordered offer would break the forward compatibility the array exists for.
func decodeVersions(raw any) ([]int, *ParamError) {
	listed, ok := raw.([]any)
	if !ok || len(listed) == 0 {
		return nil, paramError(fieldVersions)
	}

	versions := make([]int, 0, len(listed))

	for _, entry := range listed {
		version, ok := integerValue(entry)
		if !ok {
			return nil, paramError(fieldVersions)
		}

		versions = append(versions, version)
	}

	return versions, nil
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
		return Submission{}, paramError()
	}

	fields, ok := raw.(map[string]any)
	if !ok {
		return Submission{}, paramError()
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

// integerValue reads one JSON integer. A decoded wire value arrives as a float64 and
// an embedding Go host writes an int, so both are the same integer; a fractional
// value is neither.
func integerValue(raw any) (int, bool) {
	switch value := raw.(type) {
	case float64:
		return int(value), value == math.Trunc(value)
	case int:
		return value, true
	case json.Number:
		number, err := value.Int64()

		return int(number), err == nil
	default:
		return 0, false
	}
}

func decodeSubmission(raw any) (Submission, *ParamError) {
	fields, ok := raw.(map[string]any)
	if !ok {
		return Submission{}, paramError(fieldSubmission)
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
	if !ok || value == "" || len(value) > IdentifierBound {
		return "", paramError(fieldSubmission, key)
	}

	return value, nil
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
