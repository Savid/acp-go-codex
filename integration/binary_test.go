//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"

	codexacp "github.com/savid/acp-go-codex"
)

func TestSmokeSessionLifecycle(t *testing.T) {
	requireIntegration(t)

	h := newHarness(t, false)
	ctx := h.ctx(t)

	init, err := h.conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)
	require.Empty(t, init.AuthMethods)
	require.True(t, init.AgentCapabilities.LoadSession)

	cwd := t.TempDir()

	session, err := h.conn.NewSession(ctx, codexacp.NewSessionRequest(cwd))
	require.NoError(t, err)
	require.NotEmpty(t, session.SessionId)

	list, err := h.conn.ListSessions(ctx, codexacp.ListSessionsRequest())
	require.NoError(t, err)
	require.Len(t, list.Sessions, 1)

	_, err = h.conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)

	_, err = h.conn.UnstableDeleteSession(ctx, codexacp.DeleteSessionRequest(session.SessionId))
	require.NoError(t, err)

	_, err = h.conn.LoadSession(ctx, codexacp.LoadSessionRequest(session.SessionId, cwd))
	require.Equal(t, "unknown session", requestErrorData(t, err)["error"])
}

func TestLivePromptResumeAndPath(t *testing.T) {
	requireLive(t)

	h := newHarness(t, true)
	ctx := h.ctx(t)

	_, err := h.conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	cwd := t.TempDir()
	binDir := filepath.Join(t.TempDir(), "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "acp-marker"), []byte("#!/bin/sh\necho MARKER_OK\n"), 0o700))

	session, err := h.conn.NewSession(ctx, codexacp.NewSessionRequest(cwd, liveModel(),
		codexacp.WithSessionCodexOptions(codexacp.NewCodexOptions(codexacp.WithCodexExtraPathDirs(binDir), codexacp.WithCodexApprovalPolicy("never")))))
	require.NoError(t, err)

	resp, err := h.conn.Prompt(ctx, codexacp.TextPromptRequest(session.SessionId, "Reply with exactly LIVE_OK and nothing else. Do not use tools."))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, h.rec.text(), "LIVE_OK")

	resp, err = h.conn.Prompt(ctx, codexacp.TextPromptRequest(session.SessionId, "Run the shell command `acp-marker` and reply with its exact output and nothing else."))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, h.rec.text(), "MARKER_OK")

	_, err = h.conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)

	matches, err := filepath.Glob(filepath.Join(h.home, "sessions", "*", "*", "*", "rollout-*-"+string(session.SessionId)+".jsonl"))
	require.NoError(t, err)
	require.Len(t, matches, 1, "codex keeps the rollout in its own home after close")

	_, err = h.conn.ResumeSession(ctx, codexacp.ResumeSessionRequest(session.SessionId, cwd, liveModel()))
	require.NoError(t, err)

	resp, err = h.conn.Prompt(ctx, codexacp.TextPromptRequest(session.SessionId, "Reply with exactly RESUME_OK and nothing else."))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, h.rec.text(), "RESUME_OK")
}
