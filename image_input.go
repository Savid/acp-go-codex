package codexacp

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

const (
	promptImageField             = "prompt.image"
	codexInlineImageEnvelopeSize = 1024 * 1024
	promptImageTempDirPrefix     = "acp-go-codex-image-input-"

	mimeImageGIF  = "image/gif"
	mimeImageJPEG = "image/jpeg"
	mimeImagePNG  = "image/png"
	mimeImageWebP = "image/webp"
)

const (
	imageErrorMissingData         = "missing_data"
	imageErrorInvalidBase64       = "invalid_base64"
	imageErrorInvalidMediaType    = "invalid_media_type"
	imageErrorMediaTypeMismatch   = "media_type_mismatch"
	imageErrorAnimatedUnsupported = "animated_not_supported"
	imageErrorInvalidDimensions   = "invalid_dimensions"
	imageErrorTooLarge            = "too_large"
	imageErrorUnsupportedByModel  = "unsupported_by_model"
	imageErrorInvalidHandoff      = "invalid_handoff"
	imageErrorPathNotAllowed      = "path_not_allowed"
	imageErrorMissingFile         = "missing_file"
	imageErrorDigestMismatch      = "handoff_digest_mismatch"
)

var portableImageMediaTypes = map[string]struct{}{
	mimeImageGIF:  {},
	mimeImageJPEG: {},
	mimeImagePNG:  {},
	mimeImageWebP: {},
}

var (
	createPromptImageTempDir = createPrivateTempDir
	writePromptImageFile     = os.WriteFile
	removePromptImageDir     = os.RemoveAll
)

type promptImageError struct {
	code      string
	message   string
	index     int
	sizeBytes int64
	maxBytes  int64
}

func (e *promptImageError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("prompt image %d: %s", e.index, e.code)
	}

	return fmt.Sprintf("prompt image %d: %s: %s", e.index, e.code, e.message)
}

func (e *promptImageError) invalidParams() error {
	data := map[string]any{
		jsonFieldField: promptImageField,
		jsonFieldError: e.code,
		jsonFieldIndex: e.index,
	}
	if e.message != "" {
		data[jsonFieldMessage] = e.message
	}

	if e.sizeBytes > 0 || e.code == imageErrorTooLarge {
		data["sizeBytes"] = e.sizeBytes
	}

	if e.maxBytes > 0 || e.code == imageErrorTooLarge {
		data["maxBytes"] = e.maxBytes
	}

	return acp.NewInvalidParams(data)
}

type decodedPromptImage struct {
	data     []byte
	mimeType string
	index    int
}

type preparedPromptImages struct {
	images  []codex.PromptImage
	release func()
}

// promptMediaKind classifies an inbound block that carries media bytes.
type promptMediaKind int

const (
	// promptMediaImageBlock is a standard ACP image content block: the only
	// form that can carry a handoff envelope.
	promptMediaImageBlock promptMediaKind = iota
	// promptMediaImageBlob is an embedded blob resource whose declared media
	// type routes to native Codex image input.
	promptMediaImageBlob
	// promptMediaOpaqueBlob is an embedded blob resource of any other declared
	// type. Codex forwards it as a reference line and drops its bytes, but the
	// bytes still crossed ACP, so they are still gated.
	promptMediaOpaqueBlob
)

// nativeImage reports whether the block maps to native Codex image input and so
// takes the raster gates and a materialized transport.
func (k promptMediaKind) nativeImage() bool {
	return k == promptMediaImageBlock || k == promptMediaImageBlob
}

// promptMedia is one inbound block carrying bytes the input gates must account
// for, flattened out of the block union so both transports read one shape.
type promptMedia struct {
	kind     promptMediaKind
	data     string
	mimeType string
	uri      string
	meta     map[string]any
}

// gatedPromptBytes is one block's bytes after form-specific decoding, ready for
// the byte limits. size is what a byte-limit rejection reports and can exceed
// len(data) when a bounded handoff read truncated an oversize file.
type gatedPromptBytes struct {
	data []byte
	size int64
}

// validatePromptImages runs the pinned input gate order over every media-bearing
// block in request order and stops at the first failure. Indexes are assigned in
// that same order across every gated block, so one rejection always names
// exactly one block.
func validatePromptImages(
	blocks []acp.ContentBlock,
	limits ImageLimits,
	handoffRoot string,
) ([]decodedPromptImage, *promptImageError) {
	images := make([]decodedPromptImage, 0)

	var (
		promptBytes int64
		index       int
	)

	for _, block := range blocks {
		media, ok := promptMediaBlock(block)
		if !ok {
			continue
		}

		gated, mediaErr := decodePromptMedia(media, index, limits, handoffRoot)
		if mediaErr != nil {
			return nil, mediaErr
		}

		if limits.MaxInputBytesPerImage > 0 && gated.size > limits.MaxInputBytesPerImage {
			return nil, &promptImageError{
				code:      imageErrorTooLarge,
				index:     index,
				sizeBytes: gated.size,
				maxBytes:  limits.MaxInputBytesPerImage,
			}
		}

		promptBytes += gated.size
		if limits.MaxInputBytesPerPrompt > 0 && promptBytes > limits.MaxInputBytesPerPrompt {
			return nil, &promptImageError{
				code:      imageErrorTooLarge,
				index:     index,
				sizeBytes: promptBytes,
				maxBytes:  limits.MaxInputBytesPerPrompt,
			}
		}

		if media.kind.nativeImage() {
			images = append(images, decodedPromptImage{data: gated.data, mimeType: media.mimeType, index: index})
		}

		index++
	}

	return images, nil
}

// decodePromptMedia runs every gate that precedes the byte limits for one block:
// the handoff pre-gate or the embedded base64 decode, then the raster gates for
// a block that maps to native image input. The byte limits stay with the caller,
// because they are the only gates that depend on the running prompt total.
func decodePromptMedia(
	media promptMedia,
	index int,
	limits ImageLimits,
	handoffRoot string,
) (gatedPromptBytes, *promptImageError) {
	if !media.kind.nativeImage() {
		return decodeOpaquePromptBlob(media, index)
	}

	if media.kind == promptMediaImageBlock && media.data == "" && handoffIntent(media) {
		return decodeHandoffPromptImage(media, index, limits, handoffRoot)
	}

	return decodeEmbeddedPromptImage(media, index)
}

// decodeEmbeddedPromptImage gates the embedded form: non-empty data wins over a
// URI, which is provenance only and never fetched.
func decodeEmbeddedPromptImage(media promptMedia, index int) (gatedPromptBytes, *promptImageError) {
	if media.data == "" {
		return gatedPromptBytes{}, &promptImageError{code: imageErrorMissingData, index: index}
	}

	if mediaErr := checkPromptImageMediaType(media.mimeType, index); mediaErr != nil {
		return gatedPromptBytes{}, mediaErr
	}

	decoded, err := base64.StdEncoding.DecodeString(media.data)
	if err != nil {
		return gatedPromptBytes{}, &promptImageError{code: imageErrorInvalidBase64, index: index}
	}

	if mediaErr := checkPromptRaster(decoded, media.mimeType, index); mediaErr != nil {
		return gatedPromptBytes{}, mediaErr
	}

	return gatedPromptBytes{data: decoded, size: int64(len(decoded))}, nil
}

// decodeHandoffPromptImage gates the handoff form. The pre-gate runs ahead of
// every embedded gate, and the bytes it produces then take the same raster gates
// embedded bytes take, so the two forms accept and reject identically.
func decodeHandoffPromptImage(
	media promptMedia,
	index int,
	limits ImageLimits,
	handoffRoot string,
) (gatedPromptBytes, *promptImageError) {
	read, handoffErr := readPromptHandoff(handoffRoot, media, limits.MaxInputBytesPerImage)
	if handoffErr != nil {
		return gatedPromptBytes{}, &promptImageError{
			code:    handoffErr.code,
			message: handoffErr.cause.Error(),
			index:   index,
		}
	}

	if mediaErr := checkPromptImageMediaType(media.mimeType, index); mediaErr != nil {
		return gatedPromptBytes{}, mediaErr
	}

	if mediaErr := checkPromptRaster(read.data, media.mimeType, index); mediaErr != nil {
		return gatedPromptBytes{}, mediaErr
	}

	return gatedPromptBytes{data: read.data, size: read.size}, nil
}

// decodeOpaquePromptBlob gates a blob resource that reaches no native image
// transport. Codex drops such bytes downstream, but they still arrived over ACP
// and are still charged against the byte limits, so the channel cannot carry
// unbounded or undecodable payloads.
func decodeOpaquePromptBlob(media promptMedia, index int) (gatedPromptBytes, *promptImageError) {
	decoded, err := base64.StdEncoding.DecodeString(media.data)
	if err != nil {
		return gatedPromptBytes{}, &promptImageError{code: imageErrorInvalidBase64, index: index}
	}

	return gatedPromptBytes{data: decoded, size: int64(len(decoded))}, nil
}

// checkPromptImageMediaType applies the four-format allowlist to the media type
// exactly as declared. Routing normalizes casing and parameters; acceptance does
// not, so a non-canonical declaration is rejected rather than repaired.
func checkPromptImageMediaType(mimeType string, index int) *promptImageError {
	if _, accepted := portableImageMediaTypes[mimeType]; !accepted {
		return &promptImageError{code: imageErrorInvalidMediaType, index: index}
	}

	return nil
}

// checkPromptRaster runs the decode-free structural gates in their pinned order:
// format recognition, dimensions, animation, then declared-versus-sniffed.
func checkPromptRaster(data []byte, mimeType string, index int) *promptImageError {
	raster, err := inspectPromptRaster(data)

	switch {
	case errors.Is(err, errUnknownRasterFormat):
		return &promptImageError{code: imageErrorMediaTypeMismatch, index: index}
	case err != nil:
		return &promptImageError{code: imageErrorInvalidDimensions, index: index}
	}

	if raster.animated {
		return &promptImageError{code: imageErrorAnimatedUnsupported, index: index}
	}

	if raster.mimeType != mimeType {
		return &promptImageError{code: imageErrorMediaTypeMismatch, index: index}
	}

	return nil
}

// promptMediaBlock flattens a content block into the media shape the gates read,
// reporting false for a block that carries no gated bytes.
func promptMediaBlock(block acp.ContentBlock) (promptMedia, bool) {
	if block.Image != nil {
		media := promptMedia{
			kind:     promptMediaImageBlock,
			data:     block.Image.Data,
			mimeType: block.Image.MimeType,
			meta:     block.Image.Meta,
		}
		if block.Image.Uri != nil {
			media.uri = *block.Image.Uri
		}

		return media, true
	}

	if block.Resource == nil || block.Resource.Resource.BlobResourceContents == nil {
		return promptMedia{}, false
	}

	blob := block.Resource.Resource.BlobResourceContents

	declared := ""
	if blob.MimeType != nil {
		declared = *blob.MimeType
	}

	kind := promptMediaOpaqueBlob
	if codex.IsImageMediaType(declared) {
		kind = promptMediaImageBlob
	}

	return promptMedia{kind: kind, data: blob.Blob, mimeType: declared, uri: blob.Uri}, true
}

func (s *session) preparePromptImages(ctx context.Context, images []decodedPromptImage) (preparedPromptImages, error) {
	if len(images) == 0 {
		return preparedPromptImages{release: func() {}}, nil
	}

	native := make([]codex.PromptImage, len(images))
	local := false

	for index, image := range images {
		if base64.StdEncoding.EncodedLen(len(image.data))+len(image.mimeType)+13 <= codexInlineImageEnvelopeSize {
			native[index].DataURL = "data:" + image.mimeType + ";base64," + base64.StdEncoding.EncodeToString(image.data)

			continue
		}

		local = true
	}

	if !local {
		return preparedPromptImages{images: native, release: func() {}}, nil
	}

	reservation, err := s.agent.reserveScratchRoot(ctx, RuntimeResourcePrompt)
	if err != nil {
		return preparedPromptImages{}, err
	}

	dir, err := createPromptImageTempDir(s.agent.options.ScratchDir, promptImageTempDirPrefix)
	if err != nil {
		reservation()

		return preparedPromptImages{}, fmt.Errorf("create prompt image scratch: %w", err)
	}

	release := func() {
		if removeErr := removePromptImageDir(dir); removeErr == nil {
			reservation()
		}
	}

	for index, image := range images {
		if native[index].DataURL != "" {
			continue
		}

		extension := portableImageExtension(image.mimeType)

		path := filepath.Join(dir, fmt.Sprintf("image-%d%s", image.index, extension))
		if err := writePromptImageFile(path, image.data, 0o600); err != nil {
			release()

			return preparedPromptImages{}, fmt.Errorf("write prompt image scratch: %w", err)
		}

		native[index].LocalPath = path
	}

	return preparedPromptImages{images: native, release: release}, nil
}

func portableImageExtension(mimeType string) string {
	switch mimeType {
	case mimeImagePNG:
		return ".png"
	case mimeImageJPEG:
		return ".jpg"
	case mimeImageGIF:
		return ".gif"
	case mimeImageWebP:
		return ".webp"
	default:
		return ""
	}
}

type rasterInfo struct {
	mimeType string
	width    int
	height   int
	animated bool
}

// errUnknownRasterFormat reports bytes whose magic matches no allowlisted
// raster; a recognized header with no valid dimensions returns a plain error.
var errUnknownRasterFormat = errors.New("bytes do not sniff as a known raster format")

func inspectPromptRaster(data []byte) (rasterInfo, error) {
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return inspectPNG(data)
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return inspectJPEG(data)
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return inspectGIF(data)
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return inspectWebP(data)
	default:
		return rasterInfo{}, errUnknownRasterFormat
	}
}

func inspectPNG(data []byte) (rasterInfo, error) {
	if len(data) < 33 || string(data[12:16]) != "IHDR" || binary.BigEndian.Uint32(data[8:12]) != 13 {
		return rasterInfo{}, errors.New("invalid PNG dimensions")
	}

	width := int(binary.BigEndian.Uint32(data[16:20]))

	height := int(binary.BigEndian.Uint32(data[20:24]))
	if width <= 0 || height <= 0 {
		return rasterInfo{}, errors.New("invalid PNG dimensions")
	}

	animated := false

	for offset := 8; offset+12 <= len(data); {
		length := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		if length < 0 || offset+12+length > len(data) {
			break
		}

		chunkType := string(data[offset+4 : offset+8])
		if chunkType == "acTL" {
			animated = true

			break
		}

		if chunkType == "IDAT" || chunkType == "IEND" {
			break
		}

		offset += 12 + length
	}

	return rasterInfo{mimeType: mimeImagePNG, width: width, height: height, animated: animated}, nil
}

func inspectJPEG(data []byte) (rasterInfo, error) {
	for offset := 2; offset+1 < len(data); {
		if data[offset] != 0xff {
			offset++

			continue
		}

		for offset < len(data) && data[offset] == 0xff {
			offset++
		}

		if offset >= len(data) {
			break
		}

		marker := data[offset]
		offset++

		if marker == 0xd8 || marker == 0xd9 || marker == 0x01 || marker >= 0xd0 && marker <= 0xd7 {
			continue
		}

		if offset+2 > len(data) {
			break
		}

		length := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		if length < 2 || offset+length > len(data) {
			break
		}

		if jpegSOFMarker(marker) && length >= 7 {
			height := int(binary.BigEndian.Uint16(data[offset+3 : offset+5]))

			width := int(binary.BigEndian.Uint16(data[offset+5 : offset+7]))
			if width <= 0 || height <= 0 {
				break
			}

			return rasterInfo{mimeType: mimeImageJPEG, width: width, height: height}, nil
		}

		offset += length
	}

	return rasterInfo{}, errors.New("invalid JPEG dimensions")
}

func jpegSOFMarker(marker byte) bool {
	switch marker {
	case 0xc0, 0xc1, 0xc2, 0xc3, 0xc5, 0xc6, 0xc7, 0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf:
		return true
	default:
		return false
	}
}

func inspectGIF(data []byte) (rasterInfo, error) {
	if len(data) < 13 {
		return rasterInfo{}, errors.New("invalid GIF dimensions")
	}

	width := int(binary.LittleEndian.Uint16(data[6:8]))

	height := int(binary.LittleEndian.Uint16(data[8:10]))
	if width <= 0 || height <= 0 {
		return rasterInfo{}, errors.New("invalid GIF dimensions")
	}

	offset := 13
	if data[10]&0x80 != 0 {
		offset += 3 * (1 << ((data[10] & 0x07) + 1))
	}

	images := 0

	for offset < len(data) {
		switch data[offset] {
		case 0x2c:
			images++

			if images > 1 {
				return rasterInfo{mimeType: mimeImageGIF, width: width, height: height, animated: true}, nil
			}

			if offset+10 > len(data) {
				return rasterInfo{mimeType: mimeImageGIF, width: width, height: height}, nil
			}

			packed := data[offset+9]

			offset += 10
			if packed&0x80 != 0 {
				offset += 3 * (1 << ((packed & 0x07) + 1))
			}

			if offset >= len(data) {
				return rasterInfo{mimeType: mimeImageGIF, width: width, height: height}, nil
			}

			offset++
			offset = skipGIFSubBlocks(data, offset)
		case 0x21:
			if offset+2 > len(data) {
				return rasterInfo{mimeType: mimeImageGIF, width: width, height: height}, nil
			}

			offset = skipGIFSubBlocks(data, offset+2)
		case 0x3b:
			return rasterInfo{mimeType: mimeImageGIF, width: width, height: height}, nil
		default:
			return rasterInfo{mimeType: mimeImageGIF, width: width, height: height}, nil
		}
	}

	return rasterInfo{mimeType: mimeImageGIF, width: width, height: height}, nil
}

func skipGIFSubBlocks(data []byte, offset int) int {
	for offset < len(data) {
		size := int(data[offset])
		offset++

		if size == 0 {
			break
		}

		if offset+size > len(data) {
			return len(data)
		}

		offset += size
	}

	return offset
}

func inspectWebP(data []byte) (rasterInfo, error) {
	for offset := 12; offset+8 <= len(data); {
		chunkType := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))

		payload := offset + 8
		if size < 0 || payload+size > len(data) {
			break
		}

		switch chunkType {
		case "VP8X":
			if size < 10 {
				break
			}

			width := 1 + int(data[payload+4]) + int(data[payload+5])<<8 + int(data[payload+6])<<16
			height := 1 + int(data[payload+7]) + int(data[payload+8])<<8 + int(data[payload+9])<<16

			return rasterInfo{
				mimeType: mimeImageWebP,
				width:    width,
				height:   height,
				animated: data[payload]&0x02 != 0,
			}, nil
		case "VP8 ":
			if size >= 10 && data[payload+3] == 0x9d && data[payload+4] == 0x01 && data[payload+5] == 0x2a {
				width := int(binary.LittleEndian.Uint16(data[payload+6:payload+8]) & 0x3fff)

				height := int(binary.LittleEndian.Uint16(data[payload+8:payload+10]) & 0x3fff)
				if width > 0 && height > 0 {
					return rasterInfo{mimeType: mimeImageWebP, width: width, height: height}, nil
				}
			}
		case "VP8L":
			if size >= 5 && data[payload] == 0x2f {
				bits := binary.LittleEndian.Uint32(data[payload+1 : payload+5])
				width := 1 + int(bits&0x3fff)
				height := 1 + int(bits>>14&0x3fff)

				return rasterInfo{mimeType: mimeImageWebP, width: width, height: height}, nil
			}
		}

		offset = payload + size
		if size%2 != 0 {
			offset++
		}
	}

	return rasterInfo{}, errors.New("invalid WebP dimensions")
}
