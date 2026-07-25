package codexacp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
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
)

// handoffEnvelope is a validated acp-go.dev/handoff descriptor: the host's claim
// about the bytes behind the block's file URI.
type handoffEnvelope struct {
	digest    string
	sizeBytes int64
}

// handoffError is a handoff pre-gate verdict: an input taxonomy code plus the
// real cause, which is reported to the host so a deployment fault is not
// mistaken for a malformed block.
type handoffError struct {
	code  string
	cause error
}

func (e *handoffError) Error() string {
	return fmt.Sprintf("%s: %s", e.code, e.cause)
}

// handoffFile is a resolved handoff path that passed containment and the mode
// check, carrying the size the read is bounded against.
type handoffFile struct {
	path string
	size int64
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
// The verdicts are ordered so a host can tell a bad block from a bad
// deployment: a block malformed as a block is invalid_handoff, a path outside
// the root or a non-regular file is path_not_allowed, and a path inside the
// root that is absent or unopenable is missing_file.
//
// maxBytes bounds the read at maxBytes+1, so an oversize file is verdicted by
// the per-image gate instead of being held whole. Its digest is deliberately
// left unverified in that case — the caller rejects it either way, so nothing
// unverified is ever forwarded.
func readPromptHandoff(root string, media promptMedia, maxBytes int64) (gatedPromptBytes, *handoffError) {
	if root == "" {
		return gatedPromptBytes{}, &handoffError{
			code:  imageErrorInvalidHandoff,
			cause: errors.New("no input handoff root is configured"),
		}
	}

	envelope, err := parseHandoffEnvelope(media.meta)
	if err != nil {
		return gatedPromptBytes{}, &handoffError{code: imageErrorInvalidHandoff, cause: err}
	}

	path, err := handoffURIPath(media.uri)
	if err != nil {
		return gatedPromptBytes{}, &handoffError{code: imageErrorInvalidHandoff, cause: err}
	}

	file, resolveErr := resolveHandoffPath(root, path)
	if resolveErr != nil {
		return gatedPromptBytes{}, resolveErr
	}

	read, readErr := readHandoffFile(file, maxBytes)
	if readErr != nil {
		return gatedPromptBytes{}, readErr
	}

	if err := verifyHandoffDigest(read, envelope, maxBytes); err != nil {
		return gatedPromptBytes{}, &handoffError{code: imageErrorDigestMismatch, cause: err}
	}

	return read, nil
}

// parseHandoffEnvelope validates the envelope as a whole. Exactly version,
// digest, and sizeBytes are legal, so an unknown field is a rejection rather
// than something to ignore.
func parseHandoffEnvelope(meta map[string]any) (handoffEnvelope, error) {
	raw, declared := meta[handoffMetaKey]
	if !declared {
		return handoffEnvelope{}, fmt.Errorf("_meta.%s is required", handoffMetaKey)
	}

	value, ok := raw.(map[string]any)
	if !ok {
		return handoffEnvelope{}, fmt.Errorf("_meta.%s must be an object", handoffMetaKey)
	}

	if len(value) != handoffEnvelopeFields {
		return handoffEnvelope{}, fmt.Errorf(
			"_meta.%s must contain exactly version, digest, and sizeBytes",
			handoffMetaKey,
		)
	}

	version, ok := handoffInteger(value[handoffVersionKey])
	if !ok || version != handoffVersion {
		return handoffEnvelope{}, fmt.Errorf("_meta.%s.version must be %d", handoffMetaKey, handoffVersion)
	}

	digest, ok := value[handoffDigestKey].(string)
	if !ok || !handoffDigest(digest) {
		return handoffEnvelope{}, fmt.Errorf(
			"_meta.%s.digest must be %d lowercase hex characters",
			handoffMetaKey,
			handoffDigestLength,
		)
	}

	sizeBytes, ok := handoffInteger(value[handoffSizeBytesKey])
	if !ok || sizeBytes < 0 {
		return handoffEnvelope{}, fmt.Errorf("_meta.%s.sizeBytes must be a non-negative integer", handoffMetaKey)
	}

	return handoffEnvelope{digest: digest, sizeBytes: sizeBytes}, nil
}

// handoffInteger accepts the integral JSON numbers a decoded envelope can carry
// and rejects a fractional one.
func handoffInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		integer := int64(typed)

		return integer, typed == float64(integer)
	default:
		return 0, false
	}
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

// handoffURIPath extracts the local path a handoff block points at. Only a file
// URI naming this host is legal: no scheme is ever fetched.
func handoffURIPath(uri string) (string, error) {
	if uri == "" {
		return "", errors.New("uri is required")
	}

	parsed, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("uri is not parseable: %w", err)
	}

	if parsed.Scheme != handoffURIScheme {
		return "", fmt.Errorf("uri scheme %q is not %s", parsed.Scheme, handoffURIScheme)
	}

	if parsed.Host != "" && parsed.Host != handoffLocalhost {
		return "", fmt.Errorf("uri host %q is not this host", parsed.Host)
	}

	path := filepath.FromSlash(parsed.Path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("uri path %q is not absolute", parsed.Path)
	}

	return path, nil
}

// resolveHandoffPath bounds one host-supplied path to root: containment on the
// cleaned path, symlink resolution, containment again on the resolved path, then
// the mode check. Containment is tested twice because the first test cannot see
// a symlink that leaves the root, and the second cannot reject an out-of-root
// path without touching the filesystem first.
//
// Both tests compare a path against a root resolved to the same degree, so a
// root that is itself reached through a symlink — /var on Darwin, a relocated
// temp directory — neither rejects a legitimate file nor admits an escape.
func resolveHandoffPath(root string, path string) (handoffFile, *handoffError) {
	cleanPath := filepath.Clean(path)
	if !pathContainedIn(filepath.Clean(root), cleanPath) {
		return handoffFile{}, &handoffError{
			code:  imageErrorPathNotAllowed,
			cause: errors.New("path is outside the configured handoff root"),
		}
	}

	resolvedRoot, err := evalImageSymlinks(filepath.Clean(root))
	if err != nil {
		return handoffFile{}, &handoffError{
			code:  imageErrorPathNotAllowed,
			cause: fmt.Errorf("configured handoff root cannot be resolved: %w", err),
		}
	}

	// Whether the path is a symlink decides how to read an unresolvable target:
	// a dangling link is a containment failure, because its target cannot be
	// proven to be inside the root, while a plain absent file is the expected
	// operational case of a host that cleaned up early.
	link, err := lstatImageFile(cleanPath)
	if err != nil {
		return handoffFile{}, handoffLocationError(err, "path cannot be inspected")
	}

	resolvedPath, err := evalImageSymlinks(cleanPath)
	if err != nil {
		if link.Mode()&os.ModeSymlink != 0 {
			return handoffFile{}, &handoffError{
				code:  imageErrorPathNotAllowed,
				cause: errors.New("path is a symlink whose target cannot be resolved inside the root"),
			}
		}

		return handoffFile{}, handoffLocationError(err, "path cannot be resolved")
	}

	if !pathContainedIn(resolvedRoot, resolvedPath) {
		return handoffFile{}, &handoffError{
			code:  imageErrorPathNotAllowed,
			cause: errors.New("path resolves outside the configured handoff root"),
		}
	}

	info, err := statImageFile(resolvedPath)
	if err != nil {
		return handoffFile{}, handoffLocationError(err, "path cannot be inspected")
	}

	if !info.Mode().IsRegular() {
		return handoffFile{}, &handoffError{
			code:  imageErrorPathNotAllowed,
			cause: errors.New("path is not a regular file"),
		}
	}

	return handoffFile{path: resolvedPath, size: info.Size()}, nil
}

// handoffLocationError separates the expected operational failure — a host that
// cleaned the file up early — from a containment failure.
func handoffLocationError(err error, stage string) *handoffError {
	if os.IsNotExist(err) {
		return &handoffError{code: imageErrorMissingFile, cause: errors.New("path does not exist")}
	}

	return &handoffError{code: imageErrorPathNotAllowed, cause: fmt.Errorf("%s: %w", stage, err)}
}

// handoffReadBound is the per-image gate plus one byte: enough to observe that a
// file is oversize, never enough to hold it. A disabled policy limit still gets
// the hard ACP frame bound rather than an unbounded read, because the bytes come
// from a path the adapter did not choose.
func handoffReadBound(maxBytes int64) int64 {
	if maxBytes <= 0 {
		return maxACPImageDecodedBytes + 1
	}

	return maxBytes + 1
}

// readHandoffFile reads a bounded prefix of the file, so an oversize file costs
// one byte over the gate rather than its whole size. The handoff file is
// host-owned: it is read and never written, moved, or removed.
func readHandoffFile(file handoffFile, maxBytes int64) (gatedPromptBytes, *handoffError) {
	reader, err := openImageFile(file.path)
	if err != nil {
		return gatedPromptBytes{}, &handoffError{
			code:  imageErrorMissingFile,
			cause: fmt.Errorf("path cannot be opened: %w", err),
		}
	}

	defer func() { _ = reader.Close() }()

	data, err := readImageFile(io.LimitReader(reader, handoffReadBound(maxBytes)))
	if err != nil {
		return gatedPromptBytes{}, &handoffError{
			code:  imageErrorMissingFile,
			cause: fmt.Errorf("path cannot be read: %w", err),
		}
	}

	// The reported size is what a byte-limit rejection names, and it must never
	// undercount what was actually read: a file that grew after it was measured
	// would otherwise skip both the size gate and digest verification.
	read := gatedPromptBytes{data: data, size: file.size}
	if actual := int64(len(data)); actual > read.size {
		read.size = actual
	}

	return read, nil
}

// verifyHandoffDigest fails closed on any disagreement between the envelope and
// the bytes, and never falls back to the embedded form.
//
// Verification is skipped for a file past the per-image gate, which cannot be
// read whole and so cannot be hashed. The skip condition is deliberately the
// same expression as that gate: it holds only when the caller is about to reject
// the block anyway, so no unverified bytes are ever forwarded. A disabled policy
// limit therefore verifies every time, and a file past the frame bound fails
// here rather than passing unchecked.
func verifyHandoffDigest(read gatedPromptBytes, envelope handoffEnvelope, maxBytes int64) error {
	if maxBytes > 0 && read.size > maxBytes {
		return nil
	}

	if size := int64(len(read.data)); size != envelope.sizeBytes {
		return fmt.Errorf("file is %d bytes, envelope declares %d", size, envelope.sizeBytes)
	}

	sum := sha256.Sum256(read.data)

	digest := hex.EncodeToString(sum[:])
	if digest != envelope.digest {
		return fmt.Errorf("file hashes to %s, envelope declares %s", digest, envelope.digest)
	}

	return nil
}
