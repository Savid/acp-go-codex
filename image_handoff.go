package codexacp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	handoffMetaKey      = "acp-go.dev/handoff"
	handoffVersion      = 1
	handoffVersionKey   = "version"
	handoffVersionsKey  = "versions"
	handoffDigestKey    = "digest"
	handoffSizeBytesKey = "sizeBytes"
	handoffURIScheme    = "file"
	handoffLocalhost    = "localhost"

	// handoffEnvelopeFields is the exact field count of a handoff envelope.
	// Comparing the count is what rejects an unknown field and a missing one
	// with the same check.
	handoffEnvelopeFields = 3
	handoffDigestLength   = sha256.Size * 2

	// handoffParentName is the name every path not spelled under the root is
	// handed over as. A confined root refuses it, which is where containment is
	// decided.
	handoffParentName = ".."

	// handoffNumberCeiling is 2^63 as a float64: the first value an int64
	// cannot hold. A decoded JSON number is range-checked against it before any
	// conversion, because Go leaves an out-of-range float-to-integer conversion
	// undefined and the architectures this builds for do not agree on it.
	handoffNumberCeiling = 9223372036854775808.0
)

// Every client-visible handoff refusal message is one of these constants. A
// refusal names the stage that refused and nothing the caller could not already
// state: a resolved path, a filename, an observed digest, an observed byte
// count, or the text of an operating-system error would each answer questions
// about files the caller cannot otherwise see, and the same string is
// republished to every consumer of the adapter's traces.
const (
	handoffCauseRootUnset         = "no input handoff root is configured"
	handoffCauseRootUnopenable    = "configured handoff root cannot be opened"
	handoffCauseEnvelopeMissing   = "_meta." + handoffMetaKey + " is required"
	handoffCauseEnvelopeNotObject = "_meta." + handoffMetaKey + " must be an object"
	handoffCauseEnvelopeFields    = "_meta." + handoffMetaKey + " must contain exactly version, digest, and sizeBytes"
	handoffCauseEnvelopeVersion   = "_meta." + handoffMetaKey + ".version must be the supported handoff version"
	handoffCauseEnvelopeDigest    = "_meta." + handoffMetaKey + ".digest must be a lowercase hex sha256"
	handoffCauseEnvelopeSizeBytes = "_meta." + handoffMetaKey + ".sizeBytes must be a non-negative integer"
	handoffCauseURIMissing        = "uri is required"
	handoffCauseURIUnparseable    = "uri is not parseable"
	handoffCauseURIScheme         = "uri scheme is not " + handoffURIScheme
	handoffCauseURIHost           = "uri host is not this host"
	handoffCauseURIRelative       = "uri path is not absolute"
	handoffCauseOutsideRoot       = "path cannot be opened inside the configured handoff root"
	handoffCauseNotRegular        = "path is not a regular file"
	handoffCauseMissing           = "path does not exist"
	handoffCauseUnreadable        = "path cannot be read"
	handoffCauseDigestMismatch    = "handoff file does not match the declared envelope"
)

// handoffEnvelope is a validated acp-go.dev/handoff descriptor: the host's claim
// about the bytes behind the block's file URI.
type handoffEnvelope struct {
	digest    string
	sizeBytes int64
}

// handoffVerdict is a handoff pre-gate refusal: the input taxonomy code, the
// constant message the host is told, and the byte counts a size refusal reports.
type handoffVerdict struct {
	code      string
	message   string
	sizeBytes int64
	maxBytes  int64
}

// openHandoffImage opens one host-named path for reading, confined to the
// configured root. Containment is the kernel's answer to the open rather than a
// decision taken beforehand, so nothing can change between deciding a path is
// admissible and reading it; the descriptor is then required to be a regular
// file, because a confined root deliberately still opens a directory, a device
// node, or a FIFO.
//
// What it returns is therefore already proven in-root and regular, which is why
// the caller only has to bound and verify the bytes.
var openHandoffImage = func(root string, path string) (io.ReadCloser, *handoffVerdict) {
	confined, err := os.OpenRoot(root)
	if err != nil {
		return nil, &handoffVerdict{code: imageErrorPathNotAllowed, message: handoffCauseRootUnopenable}
	}

	defer func() { _ = confined.Close() }()

	file, err := confined.OpenFile(handoffRelativeName(root, path), os.O_RDONLY|handoffOpenFlags, 0)
	if err != nil {
		return nil, handoffLocationVerdict(err)
	}

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()

		return nil, &handoffVerdict{code: imageErrorPathNotAllowed, message: handoffCauseNotRegular}
	}

	return file, nil
}

// validateInputHandoffRoot rejects a relative handoff root at construction. The
// root is compared against host-supplied absolute paths, so a relative root
// would silently resolve against the adapter's working directory.
func validateInputHandoffRoot(dir string) error {
	if dir == "" || filepath.IsAbs(dir) {
		return nil
	}

	return fmt.Errorf("InputHandoffRoot must be an absolute path, got %q", dir)
}

// handoffIntent reports whether a block asked for the handoff transport: a
// handoff envelope, or a file URI. Intent is what separates a block that tried
// to be handoff and failed from a plain empty-data block, which stays
// missing_data.
func handoffIntent(media promptMedia) bool {
	if _, declared := media.meta[handoffMetaKey]; declared {
		return true
	}

	parsed, err := url.Parse(media.uri)

	return err == nil && parsed.Scheme == handoffURIScheme
}

// readPromptHandoff resolves, reads, and digest-verifies one handoff-form image
// block, returning the bytes the embedded gate chain then validates.
//
// Every verdict the request decides on its own comes first: a block malformed as
// a block is invalid_handoff, a media type outside the allowlist is
// invalid_media_type, and a declared size past the per-image bound is too_large.
// None of them opens anything, so a block this adapter was never going to accept
// costs no read and no hash, and a refused declaration does not report whether
// the path it named exists. The filesystem then answers the rest: a path the
// confined root refuses is path_not_allowed, and a name inside the root that is
// absent — including a link whose target the host already removed — is
// missing_file.
//
// Nothing about the file's own size reaches a verdict. The read is bounded by the
// envelope's own declared size, so the work one block commits this adapter to is
// the work its host asked for, and a file that grows, shrinks, or is replaced
// fails verification rather than passing on a measurement taken at another
// moment.
//
// A cancelled context aborts the block rather than verdicting it: the caller is
// no longer waiting for an answer, and the read is the longest thing the
// validation phase does.
func readPromptHandoff(
	ctx context.Context,
	root string,
	media promptMedia,
	maxBytes int64,
) ([]byte, *handoffVerdict, error) {
	if root == "" {
		return nil, &handoffVerdict{code: imageErrorInvalidHandoff, message: handoffCauseRootUnset}, nil
	}

	envelope, message := parseHandoffEnvelope(media.meta)
	if message != "" {
		return nil, &handoffVerdict{code: imageErrorInvalidHandoff, message: message}, nil
	}

	path, message := handoffURIPath(media.uri)
	if message != "" {
		return nil, &handoffVerdict{code: imageErrorInvalidHandoff, message: message}, nil
	}

	if !slices.Contains(portableImageMediaTypes, media.mimeType) {
		return nil, &handoffVerdict{code: imageErrorInvalidMediaType}, nil
	}

	// The size gate reads the host's own declaration, so an oversize block is
	// refused with nothing opened and without measuring a file the caller may not
	// be entitled to measure.
	if envelope.sizeBytes > maxBytes {
		return nil, &handoffVerdict{
			code:      imageErrorTooLarge,
			sizeBytes: envelope.sizeBytes,
			maxBytes:  maxBytes,
		}, nil
	}

	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	file, verdict := openHandoffImage(root, path)
	if verdict != nil {
		return nil, verdict, nil
	}

	// Every gate below this line refuses by returning, so the descriptor is
	// released here rather than at each of them.
	defer func() { _ = file.Close() }()

	// One byte past the declaration is enough to see that the file is not the one
	// the envelope described and never enough to hold a file the host did not ask
	// this adapter to read. Anything longer than declared fails verification
	// below: a file bigger than it claims is not the file the digest covers.
	data, err := readImageFile(io.LimitReader(file, envelope.sizeBytes+1))
	if err != nil {
		return nil, &handoffVerdict{code: imageErrorMissingFile, message: handoffCauseUnreadable}, nil
	}

	if !handoffBytesMatch(data, envelope) {
		return nil, &handoffVerdict{code: imageErrorDigestMismatch, message: handoffCauseDigestMismatch}, nil
	}

	return data, nil, nil
}

// handoffRelativeName expresses the block's path relative to the root, which is
// the only form a confined root will open. It decides nothing about
// containment: a name spelled outside the root is handed over as a name that
// climbs out of it, and the root refuses it atomically with the open.
func handoffRelativeName(dir string, path string) string {
	cleanDir := filepath.Clean(dir)

	cleanPath := filepath.Clean(path)
	if cleanPath == cleanDir {
		return "."
	}

	if relative, under := strings.CutPrefix(cleanPath, cleanDir+string(filepath.Separator)); under {
		return relative
	}

	return handoffParentName
}

// handoffLocationVerdict separates the expected operational failure — a host
// that cleaned the file up early, including a link whose target it removed —
// from a containment failure. The confined root reports every escape as
// something other than a missing file, so the two are told apart by the error
// rather than by inspecting the shape of the path.
func handoffLocationVerdict(err error) *handoffVerdict {
	if errors.Is(err, fs.ErrNotExist) {
		return &handoffVerdict{code: imageErrorMissingFile, message: handoffCauseMissing}
	}

	return &handoffVerdict{code: imageErrorPathNotAllowed, message: handoffCauseOutsideRoot}
}

// parseHandoffEnvelope validates the envelope as a whole, returning the refusal
// message for a defect and an empty string for an envelope that is entirely
// legal. Exactly version, digest, and sizeBytes are legal, so an unknown field
// is a rejection rather than something to ignore.
func parseHandoffEnvelope(meta map[string]any) (handoffEnvelope, string) {
	raw, declared := meta[handoffMetaKey]
	if !declared {
		return handoffEnvelope{}, handoffCauseEnvelopeMissing
	}

	value, ok := raw.(map[string]any)
	if !ok {
		return handoffEnvelope{}, handoffCauseEnvelopeNotObject
	}

	if len(value) != handoffEnvelopeFields {
		return handoffEnvelope{}, handoffCauseEnvelopeFields
	}

	version, ok := handoffNumber(value[handoffVersionKey])
	if !ok || version != handoffVersion {
		return handoffEnvelope{}, handoffCauseEnvelopeVersion
	}

	digest, ok := value[handoffDigestKey].(string)
	if !ok || !handoffDigest(digest) {
		return handoffEnvelope{}, handoffCauseEnvelopeDigest
	}

	sizeBytes, ok := handoffNumber(value[handoffSizeBytesKey])
	if !ok || sizeBytes < 0 {
		return handoffEnvelope{}, handoffCauseEnvelopeSizeBytes
	}

	return handoffEnvelope{digest: digest, sizeBytes: sizeBytes}, ""
}

// handoffNumber accepts the integral numbers a decoded envelope can carry. A
// JSON number arrives as a float64, or as a json.Number when a decoder was
// configured to keep the text, and both are range-checked as decoded rather
// than after a conversion an out-of-range value would make meaningless.
func handoffNumber(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case json.Number:
		decoded, err := typed.Float64()
		if err != nil {
			return 0, false
		}

		return handoffFloatNumber(decoded)
	case float64:
		return handoffFloatNumber(typed)
	default:
		return 0, false
	}
}

// handoffFloatNumber admits a float only when it is a whole number an int64 can
// hold, so the conversion below it is always defined.
func handoffFloatNumber(value float64) (int64, bool) {
	if value != math.Trunc(value) || value < 0 || value >= handoffNumberCeiling {
		return 0, false
	}

	return int64(value), true
}

// handoffDigest reports whether digest is a lowercase hex sha256. Uppercase is
// rejected rather than folded, so one set of bytes has exactly one envelope.
func handoffDigest(digest string) bool {
	if len(digest) != handoffDigestLength {
		return false
	}

	for index := range len(digest) {
		char := digest[index]
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}

	return true
}

// handoffURIPath extracts the local path a handoff block points at, returning
// the refusal message for a defect and an empty string for a legal URI. Only a
// file URI naming this host is legal: no scheme is ever fetched.
func handoffURIPath(uri string) (string, string) {
	if uri == "" {
		return "", handoffCauseURIMissing
	}

	parsed, err := url.Parse(uri)
	if err != nil {
		return "", handoffCauseURIUnparseable
	}

	if parsed.Scheme != handoffURIScheme {
		return "", handoffCauseURIScheme
	}

	if parsed.Host != "" && parsed.Host != handoffLocalhost {
		return "", handoffCauseURIHost
	}

	path := filepath.FromSlash(parsed.Path)
	if !filepath.IsAbs(path) {
		return "", handoffCauseURIRelative
	}

	return path, ""
}

// handoffBytesMatch reports whether the bytes read are exactly the bytes the
// envelope declared. It fails closed on any disagreement and never falls back to
// the embedded form, and the digest comparison is constant time so the answer
// cannot be walked one byte at a time.
func handoffBytesMatch(data []byte, envelope handoffEnvelope) bool {
	if int64(len(data)) != envelope.sizeBytes {
		return false
	}

	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])

	return subtle.ConstantTimeCompare([]byte(digest), []byte(envelope.digest)) == 1
}
