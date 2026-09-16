package codexacp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/sessionlog"
	"github.com/savid/acp-go-core/wire"
)

// configSubpath holds the current session configuration.
const configSubpath = sessionlog.ConfigSubpath

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
	Images                []storedImage     `json:"images,omitempty"`
	SessionID             string            `json:"sessionId"`
	NativeSessionID       string            `json:"nativeSessionId"`
	Cwd                   string            `json:"cwd"`
	AdditionalDirectories []string          `json:"additionalDirectories,omitempty"`
	RolloutPath           string            `json:"rolloutPath"`
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
	UpdatedAtUnixMilli    int64             `json:"updatedAtUnixMilli"`
}

func (s *session) record() sessionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	return sessionRecord{
		Images:                slices.Clone(s.images),
		SessionID:             string(s.id),
		NativeSessionID:       s.nativeID,
		Cwd:                   s.cwd,
		AdditionalDirectories: slices.Clone(s.additionalDirectories),
		RolloutPath:           s.rolloutPath,
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
		UpdatedAtUnixMilli:    time.Now().UnixMilli(),
	}
}

// commitMirror publishes the native rows and current session configuration
// as one durable generation.
func (s *session) commitMirror(ctx context.Context) error {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	s.mu.Lock()
	path := s.rolloutPath
	mirrored := s.mirrored
	s.mu.Unlock()

	if path == "" {
		return errors.New("session has no rollout to mirror")
	}

	rows, err := codex.ReadRows(path)
	if err != nil {
		return fmt.Errorf("read native session: %w", err)
	}

	if len(rows) < mirrored {
		return fmt.Errorf("native log shrank from %d to %d rows", mirrored, len(rows))
	}

	commitCtx, finish := s.agent.observe.StartSessionStore(ctx, "replace")
	err = sessionlog.Commit(commitCtx, s.agent.store, string(s.id), rows, s.record())
	finish(err)

	if err != nil {
		return fmt.Errorf("commit session mirror: %w", err)
	}

	s.mu.Lock()
	s.mirrored = len(rows)
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
	loadCtx, cancel := context.WithTimeout(ctx, a.options.SessionStoreLoadTimeout)
	defer cancel()

	loadCtx, finish := a.observe.StartSessionStore(loadCtx, "load")

	var record sessionRecord

	rows, found, err := sessionlog.Load(loadCtx, a.store, string(sessionID), &record)
	if err == nil && found {
		err = record.validate(string(sessionID))
	}

	finish(err)

	if err != nil {
		return storedSession{}, a.restoreRefused(ctx, sessionID, err)
	}

	return storedSession{rows: rows, record: record, found: found}, nil
}

func (r sessionRecord) validate(sessionID string) error {
	if !codex.ValidThreadID(r.NativeSessionID) || r.SessionID != sessionID || !filepath.IsAbs(r.Cwd) || !filepath.IsAbs(r.RolloutPath) || r.UpdatedAtUnixMilli <= 0 {
		return fmt.Errorf("invalid session record identity or location")
	}

	for _, directory := range r.AdditionalDirectories {
		if !filepath.IsAbs(directory) {
			return fmt.Errorf("invalid additional directory")
		}
	}

	if _, err := parseSessionMeta(inheritCarrier(sessionMeta{}, r).Meta()); err != nil {
		return err
	}

	return nil
}

// hydrate reconciles the store with Codex's own rollout before a load or
// resume. An existing rollout at least as long as the store wins and its
// newer rows are adopted; a missing or shorter one is materialized from the
// store at the path the app-server resolves the thread id to. A disagreement
// at a shared position, or a row Codex cannot read, fails the restore.
// It returns the rollout path and the rows the session now holds.
func (a *Agent) hydrate(ctx context.Context, sessionID acp.SessionId, stored storedSession, home string) (string, [][]byte, error) {
	path := stored.record.RolloutPath

	// A committed conversation whose native history is still empty carries
	// only its recorded location: no header to read, nothing to reconcile.
	if len(stored.rows) == 0 {
		return path, nil, nil
	}

	meta, ok := codex.ParseSessionMeta(stored.rows[0])
	if !ok || meta.ID != stored.record.NativeSessionID {
		return "", nil, a.restoreRefused(ctx, sessionID, fmt.Errorf("stored rollout does not open with session_meta for %s", sessionID))
	}

	relative, pathErr := filepath.Rel(home, path)
	if pathErr != nil || !filepath.IsLocal(relative) || !fileExists(path) {
		stamp := meta.Timestamp
		if stamp.IsZero() {
			return "", nil, a.restoreRefused(ctx, sessionID, errors.New("native session timestamp missing"))
		}

		path = codex.RolloutPath(home, stored.record.NativeSessionID, stamp)
	}

	native, err := codex.ReadRows(path)
	if err != nil {
		return "", nil, a.restoreRefused(ctx, sessionID, err)
	}

	rows, nativeWins, err := sessionlog.Reconcile(native, stored.rows)
	if err != nil {
		return "", nil, a.restoreRefused(ctx, sessionID, err)
	}

	// Validate both histories before adopting native rows or materializing stored rows.
	for index, row := range rows {
		if _, err := codex.DecodeRow(row); err != nil {
			return "", nil, a.restoreRefused(ctx, sessionID, fmt.Errorf("native row %d: %w", index, err))
		}
	}

	if nativeWins {
		if len(rows) > len(stored.rows) {
			if err := sessionlog.Commit(ctx, a.store, string(sessionID), rows, stored.record); err != nil {
				return "", nil, a.restoreRefused(ctx, sessionID, err)
			}
		}

		return path, rows, nil
	}

	if err := codex.WriteRows(path, rows); err != nil {
		return "", nil, a.restoreRefused(ctx, sessionID, err)
	}

	return path, rows, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && !info.IsDir()
}

func (a *Agent) restoreRefused(ctx context.Context, sessionID acp.SessionId, err error) error {
	a.log.ErrorContext(ctx, "codex session restore failed",
		slog.String("session_id", string(sessionID)), slog.String("reason", err.Error()))

	return wire.RestoreFailed(vendor)
}

// storedTitle derives a listing title: the first user message text, else the
// session id.
func storedTitle(sessionID string, rows [][]byte) string {
	if text := codex.FirstUserMessage(rows); text != "" {
		return wire.NormalizeTitle(text)
	}

	return sessionID
}
