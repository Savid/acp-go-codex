package codex

import "context"

const (
	loginTypeAPIKey        = "apiKey"
	loginTypeChatGPTDevice = "chatgptDeviceCode"
	// fieldAPIKey is the request parameter carrying the key. It spells the same
	// as the login type it accompanies and means something else: one is the
	// value of the type discriminator, the other is a parameter name, and they
	// move independently upstream.
	fieldAPIKey              = "apiKey"
	notifyAccountLoginDone   = "account/login/completed"
	fieldLoginID             = "loginId"
	fieldVerificationURL     = "verificationUrl"
	fieldUserCode            = "userCode"
	fieldAuthMode            = "authMode"
	fieldSuccess             = "success"
	authModeChatGPT          = "chatgpt"
	authModeAPIKeyRead       = "apiKey"
	authModeAPIKeyNotified   = "apikey"
	loginCompletionUnknownID = ""
)

// DeviceCodeLogin is the app-server answer to a chatgptDeviceCode login start.
// The native login handle stays inside this package.
type DeviceCodeLogin struct {
	LoginID         string
	VerificationURL string
	UserCode        string
}

// LoginCompletion carries the account/login/completed notification. The native
// error text is deliberately absent: a login failure is reported as a boolean
// outcome so no provider response body can ride out of this package.
type LoginCompletion struct {
	LoginID string
	Success bool
}

// StartDeviceCodeLogin begins a ChatGPT device-code login and returns the
// verification presentation. It never blocks on the provider: completion
// arrives as a notification and is separately readable through AccountRead.
func (c *AppServerClient) StartDeviceCodeLogin(ctx context.Context) (DeviceCodeLogin, error) {
	var resp map[string]any
	if err := c.rpc.Call(ctx, methodAccountLoginStart, map[string]any{fieldType: loginTypeChatGPTDevice}, &resp); err != nil {
		return DeviceCodeLogin{}, err
	}

	return DeviceCodeLogin{
		LoginID:         stringValue(resp, fieldLoginID),
		VerificationURL: stringValue(resp, fieldVerificationURL),
		UserCode:        stringValue(resp, fieldUserCode),
	}, nil
}

// StartAPIKeyLogin applies an operator-supplied API key. The app-server writes
// it into the configured credential store and answers once the write lands.
func (c *AppServerClient) StartAPIKeyLogin(ctx context.Context, key string) error {
	return c.rpc.Call(ctx, methodAccountLoginStart, map[string]any{
		fieldType:   loginTypeAPIKey,
		fieldAPIKey: key,
	}, nil)
}

// NormalizeAuthMode folds the two native spellings of one mode. account/read
// reports the login variant name while account/updated reports a lowercased
// mode, so a caller comparing either against one constant would miss half the
// signals.
func NormalizeAuthMode(mode string) string {
	switch mode {
	case authModeAPIKeyRead, authModeAPIKeyNotified:
		return authModeAPIKeyRead
	default:
		return mode
	}
}

// AuthModeChatGPT is the account mode a completed ChatGPT login reports.
const AuthModeChatGPT = authModeChatGPT

// AuthModeAPIKey is the account mode an applied API key reports.
const AuthModeAPIKey = authModeAPIKeyRead

func loginCompletionFromParams(params map[string]any) LoginCompletion {
	completion := LoginCompletion{LoginID: loginCompletionUnknownID}
	if value := stringValue(params, fieldLoginID); value != "" {
		completion.LoginID = value
	}

	success, _ := params[fieldSuccess].(bool)
	completion.Success = success

	return completion
}
