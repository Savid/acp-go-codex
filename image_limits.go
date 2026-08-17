package codexacp

import "fmt"

// defaultImageLimitBytes is the default decoded-byte bound applied to every
// ImageLimits field: 6 MiB, chosen so a maximal embedded image stays inside
// the pinned ACP SDK's 10 MiB JSON-RPC frame with headroom for JSON overhead
// and surrounding text in both directions.
const defaultImageLimitBytes int64 = 6 * 1024 * 1024

// ImageLimits bounds decoded image bytes accepted from prompts and emitted as
// image output. All limits count decoded bytes, not base64 characters or
// enclosing JSON. An explicit zero disables that adapter policy limit; it
// never bypasses hard native, provider, memory, or host request limits.
// Negative values are rejected when the agent is constructed.
type ImageLimits struct {
	// MaxInputBytesPerImage bounds one decoded prompt image. Disabling it
	// leaves the hard frame bound in place rather than removing every bound,
	// and a value above that bound is clamped to it; the advertised
	// per-image maximum is whichever of the two binds.
	MaxInputBytesPerImage int64
	// MaxInputBytesPerPrompt bounds the decoded total across every image in
	// one prompt. Disabling it removes the aggregate byte bound outright,
	// which is what the advertised per-prompt maximum then reports; the
	// number of handoff-form blocks one prompt may read stays bounded either
	// way.
	MaxInputBytesPerPrompt int64
	// MaxOutputBytesPerImage bounds one decoded emitted image.
	MaxOutputBytesPerImage int64
	// MaxOutputBytesPerToolCall bounds the decoded image total across one
	// tool call's whole content array, the unit an ACP v1 tool-call content
	// update replaces wholesale.
	MaxOutputBytesPerToolCall int64
}

func defaultImageLimits() ImageLimits {
	return ImageLimits{
		MaxInputBytesPerImage:     defaultImageLimitBytes,
		MaxInputBytesPerPrompt:    defaultImageLimitBytes,
		MaxOutputBytesPerImage:    defaultImageLimitBytes,
		MaxOutputBytesPerToolCall: defaultImageLimitBytes,
	}
}

// WithImageLimits replaces the default image byte limits (6 MiB decoded per
// field). An explicit zero field disables that adapter policy limit; negative
// fields reject agent construction.
func WithImageLimits(limits ImageLimits) Option {
	return func(options *Options) {
		options.ImageLimits = limits
	}
}

// effectiveInputBytesPerImage is the per-image decoded byte bound the input
// gates actually enforce: the configured policy limit, clamped to the largest
// decoded image the pinned ACP frame can carry. Disabling the policy limit
// leaves the frame bound in place rather than removing every bound, because
// handoff bytes come from a path the adapter did not choose. Both the gate and
// the advertisement read this function, so neither can drift from the other.
func effectiveInputBytesPerImage(configured int64) int64 {
	if configured <= 0 || configured > maxACPImageDecodedBytes {
		return maxACPImageDecodedBytes
	}

	return configured
}

// effectiveInputBytesPerPrompt is the aggregate decoded byte bound the input
// gates enforce across one prompt. Zero is not a clamp waiting to be found:
// handoff bytes are read from paths rather than out of the request frame, so a
// disabled aggregate genuinely bounds no total, and that is what the
// advertisement must report. The cap on how many handoff blocks one prompt may
// read is what bounds the work instead, and it is a count rather than a byte
// number.
func effectiveInputBytesPerPrompt(configured int64) int64 {
	return configured
}

func validateImageLimits(limits ImageLimits) error {
	fields := []struct {
		name  string
		value int64
	}{
		{name: "MaxInputBytesPerImage", value: limits.MaxInputBytesPerImage},
		{name: "MaxInputBytesPerPrompt", value: limits.MaxInputBytesPerPrompt},
		{name: "MaxOutputBytesPerImage", value: limits.MaxOutputBytesPerImage},
		{name: "MaxOutputBytesPerToolCall", value: limits.MaxOutputBytesPerToolCall},
	}

	for _, field := range fields {
		if field.value < 0 {
			return fmt.Errorf("ImageLimits.%s must be non-negative", field.name)
		}
	}

	return nil
}
