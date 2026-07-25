package codexacp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

// handoffFixture writes bytes into a handoff root and returns the block a host
// would send for them.
func handoffFixture(t *testing.T, root string, name string, data []byte) acp.ContentBlock {
	t.Helper()

	path := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, data, 0o600))

	return handoffBlock(path, mimeImagePNG, handoffEnvelopeFor(data))
}

// handoffEnvelopeFor is the envelope a truthful host sends for exactly these
// bytes.
func handoffEnvelopeFor(data []byte) map[string]any {
	sum := sha256.Sum256(data)

	return map[string]any{
		handoffVersionKey:   handoffVersion,
		handoffDigestKey:    hex.EncodeToString(sum[:]),
		handoffSizeBytesKey: len(data),
	}
}

// handoffBlock builds the wire form of a handoff image block: empty data, a file
// URI, and the envelope.
func handoffBlock(path string, mimeType string, envelope any) acp.ContentBlock {
	block := acp.ImageBlock("", mimeType)

	uri := "file://" + filepath.ToSlash(path)
	block.Image.Uri = &uri

	if envelope != nil {
		block.Image.Meta = map[string]any{handoffMetaKey: envelope}
	}

	return block
}

func testdataFixture(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)

	return data
}

// syntheticPNG builds exactly size bytes the raster inspector accepts as a
// non-animated PNG: the signature, a well-formed IHDR, then zero padding the
// chunk walk steps over. A byte-limit test needs bytes that stay structurally
// valid at an exact length, because a truncated header would let a structural
// gate produce the verdict the byte gate is supposed to produce.
func syntheticPNG(t *testing.T, size int) []byte {
	t.Helper()

	require.GreaterOrEqual(t, size, 33)

	data := make([]byte, size)
	copy(data, "\x89PNG\r\n\x1a\n")
	binary.BigEndian.PutUint32(data[8:12], 13)
	copy(data[12:16], "IHDR")
	binary.BigEndian.PutUint32(data[16:20], 1)
	binary.BigEndian.PutUint32(data[20:24], 1)

	raster, err := inspectPromptRaster(data)
	require.NoError(t, err)
	require.Equal(t, mimeImagePNG, raster.mimeType)
	require.False(t, raster.animated)

	return data
}

// countingImageReads replaces the byte reader with one that counts how often the
// read path actually reached a file, and returns the counter.
func countingImageReads(t *testing.T) *int {
	t.Helper()

	reads := 0
	original := readImageFile
	readImageFile = func(reader io.Reader) ([]byte, error) {
		reads++

		return original(reader)
	}

	t.Cleanup(func() { readImageFile = original })

	return &reads
}

func TestValidatePromptImagesHandoffFormMirrorsEmbedded(t *testing.T) {
	root := t.TempDir()
	png := testdataFixture(t, "valid.png")

	// The handoff form accepts exactly what the embedded form accepts, and the
	// bytes it yields are the file's bytes.
	handoff := handoffFixture(t, root, "nested/dir/valid.png", png)
	images, imageErr, err := validatePromptImages(t.Context(), []acp.ContentBlock{handoff}, defaultImageLimits(), root)
	require.NoError(t, err)
	require.Nil(t, imageErr)
	require.Len(t, images, 1)
	require.Equal(t, png, images[0].data)
	require.Equal(t, mimeImagePNG, images[0].mimeType)
	require.Equal(t, 0, images[0].index)
	require.Equal(t, promptImageField, images[0].field)

	// Handoff bytes are charged to the per-prompt aggregate exactly as embedded
	// bytes are: two copies of one file cross a budget sized for one.
	size := int64(len(png))
	_, imageErr, _ = validatePromptImages(
		t.Context(),
		[]acp.ContentBlock{handoff, handoff},
		ImageLimits{MaxInputBytesPerPrompt: size*2 - 1},
		root,
	)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorTooLarge, imageErr.code)
	require.Equal(t, 1, imageErr.index)
	require.Equal(t, size*2, imageErr.sizeBytes)

	// A mixed prompt indexes every gated block in request order.
	embedded := acp.ImageBlock(base64.StdEncoding.EncodeToString(png), mimeImagePNG)
	images, imageErr, _ = validatePromptImages(
		t.Context(),
		[]acp.ContentBlock{acp.TextBlock("text"), embedded, handoff},
		defaultImageLimits(),
		root,
	)
	require.Nil(t, imageErr)
	require.Len(t, images, 2)
	require.Equal(t, 0, images[0].index)
	require.Equal(t, 1, images[1].index)
	require.Equal(t, images[0].data, images[1].data)
}

func TestValidatePromptImagesHandoffRasterGatesMatchEmbedded(t *testing.T) {
	root := t.TempDir()

	// Every structural gate that claims an embedded block claims the identical
	// handoff block, because both run the same chain over the same bytes.
	cases := []struct {
		name     string
		fixture  string
		mimeType string
		code     string
	}{
		{name: "animated gif", fixture: "animated.gif", mimeType: mimeImageGIF, code: imageErrorAnimatedUnsupported},
		{name: "animated webp", fixture: "animated.webp", mimeType: mimeImageWebP, code: imageErrorAnimatedUnsupported},
		{name: "animated png", fixture: "animated-apng.png", mimeType: mimeImagePNG, code: imageErrorAnimatedUnsupported},
		{name: "declared mismatch", fixture: "mismatch.png", mimeType: mimeImagePNG, code: imageErrorMediaTypeMismatch},
		{name: "invalid dimensions", fixture: "truncated.png", mimeType: mimeImagePNG, code: imageErrorInvalidDimensions},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			data := testdataFixture(t, testCase.fixture)

			block := handoffFixture(t, root, testCase.name+"/fixture", data)
			block.Image.MimeType = testCase.mimeType

			_, imageErr, _ := validatePromptImages(t.Context(), []acp.ContentBlock{block}, defaultImageLimits(), root)
			require.NotNil(t, imageErr)
			require.Equal(t, testCase.code, imageErr.code)

			embedded := acp.ImageBlock(base64.StdEncoding.EncodeToString(data), testCase.mimeType)
			_, embeddedErr, _ := validatePromptImages(t.Context(), []acp.ContentBlock{embedded}, defaultImageLimits(), "")
			require.NotNil(t, embeddedErr)
			require.Equal(t, imageErr.code, embeddedErr.code)
		})
	}

	// The allowlist reads the declared media type on both forms.
	notARaster := handoffFixture(t, root, "opaque", []byte("not a raster at all"))
	notARaster.Image.MimeType = "image/jpg"
	_, imageErr, _ := validatePromptImages(t.Context(), []acp.ContentBlock{notARaster}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorInvalidMediaType, imageErr.code)

	notARaster.Image.MimeType = mimeImagePNG
	_, imageErr, _ = validatePromptImages(t.Context(), []acp.ContentBlock{notARaster}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorMediaTypeMismatch, imageErr.code)
}

func TestHandoffAllowlistIsCheckedBeforeTheFileIsRead(t *testing.T) {
	root := t.TempDir()
	block := handoffFixture(t, root, "valid.png", testdataFixture(t, "valid.png"))
	block.Image.MimeType = "image/heic"

	reads := countingImageReads(t)

	// A media type the adapter will never accept costs no read and no hash: the
	// allowlist runs after the location verdicts and before the bytes.
	_, imageErr, _ := validatePromptImages(t.Context(), []acp.ContentBlock{block}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorInvalidMediaType, imageErr.code)
	require.Zero(t, *reads)

	// A location defect still wins over the media type, so a broken deployment
	// is reported as one whatever the block declared.
	absent := handoffBlock(filepath.Join(root, "absent.png"), "image/heic", handoffEnvelopeFor([]byte("x")))
	_, imageErr, _ = validatePromptImages(t.Context(), []acp.ContentBlock{absent}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorMissingFile, imageErr.code)
}

func TestValidatePromptImagesHandoffFormSelection(t *testing.T) {
	root := t.TempDir()
	png := testdataFixture(t, "valid.png")
	encoded := base64.StdEncoding.EncodeToString(png)

	// Non-empty data wins over a URI and an envelope alike, so the embedded
	// form is unchanged by the pre-gate.
	dataWins := handoffFixture(t, root, "data-wins.png", []byte("ignored"))
	dataWins.Image.Data = encoded

	images, imageErr, _ := validatePromptImages(t.Context(), []acp.ContentBlock{dataWins}, defaultImageLimits(), root)
	require.Nil(t, imageErr)
	require.Equal(t, png, images[0].data)

	// Data wins even when the file the URI names does not exist at all.
	missingTarget := handoffBlock(filepath.Join(root, "absent.png"), mimeImagePNG, nil)
	missingTarget.Image.Data = encoded
	images, imageErr, _ = validatePromptImages(t.Context(), []acp.ContentBlock{missingTarget}, defaultImageLimits(), root)
	require.Nil(t, imageErr)
	require.Equal(t, png, images[0].data)

	// Empty data with no handoff intent stays missing_data: invalid_handoff
	// never claims a plain empty-data block.
	for _, block := range []acp.ContentBlock{
		acp.ImageBlock("", mimeImagePNG),
		imageBlockWithURI(t, "https://example.com/a.png"),
		imageBlockWithURI(t, "::not a uri::"),
		imageBlockWithURI(t, "blob://a"),
	} {
		_, imageErr, _ = validatePromptImages(t.Context(), []acp.ContentBlock{block}, defaultImageLimits(), root)
		require.NotNil(t, imageErr)
		require.Equal(t, imageErrorMissingData, imageErr.code)
	}

	// A file URI alone is intent, so a missing envelope is invalid_handoff
	// rather than missing_data.
	_, imageErr, _ = validatePromptImages(
		t.Context(),
		[]acp.ContentBlock{handoffBlock(filepath.Join(root, "valid.png"), mimeImagePNG, nil)},
		defaultImageLimits(),
		root,
	)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorInvalidHandoff, imageErr.code)
	require.Equal(t, handoffCauseEnvelopeMissing, imageErr.message)

	// An envelope alone is intent too, even with no URI at all.
	envelopeOnly := acp.ImageBlock("", mimeImagePNG)
	envelopeOnly.Image.Meta = map[string]any{handoffMetaKey: map[string]any{}}
	_, imageErr, _ = validatePromptImages(t.Context(), []acp.ContentBlock{envelopeOnly}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorInvalidHandoff, imageErr.code)
	require.Equal(t, handoffCauseEnvelopeFields, imageErr.message)

	// A blob resource never selects the handoff form: its URI is provenance,
	// so an empty blob under a file URI stays missing_data.
	blobMIME := mimeImagePNG
	blob := acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{
		Uri:      "file://" + filepath.ToSlash(filepath.Join(root, "valid.png")),
		MimeType: &blobMIME,
	}})
	_, imageErr, _ = validatePromptImages(t.Context(), []acp.ContentBlock{blob}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorMissingData, imageErr.code)
	require.Equal(t, promptResourceField, imageErr.field)
}

func imageBlockWithURI(t *testing.T, uri string) acp.ContentBlock {
	t.Helper()

	block := acp.ImageBlock("", mimeImagePNG)
	block.Image.Uri = &uri

	return block
}

func TestValidatePromptImagesHandoffUnsetRootRejects(t *testing.T) {
	root := t.TempDir()
	block := handoffFixture(t, root, "valid.png", testdataFixture(t, "valid.png"))

	// With no configured root the handoff form is refused, and the message says
	// why rather than blaming the block.
	_, imageErr, _ := validatePromptImages(t.Context(), []acp.ContentBlock{block}, defaultImageLimits(), "")
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorInvalidHandoff, imageErr.code)
	require.Equal(t, handoffCauseRootUnset, imageErr.message)
	require.Equal(t, promptImageField, imageErr.field)
}

func TestValidatePromptImagesHandoffEnvelopeDefects(t *testing.T) {
	root := t.TempDir()
	png := testdataFixture(t, "valid.png")
	path := filepath.Join(root, "valid.png")
	require.NoError(t, os.WriteFile(path, png, 0o600))

	sum := sha256.Sum256(png)
	digest := hex.EncodeToString(sum[:])

	// Each defect class reports its own message: one taxonomy code with one
	// message would leave the adapter unable to say which field was wrong.
	cases := []struct {
		name     string
		envelope any
		message  string
	}{
		{name: "not an object", envelope: "envelope", message: handoffCauseEnvelopeNotObject},
		{
			name:     "unknown field",
			envelope: map[string]any{handoffVersionKey: 1, handoffDigestKey: digest, handoffSizeBytesKey: len(png), "extra": true},
			message:  handoffCauseEnvelopeFields,
		},
		{
			name:     "missing version",
			envelope: map[string]any{handoffDigestKey: digest, handoffSizeBytesKey: len(png)},
			message:  handoffCauseEnvelopeFields,
		},
		{
			name:     "wrong version",
			envelope: map[string]any{handoffVersionKey: 2, handoffDigestKey: digest, handoffSizeBytesKey: len(png)},
			message:  handoffCauseEnvelopeVersion,
		},
		{
			name:     "zero version",
			envelope: map[string]any{handoffVersionKey: 0, handoffDigestKey: digest, handoffSizeBytesKey: len(png)},
			message:  handoffCauseEnvelopeVersion,
		},
		{
			name:     "fractional version",
			envelope: map[string]any{handoffVersionKey: 1.5, handoffDigestKey: digest, handoffSizeBytesKey: len(png)},
			message:  handoffCauseEnvelopeVersion,
		},
		{
			name:     "version not a number",
			envelope: map[string]any{handoffVersionKey: "1", handoffDigestKey: digest, handoffSizeBytesKey: len(png)},
			message:  handoffCauseEnvelopeVersion,
		},
		{
			name:     "digest not a string",
			envelope: map[string]any{handoffVersionKey: 1, handoffDigestKey: 0, handoffSizeBytesKey: len(png)},
			message:  handoffCauseEnvelopeDigest,
		},
		{
			name:     "digest too short",
			envelope: map[string]any{handoffVersionKey: 1, handoffDigestKey: digest[:63], handoffSizeBytesKey: len(png)},
			message:  handoffCauseEnvelopeDigest,
		},
		{
			name:     "digest uppercase",
			envelope: map[string]any{handoffVersionKey: 1, handoffDigestKey: strings.ToUpper(digest), handoffSizeBytesKey: len(png)},
			message:  handoffCauseEnvelopeDigest,
		},
		{
			name:     "digest non hex",
			envelope: map[string]any{handoffVersionKey: 1, handoffDigestKey: strings.Repeat("z", 64), handoffSizeBytesKey: len(png)},
			message:  handoffCauseEnvelopeDigest,
		},
		{
			name:     "size not a number",
			envelope: map[string]any{handoffVersionKey: 1, handoffDigestKey: digest, handoffSizeBytesKey: "12"},
			message:  handoffCauseEnvelopeSizeBytes,
		},
		{
			name:     "size negative",
			envelope: map[string]any{handoffVersionKey: 1, handoffDigestKey: digest, handoffSizeBytesKey: -1},
			message:  handoffCauseEnvelopeSizeBytes,
		},
		{
			name:     "size negative float",
			envelope: map[string]any{handoffVersionKey: 1, handoffDigestKey: digest, handoffSizeBytesKey: -1.0},
			message:  handoffCauseEnvelopeSizeBytes,
		},
		{
			name:     "size fractional",
			envelope: map[string]any{handoffVersionKey: 1, handoffDigestKey: digest, handoffSizeBytesKey: 1.5},
			message:  handoffCauseEnvelopeSizeBytes,
		},
		{
			name:     "size negative as a wide integer",
			envelope: map[string]any{handoffVersionKey: 1, handoffDigestKey: digest, handoffSizeBytesKey: int64(-1)},
			message:  handoffCauseEnvelopeSizeBytes,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, imageErr, _ := validatePromptImages(
				t.Context(),
				[]acp.ContentBlock{handoffBlock(path, mimeImagePNG, testCase.envelope)},
				defaultImageLimits(),
				root,
			)
			require.NotNil(t, imageErr)
			require.Equal(t, imageErrorInvalidHandoff, imageErr.code)
			require.Equal(t, testCase.message, imageErr.message)
		})
	}
}

func TestHandoffEnvelopeNumbersAreValidatedBeforeConversion(t *testing.T) {
	digest := strings.Repeat("a", handoffDigestLength)
	envelope := func(sizeBytes any) map[string]any {
		return map[string]any{handoffMetaKey: map[string]any{
			handoffVersionKey:   1.0,
			handoffDigestKey:    digest,
			handoffSizeBytesKey: sizeBytes,
		}}
	}

	// 2^63 is not representable as an int64. Converting it first and inspecting
	// the result asks the CPU what it thinks, and the two architectures this
	// builds for answer differently, so the range is checked on the float.
	_, message := parseHandoffEnvelope(envelope(handoffNumberCeiling))
	require.Equal(t, handoffCauseEnvelopeSizeBytes, message)

	_, message = parseHandoffEnvelope(envelope(math.MaxFloat64))
	require.Equal(t, handoffCauseEnvelopeSizeBytes, message)

	// 2^53 is the largest integer a float64 carries exactly, and it is a legal
	// declaration even though no file that size will ever match a gate.
	parsed, message := parseHandoffEnvelope(envelope(9007199254740992.0))
	require.Empty(t, message)
	require.Equal(t, int64(9007199254740992), parsed.sizeBytes)

	// The largest int64 cannot be spelled exactly as a float64, so the boundary
	// that matters is the first value above the range rather than MaxInt64.
	parsed, message = parseHandoffEnvelope(envelope(handoffNumberCeiling - 1024.0))
	require.Empty(t, message)
	require.Positive(t, parsed.sizeBytes)
}

func TestHandoffEnvelopeAcceptsDecoderNumbers(t *testing.T) {
	png := testdataFixture(t, "valid.png")
	sum := sha256.Sum256(png)

	raw := `{"` + handoffMetaKey + `":{"version":1,"digest":"` + hex.EncodeToString(sum[:]) +
		`","sizeBytes":` + strconv.Itoa(len(png)) + `}}`

	// A decoder configured to keep number text hands the envelope json.Number
	// rather than float64. Nothing in the pinned SDK does that today, and the
	// whole transport would stop working at the version gate if it started.
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()

	var meta map[string]any
	require.NoError(t, decoder.Decode(&meta))
	decoded, ok := meta[handoffMetaKey].(map[string]any)
	require.True(t, ok)
	require.IsType(t, json.Number(""), decoded[handoffVersionKey])

	envelope, message := parseHandoffEnvelope(meta)
	require.Empty(t, message)
	require.Equal(t, int64(len(png)), envelope.sizeBytes)

	// The range rules apply to the decoder's spelling too, including a number
	// that is legal JSON and larger than any float64.
	overflow := json.NewDecoder(strings.NewReader(
		`{"` + handoffMetaKey + `":{"version":1,"digest":"` + hex.EncodeToString(sum[:]) + `","sizeBytes":1e999}}`,
	))
	overflow.UseNumber()

	var overflowMeta map[string]any
	require.NoError(t, overflow.Decode(&overflowMeta))

	_, message = parseHandoffEnvelope(overflowMeta)
	require.Equal(t, handoffCauseEnvelopeSizeBytes, message)
}

func TestHandoffURIDefects(t *testing.T) {
	root := t.TempDir()
	png := testdataFixture(t, "valid.png")
	path := filepath.Join(root, "valid.png")
	require.NoError(t, os.WriteFile(path, png, 0o600))

	envelope := handoffEnvelopeFor(png)

	// A URI defect is a defect of the block, so it is invalid_handoff and never
	// a location verdict, and each defect names itself.
	cases := []struct {
		uri     string
		message string
	}{
		{uri: "", message: handoffCauseURIMissing},
		{uri: "::not a uri::", message: handoffCauseURIUnparseable},
		{uri: "https://example.com/a.png", message: handoffCauseURIScheme},
		{uri: "file://elsewhere/a.png", message: handoffCauseURIHost},
		{uri: "file:relative.png", message: handoffCauseURIRelative},
	}
	for _, testCase := range cases {
		block := acp.ImageBlock("", mimeImagePNG)
		block.Image.Meta = map[string]any{handoffMetaKey: envelope}

		if testCase.uri != "" {
			block.Image.Uri = &testCase.uri
		}

		_, imageErr, _ := validatePromptImages(t.Context(), []acp.ContentBlock{block}, defaultImageLimits(), root)
		require.NotNil(t, imageErr, testCase.uri)
		require.Equal(t, imageErrorInvalidHandoff, imageErr.code, testCase.uri)
		require.Equal(t, testCase.message, imageErr.message, testCase.uri)
	}

	// A localhost authority names this host and is accepted.
	localhost := "file://" + handoffLocalhost + filepath.ToSlash(path)
	block := acp.ImageBlock("", mimeImagePNG)
	block.Image.Uri = &localhost
	block.Image.Meta = map[string]any{handoffMetaKey: envelope}

	images, imageErr, _ := validatePromptImages(t.Context(), []acp.ContentBlock{block}, defaultImageLimits(), root)
	require.Nil(t, imageErr)
	require.Equal(t, png, images[0].data)
}

func TestHandoffPathContainment(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	png := testdataFixture(t, "valid.png")

	inside := filepath.Join(root, "valid.png")
	require.NoError(t, os.WriteFile(inside, png, 0o600))

	escaped := filepath.Join(outside, "escaped.png")
	require.NoError(t, os.WriteFile(escaped, png, 0o600))

	envelope := handoffEnvelopeFor(png)
	refuse := func(t *testing.T, path string, code string, message string) {
		t.Helper()

		_, imageErr, err := validatePromptImages(
			t.Context(),
			[]acp.ContentBlock{handoffBlock(path, mimeImagePNG, envelope)},
			defaultImageLimits(),
			root,
		)
		require.NoError(t, err)
		require.NotNil(t, imageErr)
		require.Equal(t, code, imageErr.code)
		require.Equal(t, message, imageErr.message)
	}
	accept := func(t *testing.T, path string) {
		t.Helper()

		images, imageErr, err := validatePromptImages(
			t.Context(),
			[]acp.ContentBlock{handoffBlock(path, mimeImagePNG, envelope)},
			defaultImageLimits(),
			root,
		)
		require.NoError(t, err)
		require.Nil(t, imageErr)
		require.Equal(t, png, images[0].data)
	}

	t.Run("outside the root", func(t *testing.T) {
		refuse(t, escaped, imageErrorPathNotAllowed, handoffCauseOutsideRoot)
	})

	t.Run("traversal out of the root", func(t *testing.T) {
		refuse(t, filepath.Join(root, "..", filepath.Base(outside), "escaped.png"),
			imageErrorPathNotAllowed, handoffCauseOutsideRoot)
	})

	t.Run("percent encoded traversal out of the root", func(t *testing.T) {
		// The URI decoder resolves the escape before the path is ever built, so
		// the traversal is an ordinary traversal by the time it is refused.
		uri := "file://" + filepath.ToSlash(root) + "/..%2f" + filepath.Base(outside) + "%2fescaped.png"

		block := acp.ImageBlock("", mimeImagePNG)
		block.Image.Uri = &uri
		block.Image.Meta = map[string]any{handoffMetaKey: envelope}

		_, imageErr, _ := validatePromptImages(t.Context(), []acp.ContentBlock{block}, defaultImageLimits(), root)
		require.NotNil(t, imageErr)
		require.Equal(t, imageErrorPathNotAllowed, imageErr.code)
	})

	t.Run("windows drive spelling", func(t *testing.T) {
		// A file:///C:/... uri is not a path under any posix root, and on
		// Windows it is not absolute at all, so the form is refused either way.
		uri := "file:///C:/Windows/System32/config/SAM"

		block := acp.ImageBlock("", mimeImagePNG)
		block.Image.Uri = &uri
		block.Image.Meta = map[string]any{handoffMetaKey: envelope}

		_, imageErr, _ := validatePromptImages(t.Context(), []acp.ContentBlock{block}, defaultImageLimits(), root)
		require.NotNil(t, imageErr)
		require.Contains(t, []string{imageErrorPathNotAllowed, imageErrorInvalidHandoff}, imageErr.code)
	})

	t.Run("sibling root prefix", func(t *testing.T) {
		// A directory whose name merely starts with the root must not match.
		sibling := root + "-sibling"
		require.NoError(t, os.MkdirAll(sibling, 0o700))
		t.Cleanup(func() { _ = os.RemoveAll(sibling) })

		path := filepath.Join(sibling, "valid.png")
		require.NoError(t, os.WriteFile(path, png, 0o600))
		refuse(t, path, imageErrorPathNotAllowed, handoffCauseOutsideRoot)
	})

	t.Run("symlink escaping the root", func(t *testing.T) {
		link := filepath.Join(root, "escape.png")
		require.NoError(t, os.Symlink(escaped, link))
		refuse(t, link, imageErrorPathNotAllowed, handoffCauseOutsideRoot)
	})

	t.Run("relative symlink inside the root", func(t *testing.T) {
		link := filepath.Join(root, "relative-link.png")
		require.NoError(t, os.Symlink("valid.png", link))
		accept(t, link)
	})

	t.Run("absolute symlink inside the root", func(t *testing.T) {
		// An absolute link target cannot be confined, so it is refused even
		// though it happens to point back inside the root.
		link := filepath.Join(root, "absolute-link.png")
		require.NoError(t, os.Symlink(inside, link))
		refuse(t, link, imageErrorPathNotAllowed, handoffCauseOutsideRoot)
	})

	t.Run("dangling relative symlink is a missing file", func(t *testing.T) {
		// The host wrote the link and removed the target: the same operational
		// case a plain absent file is.
		link := filepath.Join(root, "dangling.png")
		require.NoError(t, os.Symlink("never-created.png", link))
		refuse(t, link, imageErrorMissingFile, handoffCauseMissing)
	})

	t.Run("dangling absolute symlink is a containment failure", func(t *testing.T) {
		// Its target is unconfinable whether or not it exists, so the verdict
		// answers nothing about whether it does.
		link := filepath.Join(root, "dangling-absolute.png")
		require.NoError(t, os.Symlink(filepath.Join(outside, "never-created.png"), link))
		refuse(t, link, imageErrorPathNotAllowed, handoffCauseOutsideRoot)
	})

	t.Run("hardlink to an outside file is read", func(t *testing.T) {
		// A hardlink is the same inode as its target, so the root is no
		// boundary against anything that can create one. The declared digest is
		// what decides whether the bytes are the bytes that were promised.
		link := filepath.Join(root, "hardlink.png")
		require.NoError(t, os.Link(escaped, link))
		accept(t, link)
	})

	t.Run("root that cannot be opened", func(t *testing.T) {
		absentRoot := filepath.Join(root, "absent-root")

		_, imageErr, _ := validatePromptImages(
			t.Context(),
			[]acp.ContentBlock{handoffBlock(filepath.Join(absentRoot, "valid.png"), mimeImagePNG, envelope)},
			defaultImageLimits(),
			absentRoot,
		)
		require.NotNil(t, imageErr)
		require.Equal(t, imageErrorPathNotAllowed, imageErr.code)
		require.Equal(t, handoffCauseRootUnopenable, imageErr.message)
		require.NotContains(t, imageErr.message, root)
	})

	t.Run("directory is not a regular file", func(t *testing.T) {
		refuse(t, filepath.Join(root, "subdir"), imageErrorMissingFile, handoffCauseMissing)

		dir := filepath.Join(root, "realdir")
		require.NoError(t, os.MkdirAll(dir, 0o700))
		refuse(t, dir, imageErrorPathNotAllowed, handoffCauseNotRegular)
	})

	t.Run("vanished path inside the root", func(t *testing.T) {
		refuse(t, filepath.Join(root, "cleaned-up.png"), imageErrorMissingFile, handoffCauseMissing)
	})

	t.Run("the root itself", func(t *testing.T) {
		refuse(t, root, imageErrorPathNotAllowed, handoffCauseNotRegular)
	})
}

func TestHandoffRelativeNameNeverClimbsIntoTheRootByAccident(t *testing.T) {
	separator := string(filepath.Separator)
	root := separator + "srv" + separator + "handoff"

	require.Equal(t, "a.png", handoffRelativeName(root, root+separator+"a.png"))
	require.Equal(t, filepath.Join("nested", "a.png"), handoffRelativeName(root, root+separator+"nested"+separator+"a.png"))
	require.Equal(t, ".", handoffRelativeName(root, root))
	require.Equal(t, ".", handoffRelativeName(root+separator, root))

	// A sibling whose name merely starts with the root's is not under it, and a
	// name that is not under the root is handed over as one that leaves it.
	require.Equal(t, handoffParentName, handoffRelativeName(root, root+"x"+separator+"a.png"))
	require.Equal(t, handoffParentName, handoffRelativeName(root, separator+"etc"+separator+"passwd"))
	require.Equal(t, handoffParentName, handoffRelativeName(root+"x", root+separator+"a.png"))
}

func TestHandoffRootReachedThroughASymlinkStillResolves(t *testing.T) {
	// The normal shape of a temp directory on Darwin, and of any
	// operator-relocated data directory: a root reached through a symlink must
	// neither reject a legitimate file nor admit an escape.
	target := t.TempDir()
	root := filepath.Join(t.TempDir(), "handoff-link")
	require.NoError(t, os.Symlink(target, root))

	png := testdataFixture(t, "valid.png")
	block := handoffFixture(t, root, "nested/valid.png", png)

	images, imageErr, _ := validatePromptImages(t.Context(), []acp.ContentBlock{block}, defaultImageLimits(), root)
	require.Nil(t, imageErr)
	require.Equal(t, png, images[0].data)

	outside := filepath.Join(t.TempDir(), "escaped.png")
	require.NoError(t, os.WriteFile(outside, png, 0o600))

	envelope := handoffEnvelopeFor(png)

	for _, path := range []string{outside, filepath.Join(root, "..", "escaped.png")} {
		_, imageErr, _ = validatePromptImages(
			t.Context(),
			[]acp.ContentBlock{handoffBlock(path, mimeImagePNG, envelope)},
			defaultImageLimits(),
			root,
		)
		require.NotNil(t, imageErr, path)
		require.Equal(t, imageErrorPathNotAllowed, imageErr.code, path)
	}
}

func TestHandoffOverBoundReadIsRejectedAndForwardsNoBytes(t *testing.T) {
	root := t.TempDir()

	const gate int64 = 512

	limits := ImageLimits{MaxInputBytesPerImage: gate}

	// The file holds twice the gate, and the envelope declares a size well
	// inside it. Every number a host or the filesystem supplies about this file
	// says it fits; only the bytes actually read say otherwise.
	whole := syntheticPNG(t, 1024)
	require.NoError(t, os.WriteFile(filepath.Join(root, "grown.png"), whole, 0o600))

	declared := whole[:448]
	block := handoffBlock(filepath.Join(root, "grown.png"), mimeImagePNG, handoffEnvelopeFor(declared))
	blocks := []acp.ContentBlock{acp.TextBlock("describe this"), block}

	images, imageErr, err := validatePromptImages(t.Context(), blocks, limits, root)
	require.NoError(t, err)
	require.NotNil(t, imageErr)

	// too_large decided on the bytes read: one past the gate, never the 448 the
	// envelope declared and never a size taken from the file's metadata.
	require.Equal(t, imageErrorTooLarge, imageErr.code)
	require.Equal(t, gate+1, imageErr.sizeBytes)
	require.Equal(t, gate, imageErr.maxBytes)

	// No bytes survive the refusal, and the native mapper refuses to build a
	// request for an image block it was given no validated bytes for, so there
	// is no route by which those bytes could reach the harness.
	require.Empty(t, images)

	_, mapErr := codex.PromptToUserInput(blocks, nil)
	require.ErrorIs(t, mapErr, codex.ErrImageNotMaterialized)

	// A declared size past the gate is refused before the file is opened at
	// all: there is nothing to learn from reading it.
	reads := countingImageReads(t)
	oversizeDeclared := handoffBlock(filepath.Join(root, "grown.png"), mimeImagePNG, handoffEnvelopeFor(whole))

	_, imageErr, _ = validatePromptImages(t.Context(), []acp.ContentBlock{oversizeDeclared}, limits, root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorTooLarge, imageErr.code)
	require.Equal(t, int64(len(whole)), imageErr.sizeBytes)
	require.Equal(t, gate, imageErr.maxBytes)
	require.Zero(t, *reads)

	// A file that fits is read whole and verified, and the aggregate is charged
	// the bytes that were read.
	exact := syntheticPNG(t, int(gate))
	fits := handoffFixture(t, root, "exact.png", exact)

	accepted, imageErr, _ := validatePromptImages(t.Context(), []acp.ContentBlock{fits}, limits, root)
	require.Nil(t, imageErr)
	require.Equal(t, exact, accepted[0].data)

	_, imageErr, _ = validatePromptImages(
		t.Context(),
		[]acp.ContentBlock{fits, fits},
		ImageLimits{MaxInputBytesPerImage: gate, MaxInputBytesPerPrompt: gate*2 - 1},
		root,
	)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorTooLarge, imageErr.code)
	require.Equal(t, gate*2, imageErr.sizeBytes)
	require.Equal(t, gate*2-1, imageErr.maxBytes)
}

func TestHandoffBlockCountIsCappedWithTheAggregateDisabled(t *testing.T) {
	root := t.TempDir()

	// Every block is structurally valid, far inside the per-image bound, and
	// truthfully declared, so nothing but the count can refuse them. Both byte
	// limits are disabled, which is the configuration that leaves the count as
	// the only bound on the work one prompt can demand.
	block := handoffFixture(t, root, "small.png", syntheticPNG(t, 64))

	atCap := make([]acp.ContentBlock, 0, maxHandoffBlocksPerPrompt+1)
	for range maxHandoffBlocksPerPrompt {
		atCap = append(atCap, block)
	}

	images, imageErr, err := validatePromptImages(t.Context(), atCap, ImageLimits{}, root)
	require.NoError(t, err)
	require.Nil(t, imageErr)
	require.Len(t, images, maxHandoffBlocksPerPrompt)

	reads := countingImageReads(t)
	overCap := make([]acp.ContentBlock, 0, len(atCap)+1)
	overCap = append(overCap, atCap...)
	overCap = append(overCap, block)

	_, imageErr, err = validatePromptImages(t.Context(), overCap, ImageLimits{}, root)
	require.NoError(t, err)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorTooLarge, imageErr.code)
	require.Equal(t, int64(maxHandoffBlocksPerPrompt+1), imageErr.sizeBytes)
	require.Equal(t, int64(maxHandoffBlocksPerPrompt), imageErr.maxBytes)
	require.Equal(t, maxHandoffBlocksPerPrompt, imageErr.index)

	// The block that crossed the cap was never read: bounding the reads is the
	// reason the count exists.
	require.Equal(t, maxHandoffBlocksPerPrompt, *reads)

	// Embedded blocks are bounded by the frame that carries them, so they do
	// not spend the handoff count.
	embedded := acp.ImageBlock(base64.StdEncoding.EncodeToString(syntheticPNG(t, 64)), mimeImagePNG)

	mixed := append([]acp.ContentBlock{}, atCap...)
	mixed = append(mixed, embedded)

	images, imageErr, _ = validatePromptImages(t.Context(), mixed, ImageLimits{}, root)
	require.Nil(t, imageErr)
	require.Len(t, images, maxHandoffBlocksPerPrompt+1)
}

func TestHandoffFileSwappedAfterContainmentIsRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	png := syntheticPNG(t, 128)
	other := syntheticPNG(t, 256)

	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.png"), other, 0o600))

	path := filepath.Join(root, "swapped.png")
	require.NoError(t, os.WriteFile(path, png, 0o600))

	block := handoffBlock(path, mimeImagePNG, handoffEnvelopeFor(png))

	// Replaced by a link out of the root: the open is confined, so the escape
	// is refused rather than followed.
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret.png"), path))

	_, imageErr, err := validatePromptImages(t.Context(), []acp.ContentBlock{block}, defaultImageLimits(), root)
	require.NoError(t, err)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorPathNotAllowed, imageErr.code)
	require.Equal(t, handoffCauseOutsideRoot, imageErr.message)

	// Replaced by a different regular file at the same name, still inside the
	// root: containment has nothing to object to, and the declared digest is
	// what refuses the substitution.
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.WriteFile(path, other, 0o600))

	_, imageErr, _ = validatePromptImages(t.Context(), []acp.ContentBlock{block}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorDigestMismatch, imageErr.code)
	require.Equal(t, handoffCauseDigestMismatch, imageErr.message)

	// Removed outright: the operational case, reported as one.
	require.NoError(t, os.Remove(path))

	_, imageErr, _ = validatePromptImages(t.Context(), []acp.ContentBlock{block}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorMissingFile, imageErr.code)
	require.Equal(t, handoffCauseMissing, imageErr.message)
}

func TestHandoffDigestVerification(t *testing.T) {
	root := t.TempDir()
	png := testdataFixture(t, "valid.png")

	// Bytes tampered with after the host built the envelope fail closed rather
	// than falling back to anything.
	tampered := handoffFixture(t, root, "tampered.png", png)

	corrupted := append([]byte(nil), png...)
	corrupted[len(corrupted)-1] ^= 0xff
	require.NoError(t, os.WriteFile(filepath.Join(root, "tampered.png"), corrupted, 0o600))

	_, imageErr, _ := validatePromptImages(t.Context(), []acp.ContentBlock{tampered}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorDigestMismatch, imageErr.code)
	require.Equal(t, handoffCauseDigestMismatch, imageErr.message)

	// A truthful digest over an untruthful size is still a mismatch, and it
	// reports the same thing: neither the observed hash nor the observed length
	// is the caller's to learn.
	sum := sha256.Sum256(png)
	wrongSize := handoffBlock(filepath.Join(root, "sized.png"), mimeImagePNG, map[string]any{
		handoffVersionKey:   handoffVersion,
		handoffDigestKey:    hex.EncodeToString(sum[:]),
		handoffSizeBytesKey: len(png) + 1,
	})
	require.NoError(t, os.WriteFile(filepath.Join(root, "sized.png"), png, 0o600))

	_, imageErr, _ = validatePromptImages(t.Context(), []acp.ContentBlock{wrongSize}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorDigestMismatch, imageErr.code)
	require.Equal(t, handoffCauseDigestMismatch, imageErr.message)
}

func TestHandoffMessagesAreConstants(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	png := syntheticPNG(t, 448)
	secretName := "deployment-secret-name.png"
	secretPath := filepath.Join(outside, secretName)
	require.NoError(t, os.WriteFile(secretPath, png, 0o600))

	inside := filepath.Join(root, "valid.png")
	require.NoError(t, os.WriteFile(inside, png, 0o600))

	envelope := handoffEnvelopeFor(png)
	digest, _ := envelope[handoffDigestKey].(string)

	// Every message a handoff verdict can carry. A message outside this set is
	// either derived from the filesystem or interpolated from an operating-system
	// error, which is exactly what must not reach a client or a trace.
	declared := map[string]struct{}{
		handoffCauseRootUnset:         {},
		handoffCauseRootUnopenable:    {},
		handoffCauseEnvelopeMissing:   {},
		handoffCauseEnvelopeNotObject: {},
		handoffCauseEnvelopeFields:    {},
		handoffCauseEnvelopeVersion:   {},
		handoffCauseEnvelopeDigest:    {},
		handoffCauseEnvelopeSizeBytes: {},
		handoffCauseURIMissing:        {},
		handoffCauseURIUnparseable:    {},
		handoffCauseURIScheme:         {},
		handoffCauseURIHost:           {},
		handoffCauseURIRelative:       {},
		handoffCauseOutsideRoot:       {},
		handoffCauseNotRegular:        {},
		handoffCauseMissing:           {},
		handoffCauseUnreadable:        {},
		handoffCauseDigestMismatch:    {},
	}

	tamperedPath := filepath.Join(root, "tampered.png")
	require.NoError(t, os.WriteFile(tamperedPath, append([]byte(nil), png...), 0o600))

	tamperedEnvelope := handoffEnvelopeFor(append(append([]byte(nil), png...), 'x'))

	escape := filepath.Join(root, "escape.png")
	require.NoError(t, os.Symlink(secretPath, escape))

	directory := filepath.Join(root, "dir")
	require.NoError(t, os.MkdirAll(directory, 0o700))

	cases := []struct {
		name  string
		root  string
		block acp.ContentBlock
		code  string
	}{
		{name: "root unset", root: "", block: handoffBlock(inside, mimeImagePNG, envelope), code: imageErrorInvalidHandoff},
		{
			name:  "root unopenable",
			root:  filepath.Join(root, "absent"),
			block: handoffBlock(filepath.Join(root, "absent", "valid.png"), mimeImagePNG, envelope),
			code:  imageErrorPathNotAllowed,
		},
		{name: "bad envelope", root: root, block: handoffBlock(inside, mimeImagePNG, "nope"), code: imageErrorInvalidHandoff},
		{
			name:  "bad uri",
			root:  root,
			block: handoffBlock("relative.png", mimeImagePNG, envelope),
			code:  imageErrorInvalidHandoff,
		},
		{name: "escape", root: root, block: handoffBlock(escape, mimeImagePNG, envelope), code: imageErrorPathNotAllowed},
		{
			name:  "outside the root",
			root:  root,
			block: handoffBlock(secretPath, mimeImagePNG, envelope),
			code:  imageErrorPathNotAllowed,
		},
		{name: "non regular", root: root, block: handoffBlock(directory, mimeImagePNG, envelope), code: imageErrorPathNotAllowed},
		{
			name:  "absent file",
			root:  root,
			block: handoffBlock(filepath.Join(root, "absent.png"), mimeImagePNG, envelope),
			code:  imageErrorMissingFile,
		},
		{
			name:  "tampered bytes",
			root:  root,
			block: handoffBlock(tamperedPath, mimeImagePNG, tamperedEnvelope),
			code:  imageErrorDigestMismatch,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, imageErr, err := validatePromptImages(
				t.Context(),
				[]acp.ContentBlock{testCase.block},
				defaultImageLimits(),
				testCase.root,
			)
			require.NoError(t, err)
			require.NotNil(t, imageErr)
			require.Equal(t, testCase.code, imageErr.code)
			require.Contains(t, declared, imageErr.message)

			require.NotContains(t, imageErr.message, root)
			require.NotContains(t, imageErr.message, outside)
			require.NotContains(t, imageErr.message, secretName)
			require.NotContains(t, imageErr.message, digest)
			require.NotContains(t, imageErr.message, "448")

			// The wire envelope carries the same message, so a constant here is
			// only worth anything if nothing else is added on the way out.
			var requestErr *acp.RequestError
			require.ErrorAs(t, imageErr.invalidParams(), &requestErr)

			data, _ := requestErr.Data.(map[string]any)
			require.Equal(t, imageErr.message, data[jsonFieldMessage])
			require.Equal(t, promptImageField, data[jsonFieldField])
		})
	}
}

func TestHandoffReadFailureIsMissingFile(t *testing.T) {
	root := t.TempDir()
	block := handoffFixture(t, root, "valid.png", testdataFixture(t, "valid.png"))

	readErr := errors.New("read refused")
	original := readImageFile
	readImageFile = func(io.Reader) ([]byte, error) { return nil, readErr }

	t.Cleanup(func() { readImageFile = original })

	// A descriptor that opened and then failed to yield its bytes is the
	// operational case, and the error text stays inside the process.
	_, imageErr, _ := validatePromptImages(t.Context(), []acp.ContentBlock{block}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorMissingFile, imageErr.code)
	require.Equal(t, handoffCauseUnreadable, imageErr.message)
	require.NotContains(t, imageErr.message, readErr.Error())
}

func TestHandoffReadHonoursContextCancellation(t *testing.T) {
	root := t.TempDir()
	block := handoffFixture(t, root, "valid.png", testdataFixture(t, "valid.png"))

	reads := countingImageReads(t)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	// The validation loop checks between blocks, so a caller that has already
	// stopped waiting gets its own cancellation back rather than a verdict —
	// and no file is touched on its behalf.
	images, imageErr, err := validatePromptImages(cancelled, []acp.ContentBlock{block}, defaultImageLimits(), root)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, imageErr)
	require.Nil(t, images)
	require.Zero(t, *reads)

	// The read path checks again immediately before it opens anything, which is
	// the check that matters once the loop has already admitted the block.
	media, ok := promptMediaBlock(block)
	require.True(t, ok)

	data, verdict, err := readPromptHandoff(cancelled, root, media, effectiveInputBytesPerImage(0))
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, verdict)
	require.Nil(t, data)
	require.Zero(t, *reads)
}

// nativePromptRequest runs the whole input path a turn runs — validate,
// materialize, map — and returns the native Codex prompt plus its release.
func nativePromptRequest(
	t *testing.T,
	blocks []acp.ContentBlock,
	handoffRoot string,
	scratch string,
) ([]codex.UserInput, func()) {
	t.Helper()

	images, imageErr, err := validatePromptImages(t.Context(), blocks, defaultImageLimits(), handoffRoot)
	require.NoError(t, err)
	require.Nil(t, imageErr)

	promptSession := &session{agent: NewAgent(WithScratchDir(scratch), WithInputHandoffRoot(handoffRoot))}

	prepared, err := promptSession.preparePromptImages(t.Context(), images)
	require.NoError(t, err)

	input, err := promptToCodex(blocks, prepared.images)
	require.NoError(t, err)

	return input, prepared.release
}

func TestHandoffAndEmbeddedFormsBuildIdenticalNativeRequests(t *testing.T) {
	root := t.TempDir()
	png := testdataFixture(t, "valid.png")

	large := syntheticPNG(t, codexInlineImageEnvelopeSize)
	copy(large, png[:33])

	cases := []struct {
		name      string
		data      []byte
		wrapped   bool
		transport string
	}{
		{name: "inline data url transport", data: png, transport: jsonFieldURL},
		{name: "line wrapped base64 transport", data: png, wrapped: true, transport: jsonFieldURL},
		{name: "local file transport", data: large, transport: jsonFieldPath},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			text := acp.TextBlock("describe this")
			handoff := handoffFixture(t, root, testCase.name+"/fixture.png", testCase.data)

			encoded := base64.StdEncoding.EncodeToString(testCase.data)
			if testCase.wrapped {
				// A host that wraps its base64 at a column boundary describes
				// exactly the same bytes, so the two forms must still agree.
				encoded = wrapBase64(encoded, 76)
				require.Contains(t, encoded, "\n")
			}

			embedded := acp.ImageBlock(encoded, mimeImagePNG)

			handoffInput, releaseHandoff := nativePromptRequest(t, []acp.ContentBlock{text, handoff}, root, t.TempDir())
			defer releaseHandoff()

			embeddedInput, releaseEmbedded := nativePromptRequest(t, []acp.ContentBlock{text, embedded}, root, t.TempDir())
			defer releaseEmbedded()

			// The transport each case claims is the transport it got, so the
			// case name is a check rather than a label.
			require.Len(t, handoffInput, 2)
			require.Contains(t, handoffInput[1], testCase.transport)
			require.Contains(t, embeddedInput[1], testCase.transport)

			// The two forms are indistinguishable downstream, modulo the
			// adapter-owned scratch directory a materialized image lands in —
			// and the bytes behind that path are compared, not its name.
			require.Equal(t,
				normalizeNativeScratchPaths(t, embeddedInput, testCase.data),
				normalizeNativeScratchPaths(t, handoffInput, testCase.data),
			)
		})
	}
}

// wrapBase64 breaks encoded base64 into lines, which a strict decoder ignores
// and a byte-for-byte comparison of the encoded string would not.
func wrapBase64(encoded string, width int) string {
	var wrapped strings.Builder

	for start := 0; start < len(encoded); start += width {
		end := min(start+width, len(encoded))

		wrapped.WriteString(encoded[start:end])
		wrapped.WriteString("\n")
	}

	return wrapped.String()
}

// normalizeNativeScratchPaths replaces each adapter-owned scratch path with the
// bytes behind it, so the comparison is of the payload each form materialized
// rather than of a per-turn directory name.
func normalizeNativeScratchPaths(t *testing.T, input []codex.UserInput, expected []byte) []codex.UserInput {
	t.Helper()

	normalized := make([]codex.UserInput, 0, len(input))

	for _, entry := range input {
		clone := codex.UserInput{}

		for key, value := range entry {
			path, isPath := value.(string)
			if key == jsonFieldPath && isPath {
				materialized, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, expected, materialized)

				value = "<materialized>"
			}

			clone[key] = value
		}

		normalized = append(normalized, clone)
	}

	return normalized
}

func TestHandoffPathNeverReachesTheNativeRequest(t *testing.T) {
	root := t.TempDir()
	scratch := t.TempDir()

	// Large enough that the native transport is a file path rather than an
	// inline data URL, which is the case a pass-through would be invisible in.
	large := syntheticPNG(t, codexInlineImageEnvelopeSize)

	handoff := handoffFixture(t, root, "secret-host-name/handoff-fixture.png", large)

	input, release := nativePromptRequest(t, []acp.ContentBlock{handoff}, root, scratch)
	defer release()

	rendered := string(mustMarshal(t, input))
	require.NotContains(t, rendered, root)
	require.NotContains(t, rendered, "secret-host-name")
	require.NotContains(t, rendered, "handoff-fixture")

	// The bytes did arrive, in a file the adapter owns under its own scratch.
	require.Len(t, input, 1)

	path, ok := input[0][jsonFieldPath].(string)
	require.True(t, ok)

	resolvedPath, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	require.True(t, pathWithinRoot(resolvedPath, scratch))

	materialized, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, large, materialized)

	// The adapter-owned copy lives exactly as long as the turn does.
	release()
	require.NoFileExists(t, path)
}

func TestValidateInputHandoffRoot(t *testing.T) {
	require.NoError(t, validateInputHandoffRoot(""))
	require.NoError(t, validateInputHandoffRoot(t.TempDir()))
	require.Error(t, validateInputHandoffRoot(filepath.Join("relative", "handoff")))

	// Construction fails closed, so a relative root can never be consulted.
	_, err := NewAgent(WithInputHandoffRoot("relative")).Initialize(t.Context(), acp.InitializeRequest{})
	require.Error(t, err)
}

// countingHandoffOpens wraps the real opener so a descriptor that is opened and
// never closed is a test failure rather than a quiet leak. Every gate after the
// open refuses by returning, which is exactly the shape that leaks when the
// release is written at each of them instead of once.
func countingHandoffOpens(t *testing.T) {
	t.Helper()

	var opened, closed int

	original := openHandoffImage
	openHandoffImage = func(root string, path string) (io.ReadCloser, *handoffVerdict) {
		file, verdict := original(root, path)
		if verdict != nil {
			return nil, verdict
		}

		opened++

		return countedReadCloser{ReadCloser: file, closed: &closed}, nil
	}

	t.Cleanup(func() {
		openHandoffImage = original

		require.Positive(t, opened, "the test never reached an open, so it proves nothing about closing")
		require.Equal(t, opened, closed, "a handoff descriptor was opened and never closed")
	})
}

type countedReadCloser struct {
	io.ReadCloser

	closed *int
}

func (c countedReadCloser) Close() error {
	*c.closed++

	return c.ReadCloser.Close()
}

func TestHandoffRefusalsAfterTheOpenReleaseTheDescriptor(t *testing.T) {
	root := t.TempDir()

	const gate int64 = 512

	png := syntheticPNG(t, 128)
	path := filepath.Join(root, "counted.png")
	require.NoError(t, os.WriteFile(path, png, 0o600))

	over := syntheticPNG(t, int(gate)+64)
	overPath := filepath.Join(root, "over.png")
	require.NoError(t, os.WriteFile(overPath, over, 0o600))

	// Each of the four verdicts that can be reached with a descriptor already
	// open, driven with the close counter live.
	cases := []struct {
		name  string
		block acp.ContentBlock
		code  string
	}{
		{
			name:  "allowlist",
			block: handoffBlock(path, "image/heic", handoffEnvelopeFor(png)),
			code:  imageErrorInvalidMediaType,
		},
		{
			name:  "declared size past the gate",
			block: handoffBlock(path, mimeImagePNG, handoffEnvelopeFor(syntheticPNG(t, int(gate)+1))),
			code:  imageErrorTooLarge,
		},
		{
			name:  "bytes read past the gate",
			block: handoffBlock(overPath, mimeImagePNG, handoffEnvelopeFor(png)),
			code:  imageErrorTooLarge,
		},
		{
			name:  "digest mismatch",
			block: handoffBlock(path, mimeImagePNG, handoffEnvelopeFor(syntheticPNG(t, 64))),
			code:  imageErrorDigestMismatch,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			countingHandoffOpens(t)

			_, imageErr, err := validatePromptImages(
				t.Context(),
				[]acp.ContentBlock{testCase.block},
				ImageLimits{MaxInputBytesPerImage: gate},
				root,
			)
			require.NoError(t, err)
			require.NotNil(t, imageErr)
			require.Equal(t, testCase.code, imageErr.code)
		})
	}

	// The accepting path releases it too.
	t.Run("accepted", func(t *testing.T) {
		countingHandoffOpens(t)

		images, imageErr, err := validatePromptImages(
			t.Context(),
			[]acp.ContentBlock{handoffBlock(path, mimeImagePNG, handoffEnvelopeFor(png))},
			ImageLimits{MaxInputBytesPerImage: gate},
			root,
		)
		require.NoError(t, err)
		require.Nil(t, imageErr)
		require.Equal(t, png, images[0].data)
	})
}
