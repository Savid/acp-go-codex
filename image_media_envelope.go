package codexacp

import "slices"

const (
	mediaEnvelopeMetaKey = "acp-go.dev/mediaEnvelope"

	mediaEnvelopeMaxBytesKey        = "maxBytes"
	mediaEnvelopeMaxPromptBytesKey  = "maxPromptBytes"
	mediaEnvelopeMaxDimensionKey    = "maxDimension"
	mediaEnvelopeImageFormatsKey    = "imageFormats"
	mediaEnvelopeDocumentFormatsKey = "documentFormats"

	// mediaEnvelopeMaxDimension is 0 — no bound. Codex reads a raster's
	// dimensions only to prove they exist and rejects no image for being large
	// in pixels, so there is no per-dimension limit to advertise.
	mediaEnvelopeMaxDimension = 0
)

// mediaEnvelope reports the bounds this adapter actually enforces on inbound
// prompt media, so a host can reject an oversize attachment before spending a
// turn on it. Both byte values come from the same effective-limit functions the
// gates call rather than from the configured fields, so an advertised number is
// always the number a rejection reports. imageFormats is a copy of the very
// allowlist the media-type gate consults, in that list's own order, so it stays
// deterministic and cannot drift from what the gate accepts.
//
// documentFormats is empty: Codex maps no media type to a native document
// representation, so no host should route a document to it as embedded bytes.
// A zero maxPromptBytes means no aggregate byte bound is enforced at all, and 0
// for maxDimension means no bound rather than no pixels.
func mediaEnvelope(limits ImageLimits) map[string]any {
	return map[string]any{
		mediaEnvelopeMaxBytesKey:        effectiveInputBytesPerImage(limits.MaxInputBytesPerImage),
		mediaEnvelopeMaxPromptBytesKey:  effectiveInputBytesPerPrompt(limits.MaxInputBytesPerPrompt),
		mediaEnvelopeMaxDimensionKey:    mediaEnvelopeMaxDimension,
		mediaEnvelopeImageFormatsKey:    slices.Clone(portableImageMediaTypes),
		mediaEnvelopeDocumentFormatsKey: []string{},
	}
}

// capabilityMeta builds the agent capability _meta block. The family-reserved
// media envelope is unconditional; the handoff advertisement appears only when a
// handoff root is configured, so its absence tells a host that its option never
// reached this adapter.
func (a *Agent) capabilityMeta(codexMeta map[string]any) map[string]any {
	meta := map[string]any{
		codexMetaKey:         codexMeta,
		routeMetaKey:         map[string]any{routeVersionsKey: []int{routeVersion}},
		mediaEnvelopeMetaKey: mediaEnvelope(a.options.ImageLimits),
	}
	if a.options.InputHandoffRoot != "" {
		meta[handoffMetaKey] = map[string]any{handoffVersionsKey: []int{handoffVersion}}
	}

	return meta
}
