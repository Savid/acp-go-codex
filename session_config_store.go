package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/coder/acp-go-sdk"
)

const (
	sessionConfigStoreSubpath  = "session-config/v1"
	sessionConfigVersion       = 1
	sessionConfigFieldVersion  = "version"
	sessionConfigFieldRevision = "revision"
)

type durableSessionConfig struct {
	Version       int               `json:"version"`
	SessionID     string            `json:"sessionId"`
	Revision      int               `json:"revision"`
	Env           map[string]string `json:"env"`
	ExtraPathDirs []string          `json:"extraPathDirs"`
}

func (s *session) commitDurableSessionConfig(ctx context.Context, store SessionStore) error {
	if s.durableConfigCommitted {
		return nil
	}

	s.mu.Lock()
	record := durableSessionConfig{
		Version:       sessionConfigVersion,
		SessionID:     string(s.id),
		Revision:      s.durableConfigRevision + 1,
		Env:           nonNilStringMap(s.env),
		ExtraPathDirs: nonNilStrings(s.extraPathDirs),
	}
	s.mu.Unlock()

	entry, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode Codex durable session configuration: %w", err)
	}

	key := SessionKey{SessionID: string(s.id), Subpath: sessionConfigStoreSubpath}
	if err := appendRolloutEntries(ctx, store, key, []SessionStoreEntry{entry}); err != nil {
		return fmt.Errorf("commit Codex durable session configuration: %w", err)
	}

	s.durableConfigRevision = record.Revision
	s.durableConfigCommitted = true

	return nil
}

func (a *Agent) loadDurableSessionConfig(
	ctx context.Context,
	sessionID acp.SessionId,
) (durableSessionConfig, error) {
	loadCtx, cancel := a.sessionStoreContext(ctx)
	defer cancel()

	key := SessionKey{SessionID: string(sessionID), Subpath: sessionConfigStoreSubpath}

	entries, err := a.sessionStore().Load(loadCtx, key)
	if err != nil {
		return durableSessionConfig{}, err
	}

	if len(entries) == 0 {
		return durableSessionConfig{}, errors.New("stored Codex session configuration is required")
	}

	var current durableSessionConfig

	for index, entry := range entries {
		decoded, err := decodeDurableSessionConfig(entry)
		if err != nil {
			return durableSessionConfig{}, fmt.Errorf("session configuration entry %d: %w", index, err)
		}

		if decoded.SessionID != string(sessionID) {
			return durableSessionConfig{}, fmt.Errorf("session configuration entry %d has mismatched session identity", index)
		}

		if decoded.Revision != index+1 {
			return durableSessionConfig{}, fmt.Errorf("session configuration entry %d has regressed or non-contiguous revision", index)
		}

		current = decoded
	}

	return current, nil
}

func decodeDurableSessionConfig(entry SessionStoreEntry) (durableSessionConfig, error) {
	if err := rejectDuplicateRolloutKeys(entry); err != nil {
		return durableSessionConfig{}, err
	}

	members, err := decodeExactJSONObject(entry)
	if err != nil {
		return durableSessionConfig{}, errors.New("session configuration must be one JSON object")
	}

	required := []string{sessionConfigFieldVersion, jsonFieldSessionID, sessionConfigFieldRevision, metaEnvKey, metaExtraPathDirsKey}
	if len(members) != len(required) {
		return durableSessionConfig{}, errors.New("session configuration has an unknown or missing field")
	}

	for _, name := range required {
		if _, ok := members[name]; !ok {
			return durableSessionConfig{}, fmt.Errorf("session configuration field %q is required", name)
		}
	}

	var decoded durableSessionConfig
	for name, target := range map[string]any{
		sessionConfigFieldVersion:  &decoded.Version,
		jsonFieldSessionID:         &decoded.SessionID,
		sessionConfigFieldRevision: &decoded.Revision,
		metaEnvKey:                 &decoded.Env,
		metaExtraPathDirsKey:       &decoded.ExtraPathDirs,
	} {
		if decodeErr := json.Unmarshal(members[name], target); decodeErr != nil {
			return durableSessionConfig{}, fmt.Errorf("decode session configuration field %q: %w", name, decodeErr)
		}
	}

	if decoded.Version != sessionConfigVersion || decoded.SessionID == "" || decoded.Revision <= 0 ||
		decoded.Env == nil || decoded.ExtraPathDirs == nil {
		return durableSessionConfig{}, errors.New("session configuration is incomplete")
	}

	env, err := validatedSessionEnv(decoded.Env)
	if err != nil {
		return durableSessionConfig{}, err
	}

	paths, err := validatedExtraPathDirs(decoded.ExtraPathDirs)
	if err != nil {
		return durableSessionConfig{}, err
	}

	decoded.Env = env
	decoded.ExtraPathDirs = paths

	return decoded, nil
}

func resolveStoredSessionCarriers(meta sessionMeta, stored durableSessionConfig) (sessionMeta, bool) {
	if !meta.EnvPresent {
		meta.Env = cloneStringMap(stored.Env)
	}

	if !meta.ExtraPathDirsPresent {
		meta.ExtraPathDirs = cloneStrings(stored.ExtraPathDirs)
	}

	matches := maps.Equal(meta.Env, stored.Env) && slices.Equal(meta.ExtraPathDirs, stored.ExtraPathDirs)

	return meta, matches
}

func resolveActiveSessionCarriers(meta sessionMeta, active *session) sessionMeta {
	if active == nil || (meta.EnvPresent && meta.ExtraPathDirsPresent) {
		return meta
	}

	active.mu.Lock()
	defer active.mu.Unlock()

	if !meta.EnvPresent {
		meta.Env = cloneStringMap(active.env)
		meta.EnvPresent = true
	}

	if !meta.ExtraPathDirsPresent {
		meta.ExtraPathDirs = cloneStrings(active.extraPathDirs)
		meta.ExtraPathDirsPresent = true
	}

	return meta
}

func configureLoadedSessionPersistence(session *session, stored durableSessionConfig, matches bool) {
	session.mirrorMu.Lock()
	defer session.mirrorMu.Unlock()

	session.durableConfigRevision = stored.Revision
	session.durableConfigCommitted = matches
}

func (s *session) pendingDurableSessionConfig(revision int) bool {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	return revision > 0 && s.durableConfigRevision == revision && !s.durableConfigCommitted
}

func (s *session) commitPendingDurableSessionConfig(ctx context.Context) error {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	store := s.agent.options.SessionStore
	if store == nil || s.durableConfigRevision == 0 || s.durableConfigCommitted {
		return nil
	}

	return s.commitDurableSessionConfig(ctx, store)
}

func nonNilStringMap(value map[string]string) map[string]string {
	if value == nil {
		return map[string]string{}
	}

	return cloneStringMap(value)
}

func nonNilStrings(value []string) []string {
	if value == nil {
		return []string{}
	}

	return cloneStrings(value)
}
