package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

const (
	authMethodCodexLogin          = "codex-login"
	authMethodChatGPTAuthTokens   = "codex-chatgpt-auth-tokens" // #nosec G101 -- ACP auth method identifier, not a token.
	authMetaTerminalAuth          = "terminal-auth"
	authMetaCodexAuth             = "codexAuth"
	authMetaCommand               = "command"
	authMetaArgs                  = "args"
	authMetaLabel                 = "label"
	authChatGPTAuthTokensMetaPath = "chatgptAuthTokens" // #nosec G101 -- metadata field name, not a token.
)

func (a *Agent) authMethods(params acp.InitializeRequest) []acp.AuthMethod {
	methods := []acp.AuthMethod{}
	if params.ClientCapabilities.Auth.Terminal || clientMetaBool(params.ClientCapabilities.Meta, authMetaTerminalAuth) {
		args := []string{"--cli", "login", "--device-auth"}
		if a.options.CodexPath != "" {
			args = append([]string{"--codex", a.options.CodexPath}, args...)
		}
		if a.options.CodexHome != "" {
			args = append([]string{"--codex-home", a.options.CodexHome}, args...)
		}
		method := acp.AuthMethodTerminalInline{
			Id:          authMethodCodexLogin,
			Name:        "Codex Login",
			Description: acp.Ptr("Authenticate with the local Codex CLI"),
			Args:        args,
			Meta: map[string]any{
				authMetaTerminalAuth: map[string]any{
					authMetaCommand: os.Args[0],
					authMetaArgs:    args,
					authMetaLabel:   "Codex Login",
				},
			},
		}
		methods = append(methods, acp.AuthMethod{Terminal: &method})
	}
	methods = append(methods, acp.AuthMethod{
		Agent: &acp.AuthMethodAgent{
			Id:          authMethodChatGPTAuthTokens,
			Name:        "Codex ChatGPT tokens",
			Description: acp.Ptr("Provide external ChatGPT auth tokens for Codex"),
			Meta: map[string]any{
				authMetaCodexAuth: map[string]any{"type": authChatGPTAuthTokensMetaPath},
			},
		},
	})

	return methods
}

func (a *Agent) authCapabilities() acp.AgentAuthCapabilities {
	if !a.options.AllowAccountLogout {
		return acp.AgentAuthCapabilities{}
	}

	return acp.AgentAuthCapabilities{Logout: &acp.LogoutCapabilities{}}
}

func (a *Agent) Authenticate(ctx context.Context, params acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	if params.MethodId != authMethodChatGPTAuthTokens {
		return acp.AuthenticateResponse{}, acp.NewInvalidParams(map[string]any{"methodId": params.MethodId})
	}

	tokens, err := parseChatGPTAuthTokens(params.Meta)
	if err != nil {
		return acp.AuthenticateResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	a.setExternalAuthTokens(tokens)
	client, err := a.newClient(ctx, nil)
	if err != nil {
		a.clearExternalAuthTokens()
		return acp.AuthenticateResponse{}, err
	}
	defer client.Close(context.Background())

	account, _ := client.AccountRead(ctx)
	return acp.AuthenticateResponse{Meta: accountResponseMeta(account)}, nil
}

func (a *Agent) UnstableLogout(ctx context.Context, _ acp.UnstableLogoutRequest) (acp.UnstableLogoutResponse, error) {
	if !a.options.AllowAccountLogout {
		return acp.UnstableLogoutResponse{}, acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "Codex account logout is disabled; set WithAllowAccountLogout for adapter-owned CODEX_HOME",
		})
	}
	a.clearExternalAuthTokens()

	a.mu.Lock()
	sessions := make([]*Session, 0, len(a.sessions))
	for id, session := range a.sessions {
		sessions = append(sessions, session)
		delete(a.sessions, id)
	}
	a.mu.Unlock()

	var err error
	for _, session := range sessions {
		err = errors.Join(err, session.Close(ctx))
	}

	client, clientErr := a.newClient(ctx, nil)
	if clientErr != nil {
		return acp.UnstableLogoutResponse{}, errors.Join(err, clientErr)
	}
	defer client.Close(context.Background())
	err = errors.Join(err, client.Logout(ctx))

	return acp.UnstableLogoutResponse{}, err
}

func parseChatGPTAuthTokens(meta map[string]any) (codex.ChatGPTAuthTokens, error) {
	codexMeta, _ := meta[codexMetaKey].(map[string]any)
	authMeta, _ := codexMeta["auth"].(map[string]any)
	raw, _ := authMeta[authChatGPTAuthTokensMetaPath].(map[string]any)
	if raw == nil {
		return codex.ChatGPTAuthTokens{}, fmt.Errorf("_meta.codex.auth.chatgptAuthTokens is required")
	}
	tokens := codex.ChatGPTAuthTokens{
		AccessToken:  stringMeta(raw, "accessToken"),
		RefreshToken: stringMeta(raw, "refreshToken"),
		AccountID:    firstNonEmpty(stringMeta(raw, "chatgptAccountId"), stringMeta(raw, "accountId")),
		PlanType:     firstNonEmpty(stringMeta(raw, "chatgptPlanType"), stringMeta(raw, "planType")),
	}
	switch expires := raw["expiresAt"].(type) {
	case float64:
		tokens.ExpiresAtUnixSec = int64(expires)
	case json.Number:
		tokens.ExpiresAtUnixSec, _ = expires.Int64()
	}
	if tokens.AccessToken == "" {
		return codex.ChatGPTAuthTokens{}, fmt.Errorf("accessToken is required")
	}

	return tokens, nil
}

func accountResponseMeta(account codex.Account) map[string]any {
	values := redactedAccountMeta(account)
	if len(values) == 0 {
		return nil
	}

	return map[string]any{codexMetaKey: map[string]any{codexAccountMetaKey: values}}
}

func redactedAccountMeta(account codex.Account) map[string]any {
	out := map[string]any{}
	if account.ID != "" {
		out["id"] = account.ID
	}
	if account.Email != "" {
		out["email"] = account.Email
	}
	if account.PlanType != "" {
		out["planType"] = account.PlanType
	}
	return out
}

func clientMetaBool(meta map[string]any, key string) bool {
	value, _ := meta[key].(bool)
	return value
}

func codexAuthRequiredError(err error, account map[string]any) error {
	if err == nil || !isCodexAuthError(err) {
		return err
	}
	data := map[string]any{
		codexMetaKey: map[string]any{
			"auth": map[string]any{
				"reason":    "codex-auth-required",
				"methodIds": []string{authMethodCodexLogin, authMethodChatGPTAuthTokens},
			},
		},
	}
	if len(account) > 0 {
		codexMeta, _ := data[codexMetaKey].(map[string]any)
		codexMeta[codexAccountMetaKey] = cloneAnyMap(account)
	}

	return acp.NewAuthRequired(data)
}

func isCodexAuthError(err error) bool {
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "authentication required"),
		strings.Contains(text, "not authenticated"),
		strings.Contains(text, "not logged in"),
		strings.Contains(text, "login required"),
		strings.Contains(text, "unauthorized"),
		strings.Contains(text, "401"):
		return true
	default:
		return false
	}
}
