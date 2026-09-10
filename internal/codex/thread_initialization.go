package codex

import (
	"encoding/json"
	"errors"
	"time"
)

const fieldSource = "source"

//nolint:tagliatelle // Codex rollout metadata uses snake_case field names.
type initialThreadMetadata struct {
	SessionID     string          `json:"session_id"`
	ID            string          `json:"id"`
	Timestamp     string          `json:"timestamp"`
	Cwd           string          `json:"cwd"`
	Originator    string          `json:"originator"`
	CLIVersion    string          `json:"cli_version"`
	Source        json.RawMessage `json:"source"`
	ModelProvider string          `json:"model_provider"`
	HistoryMode   string          `json:"history_mode"`
}

// InitialThreadRollout records an empty thread using the identities and creation
// metadata returned by thread/start. It contains no turn or message.
func InitialThreadRollout(thread Thread) (json.RawMessage, error) {
	var metadata struct {
		CreatedAt   int64           `json:"createdAt"`
		CLIVersion  string          `json:"cliVersion"`
		Source      json.RawMessage `json:"source"`
		HistoryMode string          `json:"historyMode"`
	}

	raw, err := json.Marshal(thread.Raw)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, err
	}

	if thread.ID == "" || thread.SessionID != thread.ID || thread.Cwd == "" ||
		thread.Provider == "" || metadata.CreatedAt <= 0 || metadata.CLIVersion == "" ||
		metadata.HistoryMode != "paginated" || len(metadata.Source) == 0 || string(metadata.Source) == "null" {
		return nil, errors.New("new Codex thread is missing durable initialization metadata")
	}

	stamp := time.Unix(metadata.CreatedAt, 0).UTC().Format(time.RFC3339)

	return json.Marshal(struct {
		Ordinal   uint64                `json:"ordinal"`
		Timestamp string                `json:"timestamp"`
		Type      string                `json:"type"`
		Payload   initialThreadMetadata `json:"payload"`
	}{Timestamp: stamp, Type: "session_meta", Payload: initialThreadMetadata{
		SessionID: thread.SessionID, ID: thread.ID, Timestamp: stamp, Cwd: thread.Cwd,
		Originator: appServerClientName, CLIVersion: metadata.CLIVersion, Source: metadata.Source,
		ModelProvider: thread.Provider, HistoryMode: metadata.HistoryMode,
	}})
}
