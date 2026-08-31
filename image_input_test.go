package codexacp

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	images, err, _ := validatePromptImages(t.Context(), valid, ImageLimits{}, "")
	require.Nil(t, err)
	require.Len(t, images, 4)

	resourceMIME := "image/png"
	resource := acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{
		Uri:      "blob://fixture",
		MimeType: &resourceMIME,
		Blob:     base64.StdEncoding.EncodeToString(fixture("valid.png")),
	}})
	images, err, _ = validatePromptImages(t.Context(), []acp.ContentBlock{acp.TextBlock("text"), resource}, ImageLimits{}, "")
	require.Nil(t, err)
	require.Len(t, images, 1)

	pngSize := int64(len(fixture("valid.png")))
	images, err, _ = validatePromptImages(t.Context(), []acp.ContentBlock{block("valid.png", "image/png")}, ImageLimits{MaxInputBytesPerImage: pngSize}, "")
	require.Nil(t, err)
	require.Len(t, images, 1)
	images, err, _ = validatePromptImages(
		t.Context(),
		[]acp.ContentBlock{block("valid.png", "image/png"), block("valid.png", "image/png")},
		ImageLimits{MaxInputBytesPerPrompt: pngSize * 2},
		"",
	)
	require.Nil(t, err)
	require.Len(t, images, 2)

	cases := []struct {
		name      string
		blocks    []acp.ContentBlock
		limits    ImageLimits
		code      string
		field     string
		sizeBytes int64
		maxBytes  int64
	}{
		{name: "missing data", blocks: []acp.ContentBlock{acp.ImageBlock("", "image/png")}, code: imageErrorMissingData, field: promptImageField},
		{name: "invalid media type", blocks: []acp.ContentBlock{acp.ImageBlock("eA==", "image/jpg")}, code: imageErrorInvalidMediaType, field: promptImageField},
		{name: "non-canonical case", blocks: []acp.ContentBlock{acp.ImageBlock("eA==", "IMAGE/PNG")}, code: imageErrorInvalidMediaType, field: promptImageField},
		{name: "invalid base64", blocks: []acp.ContentBlock{acp.ImageBlock("!", "image/png")}, code: imageErrorInvalidBase64, field: promptImageField},
		{name: "per image limit", blocks: []acp.ContentBlock{block("valid.png", "image/png")}, limits: ImageLimits{MaxInputBytesPerImage: 1}, code: imageErrorTooLarge, field: promptImageField, sizeBytes: pngSize, maxBytes: 1},
		{
			name:      "prompt aggregate",
			blocks:    []acp.ContentBlock{block("valid.png", "image/png"), block("valid.png", "image/png")},
			limits:    ImageLimits{MaxInputBytesPerPrompt: pngSize*2 - 1},
			code:      imageErrorTooLarge,
			field:     promptImageField,
			sizeBytes: pngSize * 2,
			maxBytes:  pngSize*2 - 1,
		},
		{name: "invalid dimensions", blocks: []acp.ContentBlock{block("truncated.png", "image/png")}, code: imageErrorInvalidDimensions, field: promptImageField},
		{name: "unrecognized magic", blocks: []acp.ContentBlock{acp.ImageBlock(base64.StdEncoding.EncodeToString([]byte("not a raster at all")), "image/png")}, code: imageErrorMediaTypeMismatch, field: promptImageField},
		{name: "mime mismatch", blocks: []acp.ContentBlock{block("mismatch.png", "image/png")}, code: imageErrorMediaTypeMismatch, field: promptImageField},
		{name: "animated gif", blocks: []acp.ContentBlock{block("animated.gif", "image/gif")}, code: imageErrorAnimatedUnsupported, field: promptImageField},
		{name: "animated webp", blocks: []acp.ContentBlock{block("animated.webp", "image/webp")}, code: imageErrorAnimatedUnsupported, field: promptImageField},
		{name: "animated png", blocks: []acp.ContentBlock{block("animated-apng.png", "image/png")}, code: imageErrorAnimatedUnsupported, field: promptImageField},
		{name: "actl marker", blocks: []acp.ContentBlock{block("single-frame-actl.png", "image/png")}, code: imageErrorAnimatedUnsupported, field: promptImageField},
		{
			name:   "structural gate precedes per-image limit",
			blocks: []acp.ContentBlock{block("animated.gif", "image/gif")},
			limits: ImageLimits{MaxInputBytesPerImage: 1},
			code:   imageErrorAnimatedUnsupported,
			field:  promptImageField,
		},
		{
			name:   "mismatch gate precedes per-image limit",
			blocks: []acp.ContentBlock{block("mismatch.png", "image/png")},
			limits: ImageLimits{MaxInputBytesPerImage: 1},
			code:   imageErrorMediaTypeMismatch,
			field:  promptImageField,
		},
		{
			name:   "invalid dimensions gate precedes per-prompt limit",
			blocks: []acp.ContentBlock{block("truncated.png", "image/png")},
			limits: ImageLimits{MaxInputBytesPerPrompt: 1},
			code:   imageErrorInvalidDimensions,
			field:  promptImageField,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, imageErr, err := validatePromptImages(t.Context(), testCase.blocks, testCase.limits, "")
			require.NoError(t, err)
			require.NotNil(t, imageErr)
			require.Equal(t, testCase.code, imageErr.code)
			require.Equal(t, testCase.field, imageErr.field)

			if testCase.code == imageErrorTooLarge {
				require.Equal(t, testCase.sizeBytes, imageErr.sizeBytes)
				require.Equal(t, testCase.maxBytes, imageErr.maxBytes)
			}

			var requestErr *acp.RequestError
			require.ErrorAs(t, imageErr.invalidParams(), &requestErr)
		})
	}
}

func TestPromptMediaBlockVariants(t *testing.T) {
	blobBlock := func(mimeType *string, blob string) acp.ContentBlock {
		return acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{
			Uri:      "blob://a",
			MimeType: mimeType,
			Blob:     blob,
		}})
	}
	pointer := func(value string) *string { return &value }

	uri := "file:///root/a.png"
	meta := map[string]any{handoffMetaKey: map[string]any{}}
	image := acp.ImageBlock("data", mimeImagePNG)
	image.Image.Uri = &uri
	image.Image.Meta = meta

	media, ok := promptMediaBlock(image)
	require.True(t, ok)
	require.Equal(t, promptMediaImageBlock, media.kind)
	require.Equal(t, "data", media.data)
	require.Equal(t, mimeImagePNG, media.mimeType)
	require.Equal(t, uri, media.uri)
	require.Equal(t, meta, media.meta)

	media, ok = promptMediaBlock(acp.ImageBlock("data", mimeImagePNG))
	require.True(t, ok)
	require.Empty(t, media.uri)

	_, ok = promptMediaBlock(acp.TextBlock("text"))
	require.False(t, ok)
	// A text resource carries bytes too. Codex flattens them into prompt text,
	// so they reach no image gate, but they are still a block the prompt budget
	// has to account for.
	media, ok = promptMediaBlock(acp.ResourceBlock(acp.EmbeddedResourceResource{
		TextResourceContents: &acp.TextResourceContents{Uri: "file:///a", Text: "text"},
	}))
	require.True(t, ok)
	require.Equal(t, promptMediaTextResource, media.kind)
	require.Equal(t, "text", media.data)
	require.False(t, media.kind.nativeImage())
	require.False(t, media.kind.perImageBounded())
	require.Equal(t, promptResourceField, media.kind.inputField())

	// A resource carrying neither form has nothing to gate.
	_, ok = promptMediaBlock(acp.ResourceBlock(acp.EmbeddedResourceResource{}))
	require.False(t, ok)

	// A blob resource is always gated, whatever it declares — an absent or
	// non-image media type routes it to the opaque channel rather than out of
	// validation entirely.
	for _, declared := range []*string{nil, pointer("text/plain"), pointer("application/pdf")} {
		media, ok = promptMediaBlock(blobBlock(declared, "eA=="))
		require.True(t, ok)
		require.Equal(t, promptMediaOpaqueBlob, media.kind)
		require.Equal(t, "eA==", media.data)
	}

	// Routing normalizes casing and parameters; the declared value the
	// allowlist reads is preserved verbatim, so a non-canonical declaration
	// still reaches the image path and is rejected there.
	for _, declared := range []string{"IMAGE/PNG", " image/png ", "image/png; charset=binary", "Image/PNG;q=1"} {
		media, ok = promptMediaBlock(blobBlock(pointer(declared), "eA=="))
		require.True(t, ok)
		require.Equal(t, promptMediaImageBlob, media.kind, declared)
		require.Equal(t, declared, media.mimeType)
	}

	require.True(t, promptMediaImageBlock.nativeImage())
	require.True(t, promptMediaImageBlob.nativeImage())
	require.False(t, promptMediaOpaqueBlob.nativeImage())
}

// blobResourceBlock builds an embedded blob resource declaring mimeType.
func blobResourceBlock(uri string, mimeType string, blob string) acp.ContentBlock {
	return acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{
		Uri:      uri,
		MimeType: &mimeType,
		Blob:     blob,
	}})
}

func TestValidatePromptImagesGatesTheBlobChannelWhateverTheMIME(t *testing.T) {
	// A document blob larger than the per-image limit the same adapter enforces
	// for an image blob. The channel is gated by bytes, not by media type, so
	// this is a rejection rather than an unbounded forward.
	oversize := make([]byte, defaultImageLimitBytes+4495)
	require.Len(t, oversize, 6295951)

	_, imageErr, _ := validatePromptImages(
		t.Context(),
		[]acp.ContentBlock{blobResourceBlock("blob://report", "application/pdf", base64.StdEncoding.EncodeToString(oversize))},
		defaultImageLimits(),
		"",
	)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorTooLarge, imageErr.code)
	require.Equal(t, int64(len(oversize)), imageErr.sizeBytes)
	require.Equal(t, defaultImageLimitBytes, imageErr.maxBytes)
	require.Equal(t, 0, imageErr.index)

	// Corrupt base64 in a blob is rejected rather than forwarded.
	for _, mimeType := range []string{"application/pdf", "text/plain", "application/octet-stream"} {
		_, imageErr, _ = validatePromptImages(
			t.Context(),
			[]acp.ContentBlock{blobResourceBlock("blob://a", mimeType, "!")},
			defaultImageLimits(),
			"",
		)
		require.NotNil(t, imageErr, mimeType)
		require.Equal(t, imageErrorInvalidBase64, imageErr.code, mimeType)
	}

	// A blob's decoded bytes are charged to the per-prompt aggregate alongside
	// image bytes, and the rejection names the block that crossed the budget.
	png := testdataFixture(t, "valid.png")
	pdf := []byte("%PDF-1.7 document bytes")
	_, imageErr, _ = validatePromptImages(
		t.Context(),
		[]acp.ContentBlock{
			blobResourceBlock("blob://a", "application/pdf", base64.StdEncoding.EncodeToString(pdf)),
			acp.ImageBlock(base64.StdEncoding.EncodeToString(png), mimeImagePNG),
		},
		ImageLimits{MaxInputBytesPerPrompt: int64(len(png))},
		"",
	)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorTooLarge, imageErr.code)
	require.Equal(t, int64(len(pdf)+len(png)), imageErr.sizeBytes)
	require.Equal(t, 1, imageErr.index)

	// A conforming blob still passes, still reaches no native image transport,
	// and still maps to the reference line Codex has always sent for it.
	blocks := []acp.ContentBlock{blobResourceBlock("blob://a", "application/pdf", base64.StdEncoding.EncodeToString(pdf))}

	images, imageErr, _ := validatePromptImages(t.Context(), blocks, defaultImageLimits(), "")
	require.Nil(t, imageErr)
	require.Empty(t, images)

	input, err := promptToCodex(blocks, nil)
	require.NoError(t, err)
	require.Equal(t, "[resource: blob://a]", input[0][jsonFieldText])
}

func TestValidatePromptImagesNormalizesMediaTypeBeforeRouting(t *testing.T) {
	png := base64.StdEncoding.EncodeToString(testdataFixture(t, "valid.png"))

	// A raster declaration the allowlist does not accept is invalid_media_type
	// whatever its casing or parameters: normalization decides that the block
	// routes to the image path, and the image path rejects it there. It is never
	// silently forwarded down the generic resource path.
	for _, declared := range []string{
		"IMAGE/PNG",
		"Image/Png",
		"image/png; charset=binary",
		"IMAGE/PNG;q=1",
		" image/png ",
		"image/bmp",
		"IMAGE/BMP",
	} {
		_, imageErr, _ := validatePromptImages(
			t.Context(),
			[]acp.ContentBlock{blobResourceBlock("blob://a", declared, png)},
			defaultImageLimits(),
			"",
		)
		require.NotNil(t, imageErr, declared)
		require.Equal(t, imageErrorInvalidMediaType, imageErr.code, declared)
	}

	// The same normalization decides the count of image-bearing blocks on the
	// native mapping side, so a non-canonical declaration cannot desynchronize
	// validation from mapping.
	blocks := []acp.ContentBlock{blobResourceBlock("blob://a", "IMAGE/PNG", png)}
	require.True(t, codex.IsImageBearingBlock(blocks[0]))

	_, err := promptToCodex(blocks, nil)
	require.Error(t, err)
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
	removeSession := &session{agent: NewAgent(WithScratchDir(scratch))}
	prepared, err = removeSession.preparePromptImages(ctx, []decodedPromptImage{{data: large, mimeType: "image/gif"}})
	require.NoError(t, err)
	prepared.release()
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

// cancelAfterFirstCheck reports a live context the first time it is asked and a
// cancelled one afterwards. That is what a caller cancelling while a block is
// being admitted looks like from inside the validation loop, without a race
// deciding whether the test exercises it.
type cancelAfterFirstCheck struct {
	//nolint:containedctx // decorating the caller's context is what this is for.
	context.Context

	observed int
}

func (c *cancelAfterFirstCheck) Err() error {
	c.observed++
	if c.observed <= 1 {
		return nil
	}

	return context.Canceled
}

func TestValidatePromptImagesAbortsRatherThanVerdictingOnCancellation(t *testing.T) {
	root := t.TempDir()
	block := handoffFixture(t, root, "valid.png", syntheticPNG(t, 64))

	reads := countingImageReads(t)

	// Cancelled after the loop admitted the block and before the read reached
	// the filesystem: the caller gets its own error rather than a verdict
	// describing a block that was never actually judged.
	racing := &cancelAfterFirstCheck{Context: t.Context()}

	images, imageErr, err := validatePromptImages(racing, []acp.ContentBlock{block}, defaultImageLimits(), root)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, imageErr)
	require.Nil(t, images)
	require.Zero(t, *reads)

	// The same answer reaches the prompt path, so a cancelled turn never spends
	// the scratch reservation that materializing images would take.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	prompt := &session{agent: NewAgent(WithInputHandoffRoot(root))}

	input, release, err := prompt.preparePromptInput(cancelled, []acp.ContentBlock{block})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, input)
	require.Nil(t, release)
}

func TestValidatePromptImagesChargesTextResourcesToThePromptAggregate(t *testing.T) {
	text := func(body string) acp.ContentBlock {
		return acp.ResourceBlock(acp.EmbeddedResourceResource{
			TextResourceContents: &acp.TextResourceContents{Uri: "file:///notes.md", Text: body},
		})
	}

	body := strings.Repeat("a", 512)

	// Declaring bytes as text rather than as a blob does not buy a second
	// budget: the aggregate is the aggregate.
	_, imageErr, err := validatePromptImages(
		t.Context(),
		[]acp.ContentBlock{text(body), text(body)},
		ImageLimits{MaxInputBytesPerPrompt: int64(len(body))*2 - 1},
		"",
	)
	require.NoError(t, err)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorTooLarge, imageErr.code)
	require.Equal(t, int64(len(body))*2, imageErr.sizeBytes)
	require.Equal(t, promptResourceField, imageErr.field)
	require.Equal(t, 1, imageErr.index)

	// Text shares the aggregate with image bytes rather than running beside it.
	png := testdataFixture(t, "valid.png")
	_, imageErr, _ = validatePromptImages(
		t.Context(),
		[]acp.ContentBlock{
			text(body),
			acp.ImageBlock(base64.StdEncoding.EncodeToString(png), mimeImagePNG),
		},
		ImageLimits{MaxInputBytesPerPrompt: int64(len(body)+len(png)) - 1},
		"",
	)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorTooLarge, imageErr.code)
	require.Equal(t, int64(len(body)+len(png)), imageErr.sizeBytes)
	require.Equal(t, 1, imageErr.index)
	require.Equal(t, promptImageField, imageErr.field)

	// A text resource reaches no image transport and no image byte limit: the
	// per-image bound is about images.
	images, imageErr, _ := validatePromptImages(
		t.Context(),
		[]acp.ContentBlock{text(strings.Repeat("b", int(maxACPImageDecodedBytes)+1))},
		ImageLimits{},
		"",
	)
	require.Nil(t, imageErr)
	require.Empty(t, images)
}

func TestPromptInputErrorsNameTheInboundBlockType(t *testing.T) {
	png := testdataFixture(t, "valid.png")

	// Routing is decided by the declared media type; the reported field is
	// decided by the block that arrived. An image-typed resource takes the
	// image gate chain and is still a resource.
	_, imageErr, err := validatePromptImages(
		t.Context(),
		[]acp.ContentBlock{blobResourceBlock("blob://a", "image/png", base64.StdEncoding.EncodeToString([]byte("not a raster")))},
		defaultImageLimits(),
		"",
	)
	require.NoError(t, err)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorMediaTypeMismatch, imageErr.code)
	require.Equal(t, promptResourceField, imageErr.field)

	requestErr := &acp.RequestError{}
	require.ErrorAs(t, imageErr.invalidParams(), &requestErr)

	data, ok := requestErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, promptResourceField, data[jsonFieldField])

	// A plain image block still reports the image field.
	_, imageErr, _ = validatePromptImages(
		t.Context(),
		[]acp.ContentBlock{acp.ImageBlock(base64.StdEncoding.EncodeToString([]byte("not a raster")), mimeImagePNG)},
		defaultImageLimits(),
		"",
	)
	require.NotNil(t, imageErr)
	require.Equal(t, promptImageField, imageErr.field)

	// A model that cannot take images names the block that offered one rather
	// than always naming the first block in the prompt.
	unsupported := &promptImageError{
		code:  imageErrorUnsupportedByModel,
		field: promptResourceField,
		index: 3,
	}

	unsupportedErr := &acp.RequestError{}
	require.ErrorAs(t, unsupported.invalidParams(), &unsupportedErr)

	unsupportedData, ok := unsupportedErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, 3, unsupportedData[jsonFieldIndex])
	require.Equal(t, promptResourceField, unsupportedData[jsonFieldField])

	require.NotEmpty(t, png)
}
