package codexacp

import (
	"encoding/base64"
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
	require.NotContains(t, meta[codexMetaKey], "mediaEnvelope")
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()

	raw, err := json.Marshal(value)
	require.NoError(t, err)

	return raw
}

func TestMediaEnvelopeAdvertisesTheGatesActuallyEnforced(t *testing.T) {
	// The advertised formats are exactly the allowlist the gate reads, so the
	// two cannot drift apart.
	advertised := slices.Clone(mediaEnvelopeImageFormats)
	slices.Sort(advertised)

	allowed := make([]string, 0, len(portableImageMediaTypes))
	for mimeType := range portableImageMediaTypes {
		allowed = append(allowed, mimeType)
	}

	slices.Sort(allowed)
	require.Equal(t, allowed, advertised)

	png := testdataFixture(t, "valid.png")
	block := acp.ImageBlock(base64.StdEncoding.EncodeToString(png), mimeImagePNG)

	// The advertised byte bounds are the bounds the gate enforces: one byte
	// under passes, the advertised value itself passes, one over is rejected
	// with the advertised value as maxBytes.
	limits := ImageLimits{MaxInputBytesPerImage: int64(len(png)), MaxInputBytesPerPrompt: int64(len(png))}
	envelope := mediaEnvelope(limits)
	require.Equal(t, limits.MaxInputBytesPerImage, envelope[mediaEnvelopeMaxBytesKey])
	require.Equal(t, limits.MaxInputBytesPerPrompt, envelope[mediaEnvelopeMaxPromptBytesKey])

	_, imageErr := validatePromptImages([]acp.ContentBlock{block}, limits, "")
	require.Nil(t, imageErr)

	tight := ImageLimits{MaxInputBytesPerImage: int64(len(png)) - 1}
	_, imageErr = validatePromptImages([]acp.ContentBlock{block}, tight, "")
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorTooLarge, imageErr.code)
	require.Equal(t, mediaEnvelope(tight)[mediaEnvelopeMaxBytesKey], imageErr.maxBytes)

	// A configured envelope is advertised in place of the default, including a
	// disabled limit, which advertises 0 for "no adapter policy bound".
	configured := initializeCapabilityMeta(t, WithImageLimits(ImageLimits{MaxInputBytesPerImage: 1024}))
	advertisedEnvelope, ok := configured[mediaEnvelopeMetaKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(1024), advertisedEnvelope[mediaEnvelopeMaxBytesKey])
	require.Equal(t, float64(0), advertisedEnvelope[mediaEnvelopeMaxPromptBytesKey])

	// The advertised format list is a copy, so a host-side mutation of one
	// response cannot rewrite what the next one advertises.
	first, _ := mediaEnvelope(limits)[mediaEnvelopeImageFormatsKey].([]string)
	require.NotEmpty(t, first)
	first[0] = "image/mutated"

	second, _ := mediaEnvelope(limits)[mediaEnvelopeImageFormatsKey].([]string)
	require.Equal(t, mimeImagePNG, second[0])
}

func TestInitializeAdvertisesHandoffOnlyWhenRootIsSet(t *testing.T) {
	// Absence is the signal a host checks to learn its option never arrived, so
	// it is asserted both ways.
	require.NotContains(t, initializeCapabilityMeta(t), handoffMetaKey)

	meta := initializeCapabilityMeta(t, WithInputHandoffRoot(t.TempDir()))
	require.Equal(t, map[string]any{handoffVersionsKey: []any{float64(handoffVersion)}}, meta[handoffMetaKey])

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

	images, imageErr := validatePromptImages([]acp.ContentBlock{block}, defaultImageLimits(), root)
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
