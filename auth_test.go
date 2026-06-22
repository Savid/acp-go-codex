package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func TestAuthLoginLogoutGuardAndMetadata(t *testing.T) {
	client := newSpyCodexClient()
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }))
	ctx := context.Background()

	_, err := agent.Logout(ctx, acp.LogoutRequest{})
	if err == nil {
		t.Fatal("logout without explicit opt-in succeeded")
	}

	meta := map[string]any{
		codexMetaKey: map[string]any{
			"auth": map[string]any{
				authChatGPTAuthTokensMetaPath: map[string]any{
					"accessToken":      "access",
					"refreshToken":     "refresh",
					"chatgptAccountId": "acct",
					"chatgptPlanType":  "plus",
					"expiresAt":        float64(123),
				},
			},
		},
	}
	authResp, err := agent.Authenticate(ctx, acp.AuthenticateRequest{MethodId: authMethodChatGPTAuthTokens, Meta: meta})
	if err != nil {
		t.Fatalf("Authenticate returned error: %v", err)
	}
	codexMeta, ok := authResp.Meta[codexMetaKey].(map[string]any)
	if !ok {
		t.Fatalf("auth meta = %#v", authResp.Meta)
	}
	account, ok := codexMeta[codexAccountMetaKey].(map[string]any)
	if !ok {
		t.Fatalf("account meta = %#v", codexMeta)
	}
	if account["id"] != "acct" || account["email"] != "user@example.com" {
		t.Fatalf("account meta = %#v", account)
	}
	if _, ok := account["accessToken"]; ok {
		t.Fatalf("account meta leaked token: %#v", account)
	}
	if client.login.AccessToken != "access" || client.login.RefreshToken != "refresh" || client.login.ExpiresAtUnixSec != 123 {
		t.Fatalf("stored login tokens were not applied to provider: %#v", client.login)
	}
	if _, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project")); err != nil {
		t.Fatalf("token-backed NewSession returned error: %v", err)
	}
	if client.login.AccessToken != "access" {
		t.Fatalf("token-backed session did not reuse external tokens: %#v", client.login)
	}

	logoutAgent := NewAgent(
		WithAllowAccountLogout(true),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	if _, err := logoutAgent.Logout(ctx, acp.LogoutRequest{}); err != nil {
		t.Fatalf("logout with opt-in returned error: %v", err)
	}
	if !client.loggedOut {
		t.Fatal("client Logout was not called")
	}
}

func TestAuthCapabilitiesTerminalArgsAndAuthRequired(t *testing.T) {
	agent := NewAgent(WithCodexPath("/bin/codex"), WithCodexHome("/tmp/codex-home"), WithAllowAccountLogout(true))
	resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{
		ClientCapabilities: acp.ClientCapabilities{
			Auth: acp.AuthCapabilities{Terminal: true},
		},
	})
	if err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if resp.AgentCapabilities.Auth.Logout == nil {
		t.Fatal("logout capability was not advertised")
	}
	if len(resp.AuthMethods) == 0 || resp.AuthMethods[0].Terminal == nil {
		t.Fatalf("auth methods = %#v", resp.AuthMethods)
	}
	args := resp.AuthMethods[0].Terminal.Args
	if !containsAll(jsonString(args), "--codex", "/bin/codex", "--codex-home", "/tmp/codex-home", "--cli", "login") {
		t.Fatalf("terminal auth args = %#v", args)
	}

	err = codexAuthRequiredError(errors.New("not logged in"), map[string]any{"id": "acct"})
	reqErr, ok := err.(*acp.RequestError)
	if !ok || reqErr.Message != "Authentication required" {
		t.Fatalf("auth required error = %#v", err)
	}
	if codexAuthRequiredError(errors.New("other"), nil).Error() != "other" {
		t.Fatal("non-auth error was changed")
	}
}

func TestParseChatGPTAuthTokensBranches(t *testing.T) {
	if _, err := parseChatGPTAuthTokens(map[string]any{codexMetaKey: map[string]any{"auth": map[string]any{authChatGPTAuthTokensMetaPath: map[string]any{"refreshToken": "r"}}}}); err == nil {
		t.Fatal("missing access token parsed")
	}
}

func TestAuthHelperBranches(t *testing.T) {
	if _, err := parseChatGPTAuthTokens(map[string]any{}); err == nil {
		t.Fatal("missing tokens parsed")
	}
	tokens, err := parseChatGPTAuthTokens(map[string]any{codexMetaKey: map[string]any{"auth": map[string]any{authChatGPTAuthTokensMetaPath: map[string]any{"accessToken": "a", "accountId": "acct", "planType": "plus", "expiresAt": json.Number("42")}}}})
	if err != nil || tokens.ExpiresAtUnixSec != 42 || tokens.AccountID != "acct" || tokens.PlanType != "plus" {
		t.Fatalf("tokens = %#v err=%v", tokens, err)
	}
	if accountResponseMeta(codex.Account{}) != nil {
		t.Fatal("empty account emitted meta")
	}
	if clientMetaBool(map[string]any{"x": "true"}, "x") {
		t.Fatal("non-bool client meta parsed as true")
	}
	if !isCodexAuthError(errors.New("401 unauthorized")) || isCodexAuthError(errors.New("plain failure")) {
		t.Fatal("auth error classifier failed")
	}
}
