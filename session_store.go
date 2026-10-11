package codexacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/observer"
	"github.com/savid/acp-go-core/sessionlog"
	"github.com/savid/acp-go-core/wire"
)

type storedImage struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Data string `json:"data"`
	MIME string `json:"mime"`
}

// sessionRecord is the adapter-owned state a session needs to resume: where
// Codex keeps the rollout and the configuration the session was established
// with.
type sessionRecord struct {
	// Images is the encoded array of admitted images. The session keeps it
	// encoded, so a commit never re-encodes image bytes that did not change.
	Images                json.RawMessage   `json:"images,omitempty"`
	SessionID             string            `json:"sessionId"`
	NativeSessionID       string            `json:"nativeSessionId"`
	Title                 string            `json:"title,omitempty"`
	Cwd                   string            `json:"cwd"`
	AdditionalDirectories []string          `json:"additionalDirectories,omitempty"`
	NativePath            string            `json:"nativePath"`
	Env                   map[string]string `json:"env,omitempty"`
	ExtraPathDirs         []string          `json:"extraPathDirs,omitempty"`
	Model                 string            `json:"model,omitempty"`
	Mode                  string            `json:"mode,omitempty"`
	Effort                string            `json:"effort,omitempty"`
	ServiceTier           string            `json:"serviceTier,omitempty"`
	Personality           string            `json:"personality,omitempty"`
	ApprovalPolicy        any               `json:"approvalPolicy,omitempty"`
	SandboxPolicy         any               `json:"sandboxPolicy,omitempty"`
	OutputSchema          map[string]any    `json:"outputSchema,omitempty"`
}

func (s *session) record() sessionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	return sessionRecord{
		Images:                s.images,
		SessionID:             string(s.id),
		NativeSessionID:       s.nativeID,
		Title:                 s.title,
		Cwd:                   s.cwd,
		AdditionalDirectories: slices.Clone(s.additionalDirectories),
		NativePath:            s.rolloutPath,
		Env:                   maps.Clone(s.options.Env),
		ExtraPathDirs:         slices.Clone(s.options.ExtraPathDirs),
		Model:                 s.model,
		Mode:                  s.mode,
		Effort:                s.effort,
		ServiceTier:           s.serviceTier,
		Personality:           s.personality,
		ApprovalPolicy:        wire.CloneValue(s.options.ApprovalPolicy),
		SandboxPolicy:         wire.CloneValue(s.options.SandboxPolicy),
		OutputSchema:          wire.CloneMap(s.options.OutputSchema),
	}
}

// commitMirror publishes the rollout rows written since the last commit and
// the current session configuration as one durable generation.
func (s *session) commitMirror(ctx context.Context) error {
	if s.ephemeral {
		return nil
	}

	s.mu.Lock()
	path := s.rolloutPath
	s.mu.Unlock()

	if path == "" {
		return errors.New("session has no rollout to mirror")
	}

	// The record is sampled under the mirror's lock after the read, so it
	// describes the rows it is committed with; it takes s.mu, which is never
	// held across a commit.
	commitCtx, finish := s.agent.observe.StartSessionStore(ctx, observer.SessionStoreCommit)
	err := s.mirror.Commit(commitCtx, sessionlog.File(path), func(sessionlog.Tail) (any, error) { return s.record(), nil })
	finish(err)

	if err != nil {
		return fmt.Errorf("commit session mirror: %w", err)
	}

	s.mu.Lock()
	s.persisted = true
	s.mu.Unlock()

	return nil
}

// storedSession is what the store holds for one session id.
type storedSession struct {
	rows   [][]byte
	record sessionRecord
	found  bool
}

// loadStored reads the native rows and required current configuration.
func (a *Agent) loadStored(ctx context.Context, sessionID acp.SessionId) (storedSession, error) {
	loadCtx, finish := a.observe.StartSessionStore(ctx, observer.SessionStoreLoad)

	var record sessionRecord

	rows, found, err := sessionlog.Load(loadCtx, a.store, string(sessionID), &record)
	if err == nil && found {
		err = record.validate(string(sessionID))
	}

	finish(err)

	if err != nil {
		return storedSession{}, wire.RestoreRefused(ctx, a.log, vendor, sessionID, err)
	}

	return storedSession{rows: rows, record: record, found: found}, nil
}

// loadStoredRecord reads only the configuration of a stored session.
func (a *Agent) loadStoredRecord(ctx context.Context, sessionID acp.SessionId) (sessionRecord, bool, error) {
	loadCtx, finish := a.observe.StartSessionStore(ctx, observer.SessionStoreLoadRecord)

	var record sessionRecord

	found, err := sessionlog.LoadRecord(loadCtx, a.store, string(sessionID), &record)
	if err == nil && found {
		err = record.validate(string(sessionID))
	}

	finish(err)

	return record, found, err
}

func (r sessionRecord) validate(sessionID string) error {
	if !codex.ValidThreadID(r.NativeSessionID) || r.SessionID != sessionID || !filepath.IsAbs(r.Cwd) || !filepath.IsAbs(r.NativePath) {
		return errors.New("invalid session record identity or location")
	}

	for _, directory := range r.AdditionalDirectories {
		if !filepath.IsAbs(directory) {
			return errors.New("invalid additional directory")
		}
	}

	if _, err := parseSessionMeta(inheritCarrier(sessionMeta{}, r).Meta()); err != nil {
		return err
	}

	return nil
}

// decodeImages reads a record's admitted images, refusing unknown fields.
func decodeImages(encoded json.RawMessage) ([]storedImage, error) {
	if len(encoded) == 0 {
		return nil, nil
	}

	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()

	var images []storedImage
	if err := decoder.Decode(&images); err != nil {
		return nil, fmt.Errorf("decode stored images: %w", err)
	}

	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing stored image data")
	}

	return images, nil
}

// appendImage returns a new encoding of the images with saved added; the
// previous encoding is left untouched for any record still holding it.
func appendImage(encoded json.RawMessage, saved storedImage) (json.RawMessage, error) {
	item, err := json.Marshal(saved)
	if err != nil {
		return nil, err
	}

	if len(encoded) < 2 || bytes.Equal(encoded, []byte("[]")) {
		return slices.Concat([]byte("["), item, []byte("]")), nil
	}

	return slices.Concat(encoded[:len(encoded)-1], []byte(","), item, []byte("]")), nil
}

// hydrate reconciles the store with Codex's own rollout before a load or
// resume. An existing rollout at least as long as the store wins and its
// newer rows are adopted; a missing or shorter one is materialized from the
// store at the path the app-server resolves the thread id to. A
// disagreement at a shared position, or a row Codex cannot read, fails the
// restore. The thread is unloaded first, so no app-server still writes the
// rollout while it is reconciled. It returns the rollout path and the rows
// the session now holds.
func (s *session) hydrate(ctx context.Context, stored storedSession, runtime *runtime) (string, [][]byte, error) {
	path, err := hydratePath(stored, runtime.home)
	if err != nil {
		return "", nil, err
	}

	nativeID := stored.record.NativeSessionID

	unloadCtx, cancel := context.WithTimeout(ctx, threadUnloadTimeout)
	err = runtime.client.UnloadThread(unloadCtx, nativeID)

	cancel()

	if err != nil {
		return "", nil, fmt.Errorf("unload thread: %w", err)
	}

	rows, err := s.repairStored(ctx, stored, path)
	if err != nil {
		return "", nil, err
	}

	rows, err = s.mirror.Hydrate(ctx, path, rows, stored.record, func(rows [][]byte) error {
		if meta, ok := codex.ParseSessionMeta(rows[0]); !ok || meta.ID != nativeID {
			return fmt.Errorf("rollout does not open with session_meta for %s", nativeID)
		}

		for index, row := range rows {
			if _, decodeErr := codex.DecodeRow(row); decodeErr != nil {
				return fmt.Errorf("native row %d: %w", index, decodeErr)
			}
		}

		return nil
	})
	if err != nil {
		return "", nil, err
	}

	return path, rows, nil
}

// repairStored repairs a stored cut-off call before hydration. A store that
// holds one usually means the rollout holds the same call: the turn-time
// repair failed or never ran. Both are repaired, and the repaired history is
// republished to the store, so hydration reconciles the generation the store
// holds. It returns the rows the store holds.
func (s *session) repairStored(ctx context.Context, stored storedSession, path string) ([][]byte, error) {
	rows, repaired, err := codex.RepairCallArguments(stored.rows)
	if err != nil || repaired == 0 {
		return stored.rows, err
	}

	native, err := repairRollout(path)
	if err != nil {
		return nil, fmt.Errorf("repair rollout: %w", err)
	}

	s.mirror.Reset()

	commitCtx, finish := s.agent.observe.StartSessionStore(ctx, observer.SessionStoreCommit)
	err = s.mirror.Commit(commitCtx, sessionlog.Rows(rows), func(sessionlog.Tail) (any, error) { return stored.record, nil })
	finish(err)

	if err != nil {
		return nil, fmt.Errorf("republish repaired history: %w", err)
	}

	s.agent.log.WarnContext(ctx, "codex restore repaired cut-off function calls",
		slog.String("session_id", string(s.id)), slog.Int("stored_calls", repaired), slog.Int("native_calls", native))

	return rows, nil
}

// hydratePath is where the stored rollout is resident in home. A committed
// conversation whose native history is still empty carries only its
// recorded location: Codex writes no rollout before a thread's first turn,
// so the location is re-rooted by its file name in the home the thread
// resumes from, never followed outside it. A nonempty history is placed at
// its recorded path when that is inside home, else at the path its
// session_meta names.
func hydratePath(stored storedSession, home string) (string, error) {
	path := stored.record.NativePath
	nativeID := stored.record.NativeSessionID

	if len(stored.rows) == 0 {
		local, ok := codex.RelocateRollout(home, path, nativeID)
		if !ok {
			return "", errors.New("stored rollout path names no rollout for the native session")
		}

		return local, nil
	}

	meta, ok := codex.ParseSessionMeta(stored.rows[0])
	if !ok || meta.ID != nativeID {
		return "", fmt.Errorf("stored rollout does not open with session_meta for %s", nativeID)
	}

	if relative, err := filepath.Rel(home, path); err == nil && filepath.IsLocal(relative) && fileExists(path) {
		return path, nil
	}

	if meta.Timestamp.IsZero() {
		return "", errors.New("native session timestamp missing")
	}

	return codex.RolloutPath(home, nativeID, meta.Timestamp), nil
}

// resumeStoredThread replaces an unpersisted empty thread while retaining the
// ACP identity. The caller commits the new native binding before publication.
func (s *session) resumeStoredThread(ctx context.Context, runtime *runtime, rows [][]byte) (codex.Thread, error) {
	thread, err := runtime.client.ResumeThread(ctx, s.threadResume(), runtime.nativePath)
	if err == nil || len(rows) != 0 || !codex.IsMissingThread(err, s.nativeID) {
		return thread, err
	}

	native, readErr := sessionlog.ReadJSONL(s.rolloutPath, sessionlog.Cursor{})
	if readErr != nil {
		return codex.Thread{}, readErr
	}

	if len(native.Rows) != 0 {
		return codex.Thread{}, err
	}

	thread, err = runtime.client.StartThread(ctx, s.threadStart(), runtime.nativePath)
	if err != nil {
		return codex.Thread{}, err
	}

	s.nativeID = thread.ID
	s.mirror.Reset()

	return thread, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && !info.IsDir()
}
