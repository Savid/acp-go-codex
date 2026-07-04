package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	codexacp "github.com/savid/acp-go-codex"
)

type fakeAgentConnection struct {
	initErr   error
	loadErr   error
	promptErr error

	loadCwd   string
	prompt    string
	closed    bool
	sessionID acp.SessionId
}

func (f *fakeAgentConnection) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{}, f.initErr
}

func (f *fakeAgentConnection) LoadSession(_ context.Context, params acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	f.sessionID = params.SessionId
	f.loadCwd = params.Cwd

	return acp.LoadSessionResponse{}, f.loadErr
}

func (f *fakeAgentConnection) Prompt(_ context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	f.sessionID = params.SessionId
	if len(params.Prompt) > 0 && params.Prompt[0].Text != nil {
		f.prompt = params.Prompt[0].Text.Text
	}

	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, f.promptErr
}

func (f *fakeAgentConnection) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	f.closed = true

	return acp.CloseSessionResponse{}, nil
}

func TestLoadSessionFileAndSeedStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"stored"}}`+"\n"+`{"type":"event_msg"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	entries, err := loadSessionFile(path)
	if err != nil {
		t.Fatalf("loadSessionFile returned error: %v", err)
	}
	if got := sessionIDFromEntries(entries); got != "stored" {
		t.Fatalf("sessionIDFromEntries = %q", got)
	}

	store := codexacp.NewInMemorySessionStore()
	if err := seedSessionStore(context.Background(), store, "stored", entries); err != nil {
		t.Fatalf("seedSessionStore returned error: %v", err)
	}
	loaded, err := store.Load(context.Background(), codexacp.SessionKey{SessionID: "stored"})
	if err != nil || len(loaded) != len(entries) {
		t.Fatalf("store Load len=%d err=%v", len(loaded), err)
	}
}

func TestRunImportedResumeLoadsAndPrompts(t *testing.T) {
	conn := &fakeAgentConnection{}
	cfg := config{sessionID: "stored", cwd: "/tmp/project", prompt: "continue"}
	var stdout bytes.Buffer

	err := runImportedResume(context.Background(), conn, cfg, []json.RawMessage{json.RawMessage(`{"type":"event_msg"}`)}, strings.NewReader("\n"), &stdout)
	if err != nil {
		t.Fatalf("runImportedResume returned error: %v", err)
	}
	if conn.sessionID != "stored" || conn.loadCwd != "/tmp/project" || conn.prompt != "continue" || !conn.closed {
		t.Fatalf("connection state = %#v", conn)
	}
	if !strings.Contains(stdout.String(), "resumed session: stored") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunImportedResumeErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		conn *fakeAgentConnection
	}{
		{name: "initialize", conn: &fakeAgentConnection{initErr: errors.New("init")}},
		{name: "load", conn: &fakeAgentConnection{loadErr: errors.New("load")}},
		{name: "prompt", conn: &fakeAgentConnection{promptErr: errors.New("prompt")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runImportedResume(context.Background(), tc.conn, config{sessionID: "s", cwd: "/tmp/project", prompt: "p"}, nil, strings.NewReader("\n"), io.Discard)
			if err == nil {
				t.Fatal("runImportedResume succeeded")
			}
		})
	}
}

func TestParseConfigRejectsRelativeCwd(t *testing.T) {
	_, err := parseConfig([]string{"-cwd", "relative"}, io.Discard)
	if err == nil {
		t.Fatal("parseConfig accepted relative cwd")
	}
}
