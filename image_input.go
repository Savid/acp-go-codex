package codexacp

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

const (
	promptImageField             = "prompt.image"
	promptResourceField          = "prompt.resource"
	codexInlineImageEnvelopeSize = 1024 * 1024
	promptImageTempDirPrefix     = "acp-go-codex-image-input-"

	// promptImageScratchFailure is what a host is told when the adapter cannot
	// stage validated prompt images. The underlying operating-system error names
	// the adapter's scratch directory, which is the adapter's business.
	promptImageScratchFailure = "prompt images could not be staged for the harness"

	// maxHandoffBlocksPerPrompt bounds how many handoff-form blocks one prompt
	// may read. The handoff form is what decouples a block's wire size from the
	// bytes it costs, so the request frame no longer bounds the work a single
	// prompt can demand and a count has to. It sits far above any conforming
	// host's per-message image count, so it binds only a host that is hostile
	// or misconfigured.
	maxHandoffBlocksPerPrompt = 64

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

// portableImageMediaTypes is the inbound image media-type allowlist, in the
// order the media envelope advertises it. It is the only such list: the
// advertisement is a copy of this slice rather than a second list beside it, so
// an accepted format and an advertised format cannot become different sets.
var portableImageMediaTypes = []string{mimeImagePNG, mimeImageJPEG, mimeImageGIF, mimeImageWebP}

var (
	createPromptImageTempDir = createPrivateTempDir
	writePromptImageFile     = os.WriteFile
	removePromptImageDir     = os.RemoveAll
)

// promptImageError is one input gate's refusal. field names the inbound block
// type the refusal is about, which is not the same question as which gate chain
// the block's declared media type routed it into.
type promptImageError struct {
	code      string
	field     string
	message   string
	index     int
	sizeBytes int64
	maxBytes  int64
}

func (e *promptImageError) invalidParams() error {
	data := map[string]any{
		jsonFieldField: e.field,
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
	field    string
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
	// promptMediaTextResource is an embedded text resource. Codex flattens its
	// text straight into prompt text, so there is nothing to decode and no
	// raster to inspect, but the bytes crossed ACP and spend the same prompt
	// budget any other inbound bytes spend.
	promptMediaTextResource
)

// nativeImage reports whether the block maps to native Codex image input and so
// takes the raster gates and a materialized transport.
func (k promptMediaKind) nativeImage() bool {
	return k == promptMediaImageBlock || k == promptMediaImageBlob
}

// perImageBounded reports whether the block's bytes take the per-image byte
// limit. A text resource carries no image, so an image limit is not its bound;
// the per-prompt aggregate is the bound it shares with everything else.
func (k promptMediaKind) perImageBounded() bool {
	return k != promptMediaTextResource
}

// inputField is the error envelope's field for a block of this kind. A declared
// media type decides which gate chain a block takes; it does not decide what the
// block is, so a resource stays a resource however its bytes were gated.
func (k promptMediaKind) inputField() string {
	if k == promptMediaImageBlock {
		return promptImageField
	}

	return promptResourceField
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

// validatePromptImages runs the pinned input gate order over every media-bearing
// block in request order and stops at the first failure. Indexes are assigned in
// that same order across every gated block, so one rejection always names
// exactly one block.
//
// A gate refusal and an abort are different answers: the refusal describes a
// block the host can fix, while the error return means the caller stopped
// waiting and no verdict was reached.
func validatePromptImages(
	ctx context.Context,
	blocks []acp.ContentBlock,
	limits ImageLimits,
	handoffRoot string,
) ([]decodedPromptImage, *promptImageError, error) {
	images := make([]decodedPromptImage, 0)

	maxImageBytes := effectiveInputBytesPerImage(limits.MaxInputBytesPerImage)
	maxPromptBytes := effectiveInputBytesPerPrompt(limits.MaxInputBytesPerPrompt)

	var (
		promptBytes   int64
		handoffBlocks int64
		index         int
	)

	for _, block := range blocks {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}

		media, ok := promptMediaBlock(block)
		if !ok {
			continue
		}

		// An adapter with no read root reads nothing, so there is no work here
		// for the count to bound, and the root-unset invalid_handoff the read
		// reports is how a host learns its root never arrived. Nothing may stand
		// in front of it, so the count sits behind the root.
		if handoffRoot != "" && handoffForm(media) {
			handoffBlocks++

			// Counted before the block is read, because bounding the reads is
			// the whole point of counting them.
			if handoffBlocks > maxHandoffBlocksPerPrompt {
				return nil, &promptImageError{
					code:      imageErrorTooLarge,
					field:     media.kind.inputField(),
					index:     index,
					sizeBytes: handoffBlocks,
					maxBytes:  maxHandoffBlocksPerPrompt,
				}, nil
			}
		}

		data, mediaErr, err := decodePromptMedia(ctx, media, index, maxImageBytes, handoffRoot)
		if err != nil {
			return nil, nil, err
		}

		if mediaErr != nil {
			return nil, mediaErr, nil
		}

		size := int64(len(data))
		if media.kind.perImageBounded() && size > maxImageBytes {
			return nil, &promptImageError{
				code:      imageErrorTooLarge,
				field:     media.kind.inputField(),
				index:     index,
				sizeBytes: size,
				maxBytes:  maxImageBytes,
			}, nil
		}

		promptBytes += size
		if maxPromptBytes > 0 && promptBytes > maxPromptBytes {
			return nil, &promptImageError{
				code:      imageErrorTooLarge,
				field:     media.kind.inputField(),
				index:     index,
				sizeBytes: promptBytes,
				maxBytes:  maxPromptBytes,
			}, nil
		}

		if media.kind.nativeImage() {
			images = append(images, decodedPromptImage{
				data:     data,
				mimeType: media.mimeType,
				field:    media.kind.inputField(),
				index:    index,
			})
		}

		index++
	}

	return images, nil, nil
}

// handoffForm reports whether a block selects the handoff transport: an image
// block carrying no embedded bytes that asked for it.
func handoffForm(media promptMedia) bool {
	return media.kind == promptMediaImageBlock && media.data == "" && handoffIntent(media)
}

// decodePromptMedia runs every gate that precedes the byte limits for one block:
// the handoff pre-gate or the embedded base64 decode, then the raster gates for
// a block that maps to native image input. The byte limits stay with the caller,
// because they are the only gates that depend on the running prompt total.
func decodePromptMedia(
	ctx context.Context,
	media promptMedia,
	index int,
	maxImageBytes int64,
	handoffRoot string,
) ([]byte, *promptImageError, error) {
	if media.kind == promptMediaTextResource {
		// The text is already the bytes: there is nothing to decode, and the
		// only gate it takes is the prompt aggregate its caller charges.
		return []byte(media.data), nil, nil
	}

	if !media.kind.nativeImage() {
		data, mediaErr := decodeOpaquePromptBlob(media, index)

		return data, mediaErr, nil
	}

	if handoffForm(media) {
		return decodeHandoffPromptImage(ctx, media, index, maxImageBytes, handoffRoot)
	}

	data, mediaErr := decodeEmbeddedPromptImage(media, index)

	return data, mediaErr, nil
}

// decodeEmbeddedPromptImage gates the embedded form: non-empty data wins over a
// URI, which is provenance only and never fetched.
func decodeEmbeddedPromptImage(media promptMedia, index int) ([]byte, *promptImageError) {
	field := media.kind.inputField()
	if media.data == "" {
		return nil, &promptImageError{code: imageErrorMissingData, field: field, index: index}
	}

	if mediaErr := checkPromptImageMediaType(media.mimeType, field, index); mediaErr != nil {
		return nil, mediaErr
	}

	decoded, err := base64.StdEncoding.DecodeString(media.data)
	if err != nil {
		return nil, &promptImageError{code: imageErrorInvalidBase64, field: field, index: index}
	}

	if mediaErr := checkPromptRaster(decoded, media.mimeType, field, index); mediaErr != nil {
		return nil, mediaErr
	}

	return decoded, nil
}

// decodeHandoffPromptImage gates the handoff form. The pre-gate runs ahead of
// every embedded gate — including the allowlist, which it applies itself so a
// media type this adapter refuses costs no read — and the bytes it produces then
// take the same raster gates embedded bytes take, so the two forms accept and
// reject identically.
func decodeHandoffPromptImage(
	ctx context.Context,
	media promptMedia,
	index int,
	maxImageBytes int64,
	handoffRoot string,
) ([]byte, *promptImageError, error) {
	field := media.kind.inputField()

	data, verdict, err := readPromptHandoff(ctx, handoffRoot, media, maxImageBytes)
	if err != nil {
		return nil, nil, err
	}

	if verdict != nil {
		return nil, &promptImageError{
			code:      verdict.code,
			field:     field,
			message:   verdict.message,
			index:     index,
			sizeBytes: verdict.sizeBytes,
			maxBytes:  verdict.maxBytes,
		}, nil
	}

	if mediaErr := checkPromptRaster(data, media.mimeType, field, index); mediaErr != nil {
		return nil, mediaErr, nil
	}

	return data, nil, nil
}

// decodeOpaquePromptBlob gates a blob resource that reaches no native image
// transport. Codex drops such bytes downstream, but they still arrived over ACP
// and are still charged against the byte limits, so the channel cannot carry
// unbounded or undecodable payloads.
func decodeOpaquePromptBlob(media promptMedia, index int) ([]byte, *promptImageError) {
	decoded, err := base64.StdEncoding.DecodeString(media.data)
	if err != nil {
		return nil, &promptImageError{
			code:  imageErrorInvalidBase64,
			field: media.kind.inputField(),
			index: index,
		}
	}

	return decoded, nil
}

// checkPromptImageMediaType applies the four-format allowlist to the media type
// exactly as declared. Routing normalizes casing and parameters; acceptance does
// not, so a non-canonical declaration is rejected rather than repaired.
func checkPromptImageMediaType(mimeType string, field string, index int) *promptImageError {
	if !slices.Contains(portableImageMediaTypes, mimeType) {
		return &promptImageError{code: imageErrorInvalidMediaType, field: field, index: index}
	}

	return nil
}

// checkPromptRaster runs the decode-free structural gates in their pinned order:
// format recognition, dimensions, animation, then declared-versus-sniffed.
func checkPromptRaster(data []byte, mimeType string, field string, index int) *promptImageError {
	raster, err := inspectPromptRaster(data)

	switch {
	case errors.Is(err, errUnknownRasterFormat):
		return &promptImageError{code: imageErrorMediaTypeMismatch, field: field, index: index}
	case err != nil:
		return &promptImageError{code: imageErrorInvalidDimensions, field: field, index: index}
	}

	if raster.animated {
		return &promptImageError{code: imageErrorAnimatedUnsupported, field: field, index: index}
	}

	if raster.mimeType != mimeType {
		return &promptImageError{code: imageErrorMediaTypeMismatch, field: field, index: index}
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

	if block.Resource == nil {
		return promptMedia{}, false
	}

	if text := block.Resource.Resource.TextResourceContents; text != nil {
		return promptMedia{kind: promptMediaTextResource, data: text.Text, uri: text.Uri}, true
	}

	if block.Resource.Resource.BlobResourceContents == nil {
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

	var residenceBytes int64

	for _, image := range images {
		if base64.StdEncoding.EncodedLen(len(image.data))+len(image.mimeType)+13 > codexInlineImageEnvelopeSize {
			residenceBytes += int64(len(image.data))
		}
	}

	if err := s.agent.ensureRetiredResidenceCapacity(ctx, residenceBytes); err != nil {
		return preparedPromptImages{}, err
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

	if s.agent.options.HostAuthority != nil {
		if err := s.agent.options.HostAuthority.PrepareNativeTree(ctx, dir); err != nil {
			release()

			return preparedPromptImages{}, err
		}

		s.agent.mu.Lock()
		epoch := s.agent.runtimeEpoch
		s.agent.mu.Unlock()

		var once sync.Once

		release = func() {
			once.Do(func() {
				_ = s.agent.retireNativeResidenceAtEpoch(
					dir, dir, residenceBytes, reservation, epoch, removePromptImageDir,
				)
			})
		}
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
