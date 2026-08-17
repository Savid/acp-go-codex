package codexacp

import (
	"context"
	"encoding/json"
	"testing"
)

func TestHandleExtensionMethodErrorBranches(t *testing.T) {
	ctx := context.Background()
	closed := NewAgent()
	if err := closed.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if _, err := closed.HandleExtensionMethod(ctx, ForkSessionMethod, json.RawMessage(`{}`)); err == nil {
		t.Fatal("HandleExtensionMethod on closed agent succeeded")
	}

	agent := NewAgent()
	if _, err := agent.HandleExtensionMethod(ctx, ForkSessionMethod, json.RawMessage(`{}`)); err == nil {
		t.Fatal("HandleExtensionMethod accepted invalid fork request")
	}
}
