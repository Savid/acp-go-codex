package codexacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

const (
	authMethodCodexLogin          = "codex-login"
	authMethodChatGPTAuthTokens   = "codex-chatgpt-auth-tokens" // #nosec G101 -- ACP auth method identifier, not a token.
	authMetaCodexAuth             = "codexAuth"
	authChatGPTAuthTokensMetaPath = "chatgptAuthTokens" // #nosec G101 -- metadata field name, not a token.
	authMetaAuthKey               = "auth"
	authMethodTypeTerminal        = "terminal"
)

// Session-scoped provider-auth extension methods. Injection is not on this
// surface: a brokered ChatGPT credential returns through the existing
// codex-chatgpt-auth-tokens ACP auth method, so there is no injection key and a
// supplied _meta.codex.options.providerAuth is an unsupported option field.
const (
	AuthMethodsMethod    = "_codex/auth/methods"
	AuthAuthorizeMethod  = "_codex/auth/authorize"
	AuthCallbackMethod   = "_codex/auth/callback"
	AuthStatusMethod     = "_codex/auth/status"
	AuthCancelMethod     = "_codex/auth/cancel"
	AuthInventoryMethod  = "_codex/auth/inventory"
	AuthCredentialMethod = "_codex/auth/credential" // #nosec G101 -- extension method name, not a credential.
	AuthDisconnectMethod = "_codex/auth/disconnect"
)

const (
	providerAuthCapabilityKey = "providerAuth"
	providerAuthMethodsField  = "methods"

	authFailedErrorTag = "codex_auth_failed"

	authFieldSessionID          = "sessionId"
	authFieldProviderID         = "providerId"
	authFieldConnectionID       = "connectionId"
	authFieldMethodsGeneration  = "methodsGeneration"
	authFieldMethod             = "method"
	authFieldAuthorizeRequestID = "authorizeRequestId"
	authFieldInputs             = "inputs"
	authFieldFlowID             = "flowId"
	authFieldInput              = "input"
	authFieldBindingGeneration  = "bindingGeneration"

	errValueInvalid = "invalid"
)

// Closed cause enum returned by a provider-auth leg.
const (
	authCauseNativeVeto         = "native_veto"
	authCauseProviderRefused    = "provider_refused"
	authCauseTransport          = "transport"
	authCauseProcess            = "process"
	authCauseTimeout            = "timeout"
	authCauseHarvestFailed      = "harvest_failed"
	authCauseUnsupportedVariant = "unsupported_variant"
	authCauseFlowExpired        = "flow_expired"
	authCauseFlowState          = "flow_state"
	authCauseFlowCancelled      = "flow_cancelled"
	authCausePolicy             = "policy"
	authCauseBindingConflict    = "binding_conflict"
)

// codexAuthClient is the native surface the provider-auth legs drive. It is
// narrower than the session client on purpose: these legs mint logins, read the
// account, and clear it, and nothing here reaches a thread or a turn.
type codexAuthClient interface {
	StartDeviceCodeLogin(context.Context) (codex.DeviceCodeLogin, error)
	StartAPIKeyLogin(context.Context, string) error
	AccountRead(context.Context) (codex.Account, error)
	Logout(context.Context) error
}

// providerAuth is the agent-scoped broker behind the provider-auth legs. It
// owns the current method catalog, the per-session flow records, and the
// durable values-free ledger.
type providerAuth struct {
	agent  *Agent
	ledger *authLedger
	// home is the CODEX_HOME the host consented to. An unheld one leaves the two
	// account-level legs unadvertised.
	home consentedHome

	mu         sync.Mutex
	generation string
	catalog    map[string][]authCatalogMethod
	flows      map[authFlowKey]*authFlow
	byID       map[string]*authFlow
	// retired holds, per key, every idempotency key whose flow a later
	// authorize replaced. Only the newest flow can be replayed verbatim, so an
	// older key is remembered to be refused rather than treated as new.
	retired map[authFlowKey]map[string]struct{}
}

type authFlowKey struct {
	sessionID  acp.SessionId
	providerID string
}

// newProviderAuth builds the broker when a usable durable ledger root is
// configured. Without one the adapter cannot record what a leg does, so it
// advertises no leg at all.
func newProviderAuth(agent *Agent) *providerAuth {
	if !authLedgerRootConfigured(agent.options) {
		return nil
	}

	ledger, err := newAuthLedger(agent.options)
	if err != nil {
		agent.log.WarnContext(context.Background(), "provider auth surface is unavailable", loggableError(err))

		return nil
	}

	return &providerAuth{
		agent:   agent,
		ledger:  ledger,
		home:    consentDirectHome(agent.options),
		flows:   make(map[authFlowKey]*authFlow),
		byID:    make(map[string]*authFlow),
		retired: make(map[authFlowKey]map[string]struct{}),
	}
}

// consentedHome is the CODEX_HOME the exact-home consent gate authorized, held
// as the directory itself rather than as the name that reached it. The gate
// decides once and the legs it enables run whenever the host asks, so a name is
// not what consent can be granted over: anything at this agent's uid may
// repoint one, and every read the account legs make would follow it.
type consentedHome struct {
	// name is the CODEX_HOME spelling the harness itself runs under. Its
	// keystore item is partitioned by that string rather than by the directory
	// it reaches, so a lookup under a resolved path addresses another item.
	name string
	path string
	root *os.Root
}

// consentDirectHome resolves the exact-home consent gate. It authorizes the one
// directory both options resolve to and never a parent, a child, or a symlink
// target, and it opens that directory so later reads are confined to it.
func consentDirectHome(options Options) consentedHome {
	if options.ProviderAuthDirectHome == "" || options.Home == "" {
		return consentedHome{}
	}

	consented, err := filepath.EvalSymlinks(options.ProviderAuthDirectHome)
	if err != nil {
		return consentedHome{}
	}

	home, err := filepath.EvalSymlinks(options.Home)
	if err != nil || consented != home {
		return consentedHome{}
	}

	root, err := os.OpenRoot(consented)
	if err != nil {
		return consentedHome{}
	}

	return consentedHome{name: filepath.Clean(options.Home), path: consented, root: root}
}

// unchanged reports whether the consented path still reaches the directory the
// gate opened. The two account legs drive native calls the app-server resolves
// for itself, and a path repointed since consent would send them somewhere
// nobody authorized.
func (h consentedHome) unchanged() bool {
	if h.root == nil {
		return false
	}

	opened, err := h.root.Stat(".")
	if err != nil {
		return false
	}

	current, err := os.Stat(h.path)
	if err != nil {
		return false
	}

	return os.SameFile(opened, current)
}

// close releases the open directory. Nothing reads through it after the agent
// shuts down, and a failure to release a descriptor at that point answers
// nothing a caller could act on.
func (h consentedHome) close() {
	if h.root == nil {
		return
	}

	_ = h.root.Close()
}

// accountLegsGated reports whether the two legs that read or clear credentials
// in the operator's own CODEX_HOME may answer.
func (p *providerAuth) accountLegsGated() bool {
	return p.home.root != nil
}

// authMethodNames lists every advertised leg, in the order the capability
// reports them. Without the exact-home consent gate the two account-level legs
// are omitted, leaving six.
func (p *providerAuth) authMethodNames() []string {
	names := []string{
		AuthMethodsMethod,
		AuthAuthorizeMethod,
		AuthCallbackMethod,
		AuthStatusMethod,
		AuthCancelMethod,
		AuthInventoryMethod,
	}
	if p.accountLegsGated() {
		names = append(names, AuthCredentialMethod, AuthDisconnectMethod)
	}

	return names
}

// capability reports the enabled leg names and no injection key. The array is
// the host's only discovery surface for which legs exist, so an absent leg is
// omitted rather than reported false.
func (p *providerAuth) capability() map[string]any {
	return map[string]any{providerAuthMethodsField: p.authMethodNames()}
}

func (a *Agent) handleAuthExtensionMethod(ctx context.Context, method string, params json.RawMessage) (any, bool, error) {
	broker := a.providerAuth
	if broker == nil {
		return nil, false, nil
	}

	switch method {
	case AuthMethodsMethod:
		result, err := broker.methods(ctx, params)

		return result, true, err
	case AuthAuthorizeMethod:
		result, err := broker.authorize(ctx, params)

		return result, true, err
	case AuthCallbackMethod:
		result, err := broker.callback(ctx, params)

		return result, true, err
	case AuthStatusMethod:
		result, err := broker.status(ctx, params)

		return result, true, err
	case AuthCancelMethod:
		result, err := broker.cancel(ctx, params)

		return result, true, err
	case AuthInventoryMethod:
		result, err := broker.inventory(ctx, params)

		return result, true, err
	case AuthCredentialMethod:
		if !broker.accountLegsGated() {
			return nil, false, nil
		}

		result, err := broker.credential(ctx, params)

		return result, true, err
	case AuthDisconnectMethod:
		if !broker.accountLegsGated() {
			return nil, false, nil
		}

		result, err := broker.disconnect(ctx, params)

		return result, true, err
	default:
		return nil, false, nil
	}
}

// authFailedError is the uniform provider-auth leg failure. Native message
// text, native response bodies, and child stderr never reach it: every failure
// becomes this closed shape.
type authFailedError struct {
	cause      string
	providerID string
	method     string
	flowID     string
}

func (f *authFailedError) Error() string {
	return authFailedErrorTag + ": " + f.cause
}

func (f *authFailedError) requestError() *acp.RequestError {
	data := map[string]any{
		jsonFieldError: authFailedErrorTag,
		jsonFieldCause: f.cause,
		"retryable":    authCauseRetryable(f.cause),
	}
	if f.providerID != "" {
		data[authFieldProviderID] = f.providerID
	}

	if f.method != "" {
		data[authFieldMethod] = f.method
	}

	if f.flowID != "" {
		data[authFieldFlowID] = f.flowID
	}

	return acp.NewAuthRequired(data)
}

// authCauseRetryable reports whether the same call could succeed unchanged. The
// three transport-shaped causes can; a refusal, a veto, and every flow-state
// answer cannot, because repeating them changes nothing.
func authCauseRetryable(cause string) bool {
	switch cause {
	case authCauseTransport, authCauseProcess, authCauseTimeout:
		return true
	default:
		return false
	}
}

func authFailed(cause string, providerID string, method string, flowID string) error {
	failure := &authFailedError{cause: cause, providerID: providerID, method: method, flowID: flowID}

	return failure.requestError()
}

// authFlowTransition maps a leg cause to the flow transition it must also
// perform. An empty state means the cause carries no transition: a refusal the
// adapter made itself never consumes the owner's authorization.
func authFlowTransition(cause string, materialInFlight bool) (string, string) {
	switch cause {
	case authCauseNativeVeto, authCauseUnsupportedVariant:
		return authStateFailed, authReasonNativeVeto
	case authCauseProviderRefused:
		return authStateFailed, authReasonProviderRefused
	case authCauseTransport:
		if materialInFlight {
			return authStateFailed, authReasonAcceptanceUnknown
		}

		return authStateFailed, authReasonTransport
	case authCauseProcess:
		if materialInFlight {
			return authStateFailed, authReasonAcceptanceUnknown
		}

		return authStateFailed, authReasonProcess
	case authCauseTimeout:
		if materialInFlight {
			return authStateFailed, authReasonAcceptanceUnknown
		}

		return authStateFailed, authReasonTransport
	case authCauseHarvestFailed:
		return authStateFailed, authReasonHarvestFailed
	case authCauseFlowExpired:
		return authStateExpired, authReasonDeadline
	default:
		return "", ""
	}
}

// authSession resolves the session a leg addresses. An unknown, unloaded, or
// closing session gets the uniform unknown-session rejection.
func (p *providerAuth) authSession(id string) (*session, error) {
	return p.agent.session(acp.SessionId(id))
}

// nativeAuthClient reports the session's live native client narrowed to the
// provider-auth surface.
func (s *session) nativeAuthClient() codexAuthClient {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()

	auth, ok := client.(codexAuthClient)
	if !ok {
		return nil
	}

	return auth
}

// authParamFields walks a leg's params object once, rejecting an unknown field,
// a duplicate field, and a non-object body with the offending field path. Every
// request object on this surface is closed, and encoding/json alone would let a
// duplicate key silently win.
func authParamFields(raw json.RawMessage, allowed ...string) (map[string]json.RawMessage, error) {
	permitted := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		permitted[name] = struct{}{}
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))

	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, invalidAuthField("params")
	}

	fields := make(map[string]json.RawMessage, len(allowed))

	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, invalidAuthField("params")
		}

		key, _ := keyToken.(string)
		if _, ok := permitted[key]; !ok {
			return nil, unsupportedAuthField(key)
		}

		if _, duplicate := fields[key]; duplicate {
			return nil, unsupportedAuthField(key)
		}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, invalidAuthField(key)
		}

		fields[key] = value
	}

	if _, err := decoder.Token(); err != nil {
		return nil, invalidAuthField("params")
	}

	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, invalidAuthField("params")
	}

	return fields, nil
}

// authRequiredString decodes a non-empty string field.
func authRequiredString(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok {
		return "", invalidAuthField(name)
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", invalidAuthField(name)
	}

	return value, nil
}

// authString decodes a string field that may be empty but must be present.
func authString(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok {
		return "", invalidAuthField(name)
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", invalidAuthField(name)
	}

	return value, nil
}

func authRequiredInt64(fields map[string]json.RawMessage, name string) (int64, error) {
	raw, ok := fields[name]
	if !ok {
		return 0, invalidAuthField(name)
	}

	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, invalidAuthField(name)
	}

	return value, nil
}

func (p *providerAuth) goSafe(name string, fn func()) {
	go func() {
		defer recoverAgentGoroutine(context.Background(), agentLogger(p.agent), name)

		fn()
	}()
}

func loggableError(err error) slog.Attr {
	return slog.String(jsonFieldError, err.Error())
}

func invalidAuthField(path string) error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldError: errValueInvalid,
		jsonFieldField: path,
	})
}

func unsupportedAuthField(path string) error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldError: errValueUnsupported,
		jsonFieldField: path,
	})
}

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
			Type:        authMethodTypeTerminal,
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
