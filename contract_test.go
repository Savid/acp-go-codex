package codexacp

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func TestCommandContractInitializeHasNoCommandCapability(t *testing.T) {
	resp, err := NewAgent().Initialize(context.Background(), acp.InitializeRequest{})
	if err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}

	raw, err := json.Marshal(resp.AgentCapabilities)
	if err != nil {
		t.Fatalf("marshal capabilities: %v", err)
	}
	for _, forbidden := range []string{`"command"`, `"commands"`, `"slashCommands"`, `"availableCommands"`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("initialize advertised command capability %s in %s", forbidden, raw)
		}
	}
	if path, ok := commandAdvertisingMetaPath(resp.AgentCapabilities.Meta); ok {
		t.Fatalf("initialize advertised command metadata at %s: %#v", path, resp.AgentCapabilities.Meta)
	}
}

func TestCommandContractLifecycleDoesNotEmitCommands(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		run  func(context.Context, *acp.ClientSideConnection, SessionStore) error
	}{
		{
			name: "new",
			run: func(ctx context.Context, conn *acp.ClientSideConnection, _ SessionStore) error {
				_, err := conn.NewSession(ctx, NewSessionRequest("/tmp/project"))

				return err
			},
		},
		{
			name: "resume",
			run: func(ctx context.Context, conn *acp.ClientSideConnection, _ SessionStore) error {
				session, err := conn.NewSession(ctx, NewSessionRequest("/tmp/project"))
				if err != nil {
					return err
				}
				_, err = conn.ResumeSession(ctx, ResumeSessionRequest(session.SessionId, "/tmp/project"))

				return err
			},
		},
		{
			name: "load",
			run: func(ctx context.Context, conn *acp.ClientSideConnection, store SessionStore) error {
				session, err := conn.NewSession(ctx, NewSessionRequest("/tmp/project"))
				if err != nil {
					return err
				}
				err = store.Replace(ctx, SessionKey{SessionID: string(session.SessionId)}, []SessionStoreReplacement{{
					Key: SessionKey{SessionID: string(session.SessionId)},
					Entries: []SessionStoreEntry{SessionStoreEntry(
						`{"type":"session_meta","payload":{"id":"thread-1","cwd":"/tmp/project"}}`,
					)},
				}})
				if err != nil {
					return err
				}
				_, err = conn.LoadSession(ctx, LoadSessionRequest(session.SessionId, "/tmp/project"))

				return err
			},
		},
		{
			name: "fork",
			run: func(ctx context.Context, conn *acp.ClientSideConnection, _ SessionStore) error {
				session, err := conn.NewSession(ctx, NewSessionRequest("/tmp/project"))
				if err != nil {
					return err
				}
				_, err = CallForkSession(ctx, conn, ForkSessionRequest(session.SessionId, "/tmp/project"))

				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, client, store := newCommandContractConnection(t)
			if _, err := conn.Initialize(ctx, acp.InitializeRequest{}); err != nil {
				t.Fatalf("Initialize returned error: %v", err)
			}
			if err := tc.run(ctx, conn, store); err != nil {
				t.Fatalf("%s lifecycle returned error: %v", tc.name, err)
			}
			assertNoAvailableCommandUpdates(t, client.Updates())
		})
	}
}

func TestCommandContractSlashTextPassesThroughRunTurn(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		text string
	}{
		{name: "unknown", text: "/unknown args"},
		{name: "compact", text: "/compact"},
		{name: "review", text: "/review"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newSpyCodexClient()
			agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
				return client, nil
			}))
			session, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project"))
			if err != nil {
				t.Fatalf("NewSession returned error: %v", err)
			}
			if _, err := agent.Prompt(ctx, TextPromptRequest(session.SessionId, "test-turn", tc.text)); err != nil {
				t.Fatalf("Prompt returned error: %v", err)
			}

			turn, compact, review := client.commandContractSnapshot()
			if got := runTurnText(turn); got != tc.text {
				t.Fatalf("RunTurn text = %q, want %q; prompt=%#v", got, tc.text, turn.Prompt)
			}
			if compact.ThreadID != "" {
				t.Fatalf("slash text routed to CompactThread: %#v", compact)
			}
			if review.ThreadID != "" || review.Target != nil || review.Delivery != "" {
				t.Fatalf("slash text routed to StartReview: %#v", review)
			}
		})
	}
}

func TestCommandContractSkillsAreNotCommands(t *testing.T) {
	clientType := reflect.TypeOf((*codex.Client)(nil)).Elem()
	for i := range clientType.NumMethod() {
		method := clientType.Method(i)
		name := strings.ToLower(method.Name)
		if strings.Contains(name, "skill") || strings.Contains(name, "command") {
			t.Fatalf("Codex provider boundary exposes %s; skills and commands must not be projected as AvailableCommand entries", method.Name)
		}
	}
}

func newCommandContractConnection(t *testing.T) (*acp.ClientSideConnection, *recordingClient, SessionStore) {
	t.Helper()

	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	t.Cleanup(func() {
		_ = c2aR.Close()
		_ = c2aW.Close()
		_ = a2cR.Close()
		_ = a2cW.Close()
	})

	client := &recordingClient{}
	clientConn := acp.NewClientSideConnection(client, c2aW, a2cR)
	store := NewInMemorySessionStore()
	agent := NewAgent(WithSessionStore(store), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newSpyCodexClient(), nil
	}))
	agentConn := newLocalAgentConnection(agent, a2cW, c2aR)
	agent.setAgentClient(agentConn)

	return clientConn, client, store
}

func assertNoAvailableCommandUpdates(t *testing.T, updates []acp.SessionNotification) {
	t.Helper()

	for i, notification := range updates {
		if notification.Update.AvailableCommandsUpdate != nil {
			t.Fatalf("update %d emitted available_commands_update: %#v", i, notification.Update.AvailableCommandsUpdate)
		}
	}
}

func commandAdvertisingMetaPath(value any) (string, bool) {
	return commandAdvertisingMetaPathAt(value, "_meta")
}

func commandAdvertisingMetaPathAt(value any, path string) (string, bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			childPath := path + "." + key
			switch strings.ToLower(key) {
			case "command", "commands", "slashcommands", "availablecommands", "available_commands_update":
				return childPath, true
			}
			if found, ok := commandAdvertisingMetaPathAt(child, childPath); ok {
				return found, true
			}
		}
	case []any:
		for _, child := range typed {
			if found, ok := commandAdvertisingMetaPathAt(child, path+"[]"); ok {
				return found, true
			}
		}
	}

	return "", false
}

func (c *spyCodexClient) commandContractSnapshot() (codex.TurnStartRequest, codex.ThreadCompactRequest, codex.ReviewStartRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()

	turn := c.lastTurn
	turn.Prompt = append([]codex.UserInput(nil), c.lastTurn.Prompt...)

	return turn, c.compact, c.review
}

func runTurnText(turn codex.TurnStartRequest) string {
	if len(turn.Prompt) != 1 {
		return ""
	}
	text, _ := turn.Prompt[0]["text"].(string)

	return text
}
