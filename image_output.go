package codexacp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

const (
	imageOutputStage          = "image_output"
	imageOutputInvalidBase64  = "invalid_base64"
	imageOutputNotRaster      = "not_a_raster"
	imageOutputMIMEConflict   = "media_type_mismatch"
	imageOutputMissingFile    = "missing_file"
	imageOutputPathDenied     = "path_not_allowed"
	imageOutputTooLarge       = "too_large"
	imageOutputStorageFailure = "storage_failed"
	imageArtifactStorePrefix  = "images/"
	imageArtifactStoreVersion = 1
	imageArtifactRefKey       = "artifactSubpath"
	imageArtifactTTL          = 24 * time.Hour

	imageOutputTooLargeMessage = "image output exceeds the configured per-image limit"

	// maxACPImageDecodedBytes is the largest decoded image the pinned ACP Go
	// SDK can carry in one JSON-RPC frame. A single update frame above the
	// SDK's 10 MiB scanner bound disconnects a Go-SDK consumer's whole
	// connection, so emitted output is always clamped to this hard cap even
	// when the configured policy limit is larger or disabled.
	maxACPImageDecodedBytes int64 = 7_864_155
)

type imageOutputError struct {
	reason    string
	message   string
	sizeBytes int64
	maxBytes  int64
}

func (e *imageOutputError) Error() string {
	return e.message
}

// effectiveImageOutputLimit clamps a configured output byte limit to the hard
// ACP frame bound. A configured zero disables only the adapter policy limit,
// and a configured value above the frame bound is reduced to it, so emitted
// output can never exceed what a Go-SDK consumer can receive in one frame.
func effectiveImageOutputLimit(configured int64) int64 {
	if configured <= 0 || configured > maxACPImageDecodedBytes {
		return maxACPImageDecodedBytes
	}

	return configured
}

// boundedImageDecoder retains at most limit bytes while counting the full
// decoded size, so an oversize image is rejected without ever allocating its
// entire decoded body.
type boundedImageDecoder struct {
	data  []byte
	limit int64
	size  int64
}

func (w *boundedImageDecoder) Write(p []byte) (int, error) {
	w.size += int64(len(p))

	remaining := w.limit - int64(len(w.data))
	if remaining > 0 {
		retain := int64(len(p))
		if retain > remaining {
			retain = remaining
		}

		w.data = append(w.data, p[:retain]...)
	}

	return len(p), nil
}

func decodeBoundedImage(data string, retainLimit int64) ([]byte, int64, error) {
	decoded := &boundedImageDecoder{limit: retainLimit}

	if _, err := io.Copy(decoded, base64.NewDecoder(base64.StdEncoding, strings.NewReader(data))); err != nil {
		return nil, 0, err
	}

	return decoded.data, decoded.size, nil
}

type storedImageArtifact struct {
	Version            int    `json:"version"`
	NativeID           string `json:"nativeId"`
	Fingerprint        string `json:"fingerprint"`
	MimeType           string `json:"mimeType"`
	Data               string `json:"data"`
	CreatedAtUnixMilli int64  `json:"createdAtUnixMilli"`
}

var (
	evalImageSymlinks = filepath.EvalSymlinks
	statImageFile     = os.Stat
	openImageFile     = func(path string) (io.ReadCloser, error) { return os.Open(path) }
	readImageFile     = io.ReadAll
	relativeImagePath = filepath.Rel
	marshalImageJSON  = json.Marshal
)

type imageToolState struct {
	started      map[string]struct{}
	emitted      map[string]struct{}
	content      map[acp.ToolCallId][]acp.ToolCallContent
	decodedBytes map[acp.ToolCallId]int64
}

func newImageToolState() imageToolState {
	return imageToolState{
		started:      make(map[string]struct{}),
		emitted:      make(map[string]struct{}),
		content:      make(map[acp.ToolCallId][]acp.ToolCallContent),
		decodedBytes: make(map[acp.ToolCallId]int64),
	}
}

func (s *session) imageEventUpdates(ctx context.Context, event codex.Event, state *imageToolState) ([]acp.SessionUpdate, error) {
	image := event.Image
	id := acp.ToolCallId(firstNonEmpty(image.ID, "codex-image"))
	title := imageToolTitle(image.Kind)

	updates := make([]acp.SessionUpdate, 0, 2)

	if _, started := state.started[string(id)]; !started {
		startOptions := []acp.ToolCallStartOpt{
			acp.WithStartKind(acp.ToolKindOther),
			acp.WithStartStatus(acp.ToolCallStatusInProgress),
		}
		if image.RevisedPrompt != "" {
			startOptions = append(startOptions, acp.WithStartRawInput(map[string]any{jsonFieldPrompt: image.RevisedPrompt}))
		}

		updates = append(updates, acp.StartToolCall(id, title, startOptions...))
		state.started[string(id)] = struct{}{}
	}

	if event.Kind == codex.EventImageStarted {
		return updates, nil
	}

	if imageNativeFailed(image.Status) {
		failed := acp.ToolCallStatusFailed
		updates = append(updates, acp.UpdateToolCall(id,
			acp.WithUpdateStatus(failed),
			acp.WithUpdateKind(acp.ToolKindOther),
			acp.WithUpdateRawOutput(image.Raw),
		))

		return updates, nil
	}

	var (
		data     []byte
		mimeType string
		size     int64
		err      error
	)

	if image.ArtifactRef != "" {
		artifact, loadErr := s.loadImageArtifact(ctx, image.ArtifactRef)
		if loadErr != nil {
			err = &imageOutputError{
				reason:  imageOutputStorageFailure,
				message: fmt.Sprintf("load image output: %v", loadErr),
			}
		} else {
			data, err = base64.StdEncoding.DecodeString(artifact.Data)
			mimeType = artifact.MimeType
			size = int64(len(data))
		}
	} else {
		data, mimeType, size, err = s.materializeImageEvent(image)
	}

	if err != nil {
		updates = append(updates, failedImageToolUpdate(id, image.Raw))

		return updates, err
	}

	fingerprint := sha256.Sum256(data)

	artifactIdentity := string(id) + ":" + hex.EncodeToString(fingerprint[:])
	if _, emitted := state.emitted[artifactIdentity]; emitted {
		return updates, nil
	}

	limit := effectiveImageOutputLimit(s.agent.options.ImageLimits.MaxOutputBytesPerImage)
	if size > limit {
		updates = append(updates, failedImageToolUpdate(id, image.Raw))

		return updates, &imageOutputError{
			reason:    imageOutputTooLarge,
			message:   imageOutputTooLargeMessage,
			sizeBytes: size,
			maxBytes:  limit,
		}
	}

	aggregate := state.decodedBytes[id] + size

	aggregateLimit := effectiveImageOutputLimit(s.agent.options.ImageLimits.MaxOutputBytesPerToolCall)
	if aggregate > aggregateLimit {
		updates = append(updates, failedImageToolUpdate(id, image.Raw))

		return updates, &imageOutputError{
			reason:    imageOutputTooLarge,
			message:   "image output exceeds the configured per-tool-call limit",
			sizeBytes: aggregate,
			maxBytes:  aggregateLimit,
		}
	}

	if _, err := s.storeImageArtifact(ctx, image.ID, data, mimeType); err != nil {
		updates = append(updates, failedImageToolUpdate(id, image.Raw))

		return updates, &imageOutputError{
			reason:  imageOutputStorageFailure,
			message: fmt.Sprintf("store image output: %v", err),
		}
	}

	content := acp.ToolContent(acp.ImageBlock(base64.StdEncoding.EncodeToString(data), mimeType))
	state.content[id] = append(state.content[id], content)
	state.decodedBytes[id] = aggregate
	state.emitted[artifactIdentity] = struct{}{}

	updates = append(updates, acp.UpdateToolCall(id,
		acp.WithUpdateStatus(acp.ToolCallStatusCompleted),
		acp.WithUpdateKind(acp.ToolKindOther),
		acp.WithUpdateContent(append([]acp.ToolCallContent(nil), state.content[id]...)),
		acp.WithUpdateRawOutput(image.Raw),
	))

	return updates, nil
}

func imageToolTitle(kind string) string {
	if kind == "imageView" {
		return "View image"
	}

	return "Image generation"
}

func imageNativeFailed(status string) bool {
	switch strings.ToLower(status) {
	case statusFailed, jsonFieldError, statusErrored, "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func failedImageToolUpdate(id acp.ToolCallId, raw map[string]any) acp.SessionUpdate {
	return acp.UpdateToolCall(id,
		acp.WithUpdateStatus(acp.ToolCallStatusFailed),
		acp.WithUpdateKind(acp.ToolKindOther),
		acp.WithUpdateRawOutput(raw),
	)
}

func (s *session) materializeImageEvent(image codex.ImageEvent) ([]byte, string, int64, error) {
	if image.Result != "" {
		limit := effectiveImageOutputLimit(s.agent.options.ImageLimits.MaxOutputBytesPerImage)

		data, size, err := decodeBoundedImage(image.Result, limit+1)
		if err != nil {
			return nil, "", 0, &imageOutputError{
				reason:  imageOutputInvalidBase64,
				message: "image output contains invalid base64",
			}
		}

		mimeType, ok := sniffRasterMIME(data)
		if !ok {
			return nil, "", 0, &imageOutputError{
				reason:  imageOutputNotRaster,
				message: "image output bytes are not a raster",
			}
		}

		return data, mimeType, size, nil
	}

	if image.SavedPath == "" {
		return nil, "", 0, &imageOutputError{
			reason:  imageOutputMissingFile,
			message: "image output did not contain bytes or a saved file",
		}
	}

	data, mimeType, err := s.readAllowedImageFile(image.SavedPath)
	if err != nil {
		return nil, "", 0, err
	}

	return data, mimeType, int64(len(data)), nil
}

func (s *session) readAllowedImageFile(path string) ([]byte, string, error) {
	resolved, err := evalImageSymlinks(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", &imageOutputError{reason: imageOutputMissingFile, message: "image output file is missing"}
		}

		return nil, "", &imageOutputError{reason: imageOutputPathDenied, message: "image output path cannot be resolved safely"}
	}

	allowed := false

	for _, root := range s.allowedImageRoots() {
		if pathWithinRoot(resolved, root) {
			allowed = true

			break
		}
	}

	if !allowed {
		return nil, "", &imageOutputError{reason: imageOutputPathDenied, message: "image output path is outside the allowed roots"}
	}

	info, err := statImageFile(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", &imageOutputError{reason: imageOutputMissingFile, message: "image output file is missing"}
		}

		return nil, "", &imageOutputError{reason: imageOutputPathDenied, message: "image output path cannot be inspected safely"}
	}

	if !info.Mode().IsRegular() {
		return nil, "", &imageOutputError{reason: imageOutputPathDenied, message: "image output path is not a regular file"}
	}

	limit := effectiveImageOutputLimit(s.agent.options.ImageLimits.MaxOutputBytesPerImage)
	if info.Size() > limit {
		return nil, "", &imageOutputError{
			reason:    imageOutputTooLarge,
			message:   imageOutputTooLargeMessage,
			sizeBytes: info.Size(),
			maxBytes:  limit,
		}
	}

	file, err := openImageFile(resolved)
	if err != nil {
		return nil, "", &imageOutputError{reason: imageOutputMissingFile, message: "image output file cannot be opened"}
	}
	defer file.Close()

	reader := io.LimitReader(file, limit+1)

	data, err := readImageFile(reader)
	if err != nil {
		return nil, "", &imageOutputError{reason: imageOutputMissingFile, message: "image output file cannot be read"}
	}

	if int64(len(data)) > limit {
		return nil, "", &imageOutputError{
			reason:    imageOutputTooLarge,
			message:   imageOutputTooLargeMessage,
			sizeBytes: int64(len(data)),
			maxBytes:  limit,
		}
	}

	mimeType, ok := sniffRasterMIME(data)
	if !ok {
		return nil, "", &imageOutputError{reason: imageOutputNotRaster, message: "image output file is not a raster"}
	}

	return data, mimeType, nil
}

func (s *session) allowedImageRoots() []string {
	roots := []string{s.cwd}
	if scratch := s.agent.options.ScratchDir; scratch != "" {
		roots = append(roots, scratch)
	}

	s.agent.mu.Lock()
	runtimeScratch := s.agent.runtimeScratchRoot
	s.agent.mu.Unlock()

	if runtimeScratch != "" {
		roots = append(roots, runtimeScratch)
	}

	if home := s.agent.resolvedCodexHome(); home != "" {
		roots = append(roots, filepath.Join(home, "generated_images"))
	}

	return roots
}

func pathWithinRoot(path string, root string) bool {
	if path == "" || root == "" {
		return false
	}

	resolvedRoot, err := evalImageSymlinks(root)
	if err != nil {
		return false
	}

	relative, err := relativeImagePath(resolvedRoot, path)
	if err != nil {
		return false
	}

	return relative != ".." && relative != "."+string(filepath.Separator)+".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func sniffRasterMIME(data []byte) (string, bool) {
	if info, err := inspectPromptRaster(data); err == nil {
		return info.mimeType, true
	}

	switch detected := http.DetectContentType(data); detected {
	case "image/bmp", "image/x-icon", "image/vnd.microsoft.icon", "image/tiff":
		return detected, true
	}

	switch {
	case len(data) >= 4 && (string(data[:4]) == "II*\x00" || string(data[:4]) == "MM\x00*"):
		return "image/tiff", true
	default:
		return "", false
	}
}

func (s *session) storeImageArtifact(ctx context.Context, nativeID string, data []byte, mimeType string) (string, error) {
	fingerprint := sha256.Sum256(data)
	idHash := sha256.Sum256([]byte(nativeID))
	subpath := imageArtifactStorePrefix + hex.EncodeToString(idHash[:8]) + "/" + hex.EncodeToString(fingerprint[:]) + ".json"

	record := storedImageArtifact{
		Version:            imageArtifactStoreVersion,
		NativeID:           nativeID,
		Fingerprint:        hex.EncodeToString(fingerprint[:]),
		MimeType:           mimeType,
		Data:               base64.StdEncoding.EncodeToString(data),
		CreatedAtUnixMilli: timeNow().UnixMilli(),
	}

	entry, err := marshalImageJSON(record)
	if err != nil {
		return "", err
	}

	storeCtx, cancel := s.agent.sessionStoreContext(ctx)
	defer cancel()

	key := SessionKey{SessionID: string(s.id), Subpath: subpath}

	// Serialize the sweep, load, and append against a concurrent store of the
	// same artifact. The live prompt loop and the rollout tail can store the
	// same native id at once; without this an interleaved load-then-append
	// would write the key twice and a later single-entry load would fail as a
	// spurious storage_failed.
	s.imageStoreMu.Lock()
	defer s.imageStoreMu.Unlock()

	sweepErr := s.agent.sweepSessionImageArtifacts(storeCtx, string(s.id))
	if sweepErr != nil {
		return "", sweepErr
	}

	existing, err := s.agent.sessionStore().Load(storeCtx, key)
	if err != nil {
		return "", err
	}

	if len(existing) > 0 {
		var stored storedImageArtifact
		if len(existing) != 1 || json.Unmarshal(existing[0], &stored) != nil ||
			stored.Fingerprint != record.Fingerprint || stored.MimeType != record.MimeType ||
			stored.Data != record.Data {
			return "", errors.New("stored image artifact conflicts with native identity")
		}

		return subpath, nil
	}

	if err := s.agent.sessionStore().Append(storeCtx, key, []SessionStoreEntry{entry}); err != nil {
		return "", err
	}

	return subpath, nil
}

func (s *session) loadImageArtifact(ctx context.Context, subpath string) (storedImageArtifact, error) {
	if !strings.HasPrefix(subpath, imageArtifactStorePrefix) {
		return storedImageArtifact{}, errors.New("image artifact reference is invalid")
	}

	storeCtx, cancel := s.agent.sessionStoreContext(ctx)
	defer cancel()

	entries, err := s.agent.sessionStore().Load(storeCtx, SessionKey{SessionID: string(s.id), Subpath: subpath})
	if err != nil {
		return storedImageArtifact{}, err
	}

	if len(entries) != 1 {
		return storedImageArtifact{}, errors.New("image artifact bytes are unavailable")
	}

	var artifact storedImageArtifact

	unmarshalErr := json.Unmarshal(entries[0], &artifact)
	if unmarshalErr != nil {
		return storedImageArtifact{}, unmarshalErr
	}

	if artifact.Version != imageArtifactStoreVersion || artifact.Data == "" || artifact.MimeType == "" ||
		artifact.CreatedAtUnixMilli <= 0 {
		return storedImageArtifact{}, errors.New("image artifact record is incomplete")
	}

	if imageArtifactExpired(artifact.CreatedAtUnixMilli, timeNow()) {
		deleteErr := s.agent.sessionStore().Delete(storeCtx, SessionKey{
			SessionID: string(s.id),
			Subpath:   subpath,
		})
		if deleteErr != nil {
			return storedImageArtifact{}, fmt.Errorf("delete expired image artifact: %w", deleteErr)
		}

		return storedImageArtifact{}, errors.New("image artifact bytes expired")
	}

	data, err := base64.StdEncoding.DecodeString(artifact.Data)
	if err != nil {
		return storedImageArtifact{}, err
	}

	fingerprint := sha256.Sum256(data)
	if hex.EncodeToString(fingerprint[:]) != artifact.Fingerprint {
		return storedImageArtifact{}, errors.New("image artifact checksum does not match")
	}

	return artifact, nil
}

func imageArtifactExpired(createdAtUnixMilli int64, now time.Time) bool {
	return createdAtUnixMilli <= 0 ||
		!now.Before(time.UnixMilli(createdAtUnixMilli).Add(imageArtifactTTL))
}

func (a *Agent) sweepSessionImageArtifacts(ctx context.Context, sessionID string) error {
	store := a.sessionStore()

	subpaths, err := store.ListSubkeys(ctx, SessionKey{SessionID: sessionID})
	if err != nil {
		return err
	}

	now := timeNow()

	for _, subpath := range subpaths {
		if !strings.HasPrefix(subpath, imageArtifactStorePrefix) {
			continue
		}

		key := SessionKey{SessionID: sessionID, Subpath: subpath}

		entries, loadErr := store.Load(ctx, key)
		if loadErr != nil {
			return loadErr
		}

		expired := len(entries) != 1
		if !expired {
			var artifact storedImageArtifact

			expired = json.Unmarshal(entries[0], &artifact) != nil ||
				imageArtifactExpired(artifact.CreatedAtUnixMilli, now)
		}

		if !expired {
			continue
		}

		if deleteErr := store.Delete(ctx, key); deleteErr != nil {
			return deleteErr
		}
	}

	return nil
}

func (s *session) prepareDurableImageRolloutEntries(ctx context.Context, entries []SessionStoreEntry) ([]SessionStoreEntry, error) {
	out := make([]SessionStoreEntry, len(entries))
	for index, entry := range entries {
		var row map[string]any
		if err := json.Unmarshal(entry, &row); err != nil {
			return nil, err
		}

		payload, _ := row["payload"].(map[string]any)
		if stringFromAny(row[jsonFieldType]) != valueResponseItem ||
			stringFromAny(payload[jsonFieldType]) != valueImageGenerationCall {
			out[index] = cloneStoreEntry(entry)

			continue
		}

		result := stringFromAny(payload[jsonFieldResult])
		savedPath := firstNonEmpty(stringFromAny(payload["saved_path"]), stringFromAny(payload["savedPath"]))
		status := stringFromAny(payload["status"])

		if result == "" && savedPath == "" {
			out[index] = cloneStoreEntry(entry)

			continue
		}

		if imageNativeFailed(status) {
			delete(payload, jsonFieldResult)
			delete(payload, "saved_path")
			delete(payload, "savedPath")
		} else {
			image := codex.ImageEvent{
				ID:        firstNonEmpty(stringFromAny(payload["id"]), stringFromAny(payload["call_id"])),
				Kind:      valueImageGeneration,
				Status:    status,
				Result:    result,
				SavedPath: savedPath,
			}

			data, mimeType, size, err := s.materializeImageEvent(image)
			if err != nil {
				return nil, err
			}

			limit := effectiveImageOutputLimit(s.agent.options.ImageLimits.MaxOutputBytesPerImage)
			if size > limit {
				return nil, &imageOutputError{
					reason:    imageOutputTooLarge,
					message:   imageOutputTooLargeMessage,
					sizeBytes: size,
					maxBytes:  limit,
				}
			}

			subpath, err := s.storeImageArtifact(ctx, image.ID, data, mimeType)
			if err != nil {
				return nil, &imageOutputError{reason: imageOutputStorageFailure, message: fmt.Sprintf("store image output: %v", err)}
			}

			payload[jsonFieldResult] = map[string]any{imageArtifactRefKey: subpath}
			delete(payload, "saved_path")
			delete(payload, "savedPath")
		}

		sanitized, err := marshalImageJSON(row)
		if err != nil {
			return nil, err
		}

		out[index] = sanitized
	}

	return out, nil
}

func (a *Agent) hydrateStoredImageArtifacts(
	ctx context.Context,
	sessionID acp.SessionId,
	entries []SessionStoreEntry,
) ([]SessionStoreEntry, error) {
	session := &session{agent: a, id: sessionID}
	out := make([]SessionStoreEntry, len(entries))

	for index, entry := range entries {
		var row map[string]any
		if err := json.Unmarshal(entry, &row); err != nil {
			return nil, err
		}

		payload, _ := row["payload"].(map[string]any)
		result, _ := payload[jsonFieldResult].(map[string]any)

		subpath := stringFromAny(result[imageArtifactRefKey])
		if stringFromAny(row[jsonFieldType]) != valueResponseItem ||
			stringFromAny(payload[jsonFieldType]) != valueImageGenerationCall ||
			subpath == "" {
			out[index] = cloneStoreEntry(entry)

			continue
		}

		artifact, err := session.loadImageArtifact(ctx, subpath)
		if err != nil {
			return nil, &imageOutputError{
				reason:  imageOutputStorageFailure,
				message: fmt.Sprintf("load image output for replay: %v", err),
			}
		}

		payload[jsonFieldResult] = artifact.Data

		hydrated, err := marshalImageJSON(row)
		if err != nil {
			return nil, err
		}

		out[index] = hydrated
	}

	return out, nil
}
