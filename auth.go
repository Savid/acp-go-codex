package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

const (
	authMethodCodexLogin          = "codex-login"
	authMethodChatGPTAuthTokens   = "codex-chatgpt-auth-tokens" // #nosec G101 -- ACP auth method identifier, not a token.
	authMetaCodexAuth             = "codexAuth"
	authChatGPTAuthTokensMetaPath = "chatgptAuthTokens" // #nosec G101 -- metadata field name, not a token.
	authMetaAuthKey               = "auth"
)

func (a *Agent) authMethods(params acp.InitializeRequest) []acp.AuthMethod {
	methods := []acp.AuthMethod{}

	if params.ClientCapabilities.Auth.Terminal {
		args := []string{"login", "-codex-device-auth"}
		if a.options.ExecutablePath != "" {
			args = append(args, "-path", a.options.ExecutablePath)
		}

		if a.options.Home != "" {
			args = append(args, "-home", a.options.Home)
		}

		if a.options.ScratchDir != "" {
			args = append(args, "-scratch-dir", a.options.ScratchDir)
		}

		method := acp.AuthMethodTerminalInline{
			Id:          authMethodCodexLogin,
			Name:        "Codex Login",
			Description: acp.Ptr("Authenticate with the local Codex CLI"),
			Type:        "terminal",
			Args:        args,
		}
		methods = append(methods, acp.AuthMethod{Terminal: &method})
	}

	methods = append(methods, acp.AuthMethod{
		Agent: &acp.AuthMethodAgent{
			Id:          authMethodChatGPTAuthTokens,
			Name:        "Codex ChatGPT tokens",
			Description: acp.Ptr("Provide external ChatGPT auth tokens for Codex"),
			Meta: map[string]any{
				authMetaCodexAuth: map[string]any{jsonFieldType: authChatGPTAuthTokensMetaPath},
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
	if err := a.ensureOpen(); err != nil {
		return acp.AuthenticateResponse{}, err
	}

	if params.MethodId != authMethodChatGPTAuthTokens {
		return acp.AuthenticateResponse{}, acp.NewInvalidParams(map[string]any{"methodId": params.MethodId})
	}

	tokens, err := parseChatGPTAuthTokens(params.Meta)
	if err != nil {
		return acp.AuthenticateResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	a.mu.Lock()
	active := len(a.sessions)
	a.mu.Unlock()

	if active != 0 {
		return acp.AuthenticateResponse{}, acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "Codex authentication requires a quiescent runtime with no loaded sessions",
		})
	}

	if closeErr := a.closeSharedRuntime(context.Background()); closeErr != nil {
		return acp.AuthenticateResponse{}, closeErr
	}

	a.setExternalAuthTokens(tokens)

	client, err := a.sharedRuntime(ctx)
	if err != nil {
		a.clearExternalAuthTokens()

		return acp.AuthenticateResponse{}, err
	}

	account, _ := client.AccountRead(ctx)

	return acp.AuthenticateResponse{Meta: accountResponseMeta(account)}, nil
}

func (a *Agent) Logout(ctx context.Context, _ acp.LogoutRequest) (acp.LogoutResponse, error) {
	if err := a.ensureOpen(); err != nil {
		return acp.LogoutResponse{}, err
	}

	if !a.options.AllowAccountLogout {
		return acp.LogoutResponse{}, acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "Codex account logout is disabled; set WithCodexAllowAccountLogout for adapter-owned CODEX_HOME",
		})
	}

	a.mu.Lock()

	sessions := make([]*session, 0, len(a.sessions))
	for id, session := range a.sessions {
		sessions = append(sessions, session)
		delete(a.sessions, id)
	}
	a.mu.Unlock()

	var err error
	for _, session := range sessions {
		err = errors.Join(err, session.Close(ctx))
	}

	a.observe.AddActiveSession(ctx, -int64(len(sessions)))

	client, clientErr := a.sharedRuntime(ctx)
	if clientErr != nil {
		return acp.LogoutResponse{}, errors.Join(err, clientErr)
	}

	err = errors.Join(err, client.Logout(ctx))

	a.clearExternalAuthTokens()
	err = errors.Join(err, a.closeSharedRuntime(context.Background()))

	return acp.LogoutResponse{}, err
}

func parseChatGPTAuthTokens(meta map[string]any) (ChatGPTAuthTokens, error) {
	codexMeta, _ := meta[codexMetaKey].(map[string]any)
	authMeta, _ := codexMeta[authMetaAuthKey].(map[string]any)

	raw, _ := authMeta[authChatGPTAuthTokensMetaPath].(map[string]any)
	if raw == nil {
		return ChatGPTAuthTokens{}, fmt.Errorf("_meta.codex.auth.chatgptAuthTokens is required")
	}

	tokens := ChatGPTAuthTokens{
		AccessToken:  stringMeta(raw, jsonFieldAccessToken),
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
		return ChatGPTAuthTokens{}, fmt.Errorf("accessToken is required")
	}

	return tokens, nil
}

func toCodexAuthTokens(tokens ChatGPTAuthTokens) codex.ChatGPTAuthTokens {
	return codex.ChatGPTAuthTokens{
		AccessToken:      tokens.AccessToken,
		RefreshToken:     tokens.RefreshToken,
		AccountID:        tokens.AccountID,
		PlanType:         tokens.PlanType,
		ExpiresAtUnixSec: tokens.ExpiresAtUnixSec,
	}
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
