package codex

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

// loginTransport answers exactly one request shape: account/login/start.
type loginTransport struct {
	mu     sync.Mutex
	closed bool
	recv   chan rpcMessage
	sent   []rpcMessage
	result any
	fail   bool
}

func newLoginTransport(result any) *loginTransport {
	return &loginTransport{recv: make(chan rpcMessage, 8), result: result}
}

func (t *loginTransport) Send(_ context.Context, msg rpcMessage) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return errors.New("closed")
	}

	t.sent = append(t.sent, msg)

	if len(msg.ID) == 0 {
		return nil
	}

	if t.fail {
		t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: msg.ID, Error: &rpcError{Code: -32000, Message: "refused"}}

		return nil
	}

	t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: msg.ID, Result: mustRaw(t.result)}

	return nil
}

func (t *loginTransport) Recv() (rpcMessage, string, error) {
	msg, ok := <-t.recv
	if !ok {
		return rpcMessage{}, "", errors.New("closed")
	}

	return msg, string(mustRaw(msg)), nil
}

func (t *loginTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.closed {
		t.closed = true
		close(t.recv)
	}

	return nil
}

func (t *loginTransport) params() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()

	for i := len(t.sent) - 1; i >= 0; i-- {
		if t.sent[i].Method != methodAccountLoginStart {
			continue
		}

		var params map[string]any
		_ = json.Unmarshal(t.sent[i].Params, &params)

		return params
	}

	return nil
}

func TestStartDeviceCodeLogin(t *testing.T) {
	transport := newLoginTransport(map[string]any{
		"type":            loginTypeChatGPTDevice,
		"loginId":         "login-1",
		"verificationUrl": "https://auth.openai.com/codex/device",
		"userCode":        "U9KH-GPDJ7",
	})
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}

	defer client.Close(context.Background())

	login, err := client.StartDeviceCodeLogin(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceCodeLogin returned error: %v", err)
	}

	if login.LoginID != "login-1" || login.UserCode != "U9KH-GPDJ7" {
		t.Fatalf("login = %+v", login)
	}

	if login.VerificationURL != "https://auth.openai.com/codex/device" {
		t.Fatalf("verification url = %q", login.VerificationURL)
	}

	if params := transport.params(); params["type"] != loginTypeChatGPTDevice {
		t.Fatalf("params = %v", params)
	}
}

func TestStartDeviceCodeLoginPropagatesRefusal(t *testing.T) {
	transport := newLoginTransport(nil)
	transport.fail = true
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}

	defer client.Close(context.Background())

	if _, err := client.StartDeviceCodeLogin(context.Background()); err == nil {
		t.Fatal("a refused login start reported success")
	}
}

func TestStartAPIKeyLogin(t *testing.T) {
	transport := newLoginTransport(map[string]any{"type": loginTypeAPIKey})
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}

	defer client.Close(context.Background())

	if err := client.StartAPIKeyLogin(context.Background(), "sk-canary"); err != nil {
		t.Fatalf("StartAPIKeyLogin returned error: %v", err)
	}

	// The parameter name is asserted as a literal. Looking it up through the
	// constant the request sends would move the assertion with the code under
	// exactly the edit that breaks it: renaming the login type upstream would
	// rename the field too, and the app-server would still read "apiKey".
	params := transport.params()
	if params["type"] != loginTypeAPIKey || params["apiKey"] != "sk-canary" {
		t.Fatalf("params = %v", params)
	}
}

func TestNormalizeAuthMode(t *testing.T) {
	cases := map[string]string{
		authModeAPIKeyRead:     AuthModeAPIKey,
		authModeAPIKeyNotified: AuthModeAPIKey,
		AuthModeChatGPT:        AuthModeChatGPT,
		"":                     "",
	}

	for value, want := range cases {
		if got := NormalizeAuthMode(value); got != want {
			t.Fatalf("NormalizeAuthMode(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestLoginCompletedEventDecoding(t *testing.T) {
	event := eventFromRPC(rpcEvent{Method: notifyAccountLoginDone, Params: mustRaw(map[string]any{
		"loginId": "login-1",
		"success": true,
		"error":   nil,
	})})

	if event.Kind != EventLoginCompleted {
		t.Fatalf("kind = %q", event.Kind)
	}

	if event.Login.LoginID != "login-1" || !event.Login.Success {
		t.Fatalf("login = %+v", event.Login)
	}

	anonymous := eventFromRPC(rpcEvent{Method: notifyAccountLoginDone, Params: mustRaw(map[string]any{
		"loginId": nil,
		"success": true,
	})})

	if anonymous.Login.LoginID != "" || !anonymous.Login.Success {
		t.Fatalf("anonymous login = %+v", anonymous.Login)
	}
}

func TestAccountReadCarriesTheAuthMode(t *testing.T) {
	read := accountFromResponse(map[string]any{
		"account": map[string]any{"type": "chatgpt", "email": "u@example.com", "planType": "pro"},
	})
	if read.AuthMode != AuthModeChatGPT {
		t.Fatalf("read auth mode = %q", read.AuthMode)
	}

	updated := accountFromResponse(map[string]any{"authMode": "apikey", "planType": nil})
	if updated.AuthMode != AuthModeAPIKey {
		t.Fatalf("updated auth mode = %q", updated.AuthMode)
	}
}
