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

// mediaEnvelopeImageFormats is the inbound allowlist in advertisement order,
// built from the same media-type constants the allowlist itself is built from.
var mediaEnvelopeImageFormats = []string{mimeImagePNG, mimeImageJPEG, mimeImageGIF, mimeImageWebP}

// mediaEnvelope reports the bounds this adapter actually enforces on inbound
// prompt media, so a host can reject an oversize attachment before spending a
// turn on it. Every value is read from the same configuration the gates read,
// which is what keeps the advertisement from drifting away from the verdict.
//
// documentFormats is empty: Codex maps no media type to a native document
// representation, so no host should route a document to it as embedded bytes.
// A zero byte value means that adapter policy limit is disabled, matching
// ImageLimits, and 0 for maxDimension means no bound rather than no pixels.
func mediaEnvelope(limits ImageLimits) map[string]any {
	return map[string]any{
		mediaEnvelopeMaxBytesKey:        limits.MaxInputBytesPerImage,
		mediaEnvelopeMaxPromptBytesKey:  limits.MaxInputBytesPerPrompt,
		mediaEnvelopeMaxDimensionKey:    mediaEnvelopeMaxDimension,
		mediaEnvelopeImageFormatsKey:    slices.Clone(mediaEnvelopeImageFormats),
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
