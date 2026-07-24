package codexacp

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

func TestValidatePromptImagesContract(t *testing.T) {
	fixture := func(name string) []byte {
		t.Helper()

		data, err := os.ReadFile(filepath.Join("testdata", name))
		require.NoError(t, err)

		return data
	}
	block := func(name string, mimeType string) acp.ContentBlock {
		return acp.ImageBlock(base64.StdEncoding.EncodeToString(fixture(name)), mimeType)
	}

	valid := []acp.ContentBlock{
		block("valid.png", "image/png"),
		block("valid.jpg", "image/jpeg"),
		block("valid.gif", "image/gif"),
		block("valid.webp", "image/webp"),
	}
	images, err := validatePromptImages(valid, ImageLimits{})
	require.Nil(t, err)
	require.Len(t, images, 4)

	resourceMIME := "image/png"
	resource := acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{
		Uri:      "blob://fixture",
		MimeType: &resourceMIME,
		Blob:     base64.StdEncoding.EncodeToString(fixture("valid.png")),
	}})
	images, err = validatePromptImages([]acp.ContentBlock{acp.TextBlock("text"), resource}, ImageLimits{})
	require.Nil(t, err)
	require.Len(t, images, 1)

	pngSize := int64(len(fixture("valid.png")))
	images, err = validatePromptImages([]acp.ContentBlock{block("valid.png", "image/png")}, ImageLimits{MaxInputBytesPerImage: pngSize})
	require.Nil(t, err)
	require.Len(t, images, 1)
	images, err = validatePromptImages(
		[]acp.ContentBlock{block("valid.png", "image/png"), block("valid.png", "image/png")},
		ImageLimits{MaxInputBytesPerPrompt: pngSize * 2},
	)
	require.Nil(t, err)
	require.Len(t, images, 2)

	cases := []struct {
		name      string
		blocks    []acp.ContentBlock
		limits    ImageLimits
		code      string
		sizeBytes int64
		maxBytes  int64
	}{
		{name: "missing data", blocks: []acp.ContentBlock{acp.ImageBlock("", "image/png")}, code: imageErrorMissingData},
		{name: "invalid media type", blocks: []acp.ContentBlock{acp.ImageBlock("eA==", "image/jpg")}, code: imageErrorInvalidMediaType},
		{name: "non-canonical case", blocks: []acp.ContentBlock{acp.ImageBlock("eA==", "IMAGE/PNG")}, code: imageErrorInvalidMediaType},
		{name: "invalid base64", blocks: []acp.ContentBlock{acp.ImageBlock("!", "image/png")}, code: imageErrorInvalidBase64},
		{name: "per image limit", blocks: []acp.ContentBlock{block("valid.png", "image/png")}, limits: ImageLimits{MaxInputBytesPerImage: 1}, code: imageErrorTooLarge, sizeBytes: pngSize, maxBytes: 1},
		{
			name:      "prompt aggregate",
			blocks:    []acp.ContentBlock{block("valid.png", "image/png"), block("valid.png", "image/png")},
			limits:    ImageLimits{MaxInputBytesPerPrompt: pngSize*2 - 1},
			code:      imageErrorTooLarge,
			sizeBytes: pngSize * 2,
			maxBytes:  pngSize*2 - 1,
		},
		{name: "invalid dimensions", blocks: []acp.ContentBlock{block("truncated.png", "image/png")}, code: imageErrorInvalidDimensions},
		{name: "unrecognized magic", blocks: []acp.ContentBlock{acp.ImageBlock(base64.StdEncoding.EncodeToString([]byte("not a raster at all")), "image/png")}, code: imageErrorMediaTypeMismatch},
		{name: "mime mismatch", blocks: []acp.ContentBlock{block("mismatch.png", "image/png")}, code: imageErrorMediaTypeMismatch},
		{name: "animated gif", blocks: []acp.ContentBlock{block("animated.gif", "image/gif")}, code: imageErrorAnimatedUnsupported},
		{name: "animated webp", blocks: []acp.ContentBlock{block("animated.webp", "image/webp")}, code: imageErrorAnimatedUnsupported},
		{name: "animated png", blocks: []acp.ContentBlock{block("animated-apng.png", "image/png")}, code: imageErrorAnimatedUnsupported},
		{name: "actl marker", blocks: []acp.ContentBlock{block("single-frame-actl.png", "image/png")}, code: imageErrorAnimatedUnsupported},
		{
			name:   "structural gate precedes per-image limit",
			blocks: []acp.ContentBlock{block("animated.gif", "image/gif")},
			limits: ImageLimits{MaxInputBytesPerImage: 1},
			code:   imageErrorAnimatedUnsupported,
		},
		{
			name:   "mismatch gate precedes per-image limit",
			blocks: []acp.ContentBlock{block("mismatch.png", "image/png")},
			limits: ImageLimits{MaxInputBytesPerImage: 1},
			code:   imageErrorMediaTypeMismatch,
		},
		{
			name:   "invalid dimensions gate precedes per-prompt limit",
			blocks: []acp.ContentBlock{block("truncated.png", "image/png")},
			limits: ImageLimits{MaxInputBytesPerPrompt: 1},
			code:   imageErrorInvalidDimensions,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := validatePromptImages(testCase.blocks, testCase.limits)
			var imageErr *promptImageError
			require.ErrorAs(t, err, &imageErr)
			require.Equal(t, testCase.code, imageErr.code)
			require.Contains(t, imageErr.Error(), testCase.code)

			if testCase.code == imageErrorTooLarge {
				require.Equal(t, testCase.sizeBytes, imageErr.sizeBytes)
				require.Equal(t, testCase.maxBytes, imageErr.maxBytes)
			}

			var requestErr *acp.RequestError
			require.ErrorAs(t, imageErr.invalidParams(), &requestErr)
		})
	}
}

func TestPromptImageBlockVariants(t *testing.T) {
	mimeType := "image/png"
	textMIME := "text/plain"

	data, mediaType, ok := promptImageBlock(acp.ImageBlock("data", mimeType))
	require.True(t, ok)
	require.Equal(t, "data", data)
	require.Equal(t, mimeType, mediaType)

	_, _, ok = promptImageBlock(acp.TextBlock("text"))
	require.False(t, ok)
	_, _, ok = promptImageBlock(acp.ResourceBlock(acp.EmbeddedResourceResource{TextResourceContents: &acp.TextResourceContents{Uri: "file:///a", Text: "text"}}))
	require.False(t, ok)
	_, _, ok = promptImageBlock(acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{Uri: "blob://a"}}))
	require.False(t, ok)
	_, _, ok = promptImageBlock(acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{Uri: "blob://a", MimeType: &textMIME}}))
	require.False(t, ok)

	upperMIME := "IMAGE/PNG"
	_, mediaType, ok = promptImageBlock(acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{Uri: "blob://a", MimeType: &upperMIME, Blob: "eA=="}}))
	require.True(t, ok)
	require.Equal(t, upperMIME, mediaType)
}

func TestPreparePromptImagesTransportsAndFailures(t *testing.T) {
	ctx := context.Background()
	scratch := t.TempDir()
	imageSession := &session{agent: NewAgent(WithScratchDir(scratch))}

	prepared, err := imageSession.preparePromptImages(ctx, nil)
	require.NoError(t, err)
	prepared.release()

	prepared, err = imageSession.preparePromptImages(ctx, []decodedPromptImage{{data: []byte("small"), mimeType: "image/png"}})
	require.NoError(t, err)
	require.Contains(t, prepared.images[0].DataURL, "data:image/png;base64,")
	prepared.release()

	large := make([]byte, codexInlineImageEnvelopeSize)
	prepared, err = imageSession.preparePromptImages(ctx, []decodedPromptImage{{data: large, mimeType: "image/jpeg", index: 1}})
	require.NoError(t, err)
	require.FileExists(t, prepared.images[0].LocalPath)
	info, err := os.Stat(prepared.images[0].LocalPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	dir := filepath.Dir(prepared.images[0].LocalPath)
	prepared.release()
	require.NoDirExists(t, dir)

	prepared, err = imageSession.preparePromptImages(ctx, []decodedPromptImage{
		{data: []byte("small"), mimeType: "image/png"},
		{data: large, mimeType: "image/png", index: 1},
	})
	require.NoError(t, err)
	require.NotEmpty(t, prepared.images[0].DataURL)
	require.NotEmpty(t, prepared.images[1].LocalPath)
	prepared.release()

	reservationErr := errors.New("reserve")
	reservationSession := &session{agent: NewAgent(WithRuntimeResourceHooks(RuntimeResourceHooks{
		ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return nil, reservationErr
		},
	}))}
	_, err = reservationSession.preparePromptImages(ctx, []decodedPromptImage{{data: large, mimeType: "image/png"}})
	require.ErrorIs(t, err, reservationErr)

	blocked := filepath.Join(t.TempDir(), "blocked")
	require.NoError(t, os.WriteFile(blocked, []byte("x"), 0o600))
	_, err = (&session{agent: NewAgent(WithScratchDir(blocked))}).preparePromptImages(
		ctx,
		[]decodedPromptImage{{data: large, mimeType: "image/png"}},
	)
	require.Error(t, err)

	originalWrite := writePromptImageFile
	writePromptImageFile = func(string, []byte, os.FileMode) error { return errors.New("write") }
	_, err = imageSession.preparePromptImages(ctx, []decodedPromptImage{{data: large, mimeType: "image/webp"}})
	require.Error(t, err)
	writePromptImageFile = originalWrite

	originalRemove := removePromptImageDir
	removePromptImageDir = func(string) error { return errors.New("remove") }
	t.Cleanup(func() { removePromptImageDir = originalRemove })
	released := false
	removeSession := &session{agent: NewAgent(
		WithScratchDir(scratch),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
				return func() { released = true }, nil
			},
		}),
	)}
	prepared, err = removeSession.preparePromptImages(ctx, []decodedPromptImage{{data: large, mimeType: "image/gif"}})
	require.NoError(t, err)
	prepared.release()
	require.False(t, released)
}

func TestPortableImageExtensions(t *testing.T) {
	require.Equal(t, ".png", portableImageExtension("image/png"))
	require.Equal(t, ".jpg", portableImageExtension("image/jpeg"))
	require.Equal(t, ".gif", portableImageExtension("image/gif"))
	require.Equal(t, ".webp", portableImageExtension("image/webp"))
	require.Empty(t, portableImageExtension("image/bmp"))
}

func TestSelectedModelImageSupport(t *testing.T) {
	models := []codex.Model{
		{ID: "unknown"},
		{ID: "empty", InputModalities: []string{}},
		{ID: "text", InputModalities: []string{" text "}},
		{ID: "vision", InputModalities: []string{" text ", "IMAGE"}},
	}

	require.Equal(t, imageInputUnknown, selectedModelImageSupport(models, "missing"))
	require.Equal(t, imageInputUnknown, selectedModelImageSupport(models, "unknown"))
	require.Equal(t, imageInputUnknown, selectedModelImageSupport(models, "empty"))
	require.Equal(t, imageInputUnsupported, selectedModelImageSupport(models, "text"))
	require.Equal(t, imageInputSupported, selectedModelImageSupport(models, "vision"))
}

func TestRasterInspectorMalformedStructures(t *testing.T) {
	fixture := func(name string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("testdata", name))
		require.NoError(t, err)

		return data
	}

	_, err := inspectPromptRaster([]byte("not a raster"))
	require.ErrorIs(t, err, errUnknownRasterFormat)

	pngZero := append([]byte(nil), fixture("valid.png")...)
	for index := 16; index < 24; index++ {
		pngZero[index] = 0
	}
	_, err = inspectPNG(pngZero)
	require.Error(t, err)
	_, err = inspectPNG([]byte("\x89PNG\r\n\x1a\n"))
	require.Error(t, err)
	pngMalformedChunk := append([]byte(nil), fixture("valid.png")[:33]...)
	pngMalformedChunk = append(pngMalformedChunk, 0xff, 0xff, 0xff, 0xff, 'J', 'U', 'N', 'K', 0, 0, 0, 0)
	_, err = inspectPNG(pngMalformedChunk)
	require.NoError(t, err)

	for _, data := range [][]byte{
		{0xff, 0xd8, 0x00, 0xff, 0xd9},
		{0xff, 0xd8, 0xff},
		{0xff, 0xd8, 0xff, 0xff},
		{0xff, 0xd8, 0xff, 0xd9, 0xff, 0xc0},
		{0xff, 0xd8, 0xff, 0xc0},
		{0xff, 0xd8, 0xff, 0xc0, 0, 1},
		{0xff, 0xd8, 0xff, 0xc0, 0, 7, 8, 0, 0, 0, 0},
	} {
		_, err = inspectJPEG(data)
		require.Error(t, err)
	}

	gifHeader := []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00")
	_, err = inspectGIF(gifHeader[:10])
	require.Error(t, err)
	gifZero := append([]byte(nil), gifHeader...)
	gifZero[6], gifZero[7] = 0, 0
	_, err = inspectGIF(gifZero)
	require.Error(t, err)
	for _, suffix := range [][]byte{
		{0x2c},
		{0x2c, 0, 0, 0, 0, 1, 0, 1, 0, 0x80},
		{0x21},
		{0x00},
		nil,
	} {
		_, err = inspectGIF(append(append([]byte(nil), gifHeader...), suffix...))
		require.NoError(t, err)
	}
	require.Equal(t, 4, skipGIFSubBlocks([]byte{2, 1, 2, 0}, 0))
	require.Equal(t, 2, skipGIFSubBlocks([]byte{3, 1}, 0))

	webP := func(chunk string, payload []byte) []byte {
		data := append([]byte("RIFF\x00\x00\x00\x00WEBP"), []byte(chunk)...)
		size := uint32(len(payload))
		data = append(data, byte(size), byte(size>>8), byte(size>>16), byte(size>>24))
		data = append(data, payload...)

		return data
	}
	_, err = inspectWebP(webP("VP8X", []byte{0}))
	require.Error(t, err)
	lossless, err := inspectWebP(webP("VP8L", []byte{0x2f, 0, 0, 0, 0}))
	require.NoError(t, err)
	require.Equal(t, 1, lossless.width)
	_, err = inspectWebP(webP("JUNK", []byte{0}))
	require.Error(t, err)
	truncatedChunk := webP("JUNK", []byte{0})
	truncatedChunk[16] = 10
	_, err = inspectWebP(truncatedChunk)
	require.Error(t, err)
}

func TestImageLimitsOptionAndValidation(t *testing.T) {
	limits := ImageLimits{MaxInputBytesPerImage: 1, MaxInputBytesPerPrompt: 2, MaxOutputBytesPerImage: 3, MaxOutputBytesPerToolCall: 4}
	options := applyOptions([]Option{WithImageLimits(limits)})
	require.Equal(t, limits, options.ImageLimits)

	for _, invalid := range []ImageLimits{
		{MaxInputBytesPerImage: -1},
		{MaxInputBytesPerPrompt: -1},
		{MaxOutputBytesPerImage: -1},
		{MaxOutputBytesPerToolCall: -1},
	} {
		require.Error(t, validateImageLimits(invalid))
	}
	require.NoError(t, validateImageLimits(ImageLimits{}))
}
