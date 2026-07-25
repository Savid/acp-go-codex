package codexacp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

func outputFixture(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)

	return data
}

func outputSession(t *testing.T, opts ...Option) *session {
	t.Helper()

	workspace := t.TempDir()
	agent := NewAgent(append([]Option{WithHome(t.TempDir())}, opts...)...)

	return &session{agent: agent, id: "image-session", cwd: workspace}
}

func TestImageEventUpdatesLifecycleLimitsAndDedupe(t *testing.T) {
	ctx := context.Background()
	png := outputFixture(t, "valid.png")
	jpeg := outputFixture(t, "valid.jpg")
	encodedPNG := base64.StdEncoding.EncodeToString(png)

	s := outputSession(t, WithImageLimits(ImageLimits{}))
	state := newImageToolState()

	start, err := s.imageEventUpdates(ctx, codex.Event{
		Kind: codex.EventImageStarted,
		Image: codex.ImageEvent{
			ID:            "image-1",
			Kind:          "imageGeneration",
			Status:        "inProgress",
			RevisedPrompt: "draw",
		},
	}, &state)
	require.NoError(t, err)
	require.Len(t, start, 1)
	require.NotNil(t, start[0].ToolCall)

	completed, err := s.imageEventUpdates(ctx, codex.Event{
		Kind: codex.EventImageCompleted,
		Image: codex.ImageEvent{
			ID:     "image-1",
			Kind:   "imageGeneration",
			Status: "completed",
			Result: encodedPNG,
			Raw:    map[string]any{"status": "completed"},
		},
	}, &state)
	require.NoError(t, err)
	require.Len(t, completed, 1)
	require.NotNil(t, completed[0].ToolCallUpdate)
	require.Len(t, completed[0].ToolCallUpdate.Content, 1)
	require.Equal(t, encodedPNG, completed[0].ToolCallUpdate.Content[0].Content.Content.Image.Data)

	duplicate, err := s.imageEventUpdates(ctx, codex.Event{
		Kind:  codex.EventImageCompleted,
		Image: codex.ImageEvent{ID: "image-1", Kind: "imageGeneration", Status: "completed", Result: encodedPNG},
	}, &state)
	require.NoError(t, err)
	require.Empty(t, duplicate)

	failedState := newImageToolState()
	failed, err := s.imageEventUpdates(ctx, codex.Event{
		Kind:  codex.EventImageCompleted,
		Image: codex.ImageEvent{ID: "image-failed", Kind: "imageView", Status: "failed"},
	}, &failedState)
	require.NoError(t, err)
	require.Len(t, failed, 2)
	require.Equal(t, acp.ToolCallStatusFailed, *failed[1].ToolCallUpdate.Status)
	require.Equal(t, "View image", failed[0].ToolCall.Title)

	invalid, err := s.imageEventUpdates(ctx, codex.Event{
		Kind:  codex.EventImageCompleted,
		Image: codex.ImageEvent{ID: "invalid", Kind: "imageGeneration", Status: "completed", Result: "!"},
	}, &failedState)
	requireImageRefusal(t, invalid, err, imageGuidanceInvalidBase64)

	var outputErr *imageOutputError

	atLimit := outputSession(t, WithImageLimits(ImageLimits{MaxOutputBytesPerImage: int64(len(png))}))
	_, err = atLimit.imageEventUpdates(ctx, codex.Event{
		Kind:  codex.EventImageCompleted,
		Image: codex.ImageEvent{ID: "at-limit", Status: "completed", Result: encodedPNG},
	}, ptrImageToolState(newImageToolState()))
	require.NoError(t, err)

	perImage := outputSession(t, WithImageLimits(ImageLimits{MaxOutputBytesPerImage: int64(len(png) - 1)}))
	large, err := perImage.imageEventUpdates(ctx, codex.Event{
		Kind:  codex.EventImageCompleted,
		Image: codex.ImageEvent{ID: "large", Status: "completed", Result: encodedPNG},
	}, ptrImageToolState(newImageToolState()))
	requireImageRefusal(t, large, err, imageGuidanceTooLarge)

	atAggregate := outputSession(t, WithImageLimits(ImageLimits{MaxOutputBytesPerToolCall: int64(len(png) + len(jpeg))}))
	atAggregateState := newImageToolState()
	_, err = atAggregate.imageEventUpdates(ctx, codex.Event{
		Kind:  codex.EventImageCompleted,
		Image: codex.ImageEvent{ID: "at-aggregate", Status: "completed", Result: encodedPNG},
	}, &atAggregateState)
	require.NoError(t, err)
	_, err = atAggregate.imageEventUpdates(ctx, codex.Event{
		Kind:  codex.EventImageCompleted,
		Image: codex.ImageEvent{ID: "at-aggregate", Status: "completed", Result: base64.StdEncoding.EncodeToString(jpeg)},
	}, &atAggregateState)
	require.NoError(t, err)

	aggregateLimit := int64(len(png) + len(jpeg) - 1)
	aggregateSession := outputSession(t, WithImageLimits(ImageLimits{MaxOutputBytesPerToolCall: aggregateLimit}))
	aggregateState := newImageToolState()
	_, err = aggregateSession.imageEventUpdates(ctx, codex.Event{
		Kind:  codex.EventImageCompleted,
		Image: codex.ImageEvent{ID: "aggregate", Status: "completed", Result: encodedPNG},
	}, &aggregateState)
	require.NoError(t, err)
	overAggregate, err := aggregateSession.imageEventUpdates(ctx, codex.Event{
		Kind:  codex.EventImageCompleted,
		Image: codex.ImageEvent{ID: "aggregate", Status: "completed", Result: base64.StdEncoding.EncodeToString(jpeg)},
	}, &aggregateState)
	requireImageRefusal(t, overAggregate, err, imageGuidanceTooLarge)

	storageSession := &session{agent: NewAgent(WithSessionStore(appendErrorStore{}), WithImageLimits(ImageLimits{})), id: "storage"}
	_, err = storageSession.imageEventUpdates(ctx, codex.Event{
		Kind:  codex.EventImageCompleted,
		Image: codex.ImageEvent{ID: "storage", Status: "completed", Result: encodedPNG},
	}, ptrImageToolState(newImageToolState()))
	require.ErrorAs(t, err, &outputErr)
	require.Equal(t, imageOutputStorageFailure, outputErr.reason)

	subpath, err := s.storeImageArtifact(ctx, "stored", png, "image/png")
	require.NoError(t, err)
	replayState := newImageToolState()
	replayed, err := s.imageEventUpdates(ctx, codex.Event{
		Kind: codex.EventImageCompleted,
		Image: codex.ImageEvent{
			ID:          "stored",
			Status:      "completed",
			ArtifactRef: subpath,
		},
	}, &replayState)
	require.NoError(t, err)
	require.Len(t, replayed, 2)

	_, err = s.imageEventUpdates(ctx, codex.Event{
		Kind:  codex.EventImageCompleted,
		Image: codex.ImageEvent{ID: "missing-ref", Status: "completed", ArtifactRef: imageArtifactStorePrefix + "missing"},
	}, ptrImageToolState(newImageToolState()))
	require.ErrorAs(t, err, &outputErr)
	require.Equal(t, imageOutputStorageFailure, outputErr.reason)
	require.Contains(t, outputErr.Error(), "load image output")
}

func ptrImageToolState(state imageToolState) *imageToolState {
	return &state
}

func TestMaterializeImageEventAndRasterSniffing(t *testing.T) {
	s := outputSession(t, WithImageLimits(ImageLimits{}))
	png := outputFixture(t, "valid.png")

	data, mimeType, size, err := s.materializeImageEvent(codex.ImageEvent{Result: base64.StdEncoding.EncodeToString(png)})
	require.NoError(t, err)
	require.Equal(t, png, data)
	require.Equal(t, "image/png", mimeType)
	require.Equal(t, int64(len(png)), size)

	for _, testCase := range []struct {
		image  codex.ImageEvent
		reason string
	}{
		{image: codex.ImageEvent{Result: "!"}, reason: imageOutputInvalidBase64},
		{image: codex.ImageEvent{Result: base64.StdEncoding.EncodeToString([]byte("text"))}, reason: imageOutputNotRaster},
		{image: codex.ImageEvent{}, reason: imageOutputMissingFile},
	} {
		_, _, size, err = s.materializeImageEvent(testCase.image)
		require.Zero(t, size)
		var outputErr *imageOutputError
		require.ErrorAs(t, err, &outputErr)
		require.Equal(t, testCase.reason, outputErr.reason)
	}

	mimeType, ok := sniffRasterMIME(png)
	require.True(t, ok)
	require.Equal(t, "image/png", mimeType)
	mimeType, ok = sniffRasterMIME([]byte("BM\x00\x00"))
	require.True(t, ok)
	require.Equal(t, "image/bmp", mimeType)
	mimeType, ok = sniffRasterMIME([]byte("II*\x00"))
	require.True(t, ok)
	require.Equal(t, "image/tiff", mimeType)
	mimeType, ok = sniffRasterMIME([]byte{0, 0, 1, 0})
	require.True(t, ok)
	require.Equal(t, "image/x-icon", mimeType)
	_, ok = sniffRasterMIME([]byte("plain text"))
	require.False(t, ok)

	require.Equal(t, "Image generation", imageToolTitle("imageGeneration"))
	for _, status := range []string{"failed", "error", "errored", "cancelled", "canceled"} {
		require.True(t, imageNativeFailed(status))
	}
	require.False(t, imageNativeFailed("completed"))
	require.NotNil(t, failedImageToolUpdate("tool", nil).ToolCallUpdate)
}

func TestAllowedImageFileMaterialization(t *testing.T) {
	png := outputFixture(t, "valid.png")
	workspace := t.TempDir()
	scratch := t.TempDir()
	home := t.TempDir()
	generated := filepath.Join(home, "generated_images")
	require.NoError(t, os.Mkdir(generated, 0o700))

	agent := NewAgent(WithHome(home), WithScratchDir(scratch), WithImageLimits(ImageLimits{}))
	agent.runtimeScratchRoot = t.TempDir()
	s := &session{agent: agent, id: "paths", cwd: workspace}

	// The temp directory is an allowed root, so this case narrows it to a
	// directory of its own. Every root above was created before the narrowing
	// and stays outside it, which keeps each root assertion below about that
	// root alone.
	//
	// The narrowed root is a symlink to the directory that holds the file, which
	// is what the temp directory itself is on macOS. Only a root resolved to the
	// same degree as the candidate can match it, so an unresolved root fails
	// this case on every host rather than only on the hosts whose temp directory
	// happens to be a link.
	private := t.TempDir()
	tempTarget := filepath.Join(private, "tmp-target")
	require.NoError(t, os.Mkdir(tempTarget, 0o700))
	tempRoot := filepath.Join(private, "tmp")
	require.NoError(t, os.Symlink(tempTarget, tempRoot))
	outsideRoot := filepath.Join(private, "outside")
	require.NoError(t, os.Mkdir(outsideRoot, 0o700))
	narrowTempDir(t, tempRoot)

	path := filepath.Join(workspace, "image.png")
	require.NoError(t, os.WriteFile(path, png, 0o600))
	data, mimeType, err := s.readAllowedImageFile(path)
	require.NoError(t, err)
	require.Equal(t, png, data)
	require.Equal(t, "image/png", mimeType)

	for _, root := range []string{scratch, agent.runtimeScratchRoot, generated} {
		candidate := filepath.Join(root, "image.png")
		require.NoError(t, os.WriteFile(candidate, png, 0o600))
		_, _, err = s.readAllowedImageFile(candidate)
		require.NoError(t, err)
	}
	tempCandidate := filepath.Join(tempRoot, "image.png")
	require.NoError(t, os.WriteFile(tempCandidate, png, 0o600))
	_, _, err = s.readAllowedImageFile(tempCandidate)
	require.NoError(t, err)

	require.Len(t, s.allowedImageRoots(), 5)

	_, _, err = s.readAllowedImageFile(filepath.Join(workspace, "missing.png"))
	requireImageOutputReason(t, err, imageOutputMissingFile)

	outside := filepath.Join(outsideRoot, "outside.png")
	require.NoError(t, os.WriteFile(outside, png, 0o600))
	_, _, err = s.readAllowedImageFile(outside)
	requireImageOutputReason(t, err, imageOutputPathDenied)

	symlink := filepath.Join(workspace, "escape.png")
	require.NoError(t, os.Symlink(outside, symlink))
	_, _, err = s.readAllowedImageFile(symlink)
	requireImageOutputReason(t, err, imageOutputPathDenied)

	loop := filepath.Join(workspace, "loop")
	require.NoError(t, os.Symlink(loop, loop))
	_, _, err = s.readAllowedImageFile(loop)
	requireImageOutputReason(t, err, imageOutputPathDenied)

	_, _, err = s.readAllowedImageFile(workspace)
	requireImageOutputReason(t, err, imageOutputPathDenied)

	text := filepath.Join(workspace, "text.txt")
	require.NoError(t, os.WriteFile(text, []byte("text"), 0o600))
	_, _, err = s.readAllowedImageFile(text)
	requireImageOutputReason(t, err, imageOutputNotRaster)

	limited := &session{
		agent: NewAgent(WithHome(home), WithImageLimits(ImageLimits{MaxOutputBytesPerImage: int64(len(png) - 1)})),
		cwd:   workspace,
	}
	_, _, err = limited.readAllowedImageFile(path)
	requireImageOutputReason(t, err, imageOutputTooLarge)

	saved, _, savedSize, err := s.materializeImageEvent(codex.ImageEvent{SavedPath: path})
	require.NoError(t, err)
	require.Equal(t, png, saved)
	require.Equal(t, int64(len(png)), savedSize)

	deniedData, _, deniedSize, err := s.materializeImageEvent(codex.ImageEvent{SavedPath: outside})
	requireImageOutputReason(t, err, imageOutputPathDenied)
	require.Nil(t, deniedData)
	require.Zero(t, deniedSize)
}

// TestImageOutputReadsFromTheOSTempDir pins the temp directory as an allowed
// root against the real os.TempDir, not a narrowed one: the fixture is created
// through os.MkdirTemp so it sits wherever this platform actually puts temp
// files, symlinked parents included.
func TestImageOutputReadsFromTheOSTempDir(t *testing.T) {
	png := outputFixture(t, "valid.png")
	s := outputSession(t, WithImageLimits(ImageLimits{}))

	dir, err := os.MkdirTemp("", "codex-image-output")
	require.NoError(t, err)

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	path := filepath.Join(dir, "frame_01.png")
	require.NoError(t, os.WriteFile(path, png, 0o600))

	require.False(t, pathWithinRoot(path, s.cwd))

	data, mimeType, err := s.readAllowedImageFile(path)
	require.NoError(t, err)
	require.Equal(t, png, data)
	require.Equal(t, "image/png", mimeType)
}

// TestImageOutputGuidanceSplitsRecoverableFromFatal pins the blast radius of
// every image-output verdict: an ordinary mistake the model can retry carries
// guidance and keeps the turn, the adapter's own store breaking does not.
func TestImageOutputGuidanceSplitsRecoverableFromFatal(t *testing.T) {
	recoverable := map[string]string{
		imageOutputPathDenied:    imageGuidancePathDenied,
		imageOutputMissingFile:   imageGuidanceMissingFile,
		imageOutputTooLarge:      imageGuidanceTooLarge,
		imageOutputNotRaster:     imageGuidanceNotRaster,
		imageOutputInvalidBase64: imageGuidanceInvalidBase64,
		imageOutputMIMEConflict:  imageGuidanceMIMEMismatched,
	}

	for reason, guidance := range recoverable {
		message, ok := imageOutputGuidance(&imageOutputError{reason: reason})
		require.True(t, ok, reason)
		require.Equal(t, guidance, message)

		// The guidance says what to do next and never describes the input.
		require.NotContains(t, message, "root")
		require.NotContains(t, message, "path")
	}

	_, ok := imageOutputGuidance(&imageOutputError{reason: imageOutputStorageFailure})
	require.False(t, ok)

	_, ok = imageOutputGuidance(errors.New("not an image verdict"))
	require.False(t, ok)
}

func TestAllowedImageFileInjectedFailures(t *testing.T) {
	s := outputSession(t, WithImageLimits(ImageLimits{}))
	path := filepath.Join(s.cwd, "image.png")
	require.NoError(t, os.WriteFile(path, outputFixture(t, "valid.png"), 0o600))

	originalEval := evalImageSymlinks
	evalImageSymlinks = func(string) (string, error) { return "", errors.New("eval") }
	_, _, err := s.readAllowedImageFile(path)
	requireImageOutputReason(t, err, imageOutputPathDenied)
	evalImageSymlinks = originalEval

	originalStat := statImageFile
	statImageFile = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	_, _, err = s.readAllowedImageFile(path)
	requireImageOutputReason(t, err, imageOutputMissingFile)
	statImageFile = func(string) (os.FileInfo, error) { return nil, errors.New("stat") }
	_, _, err = s.readAllowedImageFile(path)
	requireImageOutputReason(t, err, imageOutputPathDenied)
	statImageFile = originalStat

	originalOpen := openImageFile
	openImageFile = func(string) (io.ReadCloser, error) { return nil, errors.New("open") }
	_, _, err = s.readAllowedImageFile(path)
	requireImageOutputReason(t, err, imageOutputMissingFile)
	openImageFile = originalOpen

	originalRead := readImageFile
	readImageFile = func(io.Reader) ([]byte, error) { return nil, errors.New("read") }
	_, _, err = s.readAllowedImageFile(path)
	requireImageOutputReason(t, err, imageOutputMissingFile)
	readImageFile = originalRead

	originalRelative := relativeImagePath
	relativeImagePath = func(string, string) (string, error) { return "", errors.New("relative") }
	require.False(t, pathWithinRoot(path, s.cwd))
	relativeImagePath = originalRelative

	// pathWithinRoot takes an already-resolved path, which is what the read path
	// hands it; spelling one through a symlinked parent is not containment.
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)

	require.False(t, pathWithinRoot("", s.cwd))
	require.False(t, pathWithinRoot(resolved, ""))
	require.False(t, pathWithinRoot(resolved, filepath.Join(t.TempDir(), "missing")))
	require.False(t, pathWithinRoot(resolved, t.TempDir()))
	require.True(t, pathWithinRoot(resolved, s.cwd))

	// The two arguments are not interchangeable: a root is not contained in a
	// path beneath it, and a directory whose name merely starts with the root's
	// is not under the root at all.
	require.False(t, pathWithinRoot(s.cwd, resolved))

	sibling := s.cwd + "-sibling"
	require.NoError(t, os.MkdirAll(sibling, 0o700))
	t.Cleanup(func() { _ = os.RemoveAll(sibling) })

	siblingFile := filepath.Join(sibling, "image.png")
	require.NoError(t, os.WriteFile(siblingFile, []byte("x"), 0o600))

	resolvedSibling, err := filepath.EvalSymlinks(siblingFile)
	require.NoError(t, err)
	require.False(t, pathWithinRoot(resolvedSibling, s.cwd))
	require.True(t, pathWithinRoot(resolvedSibling, sibling))
}

// narrowTempDir points os.TempDir at dir for the duration of one test, so a
// case can hold a directory the adapter must refuse even though the real temp
// directory is an allowed root. Every variable os.TempDir consults on any
// supported platform is set.
func narrowTempDir(t *testing.T, dir string) {
	t.Helper()

	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, dir)
	}

	require.Equal(t, dir, os.TempDir())
}

func requireImageOutputReason(t *testing.T, err error, reason string) {
	t.Helper()

	var outputErr *imageOutputError
	require.ErrorAs(t, err, &outputErr)
	require.Equal(t, reason, outputErr.reason)
}

// requireImageRefusal asserts a recoverable image-output verdict left the turn
// running and handed its guidance to the client as the tool call's own
// content, rather than returning a turn-fatal error.
func requireImageRefusal(t *testing.T, updates []acp.SessionUpdate, err error, guidance string) {
	t.Helper()

	require.NoError(t, err)
	require.NotEmpty(t, updates)

	last := updates[len(updates)-1].ToolCallUpdate
	require.NotNil(t, last)
	require.NotNil(t, last.Status)
	require.Equal(t, acp.ToolCallStatusFailed, *last.Status)
	require.Len(t, last.Content, 1)
	require.NotNil(t, last.Content[0].Content)
	require.NotNil(t, last.Content[0].Content.Content.Text)
	require.Equal(t, guidance, last.Content[0].Content.Content.Text.Text)
}

type fixedImageStore struct {
	loadEntries []SessionStoreEntry
	loadErr     error
	appendErr   error
	deleteErr   error
	listErr     error
	sessionsErr error
	subkeys     []string
	summaries   []SessionSummary
	appended    []SessionStoreEntry
	deleted     []SessionKey
}

func (s *fixedImageStore) Append(_ context.Context, _ SessionKey, entries []SessionStoreEntry) error {
	s.appended = cloneStoreEntries(entries)

	return s.appendErr
}
func (s *fixedImageStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return cloneStoreEntries(s.loadEntries), s.loadErr
}
func (*fixedImageStore) Replace(context.Context, SessionKey, []SessionStoreReplacement) error {
	return nil
}
func (s *fixedImageStore) Delete(_ context.Context, key SessionKey) error {
	s.deleted = append(s.deleted, key)

	return s.deleteErr
}
func (s *fixedImageStore) ListSessions(context.Context) ([]SessionSummary, error) {
	return append([]SessionSummary(nil), s.summaries...), s.sessionsErr
}
func (s *fixedImageStore) ListSubkeys(context.Context, SessionKey) ([]string, error) {
	return append([]string(nil), s.subkeys...), s.listErr
}

func TestImageArtifactStoreValidation(t *testing.T) {
	ctx := context.Background()
	png := outputFixture(t, "valid.png")

	store := NewInMemorySessionStore()
	s := &session{agent: NewAgent(WithSessionStore(store)), id: "store"}
	subpath, err := s.storeImageArtifact(ctx, "image", png, "image/png")
	require.NoError(t, err)
	duplicate, err := s.storeImageArtifact(ctx, "image", png, "image/png")
	require.NoError(t, err)
	require.Equal(t, subpath, duplicate)
	artifact, err := s.loadImageArtifact(ctx, subpath)
	require.NoError(t, err)
	require.Equal(t, base64.StdEncoding.EncodeToString(png), artifact.Data)

	requireImageStoreError(t, s, "bad")

	cases := []struct {
		name    string
		entries []SessionStoreEntry
		loadErr error
	}{
		{name: "load error", loadErr: errors.New("load")},
		{name: "missing"},
		{name: "bad json", entries: []SessionStoreEntry{json.RawMessage(`bad`)}},
		{name: "incomplete", entries: []SessionStoreEntry{json.RawMessage(`{"version":1}`)}},
		{name: "invalid base64", entries: []SessionStoreEntry{json.RawMessage(`{"version":1,"mimeType":"image/png","data":"!","fingerprint":"x","createdAtUnixMilli":4102444800000}`)}},
		{name: "checksum", entries: []SessionStoreEntry{json.RawMessage(`{"version":1,"mimeType":"image/png","data":"eA==","fingerprint":"wrong","createdAtUnixMilli":4102444800000}`)}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixed := &fixedImageStore{loadEntries: testCase.entries, loadErr: testCase.loadErr}
			testSession := &session{agent: NewAgent(WithSessionStore(fixed)), id: "store"}
			_, loadErr := testSession.loadImageArtifact(ctx, imageArtifactStorePrefix+"record")
			require.Error(t, loadErr)
		})
	}

	loadFailure := &fixedImageStore{loadErr: errors.New("load")}
	_, err = (&session{agent: NewAgent(WithSessionStore(loadFailure)), id: "store"}).
		storeImageArtifact(ctx, "image", png, "image/png")
	require.Error(t, err)

	appendFailure := &fixedImageStore{appendErr: errors.New("append")}
	_, err = (&session{agent: NewAgent(WithSessionStore(appendFailure)), id: "store"}).
		storeImageArtifact(ctx, "image", png, "image/png")
	require.Error(t, err)

	conflict := &fixedImageStore{loadEntries: []SessionStoreEntry{json.RawMessage(`{"version":1}`)}}
	_, err = (&session{agent: NewAgent(WithSessionStore(conflict)), id: "store"}).
		storeImageArtifact(ctx, "image", png, "image/png")
	require.Error(t, err)

	originalMarshal := marshalImageJSON
	marshalImageJSON = func(any) ([]byte, error) { return nil, errors.New("marshal") }
	_, err = s.storeImageArtifact(ctx, "marshal", png, "image/png")
	require.Error(t, err)
	marshalImageJSON = originalMarshal

	sweepFailure := &fixedImageStore{listErr: errors.New("list")}
	_, err = (&session{agent: NewAgent(WithSessionStore(sweepFailure)), id: "store"}).
		storeImageArtifact(ctx, "image", png, "image/png")
	require.Error(t, err)
}

func TestImageArtifactTTLAndSweep(t *testing.T) {
	ctx := context.Background()
	png := outputFixture(t, "valid.png")
	store := NewInMemorySessionStore()
	agent := NewAgent(WithSessionStore(store))
	s := &session{agent: agent, id: "ttl"}

	fixedNow := time.Date(2026, time.July, 24, 0, 0, 0, 0, time.UTC)
	originalNow := timeNow
	timeNow = func() time.Time { return fixedNow }
	t.Cleanup(func() { timeNow = originalNow })

	require.NoError(t, store.Append(ctx, SessionKey{SessionID: "ttl"}, []SessionStoreEntry{
		json.RawMessage(`{"type":"session_meta","payload":{"id":"ttl"}}`),
	}))
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: "ttl", Subpath: "other/data"}, []SessionStoreEntry{
		json.RawMessage(`{"kept":true}`),
	}))

	subpath, err := s.storeImageArtifact(ctx, "image", png, "image/png")
	require.NoError(t, err)

	fixedNow = fixedNow.Add(imageArtifactTTL - time.Millisecond)
	_, err = s.loadImageArtifact(ctx, subpath)
	require.NoError(t, err)

	fixedNow = fixedNow.Add(time.Millisecond)
	_, err = s.loadImageArtifact(ctx, subpath)
	require.ErrorContains(t, err, "expired")
	subkeys, err := store.ListSubkeys(ctx, SessionKey{SessionID: "ttl"})
	require.NoError(t, err)
	require.Equal(t, []string{"other/data"}, subkeys)

	require.NoError(t, store.Append(ctx, SessionKey{SessionID: "ttl", Subpath: imageArtifactStorePrefix + "invalid"}, []SessionStoreEntry{
		json.RawMessage(`bad`),
	}))
	require.NoError(t, agent.sweepSessionImageArtifacts(ctx, "ttl"))
	subkeys, err = store.ListSubkeys(ctx, SessionKey{SessionID: "ttl"})
	require.NoError(t, err)
	require.Equal(t, []string{"other/data"}, subkeys)

	listed, err := agent.ListSessions(ctx, acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 1)
}

func TestImageArtifactSweepFailures(t *testing.T) {
	ctx := context.Background()
	imageSubpath := imageArtifactStorePrefix + "artifact"

	listFailure := &fixedImageStore{listErr: errors.New("list")}
	agent := NewAgent(WithSessionStore(listFailure))
	require.Error(t, agent.sweepSessionImageArtifacts(ctx, "session"))

	loadFailure := &fixedImageStore{
		subkeys: []string{"other", imageSubpath},
		loadErr: errors.New("load"),
	}
	agent = NewAgent(WithSessionStore(loadFailure))
	require.Error(t, agent.sweepSessionImageArtifacts(ctx, "session"))

	deleteFailure := &fixedImageStore{
		subkeys:     []string{imageSubpath},
		loadEntries: []SessionStoreEntry{json.RawMessage(`bad`)},
		deleteErr:   errors.New("delete"),
	}
	agent = NewAgent(WithSessionStore(deleteFailure))
	require.Error(t, agent.sweepSessionImageArtifacts(ctx, "session"))

	expired := &fixedImageStore{
		loadEntries: []SessionStoreEntry{json.RawMessage(
			`{"version":1,"mimeType":"image/png","data":"eA==","fingerprint":"x","createdAtUnixMilli":1}`,
		)},
		deleteErr: errors.New("delete"),
	}
	s := &session{agent: NewAgent(WithSessionStore(expired)), id: "session"}
	_, err := s.loadImageArtifact(ctx, imageSubpath)
	require.ErrorContains(t, err, "delete expired")

	listFailure.summaries = []SessionSummary{{SessionID: "session"}}
	_, err = NewAgent(WithSessionStore(listFailure)).ListSessions(ctx, acp.ListSessionsRequest{})
	require.Error(t, err)
}

func requireImageStoreError(t *testing.T, s *session, subpath string) {
	t.Helper()

	_, err := s.loadImageArtifact(context.Background(), subpath)
	require.Error(t, err)
}

func TestDurableRolloutImageExtractionHydrationAndReplay(t *testing.T) {
	ctx := context.Background()
	png := outputFixture(t, "valid.png")
	encoded := base64.StdEncoding.EncodeToString(png)
	s := outputSession(t, WithImageLimits(ImageLimits{}))

	entries := []SessionStoreEntry{
		json.RawMessage(`{"type":"event_msg","payload":{"type":"agent_message","message":"hello"}}`),
		json.RawMessage(`{"type":"response_item","payload":{"type":"image_generation_call","id":"empty","status":"failed"}}`),
		json.RawMessage(`{"type":"response_item","payload":{"type":"image_generation_call","id":"failed","status":"failed","result":"ignored","saved_path":"/secret/path"}}`),
		json.RawMessage(`{"type":"response_item","payload":{"type":"image_generation_call","id":"image","status":"completed","revised_prompt":"draw","result":"` + encoded + `"}}`),
	}
	durable, err := s.prepareDurableImageRolloutEntries(ctx, entries)
	require.NoError(t, err)
	require.NotContains(t, string(durable[2]), "ignored")
	require.NotContains(t, string(durable[3]), encoded)
	require.Contains(t, string(durable[3]), imageArtifactRefKey)

	hydrated, err := s.agent.hydrateStoredImageArtifacts(ctx, s.id, durable)
	require.NoError(t, err)
	require.Contains(t, string(hydrated[3]), encoded)

	conn := newRecordingAgentClient()
	s.agent.setAgentClient(conn)
	require.NoError(t, s.replayRollout(ctx, durable))
	var imageData string
	for _, update := range conn.updates {
		if update.Update.ToolCallUpdate != nil && len(update.Update.ToolCallUpdate.Content) > 0 {
			imageData = update.Update.ToolCallUpdate.Content[0].Content.Content.Image.Data
		}
	}
	require.Equal(t, encoded, imageData)

	_, err = s.prepareDurableImageRolloutEntries(ctx, []SessionStoreEntry{json.RawMessage(`bad`)})
	require.Error(t, err)

	// A recoverable verdict has already reached the client on the live wire,
	// so the durable rollout keeps the call and carries no bytes for it rather
	// than failing the mirror and with it the turn.
	refused, err := s.prepareDurableImageRolloutEntries(ctx, []SessionStoreEntry{
		json.RawMessage(`{"type":"response_item","payload":{"type":"image_generation_call","id":"bad","status":"completed","result":"!","saved_path":"/secret/path"}}`),
	})
	require.NoError(t, err)
	require.NotContains(t, string(refused[0]), imageArtifactRefKey)
	require.NotContains(t, string(refused[0]), "/secret/path")
	require.Contains(t, string(refused[0]), "image_generation_call")

	limited := outputSession(t, WithImageLimits(ImageLimits{MaxOutputBytesPerImage: int64(len(png) - 1)}))
	oversize, err := limited.prepareDurableImageRolloutEntries(ctx, entries[3:])
	require.NoError(t, err)
	require.NotContains(t, string(oversize[0]), imageArtifactRefKey)

	storage := &session{agent: NewAgent(WithSessionStore(appendErrorStore{}), WithImageLimits(ImageLimits{})), id: "storage"}
	_, err = storage.prepareDurableImageRolloutEntries(ctx, entries[3:])
	requireImageOutputReason(t, err, imageOutputStorageFailure)

	_, err = s.agent.hydrateStoredImageArtifacts(ctx, s.id, []SessionStoreEntry{json.RawMessage(`bad`)})
	require.Error(t, err)
	missing := []SessionStoreEntry{json.RawMessage(`{"type":"response_item","payload":{"type":"image_generation_call","id":"image","result":{"artifactSubpath":"images/missing"}}}`)}
	_, err = s.agent.hydrateStoredImageArtifacts(ctx, s.id, missing)
	requireImageOutputReason(t, err, imageOutputStorageFailure)

	originalMarshal := marshalImageJSON
	marshalImageJSON = func(any) ([]byte, error) { return nil, errors.New("marshal") }
	_, err = s.prepareDurableImageRolloutEntries(ctx, entries[2:3])
	require.Error(t, err)
	marshalImageJSON = originalMarshal

	marshalImageJSON = func(any) ([]byte, error) { return nil, errors.New("marshal") }
	_, err = s.agent.hydrateStoredImageArtifacts(ctx, s.id, durable[3:])
	require.Error(t, err)
	marshalImageJSON = originalMarshal

	storeCtx, cancel := s.agent.sessionStoreContext(ctx)
	require.NoError(t, s.agent.sessionStore().Delete(storeCtx, SessionKey{SessionID: string(s.id)}))
	cancel()
	require.NoError(t, s.agent.setAgentClientForTest())
	err = s.replayImageEntriesForTest(ctx, durable)
	requireImageOutputReason(t, err, imageOutputStorageFailure)

	direct := rolloutImageEvent(map[string]any{
		"id":         "direct",
		"type":       "image_generation_call",
		"status":     "completed",
		"saved_path": "/secret/direct.png",
	})
	require.Equal(t, "direct.png", direct.Raw["savedPath"])
	emptyPath := rolloutImageEvent(map[string]any{"id": "empty-path", "savedPath": ""})
	require.NotContains(t, emptyPath.Raw, "savedPath")

	withReference, ok := rolloutEvent(
		json.RawMessage(`{"type":"response_item","payload":{"type":"image_generation_call","id":"reference","status":"completed","result":{"artifactSubpath":"images/ref"}}}`),
	)
	require.True(t, ok)
	require.Equal(t, imageArtifactStorePrefix+"ref", withReference.Image.ArtifactRef)
	withoutReference, ok := rolloutEvent(
		json.RawMessage(`{"type":"response_item","payload":{"type":"image_generation_call","id":"inline","status":"failed"}}`),
	)
	require.True(t, ok)
	require.Empty(t, withoutReference.Image.ArtifactRef)
}

func (a *Agent) setAgentClientForTest() error {
	a.setAgentClient(newRecordingAgentClient())

	return nil
}

func (s *session) replayImageEntriesForTest(ctx context.Context, entries []SessionStoreEntry) error {
	return s.replayRollout(ctx, entries)
}

func TestImageOutputFailureEnvelope(t *testing.T) {
	s := &session{agent: NewAgent()}
	err := s.mapTurnFailure(&imageOutputError{
		reason:    imageOutputTooLarge,
		message:   "large",
		sizeBytes: 2,
		maxBytes:  1,
	})

	var requestErr *acp.RequestError
	require.ErrorAs(t, err, &requestErr)
	data, ok := requestErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, imageOutputStage, data["stage"])
	require.Equal(t, imageOutputTooLarge, data["reason"])
	require.EqualValues(t, 2, data["sizeBytes"])
	require.EqualValues(t, 1, data["maxBytes"])

	err = s.mapTurnFailure(&imageOutputError{reason: imageOutputStorageFailure, message: "store"})
	require.ErrorAs(t, err, &requestErr)
}

type failingReadCloser struct {
	io.Reader
}

func (f failingReadCloser) Close() error { return nil }

func TestEffectiveImageOutputLimitClamp(t *testing.T) {
	require.Equal(t, maxACPImageDecodedBytes, effectiveImageOutputLimit(0))
	require.Equal(t, maxACPImageDecodedBytes, effectiveImageOutputLimit(-1))
	require.Equal(t, maxACPImageDecodedBytes, effectiveImageOutputLimit(maxACPImageDecodedBytes+1))
	require.Equal(t, maxACPImageDecodedBytes, effectiveImageOutputLimit(maxACPImageDecodedBytes))
	require.Equal(t, int64(1024), effectiveImageOutputLimit(1024))
}

func TestDecodeBoundedImage(t *testing.T) {
	raw := bytes.Repeat([]byte{0xAB}, 100000)
	encoded := base64.StdEncoding.EncodeToString(raw)

	full, size, err := decodeBoundedImage(encoded, int64(len(raw)))
	require.NoError(t, err)
	require.Equal(t, int64(len(raw)), size)
	require.Len(t, full, len(raw))

	bounded, size, err := decodeBoundedImage(encoded, 100)
	require.NoError(t, err)
	require.Equal(t, int64(len(raw)), size)
	require.Len(t, bounded, 100)

	_, _, err = decodeBoundedImage("!", 10)
	require.Error(t, err)
}

func TestImageOutputFrameCapEnforcedRegardlessOfPolicy(t *testing.T) {
	ctx := context.Background()

	oversize := append([]byte("BM"), bytes.Repeat([]byte{0x00}, int(maxACPImageDecodedBytes)+1)...)
	encoded := base64.StdEncoding.EncodeToString(oversize)

	for _, limit := range []int64{0, maxACPImageDecodedBytes + 4_000_000} {
		s := outputSession(t, WithImageLimits(ImageLimits{MaxOutputBytesPerImage: limit}))
		updates, err := s.imageEventUpdates(ctx, codex.Event{
			Kind:  codex.EventImageCompleted,
			Image: codex.ImageEvent{ID: "oversize", Status: "completed", Result: encoded},
		}, ptrImageToolState(newImageToolState()))

		requireImageRefusal(t, updates, err, imageGuidanceTooLarge)

		_, _, size, materializeErr := s.materializeImageEvent(codex.ImageEvent{Result: encoded})
		require.NoError(t, materializeErr)
		require.Equal(t, int64(len(oversize)), size)
		require.Equal(t, maxACPImageDecodedBytes, effectiveImageOutputLimit(limit))
	}
}

func TestStoreImageArtifactConcurrentSingleEntry(t *testing.T) {
	ctx := context.Background()
	png := outputFixture(t, "valid.png")
	store := NewInMemorySessionStore()
	s := &session{agent: NewAgent(WithSessionStore(store)), id: "concurrent"}

	const workers = 8

	var wg sync.WaitGroup

	subpaths := make([]string, workers)
	errs := make([]error, workers)

	wg.Add(workers)

	for worker := range workers {
		go func(index int) {
			defer wg.Done()

			subpaths[index], errs[index] = s.storeImageArtifact(ctx, "shared", png, "image/png")
		}(worker)
	}

	wg.Wait()

	for worker := range workers {
		require.NoError(t, errs[worker])
		require.Equal(t, subpaths[0], subpaths[worker])
	}

	storeCtx, cancel := s.agent.sessionStoreContext(ctx)
	defer cancel()

	entries, err := s.agent.sessionStore().Load(storeCtx, SessionKey{SessionID: string(s.id), Subpath: subpaths[0]})
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestReadLimitAfterStatRace(t *testing.T) {
	png := outputFixture(t, "valid.png")
	s := outputSession(t, WithImageLimits(ImageLimits{MaxOutputBytesPerImage: int64(len(png))}))
	path := filepath.Join(s.cwd, "image.png")
	require.NoError(t, os.WriteFile(path, png, 0o600))

	originalOpen := openImageFile
	openImageFile = func(string) (io.ReadCloser, error) {
		return failingReadCloser{Reader: bytes.NewReader(append(png, 0))}, nil
	}
	_, _, err := s.readAllowedImageFile(path)
	requireImageOutputReason(t, err, imageOutputTooLarge)
	openImageFile = originalOpen
}
