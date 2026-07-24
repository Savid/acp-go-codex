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
	// MaxInputBytesPerImage bounds one decoded prompt image.
	MaxInputBytesPerImage int64
	// MaxInputBytesPerPrompt bounds the decoded total across every image in
	// one prompt.
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
