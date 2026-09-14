package codexacp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/process"
	"github.com/savid/acp-go-core/sessionlog"
	"github.com/savid/acp-go-core/wire"
)

// configSubpath holds the current session configuration.
const configSubpath = sessionlog.ConfigSubpath

// sessionRecord is the adapter-owned state a session needs to resume: where
// Codex keeps the rollout and the configuration the session was established
// with.
type sessionRecord struct {
	SessionID             string            `json:"sessionId"`
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
		SessionID:             string(s.id),
		Cwd:                   s.cwd,
		AdditionalDirectories: slices.Clone(s.additionalDirectories),
		RolloutPath:           s.rolloutPath,
		Env:                   cloneStringMap(s.options.Env),
		ExtraPathDirs:         slices.Clone(s.options.ExtraPathDirs),
		Model:                 s.model,
		Mode:                  s.mode,
		Effort:                s.effort,
		ServiceTier:           s.serviceTier,
		Personality:           s.personality,
		ApprovalPolicy:        cloneAny(s.options.ApprovalPolicy),
		SandboxPolicy:         cloneAny(s.options.SandboxPolicy),
		OutputSchema:          cloneAnyMap(s.options.OutputSchema),
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
		return nil
	}

	rows, err := codex.ReadRows(path)
	if err != nil {
		return fmt.Errorf("read native session: %w", err)
	}

	if len(rows) < mirrored {
		return fmt.Errorf("native log shrank from %d to %d rows", mirrored, len(rows))
	}

	if len(rows) == 0 {
		return nil
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

	rows, err := sessionlog.Load(loadCtx, a.store, string(sessionID), &record)
	if err == nil && len(rows) > 0 {
		err = record.validate(string(sessionID))
	}

	finish(err)

	if err != nil {
		return storedSession{}, a.restoreRefused(ctx, sessionID, err)
	}

	return storedSession{rows: rows, record: record, found: len(rows) > 0}, nil
}

func (r sessionRecord) validate(sessionID string) error {
	if r.SessionID != sessionID || !filepath.IsAbs(r.Cwd) || !filepath.IsAbs(r.RolloutPath) || r.UpdatedAtUnixMilli <= 0 {
		return fmt.Errorf("invalid session record identity or location")
	}

	if err := process.ValidateNames(r.Env); err != nil {
		return err
	}

	if err := process.ValidateExtraPathDirs(r.ExtraPathDirs); err != nil {
		return err
	}

	return nil
}

// hydrate reconciles the store with Codex's own rollout before a load or
// resume. An existing rollout at least as long as the store wins and its
// newer rows are adopted; a missing or shorter one is materialized from the
// store at the path the app-server resolves the thread id to. A disagreement
// at a shared position fails the restore. It returns the rollout path and the
// rows the session now holds.
func (a *Agent) hydrate(ctx context.Context, sessionID acp.SessionId, stored storedSession, home string) (string, [][]byte, error) {
	meta, ok := codex.ParseSessionMeta(stored.rows[0])
	if !ok || meta.ID != string(sessionID) {
		return "", nil, a.restoreRefused(ctx, sessionID, fmt.Errorf("stored rollout does not open with session_meta for %s", sessionID))
	}

	path := stored.record.RolloutPath
	if path == "" || !fileExists(path) {
		stamp := meta.Timestamp
		if stamp.IsZero() {
			return "", nil, a.restoreRefused(ctx, sessionID, fmt.Errorf("native session timestamp missing"))
		}

		path = codex.RolloutPath(home, string(sessionID), stamp)
	}

	native, err := codex.ReadRows(path)
	if err != nil {
		return "", nil, a.restoreRefused(ctx, sessionID, err)
	}

	if _, err := sessionlog.Reconcile(native, stored.rows); err != nil {
		return "", nil, a.restoreRefused(ctx, sessionID, err)
	}

	if len(native) >= len(stored.rows) {
		if len(native) > len(stored.rows) {
			if err := sessionlog.Commit(ctx, a.store, string(sessionID), native, stored.record); err != nil {
				return "", nil, a.restoreRefused(ctx, sessionID, err)
			}
		}

		return path, native, nil
	}

	if err := codex.WriteRows(path, stored.rows); err != nil {
		return "", nil, a.restoreRefused(ctx, sessionID, err)
	}

	return path, stored.rows, nil
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
		return normalizeTitle(text)
	}

	return sessionID
}
