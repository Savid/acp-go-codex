package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

// authAdmissionSettle bounds how long a test waits for a second leg to decide
// what it is doing. A leg queued on an admission gate never decides on its own,
// so the wait ends and the leg ahead of it is released.
const authAdmissionSettle = time.Second

func restoreAuthClock(t *testing.T) {
	t.Helper()

	now := authNow
	t.Cleanup(func() { authNow = now })
}

func TestAuthorizeMintsADeviceFlow(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	result := fixture.authorize(t, authMethodDeviceCode, "request-1")

	if result.Interaction != authInteractionWait {
		t.Fatalf("interaction = %q, want %q", result.Interaction, authInteractionWait)
	}

	if result.CallbackInput != "" {
		t.Fatalf("codex advertised a callback input: %q", result.CallbackInput)
	}

	if result.URL != "https://auth.openai.com/codex/device" || result.UserCode != "U9KH-GPDJ7" {
		t.Fatalf("presentation = %+v", result)
	}

	if result.Message == "" || result.FlowID == "" || result.FlowExpiresAt == 0 {
		t.Fatalf("presentation = %+v", result)
	}

	record, ok, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil || !ok {
		t.Fatalf("ledger read: ok=%v err=%v", ok, err)
	}

	if record.State != authLedgerIntent || record.FlowID != result.FlowID || record.AuthorizeRequestID != "request-1" {
		t.Fatalf("ledger record = %+v", record)
	}
}

func TestAuthorizeMintsASecretFlowWithoutANativeCall(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	result := fixture.authorize(t, authMethodAPIKey, "request-1")

	if result.Interaction != authInteractionSecret {
		t.Fatalf("interaction = %q", result.Interaction)
	}

	if result.URL != "" || result.UserCode != "" {
		t.Fatalf("a secret flow carried a url or code: %+v", result)
	}

	if result.Message != "OpenAI API key" {
		t.Fatalf("message = %q, want the method label", result.Message)
	}

	if fixture.client.deviceCalls != 0 {
		t.Fatal("a secret flow minted a native login")
	}
}

func TestAuthorizeReplaysTheSameRequestIDVerbatim(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	first := fixture.authorize(t, authMethodDeviceCode, "request-1")
	generation := fixture.mintGeneration(t)

	result, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	if replayed, _ := result.(authAuthorizeResult); replayed != first {
		t.Fatal("a replayed request id did not answer verbatim")
	}

	if fixture.client.deviceCalls != 1 {
		t.Fatalf("a replay drove %d native mints", fixture.client.deviceCalls)
	}
}

// TestAuthorizeReplaysTheSameRequestIDAfterTheFlowTerminalized pins the whole
// point of the idempotency key: the repeat that matters is the one a caller
// sends after the first answer was lost, which is exactly when the flow it
// names has already completed.
func TestAuthorizeReplaysTheSameRequestIDAfterTheFlowTerminalized(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	first := fixture.authorize(t, authMethodDeviceCode, "request-1")
	fixture.client.account = codex.Account{AuthMode: codex.AuthModeChatGPT}

	fixture.broker.loginCompleted(t.Context(), codex.LoginCompletion{LoginID: "login-1", Success: true})

	if state := fixture.status(t, first.FlowID).State; state != authStateAuthenticated {
		t.Fatalf("state = %q, want authenticated", state)
	}

	before, _, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}

	generation := fixture.mintGeneration(t)

	result, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	if replayed, _ := result.(authAuthorizeResult); replayed != first {
		t.Fatal("a replayed request id did not answer verbatim after the flow terminalized")
	}

	// No second native login, and no ledger revision: the repeat consumed
	// nothing the first call had earned.
	if fixture.client.deviceCalls != 1 {
		t.Fatalf("a replay drove %d native mints", fixture.client.deviceCalls)
	}

	after, _, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}

	if after != before {
		t.Fatal("a replay rewrote the ledger entry")
	}

	if state := fixture.status(t, first.FlowID).State; state != authStateAuthenticated {
		t.Fatalf("state after replay = %q, want authenticated", state)
	}
}

// TestAuthorizeStopsReplayingOnceTheSessionCloses pins the other half: the
// record lives exactly as long as the session that owns it. A closed id answers
// nothing at all until the same thread is loaded again, and what comes back is a
// session with no history rather than the replay the id once had.
func TestAuthorizeStopsReplayingOnceTheSessionCloses(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	first := fixture.authorize(t, authMethodDeviceCode, "request-1")

	_, err := fixture.agent.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: acp.SessionId(fixture.sessionID)})
	if err != nil {
		t.Fatalf("close owning session: %v", err)
	}

	if _, err = fixture.call(t, AuthMethodsMethod, map[string]any{"sessionId": fixture.sessionID}); err == nil {
		t.Fatal("a closed session still answered a provider-auth leg")
	} else {
		requireInvalidField(t, err, "sessionId")
	}

	storeRateLimitsSession(t, fixture.agent, fixture.sessionID, fixture.client)

	if second := fixture.authorize(t, authMethodDeviceCode, "request-1"); second.FlowID == first.FlowID {
		t.Fatal("a reloaded session still replayed the closed session's idempotency key")
	}
}

// TestAuthorizeMintFailureAddressesTheFlowItNames pins the flowId a failed mint
// returns against a record a caller can actually address, and pins that the
// same key retries rather than replaying a presentation that was never
// published.
func TestAuthorizeMintFailureAddressesTheFlowItNames(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	fixture.client.deviceErr = errors.New("device mint refused")

	generation := fixture.mintGeneration(t)

	_, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	})
	requireAuthCause(t, err, authCauseTransport)

	flowID, _ := authErrorData(t, err)[authFieldFlowID].(string)
	if flowID == "" {
		t.Fatal("a mint failure carried no flow id")
	}

	if status := fixture.status(t, flowID); status.State != authStateFailed || status.Reason != authReasonTransport {
		t.Fatalf("state/reason = %q/%q, want failed/transport", status.State, status.Reason)
	}

	fixture.client.deviceErr = nil

	retried := fixture.authorize(t, authMethodDeviceCode, "request-1")
	if retried.FlowID == flowID || retried.URL == "" {
		t.Fatal("the same key after a failed mint replayed an unpublished presentation")
	}
}

func TestAuthorizeSupersedesTheEarlierFlow(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	first := fixture.authorize(t, authMethodDeviceCode, "request-1")
	second := fixture.authorize(t, authMethodDeviceCode, "request-2")

	if first.FlowID == second.FlowID {
		t.Fatal("the superseding authorize reused the flow id")
	}

	_, err := fixture.call(t, AuthStatusMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"flowId":     first.FlowID,
	})
	if err == nil {
		t.Fatal("a superseded flow id still addressed a flow")
	}

	requireInvalidField(t, err, authFieldFlowID)
}

// TestAuthorizeDropsASupersededTerminalFlow covers the flow a supersede finds
// already terminal. It was addressable for its whole life, and being replaced
// is what ends that life rather than the transition it happened to end on, so
// its id stops addressing anything and stops being held.
func TestAuthorizeDropsASupersededTerminalFlow(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	first := fixture.authorize(t, authMethodAPIKey, "request-1")

	if _, err := fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodAPIKey,
		"flowId":     first.FlowID,
		"input":      "sk-canary",
	}); err != nil {
		t.Fatalf("callback: %v", err)
	}

	if state := fixture.status(t, first.FlowID).State; state != authStateSaved {
		t.Fatalf("state = %q, want saved", state)
	}

	fixture.authorize(t, authMethodAPIKey, "request-2")

	_, err := fixture.call(t, AuthStatusMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"flowId":     first.FlowID,
	})
	if err == nil {
		t.Fatal("a superseded terminal flow id still addressed a flow")
	}

	requireInvalidField(t, err, authFieldFlowID)

	fixture.broker.mu.Lock()
	addressable := len(fixture.broker.byID)
	fixture.broker.mu.Unlock()

	if addressable != 1 {
		t.Fatalf("the addressing map holds %d flows after one supersede", addressable)
	}
}

// TestAuthorizeRefusesAKeyASupersedeRetired pins the idempotency key's whole
// promise: a repeat never destroys. Once a later authorize replaced the flow a
// key named, that key has no presentation to replay and no live flow to speak
// for, so a repeat of it is a caller addressing failure rather than a fresh
// mint that would cancel the flow the caller never asked about.
func TestAuthorizeRefusesAKeyASupersedeRetired(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	fixture.authorize(t, authMethodDeviceCode, "request-1")
	live := fixture.authorize(t, authMethodDeviceCode, "request-2")

	generation := fixture.mintGeneration(t)

	_, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	})
	if err == nil {
		t.Fatal("a retired idempotency key minted a new flow")
	}

	requireInvalidField(t, err, authFieldAuthorizeRequestID)

	if status := fixture.status(t, live.FlowID); status.State != authStatePending {
		t.Fatalf("the live flow is %q/%q after a retired key was repeated", status.State, status.Reason)
	}

	if fixture.client.deviceCalls != 2 {
		t.Fatalf("a retired key drove %d native mints", fixture.client.deviceCalls)
	}
}

func TestAuthorizeBumpsTheRevisionOnAPriorLedgerEntry(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	fixture.authorize(t, authMethodDeviceCode, "request-1")

	first, _, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil {
		t.Fatalf("ledger read: %v", err)
	}

	fixture.authorize(t, authMethodDeviceCode, "request-2")

	second, _, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil {
		t.Fatalf("ledger read: %v", err)
	}

	if second.Revision != first.Revision+1 {
		t.Fatalf("revision went %d -> %d", first.Revision, second.Revision)
	}

	if second.CreatedAt != first.CreatedAt {
		t.Fatal("the superseding authorize reset the creation timestamp")
	}
}

func TestAuthorizeAddressingFailures(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	generation := fixture.mintGeneration(t)

	base := map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	}

	for _, field := range []string{"sessionId", "providerId", "connectionId", "methodsGeneration", "method", "authorizeRequestId"} {
		params := map[string]any{}
		for key, value := range base {
			params[key] = value
		}

		delete(params, field)

		_, err := fixture.call(t, AuthAuthorizeMethod, params)
		if err == nil {
			t.Fatalf("authorize accepted a request missing %q", field)
		}

		if field != "sessionId" {
			requireInvalidField(t, err, field)
		}
	}
}

func TestAuthorizeRejectsAnswersForPromptsNobodyPublished(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	generation := fixture.mintGeneration(t)

	for _, inputs := range []any{map[string]any{"account": "x"}, "not-an-object"} {
		_, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
			"sessionId":          fixture.sessionID,
			"providerId":         authProviderOpenAI,
			"connectionId":       "connection-1",
			"methodsGeneration":  generation,
			"method":             authMethodDeviceCode,
			"authorizeRequestId": "request-1",
			"inputs":             inputs,
		})
		if err == nil {
			t.Fatalf("authorize accepted inputs %v", inputs)
		}

		requireInvalidField(t, err, authFieldInputs)
	}

	if _, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
		"inputs":             map[string]any{},
	}); err != nil {
		t.Fatalf("authorize rejected an empty inputs object: %v", err)
	}
}

func TestAuthorizeFencesTheMethodsGeneration(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	stale := fixture.mintGeneration(t)
	fresh := fixture.mintGeneration(t)

	_, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  stale,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	})
	if err == nil {
		t.Fatal("a superseded generation was accepted")
	}

	requireInvalidField(t, err, authFieldMethodsGeneration)

	_, err = fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  fresh,
		"method":             "chatgpt",
		"authorizeRequestId": "request-1",
	})
	if err == nil {
		t.Fatal("an unpublished method was accepted")
	}

	requireInvalidField(t, err, authFieldMethod)
}

func TestAuthorizeRejectsAMethodBeforeAnyCatalogExists(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	_, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  "never-minted",
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	})
	if err == nil {
		t.Fatal("authorize answered with no catalog")
	}

	requireInvalidField(t, err, authFieldMethodsGeneration)
}

func TestAuthorizeMintFailures(t *testing.T) {
	cases := map[string]struct {
		install func(*providerAuthFixture)
		cause   string
	}{
		"native": {
			install: func(f *providerAuthFixture) { f.client.deviceErr = errors.New("refused") },
			cause:   authCauseTransport,
		},
		"baseline": {
			install: func(f *providerAuthFixture) { f.client.accountErr = errors.New("refused") },
			cause:   authCauseTransport,
		},
		"process": {
			install: func(f *providerAuthFixture) { f.client.deviceErr = codex.ErrConnectionClosed },
			cause:   authCauseProcess,
		},
		"loopback": {
			install: func(f *providerAuthFixture) {
				f.client.deviceLogin.VerificationURL = "https://127.0.0.1/device"
			},
			cause: authCauseUnsupportedVariant,
		},
		"badurl": {
			install: func(f *providerAuthFixture) {
				f.client.deviceLogin.VerificationURL = "http://auth.openai.com/device"
			},
			cause: authCauseNativeVeto,
		},
		"badcode": {
			install: func(f *providerAuthFixture) {
				f.client.deviceLogin.UserCode = "</script>ABCD"
			},
			cause: authCauseNativeVeto,
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newProviderAuthFixture(t)
			testCase.install(fixture)

			generation := fixture.mintGeneration(t)

			_, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
				"sessionId":          fixture.sessionID,
				"providerId":         authProviderOpenAI,
				"connectionId":       "connection-1",
				"methodsGeneration":  generation,
				"method":             authMethodDeviceCode,
				"authorizeRequestId": "request-1",
			})
			if err == nil {
				t.Fatal("a failed mint reported success")
			}

			requireAuthCause(t, err, testCase.cause)
		})
	}
}

func TestAuthorizeFailsWithoutTheNativeAuthSurface(t *testing.T) {
	home := t.TempDir()
	agent := NewAgent(WithHome(home), WithProviderAuthRoot(t.TempDir()), WithProviderAuthDirectHome(home))

	t.Cleanup(func() { _ = agent.Close() })

	storeRateLimitsSession(t, agent, "plain", newSpyCodexClient())

	result, err := agent.HandleExtensionMethod(t.Context(), AuthMethodsMethod, json.RawMessage(`{"sessionId":"plain"}`))
	if err != nil {
		t.Fatalf("methods: %v", err)
	}

	methods, _ := result.(authMethodsResult)
	generation := methods.Generation
	params, err := json.Marshal(map[string]any{
		"sessionId":          "plain",
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	_, err = agent.HandleExtensionMethod(t.Context(), AuthAuthorizeMethod, params)
	if err == nil {
		t.Fatal("authorize answered without the native auth surface")
	}

	requireAuthCause(t, err, authCauseTransport)
}

func TestAuthorizeFailsWhenTheFlowIDCannotBeMinted(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	generation := fixture.mintGeneration(t)

	restore := authRandRead
	authRandRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }

	defer func() { authRandRead = restore }()

	_, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	})
	if err == nil {
		t.Fatal("authorize succeeded without a flow id")
	}

	requireAuthCause(t, err, authCauseProcess)
}

func TestAuthorizeFailsWhenTheLedgerCannotRecord(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	generation := fixture.mintGeneration(t)

	restoreLedgerHooks(t)

	ledgerCreateTemp = func(string, string) (ledgerFile, error) { return nil, errors.New("temp") }

	_, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  generation,
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	})
	if err == nil {
		t.Fatal("authorize returned before recording the flow")
	}

	requireAuthCause(t, err, authCauseProcess)

	if fixture.client.deviceCalls != 0 {
		t.Fatal("authorize minted natively before recording")
	}
}

func TestAuthCallbackAppliesASecret(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodAPIKey, "request-1")

	result, err := fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodAPIKey,
		"flowId":     flow.FlowID,
		"input":      "sk-canary",
	})
	if err != nil {
		t.Fatalf("callback: %v", err)
	}

	if applied, _ := result.(authFlowIDResult); applied.FlowID != flow.FlowID {
		t.Fatalf("callback returned %+v", result)
	}

	if fixture.client.apiKeyValue != "sk-canary" {
		t.Fatalf("native apply received %q", fixture.client.apiKeyValue)
	}

	status := fixture.status(t, flow.FlowID)
	if status.State != authStateSaved {
		t.Fatalf("state = %q, want saved", status.State)
	}

	if status.ExpiresAt != 0 {
		t.Fatal("a saved flow claimed a credential expiry")
	}

	record, _, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil {
		t.Fatalf("ledger read: %v", err)
	}

	if record.State != authLedgerConfirmed {
		t.Fatalf("ledger state = %q", record.State)
	}
}

// TestSecretApplyOutlivingACancelAnswersForTheClosedFlow pins the answer a leg
// gives when the owner closed the flow while the native write was in flight.
// The write landed, so the key is resident and its provenance is still
// recorded — but the outcome is no longer this flow's to report.
func TestSecretApplyOutlivingACancelAnswersForTheClosedFlow(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodAPIKey, "request-1")

	fixture.client.beforeAPIKeyLogin = func() {
		if _, err := fixture.call(t, AuthCancelMethod, map[string]any{
			"sessionId":  fixture.sessionID,
			"providerId": authProviderOpenAI,
			"flowId":     flow.FlowID,
		}); err != nil {
			t.Errorf("cancel: %v", err)
		}
	}

	_, err := fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodAPIKey,
		"flowId":     flow.FlowID,
		"input":      "sk-canary",
	})
	requireAuthCause(t, err, authCauseFlowCancelled)

	if status := fixture.status(t, flow.FlowID); status.State != authStateCancelled {
		t.Fatalf("state = %q, want cancelled", status.State)
	}

	record, ok, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil || !ok {
		t.Fatalf("ledger read: %v ok=%v", err, ok)
	}

	if record.State != authLedgerConfirmed {
		t.Fatalf("ledger state = %q, want the resident key recorded", record.State)
	}
}

// TestALateConfirmationLeavesTheSuccessorsLineageAlone pins the provider's one
// ledger entry against a leg that outlived its own flow. A fresh authorize
// supersedes the old record and mints the next revision; the superseded flow's
// confirmation would otherwise rename its own lineage over it, leaving the host
// holding one generation and the ledger naming another.
func TestALateConfirmationLeavesTheSuccessorsLineageAlone(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	first := fixture.authorize(t, authMethodAPIKey, "request-1")

	applying := make(chan struct{})
	release := make(chan struct{})

	fixture.client.beforeAPIKeyLogin = func() {
		close(applying)
		<-release
	}

	applied := make(chan error, 1)

	go func() {
		_, err := fixture.call(t, AuthCallbackMethod, map[string]any{
			"sessionId":  fixture.sessionID,
			"providerId": authProviderOpenAI,
			"method":     authMethodAPIKey,
			"flowId":     first.FlowID,
			"input":      "sk-canary",
		})
		applied <- err
	}()

	<-applying

	// The replacing authorize supersedes the live flow immediately and then
	// queues for the provider's entry, which the apply is holding. Both legs are
	// therefore live at once, which is the only way the late confirmation this
	// pins can happen at all.
	var second authAuthorizeResult

	superseded := make(chan struct{})

	go func() {
		defer close(superseded)

		second = fixture.authorize(t, authMethodAPIKey, "request-2")
	}()

	select {
	case <-superseded:
	case <-time.After(authAdmissionSettle):
	}

	close(release)

	<-superseded

	requireAuthCause(t, <-applied, authCauseFlowCancelled)

	record, ok, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil || !ok {
		t.Fatalf("ledger read: %v ok=%v", err, ok)
	}

	if record.FlowID != second.FlowID || record.Revision != 2 {
		t.Fatalf("ledger names %+v, want the successor at revision 2", record)
	}
}

// TestSecretApplyOutlivingAnExpiryAnswersForTheClosedFlow is the other closed
// record a late apply can land in. Expiry is not the owner's cancel, so the
// answer names the flow's state rather than an owner decision nobody made.
func TestSecretApplyOutlivingAnExpiryAnswersForTheClosedFlow(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodAPIKey, "request-1")

	fixture.client.beforeAPIKeyLogin = func() {
		fixture.broker.terminalize(fixture.broker.byID[flow.FlowID], authStateExpired, authReasonDeadline, 0)
	}

	_, err := fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodAPIKey,
		"flowId":     flow.FlowID,
		"input":      "sk-canary",
	})
	requireAuthCause(t, err, authCauseFlowState)
}

// TestSecretApplyFailsClosedWhenTheLedgerCannotBeRead pins the confirmation
// against a ledger it could not compare. Writing over an entry nobody read is
// how a late leg renames its lineage over a successor's.
func TestSecretApplyFailsClosedWhenTheLedgerCannotBeRead(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodAPIKey, "request-1")

	restoreLedgerHooks(t)

	ledgerReadFile = func(string) ([]byte, error) { return nil, errors.New("read") }

	_, err := fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodAPIKey,
		"flowId":     flow.FlowID,
		"input":      "sk-canary",
	})
	requireAuthCause(t, err, authCauseProcess)
}

func TestAuthLedgerAdvancedPast(t *testing.T) {
	record := authLedgerRecord{Revision: 2, BindingGeneration: 3}

	cases := map[string]struct {
		prior authLedgerRecord
		want  bool
	}{
		"same":             {authLedgerRecord{Revision: 2, BindingGeneration: 3}, false},
		"older generation": {authLedgerRecord{Revision: 9, BindingGeneration: 2}, false},
		"newer generation": {authLedgerRecord{Revision: 1, BindingGeneration: 4}, true},
		"older revision":   {authLedgerRecord{Revision: 1, BindingGeneration: 3}, false},
		"newer revision":   {authLedgerRecord{Revision: 3, BindingGeneration: 3}, true},
	}

	for name, testCase := range cases {
		if got := authLedgerAdvancedPast(testCase.prior, record); got != testCase.want {
			t.Fatalf("%s: advancedPast = %v, want %v", name, got, testCase.want)
		}
	}
}

func (f *providerAuthFixture) status(t *testing.T, flowID string) authStatusResult {
	t.Helper()

	result, err := f.call(t, AuthStatusMethod, map[string]any{
		"sessionId":  f.sessionID,
		"providerId": authProviderOpenAI,
		"flowId":     flowID,
	})
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	status, _ := result.(authStatusResult)

	return status
}

func TestAuthCallbackRejectionPaths(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	secret := fixture.authorize(t, authMethodAPIKey, "request-secret")

	base := map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodAPIKey,
		"flowId":     secret.FlowID,
		"input":      "sk-canary",
	}

	for _, field := range []string{"sessionId", "providerId", "method", "flowId", "input"} {
		params := map[string]any{}
		for key, value := range base {
			params[key] = value
		}

		delete(params, field)

		_, err := fixture.call(t, AuthCallbackMethod, params)
		if err == nil {
			t.Fatalf("callback accepted a request missing %q", field)
		}
	}

	_, err := fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodDeviceCode,
		"flowId":     secret.FlowID,
		"input":      "sk-canary",
	})
	if err == nil {
		t.Fatal("callback accepted a method the flow did not record")
	}

	requireInvalidField(t, err, authFieldMethod)

	_, err = fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodAPIKey,
		"flowId":     secret.FlowID,
		"input":      strings.Repeat("k", authMaxSecretBytes+1),
	})
	if err == nil {
		t.Fatal("callback accepted an oversize secret")
	}

	requireInvalidField(t, err, authFieldInput)
}

func TestAuthCallbackRefusesADeviceFlow(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	_, err := fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodDeviceCode,
		"flowId":     flow.FlowID,
		"input":      "code",
	})
	if err == nil {
		t.Fatal("callback accepted a submission on a wait flow")
	}

	requireInvalidField(t, err, authFieldInput)
}

func TestAuthCallbackOnATerminalFlowIsAFlowStateFailure(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodAPIKey, "request-1")

	if _, err := fixture.call(t, AuthCancelMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"flowId":     flow.FlowID,
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	_, err := fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodAPIKey,
		"flowId":     flow.FlowID,
		"input":      "sk-canary",
	})
	if err == nil {
		t.Fatal("callback answered a terminal flow")
	}

	requireAuthCause(t, err, authCauseFlowState)
}

func TestAuthCallbackNativeFailures(t *testing.T) {
	cases := map[string]struct {
		install func(*providerAuthFixture)
		cause   string
		reason  string
	}{
		"apply": {
			install: func(f *providerAuthFixture) { f.client.apiKeyErr = errors.New("refused") },
			cause:   authCauseTransport,
			reason:  authReasonAcceptanceUnknown,
		},
		"ledger": {
			install: func(f *providerAuthFixture) {
				restoreLedgerHooksForFixture(f)
			},
			cause:  authCauseProcess,
			reason: authReasonAcceptanceUnknown,
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newProviderAuthFixture(t)
			flow := fixture.authorize(t, authMethodAPIKey, "request-1")

			if name == "ledger" {
				restoreLedgerHooks(t)

				ledgerCreateTemp = func(string, string) (ledgerFile, error) { return nil, errors.New("temp") }
			} else {
				testCase.install(fixture)
			}

			_, err := fixture.call(t, AuthCallbackMethod, map[string]any{
				"sessionId":  fixture.sessionID,
				"providerId": authProviderOpenAI,
				"method":     authMethodAPIKey,
				"flowId":     flow.FlowID,
				"input":      "sk-canary",
			})
			if err == nil {
				t.Fatal("a failed apply reported success")
			}

			requireAuthCause(t, err, testCase.cause)

			status := fixture.status(t, flow.FlowID)
			if status.State != authStateFailed || status.Reason != testCase.reason {
				t.Fatalf("status = %+v, want failed/%s", status, testCase.reason)
			}
		})
	}
}

func restoreLedgerHooksForFixture(*providerAuthFixture) {}

func TestAuthCallbackFailsWithoutTheNativeAuthSurface(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodAPIKey, "request-1")

	session, err := fixture.agent.session(acp.SessionId(fixture.sessionID))
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	session.mu.Lock()
	session.client = newSpyCodexClient()
	session.mu.Unlock()

	_, err = fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodAPIKey,
		"flowId":     flow.FlowID,
		"input":      "sk-canary",
	})
	if err == nil {
		t.Fatal("callback answered without the native auth surface")
	}

	requireAuthCause(t, err, authCauseTransport)
}

func TestAuthStatusDrivesTheBackstopBehindItsFloor(t *testing.T) {
	restoreAuthClock(t)

	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	// authorize read the account the backstop measures against; the polls this
	// test counts are the reads after it.
	baselineReads := fixture.client.accountRead

	if status := fixture.status(t, flow.FlowID); status.State != authStatePending {
		t.Fatalf("state = %q, want pending", status.State)
	}

	if fixture.client.accountRead != baselineReads+1 {
		t.Fatalf("first status drove %d reads", fixture.client.accountRead-baselineReads)
	}

	fixture.status(t, flow.FlowID)

	if fixture.client.accountRead != baselineReads+1 {
		t.Fatalf("a second status inside the floor drove %d reads", fixture.client.accountRead-baselineReads)
	}

	base := time.Now()
	authNow = func() time.Time { return base.Add(authPollFloor + time.Second) }

	fixture.client.account = codex.Account{AuthMode: codex.AuthModeChatGPT}

	status := fixture.status(t, flow.FlowID)
	if status.State != authStateAuthenticated {
		t.Fatalf("state = %q, want authenticated", status.State)
	}

	if status.ExpiresAt == 0 {
		t.Fatal("an authenticated flow carried no credential expiry")
	}

	wantExpiry := authNow().Add(codexAccessTokenLifetime).UnixMilli()
	if status.ExpiresAt != wantExpiry {
		t.Fatalf("expiresAt = %d, want %d", status.ExpiresAt, wantExpiry)
	}
}

// TestAuthStatusProbeRefusesAnAccountThatDidNotChange pins the backstop against
// the flow rather than against CODEX_HOME. The home already holds a ChatGPT
// credential, nobody has visited the verification URL, and account/read answers
// exactly what it answered before the flow started — so the flow has learned
// nothing and must stay pending.
func TestAuthStatusProbeRefusesAnAccountThatDidNotChange(t *testing.T) {
	restoreAuthClock(t)

	fixture := newProviderAuthFixture(t)
	fixture.client.account = codex.Account{
		ID:       "acct-resident",
		Email:    "resident@example.test",
		PlanType: "plus",
		AuthMode: codex.AuthModeChatGPT,
	}

	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	if status := fixture.status(t, flow.FlowID); status.State != authStatePending {
		t.Fatalf("state = %q, want pending", status.State)
	}
}

// TestAuthStatusProbeCompletesWhenTheAccountSwitched pins the other half: a
// login that replaced the resident account is a change the backstop can see.
func TestAuthStatusProbeCompletesWhenTheAccountSwitched(t *testing.T) {
	restoreAuthClock(t)

	fixture := newProviderAuthFixture(t)
	fixture.client.account = codex.Account{ID: "acct-resident", AuthMode: codex.AuthModeChatGPT}

	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	fixture.client.account = codex.Account{ID: "acct-new", AuthMode: codex.AuthModeChatGPT}

	if status := fixture.status(t, flow.FlowID); status.State != authStateAuthenticated {
		t.Fatalf("state = %q, want authenticated", status.State)
	}
}

// TestLoginCompletedConfirmsAReloginOfTheSameAccount pins the correlated path
// against the differential one. codex names this flow's loginId in its own
// completion notification, which proves the login the backstop could only
// infer, so an owner logging back into the account the home already held is
// still a completed login.
func TestLoginCompletedConfirmsAReloginOfTheSameAccount(t *testing.T) {
	restoreAuthClock(t)

	fixture := newProviderAuthFixture(t)
	fixture.client.account = codex.Account{ID: "acct-resident", AuthMode: codex.AuthModeChatGPT}

	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	fixture.broker.loginCompleted(t.Context(), codex.LoginCompletion{LoginID: "login-1", Success: true})

	if status := fixture.status(t, flow.FlowID); status.State != authStateAuthenticated {
		t.Fatalf("state = %q, want authenticated", status.State)
	}
}

func TestAuthStatusProbeIsInertOutsideAPendingDeviceFlow(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	secret := fixture.authorize(t, authMethodAPIKey, "request-1")

	fixture.status(t, secret.FlowID)

	if fixture.client.accountRead != 0 {
		t.Fatal("a secret flow drove the account backstop")
	}
}

func TestAuthStatusProbeToleratesNativeFailure(t *testing.T) {
	restoreAuthClock(t)

	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")
	fixture.client.accountErr = errors.New("read")

	if status := fixture.status(t, flow.FlowID); status.State != authStatePending {
		t.Fatalf("state = %q, want pending", status.State)
	}
}

func TestAuthStatusProbeSkipsAMissingNativeSurface(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	session, err := fixture.agent.session(acp.SessionId(fixture.sessionID))
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	session.mu.Lock()
	session.client = newSpyCodexClient()
	session.mu.Unlock()

	if status := fixture.status(t, flow.FlowID); status.State != authStatePending {
		t.Fatalf("state = %q, want pending", status.State)
	}
}

func TestAuthStatusProbeStopsWhenTheLedgerCannotConfirm(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")
	fixture.client.account = codex.Account{AuthMode: codex.AuthModeChatGPT}

	restoreLedgerHooks(t)

	ledgerCreateTemp = func(string, string) (ledgerFile, error) { return nil, errors.New("temp") }

	status := fixture.status(t, flow.FlowID)
	if status.State != authStateFailed || status.Reason != authReasonAcceptanceUnknown {
		t.Fatalf("status = %+v, want failed/acceptance_unknown", status)
	}
}

func TestAuthStatusAddressingFailures(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	cases := []map[string]any{
		{"providerId": authProviderOpenAI, "flowId": flow.FlowID},
		{"sessionId": fixture.sessionID, "flowId": flow.FlowID},
		{"sessionId": fixture.sessionID, "providerId": authProviderOpenAI},
		{"sessionId": fixture.sessionID, "providerId": "other", "flowId": flow.FlowID},
		{"sessionId": fixture.sessionID, "providerId": authProviderOpenAI, "flowId": "absent"},
		{"sessionId": "absent", "providerId": authProviderOpenAI, "flowId": flow.FlowID},
	}

	for _, params := range cases {
		if _, err := fixture.call(t, AuthStatusMethod, params); err == nil {
			t.Fatalf("status accepted %v", params)
		}
	}
}

func TestAuthCancelIsIdempotent(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	params := map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"flowId":     flow.FlowID,
	}

	if _, err := fixture.call(t, AuthCancelMethod, params); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	status := fixture.status(t, flow.FlowID)
	if status.State != authStateCancelled || status.Reason != authReasonOwnerCancel {
		t.Fatalf("status = %+v", status)
	}

	result, err := fixture.call(t, AuthCancelMethod, params)
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}

	if applied, _ := result.(authFlowIDResult); applied.FlowID != flow.FlowID {
		t.Fatal("the idempotent cancel did not echo the flow id")
	}

	if fixture.status(t, flow.FlowID).Reason != authReasonOwnerCancel {
		t.Fatal("the second cancel rewrote the terminal reason")
	}
}

func TestAuthCancelAddressingFailure(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	_, err := fixture.call(t, AuthCancelMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"flowId":     "absent",
	})
	if err == nil {
		t.Fatal("cancel accepted an unknown flow id")
	}

	requireInvalidField(t, err, authFieldFlowID)
}

func TestAuthFlowExpiresOnItsDeadline(t *testing.T) {
	restoreAuthClock(t)

	fixture := newProviderAuthFixture(t)
	authNow = func() time.Time { return time.Now().Add(-authSafetyDeadline - time.Second) }

	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fixture.status(t, flow.FlowID).State == authStateExpired {
			break
		}

		time.Sleep(5 * time.Millisecond)
	}

	status := fixture.status(t, flow.FlowID)
	if status.State != authStateExpired || status.Reason != authReasonDeadline {
		t.Fatalf("status = %+v, want expired/deadline", status)
	}
}

func TestLoginCompletedConfirmsAndRefuses(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")
	fixture.client.account = codex.Account{AuthMode: codex.AuthModeChatGPT}

	fixture.broker.loginCompleted(t.Context(), codex.LoginCompletion{LoginID: "login-1", Success: true})

	if status := fixture.status(t, flow.FlowID); status.State != authStateAuthenticated {
		t.Fatalf("state = %q, want authenticated", status.State)
	}

	refused := newProviderAuthFixture(t)
	refusedFlow := refused.authorize(t, authMethodDeviceCode, "request-1")

	refused.broker.loginCompleted(t.Context(), codex.LoginCompletion{LoginID: "login-1"})

	status := refused.status(t, refusedFlow.FlowID)
	if status.State != authStateFailed || status.Reason != authReasonProviderRefused {
		t.Fatalf("status = %+v, want failed/provider_refused", status)
	}
}

func TestLoginCompletedIgnoresUnmatchedCompletions(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	fixture.broker.loginCompleted(t.Context(), codex.LoginCompletion{})
	fixture.broker.loginCompleted(t.Context(), codex.LoginCompletion{LoginID: "other", Success: true})

	if status := fixture.status(t, flow.FlowID); status.State != authStatePending {
		t.Fatalf("state = %q, want pending", status.State)
	}
}

func TestLoginCompletedStopsWhenTheSessionIsGone(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	fixture.authorize(t, authMethodDeviceCode, "request-1")

	fixture.agent.removeSession(acp.SessionId(fixture.sessionID))

	fixture.broker.loginCompleted(t.Context(), codex.LoginCompletion{LoginID: "login-1", Success: true})

	fixture.broker.mu.Lock()
	defer fixture.broker.mu.Unlock()

	for _, flow := range fixture.broker.flows {
		if authTerminal(flow.state) {
			t.Fatalf("a flow was terminalized without its session: %+v", flow.state)
		}
	}
}

func TestProviderAuthEventRoutesLoginCompletion(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")
	fixture.client.account = codex.Account{AuthMode: codex.AuthModeChatGPT}

	fixture.agent.applyCodexClientEvent(t.Context(), fixture.client, codex.Event{
		Kind:  codex.EventLoginCompleted,
		Login: codex.LoginCompletion{LoginID: "login-1", Success: true},
	})

	if status := fixture.status(t, flow.FlowID); status.State != authStateAuthenticated {
		t.Fatalf("state = %q, want authenticated", status.State)
	}
}

// TestLoginCompletionReachesTheBrokerThroughTheEventSink enters where
// production enters. The app-server hands every decoded notification to the
// sink, and the sink is the only handler an agent installs, so a completion the
// sink drops is a completion no running agent ever sees however faithfully the
// broker handles one delivered by hand.
func TestLoginCompletionReachesTheBrokerThroughTheEventSink(t *testing.T) {
	restoreAuthClock(t)

	fixture := newProviderAuthFixture(t)
	fixture.client.account = codex.Account{ID: "acct-resident", AuthMode: codex.AuthModeChatGPT}

	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	sink := &codexClientEventSink{agent: fixture.agent}
	if err := sink.SetClient(fixture.client); err != nil {
		t.Fatal(err)
	}
	sink.Handle(t.Context(), codex.Event{
		Kind:  codex.EventLoginCompleted,
		Login: codex.LoginCompletion{LoginID: "login-1", Success: true},
	})

	if status := fixture.status(t, flow.FlowID); status.State != authStateAuthenticated {
		t.Fatalf("state = %q, want authenticated", status.State)
	}
}

// TestRejectedLoginFailsThroughTheEventSink pins the other half of the same
// notification. A declined grant is the only prompt refusal signal this surface
// has; without it the flow waits out the safety deadline and reports an expiry
// for a login the owner actively refused.
func TestRejectedLoginFailsThroughTheEventSink(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	sink := &codexClientEventSink{agent: fixture.agent}
	if err := sink.SetClient(fixture.client); err != nil {
		t.Fatal(err)
	}
	sink.Handle(t.Context(), codex.Event{
		Kind:  codex.EventLoginCompleted,
		Login: codex.LoginCompletion{LoginID: "login-1", Success: false},
	})

	status := fixture.status(t, flow.FlowID)
	if status.State != authStateFailed || status.Reason != authReasonProviderRefused {
		t.Fatalf("status = %+v, want failed/provider_refused", status)
	}
}

func TestProviderAuthEventIsInertWithoutABroker(t *testing.T) {
	agent := NewAgent()
	agent.applyCodexClientEvent(t.Context(), newSpyCodexClient(), codex.Event{
		Kind:  codex.EventLoginCompleted,
		Login: codex.LoginCompletion{LoginID: "login-1", Success: true},
	})
}

func TestAuthDisconnectClearsTheFencedAccount(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodAPIKey, "request-1")

	if _, err := fixture.call(t, AuthCallbackMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"method":     authMethodAPIKey,
		"flowId":     flow.FlowID,
		"input":      "sk-canary",
	}); err != nil {
		t.Fatalf("callback: %v", err)
	}

	record, _, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil {
		t.Fatalf("ledger read: %v", err)
	}

	if _, disconnectErr := fixture.call(t, AuthDisconnectMethod, map[string]any{
		"sessionId":         fixture.sessionID,
		"providerId":        authProviderOpenAI,
		"connectionId":      record.ConnectionID,
		"bindingGeneration": record.BindingGeneration,
	}); disconnectErr != nil {
		t.Fatalf("disconnect: %v", disconnectErr)
	}

	if fixture.client.logoutCalls != 1 {
		t.Fatalf("disconnect drove %d native removals", fixture.client.logoutCalls)
	}

	after, _, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil {
		t.Fatalf("ledger read: %v", err)
	}

	if after.State != authLedgerRemoved {
		t.Fatalf("ledger state = %q", after.State)
	}

	if after.BindingGeneration != record.BindingGeneration+1 {
		t.Fatalf("binding generation went %d -> %d", record.BindingGeneration, after.BindingGeneration)
	}

	if _, repeatErr := fixture.call(t, AuthDisconnectMethod, map[string]any{
		"sessionId":         fixture.sessionID,
		"providerId":        authProviderOpenAI,
		"connectionId":      after.ConnectionID,
		"bindingGeneration": after.BindingGeneration,
	}); repeatErr == nil {
		t.Fatal("disconnect answered against a record it had already removed")
	} else {
		requireAuthCause(t, repeatErr, authCauseBindingConflict)
	}

	if fixture.client.logoutCalls != 1 {
		t.Fatalf("a repeat drove %d native account removals", fixture.client.logoutCalls)
	}
}

func TestAuthDisconnectFencing(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	base := map[string]any{
		"sessionId":         fixture.sessionID,
		"providerId":        authProviderOpenAI,
		"connectionId":      "connection-1",
		"bindingGeneration": 1,
	}

	_, err := fixture.call(t, AuthDisconnectMethod, base)
	if err == nil {
		t.Fatal("disconnect answered with no ledger entry")
	}

	requireAuthCause(t, err, authCauseBindingConflict)

	seeded := sampleLedgerRecord()
	if writeErr := fixture.broker.ledger.write(seeded); writeErr != nil {
		t.Fatalf("seed ledger: %v", writeErr)
	}

	// The seeded entry names connection-1 at generation 3, so base is a stale
	// generation against a live entry.
	_, err = fixture.call(t, AuthDisconnectMethod, base)
	if err == nil {
		t.Fatal("disconnect accepted a stale binding generation")
	}

	requireAuthCause(t, err, authCauseBindingConflict)

	wrongConnection := map[string]any{}
	for key, value := range base {
		wrongConnection[key] = value
	}

	wrongConnection["connectionId"] = "connection-other"
	wrongConnection["bindingGeneration"] = seeded.BindingGeneration

	_, err = fixture.call(t, AuthDisconnectMethod, wrongConnection)
	if err == nil {
		t.Fatal("disconnect accepted a differently fenced connection")
	}

	requireAuthCause(t, err, authCauseBindingConflict)

	if fixture.client.logoutCalls != 0 {
		t.Fatalf("a refused fence drove %d native removals", fixture.client.logoutCalls)
	}

	live, _, readErr := fixture.broker.ledger.read(authProviderOpenAI)
	if readErr != nil || live.BindingGeneration != seeded.BindingGeneration || live.State != seeded.State {
		t.Fatalf("a refused fence mutated the ledger: %#v/%v", live, readErr)
	}

	for _, field := range []string{"sessionId", "providerId", "connectionId", "bindingGeneration"} {
		params := map[string]any{}
		for key, value := range base {
			params[key] = value
		}

		delete(params, field)

		if _, err := fixture.call(t, AuthDisconnectMethod, params); err == nil {
			t.Fatalf("disconnect accepted a request missing %q", field)
		}
	}
}

func TestAuthDisconnectFailurePaths(t *testing.T) {
	seed := func(f *providerAuthFixture) map[string]any {
		record := sampleLedgerRecord()
		if err := f.broker.ledger.write(record); err != nil {
			t.Fatalf("seed ledger: %v", err)
		}

		return map[string]any{
			"sessionId":         f.sessionID,
			"providerId":        authProviderOpenAI,
			"connectionId":      record.ConnectionID,
			"bindingGeneration": record.BindingGeneration,
		}
	}

	t.Run("logout", func(t *testing.T) {
		fixture := newProviderAuthFixture(t)
		params := seed(fixture)
		fixture.client.logoutErr = errors.New("refused")

		_, err := fixture.call(t, AuthDisconnectMethod, params)
		if err == nil {
			t.Fatal("a failed removal reported success")
		}

		requireAuthCause(t, err, authCauseTransport)
	})

	t.Run("residue", func(t *testing.T) {
		fixture := newProviderAuthFixture(t)
		params := seed(fixture)
		fixture.client.account = codex.Account{AuthMode: codex.AuthModeChatGPT}

		_, err := fixture.call(t, AuthDisconnectMethod, params)
		if err == nil {
			t.Fatal("disconnect reported success with a resident account")
		}

		requireAuthCause(t, err, authCauseHarvestFailed)
	})

	t.Run("nosurface", func(t *testing.T) {
		fixture := newProviderAuthFixture(t)
		params := seed(fixture)

		session, err := fixture.agent.session(acp.SessionId(fixture.sessionID))
		if err != nil {
			t.Fatalf("session: %v", err)
		}

		session.mu.Lock()
		session.client = newSpyCodexClient()
		session.mu.Unlock()

		_, err = fixture.call(t, AuthDisconnectMethod, params)
		if err == nil {
			t.Fatal("disconnect answered without the native auth surface")
		}

		requireAuthCause(t, err, authCauseTransport)
	})

	t.Run("ledgerread", func(t *testing.T) {
		fixture := newProviderAuthFixture(t)
		params := seed(fixture)

		restoreLedgerHooks(t)

		ledgerReadFile = func(string) ([]byte, error) { return nil, errors.New("read") }

		_, err := fixture.call(t, AuthDisconnectMethod, params)
		if err == nil {
			t.Fatal("a failed ledger read was swallowed")
		}

		requireAuthCause(t, err, authCauseHarvestFailed)
	})

	t.Run("intentwrite", func(t *testing.T) {
		fixture := newProviderAuthFixture(t)
		params := seed(fixture)

		restoreLedgerHooks(t)

		ledgerCreateTemp = func(string, string) (ledgerFile, error) { return nil, errors.New("temp") }

		_, err := fixture.call(t, AuthDisconnectMethod, params)
		if err == nil {
			t.Fatal("a failed ledger write was swallowed")
		}

		requireAuthCause(t, err, authCauseProcess)
	})

	t.Run("removalwrite", func(t *testing.T) {
		fixture := newProviderAuthFixture(t)
		params := seed(fixture)

		restoreLedgerHooks(t)

		writes := 0
		realCreateTemp := ledgerCreateTemp
		ledgerCreateTemp = func(dir string, pattern string) (ledgerFile, error) {
			writes++
			if writes > 1 {
				return nil, errors.New("temp")
			}

			return realCreateTemp(dir, pattern)
		}

		_, err := fixture.call(t, AuthDisconnectMethod, params)
		if err == nil {
			t.Fatal("a failed removal record was swallowed")
		}

		requireAuthCause(t, err, authCauseProcess)
	})
}

// ladderProbeClient answers one question at the moment the native interrupt
// reaches the app-server: was the session's provider-auth flow still live? The
// shutdown ladder fixes flow cancellation at step 4 and the native interrupt at
// step 5, so every observation this records must be false.
type ladderProbeClient struct {
	*authSpyClient

	live func() bool

	probeMu  sync.Mutex
	observed []bool
}

func (c *ladderProbeClient) CancelTurn(context.Context, string, string) error {
	c.probeMu.Lock()
	defer c.probeMu.Unlock()

	c.observed = append(c.observed, c.live())

	return nil
}

func (c *ladderProbeClient) observations() []bool {
	c.probeMu.Lock()
	defer c.probeMu.Unlock()

	return append([]bool(nil), c.observed...)
}

// TestShutdownLadderCancelsProviderAuthBeforeTheNativeInterrupt pins step 4's
// fixed position on all three teardown surfaces. A flow cancelled after the
// interrupt has already been abandoned to a process being torn down: its
// completer stays armed and its record stays nonterminal for a session nothing
// will readmit.
func TestShutdownLadderCancelsProviderAuthBeforeTheNativeInterrupt(t *testing.T) {
	for _, test := range []struct {
		name     string
		teardown func(*providerAuthFixture)
	}{
		{"session/close", func(f *providerAuthFixture) {
			_, _ = f.agent.CloseSession(context.Background(), acp.CloseSessionRequest{
				SessionId: acp.SessionId(f.sessionID),
			})
		}},
		{"session/delete", func(f *providerAuthFixture) {
			_, _ = f.agent.UnstableDeleteSession(context.Background(), acp.UnstableDeleteSessionRequest{
				SessionId: acp.SessionId(f.sessionID),
			})
		}},
		{"Agent.Close", func(f *providerAuthFixture) { _ = f.agent.Close() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newProviderAuthFixture(t)
			flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

			session := fixture.agent.activeSession(acp.SessionId(fixture.sessionID))
			if session == nil {
				t.Fatal("the fixture session is not addressable")
			}

			probe := &ladderProbeClient{authSpyClient: fixture.client, live: func() bool {
				fixture.broker.mu.Lock()
				defer fixture.broker.mu.Unlock()

				return fixture.broker.byID[flow.FlowID] != nil
			}}

			session.mu.Lock()
			session.client = probe
			session.mu.Unlock()

			session.beginTurn(context.Background(), "ladder-nonce")
			session.setTurnID("native-ladder-turn")

			test.teardown(fixture)

			observed := probe.observations()
			if len(observed) == 0 {
				t.Fatal("the teardown issued no native interrupt to order step 4 against")
			}

			for index, live := range observed {
				if live {
					t.Fatalf("native interrupt %d ran while the provider-auth flow was still armed", index)
				}
			}

			fixture.broker.mu.Lock()
			survivor := fixture.broker.byID[flow.FlowID]
			fixture.broker.mu.Unlock()

			if survivor != nil {
				t.Fatal("the teardown left the flow addressable")
			}
		})
	}
}

// TestAuthDeleteSessionTerminalizesPendingFlows pins the ladder's fourth rung on
// session/delete. Delete retires an id for good — readmission is keyed by that
// same id, so nothing later can undo a mark or resolve a flow left armed against
// it. A pending flow that survived a delete would hold a nonterminal record and
// an armed completer for a session that no longer exists.
func TestAuthDeleteSessionTerminalizesPendingFlows(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	other := fixture.authorizeOtherSession(t)

	fixture.broker.mu.Lock()
	record := fixture.broker.byID[flow.FlowID]
	fixture.broker.mu.Unlock()

	if record == nil {
		t.Fatal("the authorized flow is not addressable")
	}

	// The fixture names its thread `thread-1` rather than a native uuid, so the
	// last rung — native thread cleanup — refuses. The tombstone and the rung
	// under test both stand before it, and that is what is asserted.
	_, _ = fixture.agent.UnstableDeleteSession(context.Background(), acp.UnstableDeleteSessionRequest{
		SessionId: acp.SessionId(fixture.sessionID),
	})

	if !fixture.agent.isDeleted(acp.SessionId(fixture.sessionID)) {
		t.Fatal("the delete never committed its tombstone")
	}

	fixture.broker.mu.Lock()
	addressable := fixture.broker.byID[flow.FlowID]
	survivor := fixture.broker.byID[other]
	state := record.state
	reason := record.reason
	fixture.broker.mu.Unlock()

	if addressable != nil {
		t.Fatal("a deleted session left its flow addressable")
	}

	if state != authStateCancelled || reason != authReasonSessionClosed {
		t.Fatalf("flow terminalized as %q/%q, want %q/%q", state, reason, authStateCancelled, authReasonSessionClosed)
	}

	select {
	case <-record.disarm:
	default:
		t.Fatal("a deleted session left its completer armed")
	}

	if survivor == nil {
		t.Fatal("deleting one session cancelled a peer's flow")
	}
}

func TestAuthCloseSessionCancelsPendingFlows(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	other := fixture.authorizeOtherSession(t)

	fixture.broker.closeSession(acp.SessionId(fixture.sessionID))

	fixture.broker.mu.Lock()
	closed := fixture.broker.byID[flow.FlowID]
	survivor := fixture.broker.byID[other]
	fixture.broker.mu.Unlock()

	if closed != nil {
		t.Fatal("a closed session left its flow addressable")
	}

	if survivor == nil {
		t.Fatal("closing one session cancelled a peer's flow")
	}
}

func (f *providerAuthFixture) authorizeOtherSession(t *testing.T) string {
	t.Helper()

	storeRateLimitsSession(t, f.agent, "thread-2", newAuthSpyClient())

	generation := f.mintGeneration(t)

	raw, err := json.Marshal(map[string]any{
		"sessionId":          "thread-2",
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-2",
		"methodsGeneration":  generation,
		"method":             authMethodAPIKey,
		"authorizeRequestId": "request-other",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	result, err := f.agent.HandleExtensionMethod(t.Context(), AuthAuthorizeMethod, raw)
	if err != nil {
		t.Fatalf("authorize peer: %v", err)
	}

	presentation, _ := result.(authAuthorizeResult)

	return presentation.FlowID
}

func TestAuthCloseSessionRunsFromCloseSession(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	if _, err := fixture.agent.CloseSession(t.Context(), acp.CloseSessionRequest{
		SessionId: acp.SessionId(fixture.sessionID),
	}); err != nil {
		t.Fatalf("close session: %v", err)
	}

	fixture.broker.mu.Lock()
	defer fixture.broker.mu.Unlock()

	if _, present := fixture.broker.byID[flow.FlowID]; present {
		t.Fatal("close session left the flow addressable")
	}
}

func TestAuthSupersedeIsInertOnAnAbsentFlow(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	fixture.broker.supersede(authFlowKey{sessionID: "absent", providerID: authProviderOpenAI}, authReasonSuperseded)

	fixture.broker.mu.Lock()
	defer fixture.broker.mu.Unlock()

	if len(fixture.broker.flows) != 0 {
		t.Fatal("supersede invented a flow")
	}
}

func TestAuthNativeCauseClassification(t *testing.T) {
	if got := authNativeCause(context.Background(), errors.New("boom")); got != authCauseTransport {
		t.Fatalf("cause = %s, want transport", got)
	}

	if got := authNativeCause(context.Background(), codex.ErrConnectionClosed); got != authCauseProcess {
		t.Fatalf("cause = %s, want process", got)
	}

	expired, cancel := context.WithCancel(context.Background())
	cancel()

	if got := authNativeCause(expired, errors.New("boom")); got != authCauseTimeout {
		t.Fatalf("cause = %s, want timeout", got)
	}
}

// adversarialConnectionIDs are the caller-minted values the bound refuses. Each
// is a shape the id would otherwise carry into a durable ledger entry and into
// the adapter's own logs, and the two replacement-rune spellings are one Go
// string reached from two different wire encodings, which aliases one
// connection onto another's entry.
func adversarialConnectionIDs() map[string]string {
	return map[string]string{
		"empty":              "",
		"path separators":    "../../../etc/passwd",
		"windows separators": `..\..\connection`,
		"newline":            "connection\n1",
		"nul":                "connection\x00 1",
		"bidi override":      "connection\u202e1",
		"space":              "connection 1",
		"colon":              "connection:1",
		"replacement rune":   "connection-�",
		"non ascii":          "connection-é",
		"unbounded":          strings.Repeat("c", authConnectionIDMaxBytes+1),
	}
}

func TestConnectionIDIsRefusedAtEverySurfaceEntry(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	generation := fixture.mintGeneration(t)

	seeded := sampleLedgerRecord()
	if err := fixture.broker.ledger.write(seeded); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	for name, connectionID := range adversarialConnectionIDs() {
		t.Run(name, func(t *testing.T) {
			_, err := fixture.call(t, AuthAuthorizeMethod, map[string]any{
				"sessionId":          fixture.sessionID,
				"providerId":         authProviderOpenAI,
				"connectionId":       connectionID,
				"methodsGeneration":  generation,
				"method":             authMethodDeviceCode,
				"authorizeRequestId": "request-1",
			})
			requireInvalidField(t, err, authFieldConnectionID)

			_, err = fixture.call(t, AuthDisconnectMethod, map[string]any{
				"sessionId":         fixture.sessionID,
				"providerId":        authProviderOpenAI,
				"connectionId":      connectionID,
				"bindingGeneration": seeded.BindingGeneration,
			})
			requireInvalidField(t, err, authFieldConnectionID)
		})
	}

	// Every refusal above landed before the leg read the entry the live binding
	// names, so nothing recorded a value the bound rejects.
	live, ok, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil || !ok {
		t.Fatalf("ledger read: ok=%v err=%v", ok, err)
	}

	if live.ConnectionID != seeded.ConnectionID || live.BindingGeneration != seeded.BindingGeneration {
		t.Fatalf("a refused connection id reached the live entry: %+v", live)
	}

	if fixture.client.deviceCalls != 0 {
		t.Fatalf("a refused connection id drove %d native mints", fixture.client.deviceCalls)
	}
}

func TestConnectionIDAcceptsTheOpaqueTokenAConsumerMints(t *testing.T) {
	for _, connectionID := range []string{
		"pac_2f1c9b4e-8d3a-4c17-9f21-0b6e5a7c8d90",
		"connection-1",
		"C0",
		strings.Repeat("c", authConnectionIDMaxBytes),
	} {
		if !authValidConnectionID(connectionID) {
			t.Fatalf("connection id %q was refused", connectionID)
		}
	}
}

func mustAuthParams(t *testing.T, params map[string]any) json.RawMessage {
	t.Helper()

	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}

	return raw
}

// TestProviderAuthLegsStopAtShutdown pins the broker against the window in which
// the agent has swept every flow but a leg dispatched before that is still
// running. The session it named is gone whatever the agent's own bookkeeping
// still says, so the leg is answered as addressing a session nobody knows.
func TestProviderAuthLegsStopAtShutdown(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	fixture.authorize(t, authMethodDeviceCode, "request-1")

	if err := fixture.agent.Close(); err != nil {
		t.Fatalf("close agent: %v", err)
	}

	_, err := fixture.broker.methods(context.Background(), mustAuthParams(t, map[string]any{
		"sessionId": fixture.sessionID,
	}))
	if err == nil {
		t.Fatal("a leg answered after the broker shut down")
	}

	requireInvalidField(t, err, "sessionId")
}

// TestADeviceLoginCompletingAfterARemovalRecordsNothing pins the device half of
// the credential slot. The removal bumped the binding generation and cleared the
// account, so a login that completes afterwards names a binding the host no
// longer holds. Recording it would put the provider's entry back at a generation
// nothing released and have inventory report a credential the removal took.
func TestADeviceLoginCompletingAfterARemovalRecordsNothing(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	if _, err := fixture.call(t, AuthDisconnectMethod, map[string]any{
		"sessionId":         fixture.sessionID,
		"providerId":        authProviderOpenAI,
		"connectionId":      "connection-1",
		"bindingGeneration": 1,
	}); err != nil {
		t.Fatalf("disconnect: %v", err)
	}

	fixture.client.account = codex.Account{ID: "account-2", AuthMode: codex.AuthModeChatGPT}

	if status := fixture.status(t, flow.FlowID); status.State != authStatePending {
		t.Fatalf("state = %q, want a flow that recorded nothing", status.State)
	}

	record, ok, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil || !ok {
		t.Fatalf("ledger read: %v ok=%v", err, ok)
	}

	if record.State != authLedgerRemoved || record.BindingGeneration != 2 {
		t.Fatalf("a completion wrote over the removal: %+v", record)
	}
}
