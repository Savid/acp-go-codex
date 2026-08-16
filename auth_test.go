package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
		WithHome(t.TempDir()),
		WithCodexAllowAccountLogout(true),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	if _, err := logoutAgent.Logout(ctx, acp.LogoutRequest{}); err != nil {
		t.Fatalf("logout with opt-in returned error: %v", err)
	}
	if !client.loggedOut {
		t.Fatal("client Logout was not called")
	}
}

func TestLogoutRejectsAnEnvironmentSelectedCodexHomeBeforeNativeCreation(t *testing.T) {
	client := newSpyCodexClient()
	agent := NewAgent(
		WithCodexAllowAccountLogout(true),
		WithEnv(map[string]string{"CODEX_HOME": t.TempDir()}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			return client, nil
		}),
	)

	_, err := agent.Logout(t.Context(), acp.LogoutRequest{})
	if err == nil || !containsAll(err.Error(), "explicit WithHome", "reserved") {
		t.Fatalf("logout error = %v", err)
	}
	if client.loggedOut {
		t.Fatal("logout reached the native client")
	}
}

func TestAuthCapabilitiesTerminalArgsAndAuthRequired(t *testing.T) {
	agent := NewAgent(
		WithExecutablePath("/bin/codex"),
		WithHome("/tmp/codex-home"),
		WithScratchDir("/tmp/codex-scratch"),
		WithCodexAllowAccountLogout(true),
	)
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
	if !containsAll(jsonString(args), "login", "-codex-device-auth", "-path", "/bin/codex", "-home", "/tmp/codex-home", "-scratch-dir", "/tmp/codex-scratch") {
		t.Fatalf("terminal auth args = %#v", args)
	}

	err = codexAuthRequiredError(errors.New("not logged in"), map[string]any{"id": "acct"})
	reqErr, ok := err.(*acp.RequestError)
	if !ok || reqErr.Message != "Authentication required" {
		t.Fatalf("auth required error = %#v", err)
	}

	authData, ok := reqErr.Data.(map[string]any)
	if !ok {
		t.Fatalf("auth required data = %#v", reqErr.Data)
	}
	if authData[jsonFieldError] != valueTurnFailed {
		t.Fatalf("auth required error tag = %v, want %s", authData[jsonFieldError], valueTurnFailed)
	}
	if authData[jsonFieldCause] != codex.CauseProvider {
		t.Fatalf("auth required cause = %v, want %s", authData[jsonFieldCause], codex.CauseProvider)
	}
	if authData[jsonFieldMessage] != "not logged in" {
		t.Fatalf("auth required message = %v, want native cause text", authData[jsonFieldMessage])
	}
	codexAuthMeta := asType[map[string]any](t, asType[map[string]any](t, authData[codexMetaKey])[authMetaAuthKey])
	if codexAuthMeta[jsonFieldReason] != "codex-auth-required" {
		t.Fatalf("auth required _meta.codex.auth = %#v", codexAuthMeta)
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
	if !isCodexAuthError(errors.New("401 unauthorized")) || isCodexAuthError(errors.New("plain failure")) {
		t.Fatal("auth error classifier failed")
	}
	if converted := toCodexAuthTokens(tokens); converted.AccessToken != "a" || converted.AccountID != "acct" || converted.ExpiresAtUnixSec != 42 {
		t.Fatalf("converted tokens = %#v", converted)
	}
	if codexAuthRequiredError(nil, nil) != nil {
		t.Fatal("nil auth error changed")
	}
}

func TestAuthErrorBranches(t *testing.T) {
	ctx := context.Background()
	closed := NewAgent()
	if err := closed.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if _, err := closed.Authenticate(ctx, acp.AuthenticateRequest{MethodId: authMethodChatGPTAuthTokens}); err == nil {
		t.Fatal("Authenticate on closed agent succeeded")
	}
	if _, err := closed.Logout(ctx, acp.LogoutRequest{}); err == nil {
		t.Fatal("Logout on closed agent succeeded")
	}

	agent := NewAgent()
	if _, err := agent.Authenticate(ctx, acp.AuthenticateRequest{MethodId: authMethodTypeTerminal}); err == nil {
		t.Fatal("Authenticate accepted unsupported method")
	}
	if _, err := agent.Authenticate(ctx, acp.AuthenticateRequest{MethodId: authMethodChatGPTAuthTokens, Meta: map[string]any{}}); err == nil {
		t.Fatal("Authenticate accepted missing token metadata")
	}

	meta := map[string]any{codexMetaKey: map[string]any{"auth": map[string]any{authChatGPTAuthTokensMetaPath: map[string]any{"accessToken": "access"}}}}
	newClientErr := errors.New("new client failed")
	failingNewClient := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return nil, newClientErr
	}))
	if _, err := failingNewClient.Authenticate(ctx, acp.AuthenticateRequest{MethodId: authMethodChatGPTAuthTokens, Meta: meta}); !errors.Is(err, newClientErr) {
		t.Fatalf("Authenticate newClient error = %v", err)
	}
	if _, ok := failingNewClient.externalAuthTokens(); ok {
		t.Fatal("Authenticate left external auth tokens after newClient failure")
	}

	loginClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), loginErr: errors.New("login failed")}
	loginAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return loginClient, nil
	}))
	if _, err := loginAgent.Authenticate(ctx, acp.AuthenticateRequest{MethodId: authMethodChatGPTAuthTokens, Meta: meta}); err == nil {
		t.Fatal("Authenticate swallowed login failure")
	}
	if !loginClient.closed {
		t.Fatal("newClient did not close client after login failure")
	}

	logoutClientErr := errors.New("logout client failed")
	logoutNewClient := NewAgent(
		WithHome(t.TempDir()),
		WithCodexAllowAccountLogout(true),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return nil, logoutClientErr }),
	)
	if _, err := logoutNewClient.Logout(ctx, acp.LogoutRequest{}); !errors.Is(err, logoutClientErr) {
		t.Fatalf("Logout newClient error = %v", err)
	}

	logoutErr := errors.New("logout failed")
	logoutClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), logoutErr: logoutErr}
	logoutAgent := NewAgent(
		WithHome(t.TempDir()),
		WithCodexAllowAccountLogout(true),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return logoutClient, nil }),
	)
	closeErr := errors.New("logout close failed")
	logoutSession := newSession(logoutAgent, "logout-session", "/tmp/project", nil, codex.Thread{ID: "logout-thread"}, &errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: closeErr}, sessionMeta{}, nil)
	if err := logoutAgent.storeStartedSession(logoutSession); err != nil {
		t.Fatalf("store logout session: %v", err)
	}
	if _, err := logoutAgent.Logout(ctx, acp.LogoutRequest{}); !errors.Is(err, logoutErr) {
		t.Fatalf("Logout error = %v", err)
	}
}

type authSpyClient struct {
	*spyCodexClient

	deviceLogin codex.DeviceCodeLogin
	deviceErr   error
	deviceCalls int
	apiKeyErr   error
	apiKeyValue string
	apiKeyCalls int
	// beforeAPIKeyLogin runs where the native write blocks, which is the window
	// a cancel, a supersede, or an expiry lands in on a real app-server.
	beforeAPIKeyLogin func()
	// beforeDeviceLogin and beforeLogout are the same window for the other two
	// mutating native calls, so a test can hold one leg inside its call and run
	// the leg that races it.
	beforeDeviceLogin func()
	beforeLogout      func()
	account           codex.Account
	accountErr        error
	accountRead       int
	logoutErr         error
	logoutCalls       int
}

// takeHook claims a one-shot native-call hook. Claiming under the mutex is what
// lets two goroutines call one spy method at once: the hook fires for the first
// caller only, and the second never sees a half-written field.
func (c *authSpyClient) takeHook(hook *func()) func() {
	c.mu.Lock()
	defer c.mu.Unlock()

	claimed := *hook
	*hook = nil

	return claimed
}

func newAuthSpyClient() *authSpyClient {
	return &authSpyClient{
		spyCodexClient: newSpyCodexClient(),
		deviceLogin: codex.DeviceCodeLogin{
			LoginID:         "login-1",
			VerificationURL: "https://auth.openai.com/codex/device",
			UserCode:        "U9KH-GPDJ7",
		},
	}
}

func (c *authSpyClient) StartDeviceCodeLogin(context.Context) (codex.DeviceCodeLogin, error) {
	if hook := c.takeHook(&c.beforeDeviceLogin); hook != nil {
		hook()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.deviceCalls++

	return c.deviceLogin, c.deviceErr
}

func (c *authSpyClient) StartAPIKeyLogin(_ context.Context, key string) error {
	if hook := c.takeHook(&c.beforeAPIKeyLogin); hook != nil {
		hook()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.apiKeyCalls++
	c.apiKeyValue = key

	return c.apiKeyErr
}

func (c *authSpyClient) AccountRead(context.Context) (codex.Account, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accountRead++

	return c.account, c.accountErr
}

func (c *authSpyClient) Logout(context.Context) error {
	if hook := c.takeHook(&c.beforeLogout); hook != nil {
		hook()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.logoutCalls++

	return c.logoutErr
}

// providerAuthFixture is one agent with a durable ledger root, a consented
// CODEX_HOME, and a single live session.
type providerAuthFixture struct {
	agent     *Agent
	broker    *providerAuth
	client    *authSpyClient
	sessionID string
	home      string
	root      string
}

func newProviderAuthFixture(t *testing.T, opts ...Option) *providerAuthFixture {
	t.Helper()

	home := t.TempDir()
	root := t.TempDir()
	client := newAuthSpyClient()

	all := append([]Option{
		WithHome(home),
		WithProviderAuthRoot(root),
		WithProviderAuthDirectHome(home),
	}, opts...)

	agent := NewAgent(all...)
	if agent.providerAuth == nil {
		t.Fatal("provider auth broker was not built")
	}

	t.Cleanup(func() { _ = agent.Close() })

	storeRateLimitsSession(t, agent, "thread-1", client)

	return &providerAuthFixture{
		agent:     agent,
		broker:    agent.providerAuth,
		client:    client,
		sessionID: "thread-1",
		home:      home,
		root:      root,
	}
}

func (f *providerAuthFixture) call(t *testing.T, method string, params map[string]any) (any, error) {
	t.Helper()

	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}

	return f.agent.HandleExtensionMethod(context.Background(), method, raw)
}

func (f *providerAuthFixture) mintGeneration(t *testing.T) string {
	t.Helper()

	result, err := f.call(t, AuthMethodsMethod, map[string]any{"sessionId": f.sessionID})
	if err != nil {
		t.Fatalf("methods: %v", err)
	}

	methods, _ := result.(authMethodsResult)

	return methods.Generation
}

func (f *providerAuthFixture) authorize(t *testing.T, method string, requestID string) authAuthorizeResult {
	t.Helper()

	generation := f.mintGeneration(t)

	result, err := f.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          f.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             method,
		"authorizeRequestId": requestID,
	})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}

	presentation, _ := result.(authAuthorizeResult)

	return presentation
}

func authErrorData(t *testing.T, err error) map[string]any {
	t.Helper()

	var requestErr *acp.RequestError
	if !errors.As(err, &requestErr) {
		t.Fatalf("error %v is not a request error", err)
	}

	data, ok := requestErr.Data.(map[string]any)
	if !ok {
		t.Fatalf("error data %v is not an object", requestErr.Data)
	}

	return data
}

func requireAuthCause(t *testing.T, err error, cause string) {
	t.Helper()

	data := authErrorData(t, err)
	if data[jsonFieldError] != authFailedErrorTag {
		t.Fatalf("error tag = %v, want %v", data[jsonFieldError], authFailedErrorTag)
	}

	if data[jsonFieldCause] != cause {
		t.Fatalf("cause = %v, want %v", data[jsonFieldCause], cause)
	}
}

func requireInvalidField(t *testing.T, err error, field string) {
	t.Helper()

	data := authErrorData(t, err)
	if data[jsonFieldField] != field {
		t.Fatalf("field = %v, want %v", data[jsonFieldField], field)
	}
}

func TestProviderAuthSurfaceIsUnadvertisedWithoutARoot(t *testing.T) {
	agent := NewAgent(WithHome(t.TempDir()))
	if agent.providerAuth != nil {
		t.Fatal("provider auth broker was built without a ledger root")
	}

	resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}

	meta, _ := resp.AgentCapabilities.Meta[codexMetaKey].(map[string]any)
	if _, present := meta[providerAuthCapabilityKey]; present {
		t.Fatal("provider auth capability was advertised without a ledger root")
	}

	if _, err := agent.HandleExtensionMethod(context.Background(), AuthMethodsMethod, json.RawMessage(`{}`)); err == nil {
		t.Fatal("an unadvertised leg answered")
	}
}

func TestProviderAuthUnusableRootLeavesSurfaceUnadvertised(t *testing.T) {
	root := t.TempDir()
	blocked := root + "/blocked"

	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed blocked root: %v", err)
	}

	agent := NewAgent(WithHome(t.TempDir()), WithProviderAuthRoot(blocked))
	if agent.providerAuth != nil {
		t.Fatal("provider auth broker was built on an unusable root")
	}
}

func TestProviderAuthRelativeRootFailsTheAgentClosed(t *testing.T) {
	agent := NewAgent(WithProviderAuthRoot("relative/root"), WithProviderAuthDirectHome("also/relative"))
	if agent.providerAuth != nil {
		t.Fatal("provider auth broker was built under a failed options verdict")
	}

	_, err := agent.Initialize(context.Background(), acp.InitializeRequest{})
	requireOptionsInternalError(t, err)
}

func TestProviderAuthCapabilityListsTheEnabledLegs(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	resp, err := fixture.agent.Initialize(context.Background(), acp.InitializeRequest{})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}

	meta, _ := resp.AgentCapabilities.Meta[codexMetaKey].(map[string]any)
	capability, _ := meta[providerAuthCapabilityKey].(map[string]any)

	methods, _ := capability[providerAuthMethodsField].([]string)
	if len(methods) != 8 {
		t.Fatalf("advertised %d legs, want 8", len(methods))
	}

	if _, present := capability["injectionKey"]; present {
		t.Fatal("codex advertised an injection key")
	}
}

func TestProviderAuthWithoutConsentGateAdvertisesSixLegs(t *testing.T) {
	home := t.TempDir()
	agent := NewAgent(WithHome(home), WithProviderAuthRoot(t.TempDir()))

	if got := len(agent.providerAuth.authMethodNames()); got != 6 {
		t.Fatalf("advertised %d legs, want 6", got)
	}

	for _, method := range []string{AuthCredentialMethod, AuthDisconnectMethod} {
		if _, err := agent.HandleExtensionMethod(context.Background(), method, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("%s answered without the consent gate", method)
		}
	}
}

func TestProviderAuthConsentGateRequiresExactHomeEquality(t *testing.T) {
	home := t.TempDir()

	file := filepath.Join(home, "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed a non-directory: %v", err)
	}

	cases := map[string]Options{
		"unset":      {Home: home},
		"parent":     {Home: home, ProviderAuthDirectHome: filepath.Dir(home)},
		"child":      {Home: home, ProviderAuthDirectHome: filepath.Join(home, "nested")},
		"nohome":     {ProviderAuthDirectHome: home},
		"absenthome": {Home: filepath.Join(home, "absent"), ProviderAuthDirectHome: home},
		"notadir":    {Home: file, ProviderAuthDirectHome: file},
		"equal":      {Home: home, ProviderAuthDirectHome: home + "/."},
	}

	resolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatalf("resolve home: %v", err)
	}

	for name, options := range cases {
		granted := consentDirectHome(options)

		t.Cleanup(granted.close)

		if name == "equal" {
			if granted.path != resolved || granted.root == nil {
				t.Fatalf("%s: gate = %+v, want the open %q", name, granted, resolved)
			}

			continue
		}

		if granted.root != nil {
			t.Fatalf("%s: gate granted %q", name, granted.path)
		}
	}
}

// TestConsentedHomeRefusesADirectoryReplacedAfterConsent pins what consent is
// held over. A name is not a directory: anything at this agent's uid can point
// the consented path somewhere else, and the account legs drive native calls
// that resolve the path again when they run.
func TestConsentedHomeRefusesADirectoryReplacedAfterConsent(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	other := filepath.Join(root, "other")

	for _, dir := range []string{home, other} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	granted := consentDirectHome(Options{Home: home, ProviderAuthDirectHome: home})

	t.Cleanup(granted.close)

	if !granted.unchanged() {
		t.Fatal("the gate refused the directory it opened")
	}

	if err := os.Rename(home, filepath.Join(root, "moved")); err != nil {
		t.Fatalf("move the consented home: %v", err)
	}

	if granted.unchanged() {
		t.Fatal("a path reaching nothing still read as the consented home")
	}

	if err := os.Symlink(other, home); err != nil {
		t.Fatalf("point the consented path elsewhere: %v", err)
	}

	if granted.unchanged() {
		t.Fatal("a repointed path still read as the consented home")
	}

	if (consentedHome{}).unchanged() {
		t.Fatal("an ungranted gate read as consented")
	}

	granted.close()

	if granted.unchanged() {
		t.Fatal("a released directory still read as the consented home")
	}
}

func TestProviderAuthClosedParamObjects(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	cases := []struct {
		name   string
		params json.RawMessage
		field  string
	}{
		{"unknown", json.RawMessage(`{"sessionId":"thread-1","extra":1}`), "extra"},
		{"duplicate", json.RawMessage(`{"sessionId":"thread-1","sessionId":"thread-1"}`), "sessionId"},
		{"nonobject", json.RawMessage(`[]`), "params"},
		{"trailing", json.RawMessage(`{"sessionId":"thread-1"} {}`), "params"},
		{"unclosed", json.RawMessage(`{"sessionId":"thread-1"`), "params"},
		{"truncated", json.RawMessage(`{"sessionId":`), "sessionId"},
		{"badvalue", json.RawMessage(`{"sessionId":@}`), "sessionId"},
		{"missing", json.RawMessage(`{}`), "sessionId"},
		{"empty", json.RawMessage(`{"sessionId":""}`), "sessionId"},
		{"wrongtype", json.RawMessage(`{"sessionId":7}`), "sessionId"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := fixture.agent.HandleExtensionMethod(context.Background(), AuthMethodsMethod, testCase.params)
			if err == nil {
				t.Fatal("malformed params were accepted")
			}

			requireInvalidField(t, err, testCase.field)
		})
	}
}

func TestProviderAuthUnknownSessionIsRejected(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	_, err := fixture.call(t, AuthMethodsMethod, map[string]any{"sessionId": "absent"})
	if err == nil {
		t.Fatal("an unknown session was accepted")
	}

	requireInvalidField(t, err, jsonFieldSessionID)
}

func TestProviderAuthCauseRetryability(t *testing.T) {
	retryable := map[string]bool{
		authCauseTransport:          true,
		authCauseProcess:            true,
		authCauseTimeout:            true,
		authCauseNativeVeto:         false,
		authCauseProviderRefused:    false,
		authCauseHarvestFailed:      false,
		authCauseUnsupportedVariant: false,
		authCauseFlowExpired:        false,
		authCauseFlowState:          false,
		authCauseFlowCancelled:      false,
		authCausePolicy:             false,
		authCauseBindingConflict:    false,
	}

	for cause, want := range retryable {
		if got := authCauseRetryable(cause); got != want {
			t.Fatalf("%s retryable = %v, want %v", cause, got, want)
		}
	}
}

func TestProviderAuthFailedErrorShape(t *testing.T) {
	failure := &authFailedError{cause: authCauseTransport, providerID: "openai", method: "apiKey", flowID: "flow-1"}
	if failure.Error() != authFailedErrorTag+": "+authCauseTransport {
		t.Fatalf("error text = %q", failure.Error())
	}

	request := failure.requestError()
	if request.Code != -32000 {
		t.Fatalf("code = %d, want -32000", request.Code)
	}

	data, _ := request.Data.(map[string]any)
	for _, key := range []string{jsonFieldError, jsonFieldCause, "retryable", authFieldProviderID, authFieldMethod, authFieldFlowID} {
		if _, ok := data[key]; !ok {
			t.Fatalf("error data is missing %q", key)
		}
	}

	bare, _ := (&authFailedError{cause: authCausePolicy}).requestError().Data.(map[string]any)
	for _, key := range []string{authFieldProviderID, authFieldMethod, authFieldFlowID} {
		if _, ok := bare[key]; ok {
			t.Fatalf("bare error data carries %q", key)
		}
	}
}

func TestProviderAuthFlowTransitions(t *testing.T) {
	cases := []struct {
		cause    string
		inFlight bool
		state    string
		reason   string
	}{
		{authCauseNativeVeto, false, authStateFailed, authReasonNativeVeto},
		{authCauseUnsupportedVariant, false, authStateFailed, authReasonNativeVeto},
		{authCauseProviderRefused, false, authStateFailed, authReasonProviderRefused},
		{authCauseTransport, false, authStateFailed, authReasonTransport},
		{authCauseTransport, true, authStateFailed, authReasonAcceptanceUnknown},
		{authCauseProcess, false, authStateFailed, authReasonProcess},
		{authCauseProcess, true, authStateFailed, authReasonAcceptanceUnknown},
		{authCauseTimeout, false, authStateFailed, authReasonTransport},
		{authCauseTimeout, true, authStateFailed, authReasonAcceptanceUnknown},
		{authCauseHarvestFailed, false, authStateFailed, authReasonHarvestFailed},
		{authCauseFlowExpired, false, authStateExpired, authReasonDeadline},
		{authCausePolicy, false, "", ""},
		{authCauseBindingConflict, false, "", ""},
		{authCauseFlowState, false, "", ""},
		{authCauseFlowCancelled, false, "", ""},
	}

	for _, testCase := range cases {
		state, reason := authFlowTransition(testCase.cause, testCase.inFlight)
		if state != testCase.state || reason != testCase.reason {
			t.Fatalf("%s(inFlight=%v) = %q/%q, want %q/%q", testCase.cause, testCase.inFlight, state, reason, testCase.state, testCase.reason)
		}
	}
}

func TestProviderAuthNativeClientRequiresTheAuthSurface(t *testing.T) {
	agent := NewAgent(WithHome(t.TempDir()), WithProviderAuthRoot(t.TempDir()))
	storeRateLimitsSession(t, agent, "plain", newSpyCodexClient())

	session, err := agent.session("plain")
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	if session.nativeAuthClient() != nil {
		t.Fatal("a client without the auth surface was accepted")
	}
}

func TestProviderAuthInjectionOptionKeyIsUnsupported(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	_, err := fixture.agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd: "/tmp/project",
		Meta: map[string]any{
			codexMetaKey: map[string]any{
				"options": map[string]any{"providerAuth": map[string]any{}},
			},
		},
	})
	if err == nil {
		t.Fatal("codex accepted an injection option key")
	}

	requireInvalidField(t, err, "_meta.codex.options.providerAuth")
}

func TestProviderAuthGoSafeRecoversAPanic(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	done := make(chan struct{})

	fixture.broker.goSafe("panicking", func() {
		defer close(done)

		panic("provider auth goroutine")
	})

	<-done
}

func TestLoggableErrorCarriesTheMessage(t *testing.T) {
	attr := loggableError(errors.New("boom"))
	if attr.Value.String() != "boom" {
		t.Fatalf("attr = %v", attr)
	}
}

func TestProviderAuthUnknownExtensionMethodFallsThrough(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	if _, err := fixture.call(t, "_codex/auth/invented", map[string]any{}); err == nil {
		t.Fatal("an invented leg answered")
	}
}

func TestAuthParamFieldsRejectsANonStringKey(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	_, err := fixture.agent.HandleExtensionMethod(t.Context(), AuthMethodsMethod, json.RawMessage(`{1:2}`))
	if err == nil {
		t.Fatal("a non-string key was accepted")
	}

	requireInvalidField(t, err, "params")
}

// TestProviderAuthLegsRejectMalformedParams runs the closed-object and
// unknown-session rules over every leg, because each leg decodes its own params.
func TestProviderAuthLegsRejectMalformedParams(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	legs := map[string]map[string]any{
		AuthMethodsMethod:    {"sessionId": "absent"},
		AuthAuthorizeMethod:  {"sessionId": "absent", "providerId": authProviderOpenAI, "connectionId": "c", "methodsGeneration": "g", "method": "m", "authorizeRequestId": "r"},
		AuthCallbackMethod:   {"sessionId": "absent", "providerId": authProviderOpenAI, "method": "m", "flowId": "f", "input": "i"},
		AuthStatusMethod:     {"sessionId": "absent", "providerId": authProviderOpenAI, "flowId": "f"},
		AuthCancelMethod:     {"sessionId": "absent", "providerId": authProviderOpenAI, "flowId": "f"},
		AuthInventoryMethod:  {"sessionId": "absent"},
		AuthCredentialMethod: {"sessionId": "absent", "providerId": authProviderOpenAI, "flowId": "f"},
		AuthDisconnectMethod: {"sessionId": "absent", "providerId": authProviderOpenAI, "connectionId": "c", "bindingGeneration": 1},
	}

	for method, params := range legs {
		t.Run(method, func(t *testing.T) {
			if _, err := fixture.agent.HandleExtensionMethod(t.Context(), method, json.RawMessage(`{"unknown":1}`)); err == nil {
				t.Fatal("an unknown field was accepted")
			}

			_, err := fixture.call(t, method, params)
			if err == nil {
				t.Fatal("an unknown session was accepted")
			}

			requireInvalidField(t, err, jsonFieldSessionID)
		})
	}
}

func TestProviderAuthTypedFieldDecoding(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodAPIKey, "request-1")

	_, err := fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodAPIKey,
		"flowId":     flow.FlowID,
		"input":      7,
	})
	if err == nil {
		t.Fatal("a non-string input was accepted")
	}

	requireInvalidField(t, err, authFieldInput)

	_, err = fixture.call(t, AuthDisconnectMethod, map[string]any{
		"sessionId":         fixture.sessionID,
		"providerId":        authProviderOpenAI,
		"connectionId":      "connection-1",
		"bindingGeneration": "one",
	})
	if err == nil {
		t.Fatal("a non-integer binding generation was accepted")
	}

	requireInvalidField(t, err, authFieldBindingGeneration)
}

func TestAuthFlowStopCompleterIsIdempotent(t *testing.T) {
	flow := &authFlow{disarm: make(chan struct{})}

	flow.stopCompleter()
	flow.stopCompleter()

	select {
	case <-flow.disarm:
	default:
		t.Fatal("the completer was not disarmed")
	}
}

func TestProviderAuthLegsRejectAnUnknownFlowID(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	legs := map[string]map[string]any{
		AuthCallbackMethod:   {"sessionId": fixture.sessionID, "providerId": authProviderOpenAI, "method": authMethodAPIKey, "flowId": "absent", "input": "i"},
		AuthCredentialMethod: {"sessionId": fixture.sessionID, "providerId": authProviderOpenAI, "flowId": "absent"},
	}

	for method, params := range legs {
		t.Run(method, func(t *testing.T) {
			_, err := fixture.call(t, method, params)
			if err == nil {
				t.Fatal("an unknown flow id was accepted")
			}

			requireInvalidField(t, err, authFieldFlowID)
		})
	}
}

func TestAuthInventoryRejectsAnEmptySessionID(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	_, err := fixture.agent.HandleExtensionMethod(t.Context(), AuthInventoryMethod, json.RawMessage(`{"sessionId":""}`))
	if err == nil {
		t.Fatal("an empty session id was accepted")
	}

	requireInvalidField(t, err, jsonFieldSessionID)
}
