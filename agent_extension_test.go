package codexacp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
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

// Extension params the adapter cannot decode, or that do not validate as a
// whole, name `params` rather than a Go decoder message.
func TestExtensionParamsRefusalNamesParams(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return newSpyCodexClient(), nil
	}))

	t.Cleanup(func() { _ = agent.Close() })

	want := map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: jsonFieldParams}

	for name, params := range map[string]string{
		"undecodable fork":  ForkSessionMethod + "\x00[]",
		"invalid fork":      ForkSessionMethod + "\x00{}",
		"undecodable steer": SteerTurnMethod + "\x00[]",
		"rate limits array": RateLimitsMethod + "\x00[]",
	} {
		t.Run(name, func(t *testing.T) {
			method, raw, found := splitTestExtensionCase(params)
			require.True(t, found)

			_, err := agent.HandleExtensionMethod(ctx, method, []byte(raw))
			requireInvalidParamsData(t, err, want)
		})
	}
}

func splitTestExtensionCase(value string) (string, string, bool) {
	for index := range len(value) {
		if value[index] == 0 {
			return value[:index], value[index+1:], true
		}
	}

	return "", "", false
}
