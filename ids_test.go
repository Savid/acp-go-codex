package codexacp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/savid/acp-go-codex/internal/codex"
)

func TestSessionIDErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	origRand := sessionIDRandReader
	t.Cleanup(func() { sessionIDRandReader = origRand })
	sessionIDRandReader = strings.NewReader("short")
	if _, err := newSessionID(); err == nil {
		t.Fatal("random failure did not return error")
	}
	if _, err := NewAgent().NewSession(ctx, NewSessionRequest("/tmp/project")); err == nil {
		t.Fatal("NewSession ignored session id generation failure")
	}
	forkIDAgent := NewAgent()
	if err := forkIDAgent.storeStartedSession(newSession(forkIDAgent, "parent-id", "/tmp/project", nil, codex.Thread{ID: "parent-thread"}, newSpyCodexClient(), sessionMeta{}, nil)); err != nil {
		t.Fatalf("store fork id parent: %v", err)
	}
	raw, err := json.Marshal(ForkSessionRequest("parent-id", "/tmp/project"))
	if err != nil {
		t.Fatalf("marshal fork request: %v", err)
	}
	if _, err := forkIDAgent.HandleExtensionMethod(ctx, ForkSessionMethod, raw); err == nil {
		t.Fatal("ForkSession ignored session id generation failure")
	}
}
