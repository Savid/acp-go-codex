package codexacp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
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

	sum := sha256.Sum256(data)

	return handoffBlock(path, mimeImagePNG, map[string]any{
		handoffVersionKey:   handoffVersion,
		handoffDigestKey:    hex.EncodeToString(sum[:]),
		handoffSizeBytesKey: len(data),
	})
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

func TestValidatePromptImagesHandoffFormMirrorsEmbedded(t *testing.T) {
	root := t.TempDir()
	png := testdataFixture(t, "valid.png")

	// The handoff form accepts exactly what the embedded form accepts, and the
	// bytes it yields are the file's bytes.
	handoff := handoffFixture(t, root, "nested/dir/valid.png", png)
	images, imageErr := validatePromptImages([]acp.ContentBlock{handoff}, defaultImageLimits(), root)
	require.Nil(t, imageErr)
	require.Len(t, images, 1)
	require.Equal(t, png, images[0].data)
	require.Equal(t, mimeImagePNG, images[0].mimeType)
	require.Equal(t, 0, images[0].index)

	// Handoff bytes are charged to the per-prompt aggregate exactly as embedded
	// bytes are: two copies of one file cross a budget sized for one.
	size := int64(len(png))
	_, imageErr = validatePromptImages(
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
	images, imageErr = validatePromptImages(
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

			_, imageErr := validatePromptImages([]acp.ContentBlock{block}, defaultImageLimits(), root)
			require.NotNil(t, imageErr)
			require.Equal(t, testCase.code, imageErr.code)

			embedded := acp.ImageBlock(base64.StdEncoding.EncodeToString(data), testCase.mimeType)
			_, embeddedErr := validatePromptImages([]acp.ContentBlock{embedded}, defaultImageLimits(), "")
			require.NotNil(t, embeddedErr)
			require.Equal(t, imageErr.code, embeddedErr.code)
		})
	}

	// The allowlist reads the declared media type on both forms.
	notARaster := handoffFixture(t, root, "opaque", []byte("not a raster at all"))
	notARaster.Image.MimeType = "image/jpg"
	_, imageErr := validatePromptImages([]acp.ContentBlock{notARaster}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorInvalidMediaType, imageErr.code)

	notARaster.Image.MimeType = mimeImagePNG
	_, imageErr = validatePromptImages([]acp.ContentBlock{notARaster}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorMediaTypeMismatch, imageErr.code)
}

func TestValidatePromptImagesHandoffFormSelection(t *testing.T) {
	root := t.TempDir()
	png := testdataFixture(t, "valid.png")
	encoded := base64.StdEncoding.EncodeToString(png)

	// Non-empty data wins over a URI and an envelope alike, so the embedded
	// form is unchanged by the pre-gate.
	dataWins := handoffFixture(t, root, "data-wins.png", []byte("ignored"))
	dataWins.Image.Data = encoded

	images, imageErr := validatePromptImages([]acp.ContentBlock{dataWins}, defaultImageLimits(), root)
	require.Nil(t, imageErr)
	require.Equal(t, png, images[0].data)

	// Data wins even when the file the URI names does not exist at all.
	missingTarget := handoffBlock(filepath.Join(root, "absent.png"), mimeImagePNG, nil)
	missingTarget.Image.Data = encoded
	images, imageErr = validatePromptImages([]acp.ContentBlock{missingTarget}, defaultImageLimits(), root)
	require.Nil(t, imageErr)
	require.Equal(t, png, images[0].data)

	// Empty data with no handoff intent stays missing_data: invalid_handoff
	// never claims a plain empty block.
	for _, block := range []acp.ContentBlock{
		acp.ImageBlock("", mimeImagePNG),
		imageBlockWithURI(t, "https://example.com/a.png"),
		imageBlockWithURI(t, "::not a uri::"),
		imageBlockWithURI(t, "blob://a"),
	} {
		_, imageErr = validatePromptImages([]acp.ContentBlock{block}, defaultImageLimits(), root)
		require.NotNil(t, imageErr)
		require.Equal(t, imageErrorMissingData, imageErr.code)
	}

	// A file URI alone is intent, so a missing envelope is invalid_handoff
	// rather than missing_data.
	_, imageErr = validatePromptImages(
		[]acp.ContentBlock{handoffBlock(filepath.Join(root, "valid.png"), mimeImagePNG, nil)},
		defaultImageLimits(),
		root,
	)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorInvalidHandoff, imageErr.code)

	// An envelope alone is intent too, even with no URI at all.
	envelopeOnly := acp.ImageBlock("", mimeImagePNG)
	envelopeOnly.Image.Meta = map[string]any{handoffMetaKey: map[string]any{}}
	_, imageErr = validatePromptImages([]acp.ContentBlock{envelopeOnly}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorInvalidHandoff, imageErr.code)

	// A blob resource never selects the handoff form: its URI is provenance,
	// so an empty blob under a file URI stays missing_data.
	blobMIME := mimeImagePNG
	blob := acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{
		Uri:      "file://" + filepath.ToSlash(filepath.Join(root, "valid.png")),
		MimeType: &blobMIME,
	}})
	_, imageErr = validatePromptImages([]acp.ContentBlock{blob}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorMissingData, imageErr.code)
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
	_, imageErr := validatePromptImages([]acp.ContentBlock{block}, defaultImageLimits(), "")
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorInvalidHandoff, imageErr.code)
	require.Contains(t, imageErr.message, "no input handoff root is configured")
	require.Contains(t, imageErr.Error(), "no input handoff root is configured")
}

func TestValidatePromptImagesHandoffEnvelopeDefects(t *testing.T) {
	root := t.TempDir()
	png := testdataFixture(t, "valid.png")
	path := filepath.Join(root, "valid.png")
	require.NoError(t, os.WriteFile(path, png, 0o600))

	sum := sha256.Sum256(png)
	digest := hex.EncodeToString(sum[:])

	cases := []struct {
		name     string
		envelope any
	}{
		{name: "not an object", envelope: "envelope"},
		{
			name:     "unknown field",
			envelope: map[string]any{handoffVersionKey: 1, handoffDigestKey: digest, handoffSizeBytesKey: len(png), "extra": true},
		},
		{name: "missing version", envelope: map[string]any{handoffDigestKey: digest, handoffSizeBytesKey: len(png)}},
		{
			name:     "wrong version",
			envelope: map[string]any{handoffVersionKey: 2, handoffDigestKey: digest, handoffSizeBytesKey: len(png)},
		},
		{
			name:     "fractional version",
			envelope: map[string]any{handoffVersionKey: 1.5, handoffDigestKey: digest, handoffSizeBytesKey: len(png)},
		},
		{
			name:     "version not a number",
			envelope: map[string]any{handoffVersionKey: "1", handoffDigestKey: digest, handoffSizeBytesKey: len(png)},
		},
		{
			name:     "digest not a string",
			envelope: map[string]any{handoffVersionKey: 1, handoffDigestKey: 0, handoffSizeBytesKey: len(png)},
		},
		{
			name:     "digest too short",
			envelope: map[string]any{handoffVersionKey: 1, handoffDigestKey: digest[:63], handoffSizeBytesKey: len(png)},
		},
		{
			name:     "digest uppercase",
			envelope: map[string]any{handoffVersionKey: 1, handoffDigestKey: strings.ToUpper(digest), handoffSizeBytesKey: len(png)},
		},
		{
			name:     "digest non hex",
			envelope: map[string]any{handoffVersionKey: 1, handoffDigestKey: strings.Repeat("z", 64), handoffSizeBytesKey: len(png)},
		},
		{
			name:     "size not a number",
			envelope: map[string]any{handoffVersionKey: 1, handoffDigestKey: digest, handoffSizeBytesKey: "12"},
		},
		{
			name:     "size negative",
			envelope: map[string]any{handoffVersionKey: 1, handoffDigestKey: digest, handoffSizeBytesKey: -1},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, imageErr := validatePromptImages(
				[]acp.ContentBlock{handoffBlock(path, mimeImagePNG, testCase.envelope)},
				defaultImageLimits(),
				root,
			)
			require.NotNil(t, imageErr)
			require.Equal(t, imageErrorInvalidHandoff, imageErr.code)
			require.NotEmpty(t, imageErr.message)
		})
	}

	// An int64 and a float64 both satisfy the integer fields, because a decoded
	// envelope carries JSON numbers.
	envelope, err := parseHandoffEnvelope(map[string]any{handoffMetaKey: map[string]any{
		handoffVersionKey:   int64(handoffVersion),
		handoffDigestKey:    digest,
		handoffSizeBytesKey: float64(len(png)),
	}})
	require.NoError(t, err)
	require.Equal(t, int64(len(png)), envelope.sizeBytes)
	require.Equal(t, digest, envelope.digest)
}

func TestHandoffURIDefects(t *testing.T) {
	root := t.TempDir()
	png := testdataFixture(t, "valid.png")
	path := filepath.Join(root, "valid.png")
	require.NoError(t, os.WriteFile(path, png, 0o600))

	sum := sha256.Sum256(png)
	envelope := map[string]any{
		handoffVersionKey:   handoffVersion,
		handoffDigestKey:    hex.EncodeToString(sum[:]),
		handoffSizeBytesKey: len(png),
	}

	// A URI defect is a defect of the block, so it is invalid_handoff and never
	// a location verdict.
	for _, uri := range []string{
		"",
		"::not a uri::",
		"https://example.com/a.png",
		"file://elsewhere/a.png",
		"file:relative.png",
	} {
		block := acp.ImageBlock("", mimeImagePNG)
		block.Image.Meta = map[string]any{handoffMetaKey: envelope}
		if uri != "" {
			block.Image.Uri = &uri
		}

		_, imageErr := validatePromptImages([]acp.ContentBlock{block}, defaultImageLimits(), root)
		require.NotNil(t, imageErr, uri)
		require.Equal(t, imageErrorInvalidHandoff, imageErr.code, uri)
	}

	// A localhost authority names this host and is accepted.
	localhost := "file://" + handoffLocalhost + filepath.ToSlash(path)
	block := acp.ImageBlock("", mimeImagePNG)
	block.Image.Uri = &localhost
	block.Image.Meta = map[string]any{handoffMetaKey: envelope}

	images, imageErr := validatePromptImages([]acp.ContentBlock{block}, defaultImageLimits(), root)
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

	sum := sha256.Sum256(png)
	envelope := map[string]any{
		handoffVersionKey:   handoffVersion,
		handoffDigestKey:    hex.EncodeToString(sum[:]),
		handoffSizeBytesKey: len(png),
	}
	reject := func(t *testing.T, path string, code string) {
		t.Helper()

		_, imageErr := validatePromptImages(
			[]acp.ContentBlock{handoffBlock(path, mimeImagePNG, envelope)},
			defaultImageLimits(),
			root,
		)
		require.NotNil(t, imageErr)
		require.Equal(t, code, imageErr.code)
		require.NotEmpty(t, imageErr.message)
	}

	t.Run("outside the root", func(t *testing.T) {
		reject(t, escaped, imageErrorPathNotAllowed)
	})

	t.Run("traversal out of the root", func(t *testing.T) {
		reject(t, filepath.Join(root, "..", filepath.Base(outside), "escaped.png"), imageErrorPathNotAllowed)
	})

	t.Run("sibling root prefix", func(t *testing.T) {
		// A directory whose name merely starts with the root must not match.
		sibling := root + "-sibling"
		require.NoError(t, os.MkdirAll(sibling, 0o700))
		t.Cleanup(func() { _ = os.RemoveAll(sibling) })

		path := filepath.Join(sibling, "valid.png")
		require.NoError(t, os.WriteFile(path, png, 0o600))
		reject(t, path, imageErrorPathNotAllowed)
	})

	t.Run("symlink escaping the root", func(t *testing.T) {
		link := filepath.Join(root, "link.png")
		require.NoError(t, os.Symlink(escaped, link))
		reject(t, link, imageErrorPathNotAllowed)
	})

	t.Run("dangling symlink is a containment failure", func(t *testing.T) {
		// The target cannot be shown to be inside the root, and reporting it as
		// a missing file would answer whether an arbitrary outside path exists.
		link := filepath.Join(root, "dangling.png")
		require.NoError(t, os.Symlink(filepath.Join(outside, "never-created.png"), link))
		reject(t, link, imageErrorPathNotAllowed)
	})

	t.Run("unresolvable root", func(t *testing.T) {
		_, imageErr := validatePromptImages(
			[]acp.ContentBlock{handoffBlock(filepath.Join(root, "absent-root", "valid.png"), mimeImagePNG, envelope)},
			defaultImageLimits(),
			filepath.Join(root, "absent-root"),
		)
		require.NotNil(t, imageErr)
		require.Equal(t, imageErrorPathNotAllowed, imageErr.code)
		require.Contains(t, imageErr.message, "configured handoff root cannot be resolved")
	})

	t.Run("inspection failure is a containment failure", func(t *testing.T) {
		statErr := errors.New("lstat refused")
		original := lstatImageFile
		lstatImageFile = func(string) (os.FileInfo, error) { return nil, statErr }
		t.Cleanup(func() { lstatImageFile = original })

		reject(t, inside, imageErrorPathNotAllowed)
	})

	t.Run("directory is not a regular file", func(t *testing.T) {
		reject(t, filepath.Join(root, "subdir"), imageErrorMissingFile)

		dir := filepath.Join(root, "realdir")
		require.NoError(t, os.MkdirAll(dir, 0o700))
		reject(t, dir, imageErrorPathNotAllowed)
	})

	t.Run("device is not a regular file", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("device nodes under a temp root are linux-specific")
		}

		link := filepath.Join(root, "null")
		require.NoError(t, os.Symlink(os.DevNull, link))
		reject(t, link, imageErrorPathNotAllowed)
	})

	t.Run("vanished path inside the root", func(t *testing.T) {
		reject(t, filepath.Join(root, "cleaned-up.png"), imageErrorMissingFile)
	})

	t.Run("the root itself", func(t *testing.T) {
		reject(t, root, imageErrorPathNotAllowed)
	})
}

func TestHandoffRootReachedThroughASymlinkStillResolves(t *testing.T) {
	// The regression this pins: comparing an unresolved host path against a
	// resolved root rejects every legitimate file whenever the root itself is
	// reached through a symlink — which is the normal shape of a temp directory
	// on Darwin, and of any operator-relocated data directory.
	target := t.TempDir()
	root := filepath.Join(t.TempDir(), "handoff-link")
	require.NoError(t, os.Symlink(target, root))

	png := testdataFixture(t, "valid.png")
	block := handoffFixture(t, root, "nested/valid.png", png)

	images, imageErr := validatePromptImages([]acp.ContentBlock{block}, defaultImageLimits(), root)
	require.Nil(t, imageErr)
	require.Equal(t, png, images[0].data)

	// Containment still binds through the symlinked root: a path outside it is
	// refused, and so is one that escapes by traversal.
	outside := filepath.Join(t.TempDir(), "escaped.png")
	require.NoError(t, os.WriteFile(outside, png, 0o600))

	sum := sha256.Sum256(png)
	envelope := map[string]any{
		handoffVersionKey:   handoffVersion,
		handoffDigestKey:    hex.EncodeToString(sum[:]),
		handoffSizeBytesKey: len(png),
	}

	for _, path := range []string{outside, filepath.Join(root, "..", "escaped.png")} {
		_, imageErr = validatePromptImages(
			[]acp.ContentBlock{handoffBlock(path, mimeImagePNG, envelope)},
			defaultImageLimits(),
			root,
		)
		require.NotNil(t, imageErr, path)
		require.Equal(t, imageErrorPathNotAllowed, imageErr.code, path)
	}
}

func TestHandoffReadBound(t *testing.T) {
	// A configured limit is read one byte past, which is what makes an oversize
	// file observable without being resident.
	require.Equal(t, int64(1025), handoffReadBound(1024))

	// A disabled policy limit is not an unbounded read: the bytes come from a
	// path the adapter did not choose, so the hard ACP frame bound applies.
	require.Equal(t, maxACPImageDecodedBytes+1, handoffReadBound(0))
	require.Equal(t, maxACPImageDecodedBytes+1, handoffReadBound(-1))
}

func TestHandoffGrownFileCannotSkipTheByteGate(t *testing.T) {
	root := t.TempDir()
	png := testdataFixture(t, "valid.png")
	block := handoffFixture(t, root, "valid.png", png)

	// A file measured before it grew reports a stale, smaller size. The byte gate
	// must still verdict on what was actually read, or a host could clear both
	// the gate and digest verification by declaring the truncated prefix.
	original := statImageFile
	statImageFile = func(path string) (os.FileInfo, error) {
		info, err := original(path)
		if err != nil {
			return nil, err
		}

		return shrunkFileInfo{FileInfo: info}, nil
	}
	t.Cleanup(func() { statImageFile = original })

	gate := int64(len(png)) - 2
	_, imageErr := validatePromptImages(
		[]acp.ContentBlock{block},
		ImageLimits{MaxInputBytesPerImage: gate},
		root,
	)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorTooLarge, imageErr.code)
	require.Equal(t, gate+1, imageErr.sizeBytes)
	require.Equal(t, gate, imageErr.maxBytes)

	// With room to read the whole file, the stale measurement costs nothing: the
	// bytes are read to the gate bound and verified honestly.
	images, imageErr := validatePromptImages([]acp.ContentBlock{block}, defaultImageLimits(), root)
	require.Nil(t, imageErr)
	require.Equal(t, png, images[0].data)
}

// shrunkFileInfo reports one byte, standing in for a file measured before it
// grew.
type shrunkFileInfo struct {
	os.FileInfo
}

func (shrunkFileInfo) Size() int64 {
	return 1
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

	_, imageErr := validatePromptImages([]acp.ContentBlock{tampered}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorDigestMismatch, imageErr.code)
	require.Contains(t, imageErr.message, "hashes to")

	// A truthful digest over an untruthful size is still a mismatch.
	sum := sha256.Sum256(png)
	wrongSize := handoffBlock(filepath.Join(root, "sized.png"), mimeImagePNG, map[string]any{
		handoffVersionKey:   handoffVersion,
		handoffDigestKey:    hex.EncodeToString(sum[:]),
		handoffSizeBytesKey: len(png) + 1,
	})
	require.NoError(t, os.WriteFile(filepath.Join(root, "sized.png"), png, 0o600))

	_, imageErr = validatePromptImages([]acp.ContentBlock{wrongSize}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorDigestMismatch, imageErr.code)
	require.Contains(t, imageErr.message, "envelope declares")
}

func TestHandoffOversizeFileIsTooLargeNotUnverified(t *testing.T) {
	root := t.TempDir()
	png := testdataFixture(t, "valid.png")

	// A file past the per-image gate is read only to the bound, so its digest
	// cannot be checked — and it is rejected too_large rather than forwarded,
	// with the file's real size reported.
	block := handoffFixture(t, root, "oversize.png", png)
	limits := ImageLimits{MaxInputBytesPerImage: int64(len(png)) - 1}

	_, imageErr := validatePromptImages([]acp.ContentBlock{block}, limits, root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorTooLarge, imageErr.code)
	require.Equal(t, int64(len(png)), imageErr.sizeBytes)
	require.Equal(t, limits.MaxInputBytesPerImage, imageErr.maxBytes)

	// A file whose bytes lie about their digest and also exceed the gate is
	// still rejected: the size verdict wins, and nothing unverified passes.
	require.NoError(t, os.WriteFile(filepath.Join(root, "oversize.png"), append(append([]byte(nil), png...), png...), 0o600))
	_, imageErr = validatePromptImages([]acp.ContentBlock{block}, limits, root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorTooLarge, imageErr.code)

	// Exactly at the gate the whole file fits, so the digest is verified.
	images, imageErr := validatePromptImages(
		[]acp.ContentBlock{handoffFixture(t, root, "exact.png", png)},
		ImageLimits{MaxInputBytesPerImage: int64(len(png))},
		root,
	)
	require.Nil(t, imageErr)
	require.Equal(t, png, images[0].data)

	// A disabled per-image limit reads the whole file and still verifies it.
	images, imageErr = validatePromptImages(
		[]acp.ContentBlock{handoffFixture(t, root, "unbounded.png", png)},
		ImageLimits{},
		root,
	)
	require.Nil(t, imageErr)
	require.Equal(t, png, images[0].data)
}

func TestHandoffReadFailuresAreMissingFile(t *testing.T) {
	root := t.TempDir()
	png := testdataFixture(t, "valid.png")
	block := handoffFixture(t, root, "valid.png", png)

	openErr := errors.New("open refused")
	originalOpen := openImageFile
	openImageFile = func(string) (io.ReadCloser, error) { return nil, openErr }

	_, imageErr := validatePromptImages([]acp.ContentBlock{block}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorMissingFile, imageErr.code)
	require.Contains(t, imageErr.message, openErr.Error())

	openImageFile = originalOpen

	readErr := errors.New("read refused")
	originalRead := readImageFile
	readImageFile = func(io.Reader) ([]byte, error) { return nil, readErr }

	_, imageErr = validatePromptImages([]acp.ContentBlock{block}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorMissingFile, imageErr.code)
	require.Contains(t, imageErr.message, readErr.Error())

	readImageFile = originalRead
}

func TestHandoffResolutionFailuresAreContainmentVerdicts(t *testing.T) {
	root := t.TempDir()
	png := testdataFixture(t, "valid.png")
	block := handoffFixture(t, root, "valid.png", png)

	// A resolver or stat failure that is not a missing file is a containment
	// failure: the adapter cannot prove the path is inside the root.
	resolveErr := errors.New("resolve refused")
	originalEval := evalImageSymlinks
	evalImageSymlinks = func(path string) (string, error) {
		if filepath.Base(path) == "valid.png" {
			return "", resolveErr
		}

		return originalEval(path)
	}

	_, imageErr := validatePromptImages([]acp.ContentBlock{block}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorPathNotAllowed, imageErr.code)
	require.Contains(t, imageErr.message, resolveErr.Error())

	evalImageSymlinks = originalEval

	statErr := errors.New("stat refused")
	originalStat := statImageFile
	statImageFile = func(string) (os.FileInfo, error) { return nil, statErr }

	_, imageErr = validatePromptImages([]acp.ContentBlock{block}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorPathNotAllowed, imageErr.code)
	require.Contains(t, imageErr.message, statErr.Error())

	statImageFile = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

	_, imageErr = validatePromptImages([]acp.ContentBlock{block}, defaultImageLimits(), root)
	require.NotNil(t, imageErr)
	require.Equal(t, imageErrorMissingFile, imageErr.code)

	statImageFile = originalStat
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

	images, imageErr := validatePromptImages(blocks, defaultImageLimits(), handoffRoot)
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
	large := make([]byte, codexInlineImageEnvelopeSize)
	copy(large, png)

	cases := []struct {
		name string
		data []byte
	}{
		{name: "inline data url transport", data: png},
		{name: "local file transport", data: large},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			text := acp.TextBlock("describe this")
			handoff := handoffFixture(t, root, testCase.name+"/fixture.png", testCase.data)
			embedded := acp.ImageBlock(base64.StdEncoding.EncodeToString(testCase.data), mimeImagePNG)

			handoffInput, releaseHandoff := nativePromptRequest(t, []acp.ContentBlock{text, handoff}, root, t.TempDir())
			defer releaseHandoff()

			embeddedInput, releaseEmbedded := nativePromptRequest(t, []acp.ContentBlock{text, embedded}, root, t.TempDir())
			defer releaseEmbedded()

			// The two forms are indistinguishable downstream, modulo the
			// adapter-owned scratch directory a materialized image lands in.
			require.Equal(t, normalizeNativeScratchPaths(t, embeddedInput), normalizeNativeScratchPaths(t, handoffInput))
		})
	}
}

// normalizeNativeScratchPaths replaces each adapter-owned scratch path with its
// final path element, which is the only part of a materialized image path that
// is not a per-turn temporary directory name.
func normalizeNativeScratchPaths(t *testing.T, input []codex.UserInput) []codex.UserInput {
	t.Helper()

	normalized := make([]codex.UserInput, 0, len(input))

	for _, entry := range input {
		clone := codex.UserInput{}
		for key, value := range entry {
			path, isPath := value.(string)
			if key == jsonFieldPath && isPath {
				value = filepath.Base(path)
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
	large := make([]byte, codexInlineImageEnvelopeSize)
	copy(large, testdataFixture(t, "valid.png"))

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

func TestHandoffErrorString(t *testing.T) {
	err := &handoffError{code: imageErrorInvalidHandoff, cause: errors.New("cause")}
	require.Equal(t, imageErrorInvalidHandoff+": cause", err.Error())
}

func TestValidateInputHandoffRoot(t *testing.T) {
	require.NoError(t, validateInputHandoffRoot(""))
	require.NoError(t, validateInputHandoffRoot(t.TempDir()))
	require.Error(t, validateInputHandoffRoot(filepath.Join("relative", "handoff")))

	// Construction fails closed, so a relative root can never be consulted.
	_, err := NewAgent(WithInputHandoffRoot("relative")).Initialize(t.Context(), acp.InitializeRequest{})
	require.Error(t, err)
}
