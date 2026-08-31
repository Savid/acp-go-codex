package codexacp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func initializeCapabilityMeta(t *testing.T, opts ...Option) map[string]any {
	t.Helper()

	resp, err := NewAgent(opts...).Initialize(t.Context(), acp.InitializeRequest{})
	require.NoError(t, err)

	// Round-trip through JSON so the assertions see the shape a host receives
	// rather than the Go values behind it.
	raw, err := json.Marshal(resp.AgentCapabilities.Meta)
	require.NoError(t, err)

	var meta map[string]any
	require.NoError(t, json.Unmarshal(raw, &meta))

	return meta
}

func TestInitializeAdvertisesMediaEnvelope(t *testing.T) {
	meta := initializeCapabilityMeta(t)

	envelope, ok := meta[mediaEnvelopeMetaKey].(map[string]any)
	require.True(t, ok, "media envelope must be advertised unconditionally")

	// The exact shape: five fields, no more.
	require.Len(t, envelope, 5)
	require.Equal(t, float64(defaultImageLimitBytes), envelope[mediaEnvelopeMaxBytesKey])
	require.Equal(t, float64(defaultImageLimitBytes), envelope[mediaEnvelopeMaxPromptBytesKey])
	require.Equal(t, float64(0), envelope[mediaEnvelopeMaxDimensionKey])
	require.Equal(t, []any{"image/png", "image/jpeg", "image/gif", "image/webp"}, envelope[mediaEnvelopeImageFormatsKey])

	// documentFormats is an empty array, never null: Codex maps no media type
	// to a native document representation.
	require.Equal(t, []any{}, envelope[mediaEnvelopeDocumentFormatsKey])
	require.Contains(t, string(mustMarshal(t, envelope)), `"documentFormats":[]`)

	// The envelope sits beside the other family-reserved literal rather than
	// under the vendor key.
	require.Contains(t, meta, routeMetaKey)
	require.NotContains(t, meta[codexMetaKey], mediaEnvelopeMetaKey)
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()

	raw, err := json.Marshal(value)
	require.NoError(t, err)

	return raw
}

// TestMediaEnvelopeAdvertisesTheAllowlistTheGateReads drives the advertised
// formats through the media-type gate itself rather than comparing them with a
// second list. Two literal lists agree until someone edits one of them, and a
// host cannot see the difference from outside; what this pins is that every
// format the adapter advertises is one the gate admits, and every format it
// admits is one it advertises.
func TestMediaEnvelopeAdvertisesTheAllowlistTheGateReads(t *testing.T) {
	advertised, ok := mediaEnvelope(defaultImageLimits())[mediaEnvelopeImageFormatsKey].([]string)
	require.True(t, ok)
	require.NotEmpty(t, advertised)

	for _, mimeType := range advertised {
		require.Nil(t, checkPromptImageMediaType(mimeType, promptImageField, 0), mimeType)
	}

	// The other direction, over the portable media types this package declares:
	// a format the gate admits but never advertises is the same defect read the
	// other way round.
	for _, mimeType := range []string{mimeImagePNG, mimeImageJPEG, mimeImageGIF, mimeImageWebP} {
		require.True(t, slices.Contains(advertised, mimeType), mimeType)
	}

	// A media type outside the list is refused by that same gate, so the
	// advertisement describes the whole allowlist rather than a part of it.
	for _, mimeType := range []string{"image/avif", "image/svg+xml", "image/heic", "application/pdf", "IMAGE/PNG"} {
		refused := checkPromptImageMediaType(mimeType, promptImageField, 0)
		require.NotNil(t, refused, mimeType)
		require.Equal(t, imageErrorInvalidMediaType, refused.code)
		require.False(t, slices.Contains(advertised, mimeType), mimeType)
	}

	// The advertised list is a copy, so a host-side mutation of one response
	// rewrites neither the next advertisement nor what the gate accepts.
	advertised[0] = "image/mutated"

	second, _ := mediaEnvelope(defaultImageLimits())[mediaEnvelopeImageFormatsKey].([]string)
	require.Equal(t, mimeImagePNG, second[0])
	require.Nil(t, checkPromptImageMediaType(mimeImagePNG, promptImageField, 0))
}

func TestMediaEnvelopeAdvertisesTheBoundTheGateReports(t *testing.T) {
	root := t.TempDir()

	// Two file sizes: one that only a small configured limit refuses, and one
	// past the hard frame bound, which is what binds when the configured limit
	// is disabled or larger than the frame can carry.
	small := syntheticPNG(t, 512)
	smallBlock := handoffFixture(t, root, "small.png", small)

	overFrame := syntheticPNG(t, int(maxACPImageDecodedBytes)+1)
	overFrameBlock := handoffFixture(t, root, "over-frame.png", overFrame)

	perImage := []struct {
		name       string
		configured int64
		clamped    bool
		block      acp.ContentBlock
	}{
		{name: "below the clamp", configured: int64(len(small)) - 1, block: smallBlock},
		{name: "disabled", configured: 0, clamped: true, block: overFrameBlock},
		{name: "above the clamp", configured: maxACPImageDecodedBytes * 2, clamped: true, block: overFrameBlock},
	}
	for _, testCase := range perImage {
		t.Run("maxBytes "+testCase.name, func(t *testing.T) {
			limits := ImageLimits{MaxInputBytesPerImage: testCase.configured}

			claimed, ok := mediaEnvelope(limits)[mediaEnvelopeMaxBytesKey].(int64)
			require.True(t, ok)

			// Driven through the real gate: the number a rejection reports is
			// the number the advertisement promised, never the option field.
			_, imageErr, err := validatePromptImages(t.Context(), []acp.ContentBlock{testCase.block}, limits, root)
			require.NoError(t, err)
			require.NotNil(t, imageErr)
			require.Equal(t, imageErrorTooLarge, imageErr.code)
			require.Equal(t, claimed, imageErr.maxBytes)

			// Where the frame binds rather than the option, the advertisement
			// carries the bound and not the option it was configured with.
			if testCase.clamped {
				require.NotEqual(t, testCase.configured, claimed)
				require.Equal(t, maxACPImageDecodedBytes, claimed)
			}
		})
	}

	// A disabled per-image limit is still a bound, and a file past it is
	// too_large rather than anything the digest or the raster chain decides.
	_, imageErr, _ := validatePromptImages(t.Context(), []acp.ContentBlock{overFrameBlock}, ImageLimits{}, root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorTooLarge, imageErr.code)
	require.Equal(t, maxACPImageDecodedBytes, imageErr.maxBytes)

	// The aggregate is advertised from the same function its own gate reads,
	// and the two bounds are deliberately different numbers so an advertisement
	// copied from the per-image field cannot pass.
	perImageBound := int64(len(small))
	perPromptBound := perImageBound*2 - 1

	aggregate := ImageLimits{MaxInputBytesPerImage: perImageBound, MaxInputBytesPerPrompt: perPromptBound}

	claimedPrompt, ok := mediaEnvelope(aggregate)[mediaEnvelopeMaxPromptBytesKey].(int64)
	require.True(t, ok)
	require.NotEqual(t, mediaEnvelope(aggregate)[mediaEnvelopeMaxBytesKey], claimedPrompt)

	_, imageErr, err := validatePromptImages(
		t.Context(),
		[]acp.ContentBlock{smallBlock, smallBlock},
		aggregate,
		root,
	)
	require.NoError(t, err)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorTooLarge, imageErr.code)
	require.Equal(t, claimedPrompt, imageErr.maxBytes)
	require.Equal(t, perImageBound*2, imageErr.sizeBytes)

	// A disabled aggregate enforces no byte total at all, so zero is the
	// truthful advertisement rather than a sentinel standing in for a clamp.
	configured := initializeCapabilityMeta(t, WithImageLimits(ImageLimits{MaxInputBytesPerImage: 1024}))

	advertisedEnvelope, ok := configured[mediaEnvelopeMetaKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(1024), advertisedEnvelope[mediaEnvelopeMaxBytesKey])
	require.Equal(t, float64(0), advertisedEnvelope[mediaEnvelopeMaxPromptBytesKey])

	unbounded := make([]acp.ContentBlock, 0, 3)
	for range 3 {
		unbounded = append(unbounded, smallBlock)
	}

	images, imageErr, _ := validatePromptImages(t.Context(), unbounded, ImageLimits{}, root)
	require.Nil(t, imageErr)
	require.Len(t, images, 3)
}

func TestEffectiveInputByteBoundsAreWhatTheGatesEnforce(t *testing.T) {
	// The per-image bound is the tighter of the configured limit and the frame
	// the response has to fit in, and a disabled limit leaves the frame bound.
	require.Equal(t, maxACPImageDecodedBytes, effectiveInputBytesPerImage(0))
	require.Equal(t, maxACPImageDecodedBytes, effectiveInputBytesPerImage(-1))
	require.Equal(t, maxACPImageDecodedBytes, effectiveInputBytesPerImage(maxACPImageDecodedBytes+1))
	require.Equal(t, maxACPImageDecodedBytes, effectiveInputBytesPerImage(maxACPImageDecodedBytes))
	require.Equal(t, int64(1024), effectiveInputBytesPerImage(1024))

	// The aggregate has no frame to fall back on, because handoff bytes are
	// read from paths rather than carried in the request, so a disabled
	// aggregate really is unbounded and says so.
	require.Equal(t, int64(0), effectiveInputBytesPerPrompt(0))
	require.Equal(t, int64(1024), effectiveInputBytesPerPrompt(1024))
	require.Equal(t, maxACPImageDecodedBytes*4, effectiveInputBytesPerPrompt(maxACPImageDecodedBytes*4))
}

func TestInitializeAdvertisesHandoffOnlyWhenRootIsSet(t *testing.T) {
	// Absence is the signal a host checks to learn its option never arrived, so
	// it is asserted both ways.
	require.NotContains(t, initializeCapabilityMeta(t), handoffMetaKey)

	meta := initializeCapabilityMeta(t, WithInputHandoffRoot(t.TempDir()))
	require.Equal(t, map[string]any{handoffVersionKey: float64(handoffVersion)}, meta[handoffMetaKey])

	// The media envelope is advertised either way.
	require.Contains(t, meta, mediaEnvelopeMetaKey)
}

func TestWithInputHandoffRootOption(t *testing.T) {
	root := t.TempDir()
	require.Equal(t, root, applyOptions([]Option{WithInputHandoffRoot(root)}).InputHandoffRoot)
	require.Empty(t, applyOptions(nil).InputHandoffRoot)
}

func TestHandoffRootIsNeverWrittenTo(t *testing.T) {
	root := t.TempDir()
	png := testdataFixture(t, "valid.png")
	block := handoffFixture(t, root, "valid.png", png)

	before, err := os.ReadDir(root)
	require.NoError(t, err)

	images, imageErr, _ := validatePromptImages(t.Context(), []acp.ContentBlock{block}, defaultImageLimits(), root)
	require.Nil(t, imageErr)
	require.Len(t, images, 1)

	// Reading a handoff block creates, moves, and removes nothing: the file is
	// the host's to delete the moment the prompt returns.
	after, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, after, len(before))
	require.FileExists(t, filepath.Join(root, "valid.png"))

	stored, err := os.ReadFile(filepath.Join(root, "valid.png"))
	require.NoError(t, err)
	require.Equal(t, png, stored)
}
