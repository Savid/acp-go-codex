package codexacp

import (
	"context"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
)

// TestDisconnectAndSecretApplyAdmitOneWriterToTheSlot holds a disconnect inside
// its native removal and starts the secret apply that races it. Both rewrite the
// one credential slot the provider owns, and an apply that compares lineage only
// after its native write leaves the key resident under an entry that says
// removed: inventory skips a removed entry, so the credential is live on the
// provider and invisible on every host surface, and no later disconnect can
// fence what no record names.
func TestDisconnectAndSecretApplyAdmitOneWriterToTheSlot(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodAPIKey, "request-1")

	removing := make(chan struct{})
	release := make(chan struct{})

	fixture.client.beforeLogout = func() {
		close(removing)
		<-release
	}

	disconnected := make(chan error, 1)

	go func() {
		_, err := fixture.call(t, AuthDisconnectMethod, map[string]any{
			"sessionId":         fixture.sessionID,
			"providerId":        authProviderOpenAI,
			"connectionId":      "connection-1",
			"bindingGeneration": 1,
		})
		disconnected <- err
	}()

	<-removing

	applied := make(chan error, 1)

	go func() {
		_, err := fixture.call(t, AuthCallbackMethod, map[string]any{
			"sessionId":  fixture.sessionID,
			"providerId": authProviderOpenAI,
			"method":     authMethodAPIKey,
			"flowId":     flow.FlowID,
			"input":      "sk-canary",
		})
		applied <- err
	}()

	close(release)

	disconnectErr := <-disconnected
	applyErr := <-applied

	if disconnectErr != nil {
		t.Fatalf("disconnect: %v", disconnectErr)
	}

	if fixture.client.apiKeyCalls != 0 {
		t.Fatalf("the apply wrote %d keys into a slot disconnect had already cleared", fixture.client.apiKeyCalls)
	}

	requireAuthCause(t, applyErr, authCauseBindingConflict)

	record, ok, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil || !ok || record.State != authLedgerRemoved {
		t.Fatalf("ledger record = %+v ok=%v err=%v", record, ok, err)
	}
}

// TestASecondCallbackIsRefusedWhileTheFirstIsWriting holds one callback inside
// its native write and submits a second against the same flow. The flow is still
// pending for as long as the first call blocks, so a state check taken before
// the write and released for its duration admits both: two secrets are applied,
// both legs answer saved, and the resident key is whichever native write landed
// last.
func TestASecondCallbackIsRefusedWhileTheFirstIsWriting(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodAPIKey, "request-1")

	submit := func(secret string) (any, error) {
		return fixture.call(t, AuthCallbackMethod, map[string]any{
			"sessionId":  fixture.sessionID,
			"providerId": authProviderOpenAI,
			"method":     authMethodAPIKey,
			"flowId":     flow.FlowID,
			"input":      secret,
		})
	}

	writing := make(chan struct{})
	secondAnswered := make(chan struct{})

	fixture.client.beforeAPIKeyLogin = func() {
		close(writing)
		<-secondAnswered
	}

	first := make(chan error, 1)

	go func() {
		_, err := submit("sk-first")
		first <- err
	}()

	<-writing

	_, secondErr := submit("sk-second")

	close(secondAnswered)

	firstErr := <-first

	if secondErr == nil {
		t.Fatal("two callbacks were admitted to one flow, and the last native write won")
	}

	requireAuthCause(t, secondErr, authCauseFlowState)

	if firstErr != nil {
		t.Fatalf("first callback: %v", firstErr)
	}

	if fixture.client.apiKeyCalls != 1 {
		t.Fatalf("one flow drove %d native key writes", fixture.client.apiKeyCalls)
	}

	if fixture.client.apiKeyValue != "sk-first" {
		t.Fatalf("the resident key is %q rather than the claimant's", fixture.client.apiKeyValue)
	}
}

// TestConcurrentIdenticalAuthorizesMintOneFlow holds one authorize inside its
// native mint and issues the same request again. Neither the replay check nor
// the retired-key check can see a flow the first call has not published yet, so
// an unserialized authorize mints twice for one idempotency key — which is the
// one outcome the key exists to prevent.
func TestConcurrentIdenticalAuthorizesMintOneFlow(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	request := map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  fixture.mintGeneration(t),
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	}

	minting := make(chan struct{})
	release := make(chan struct{})

	fixture.client.beforeDeviceLogin = func() {
		close(minting)
		<-release
	}

	type answer struct {
		presentation authAuthorizeResult
		err          error
	}

	answers := make(chan answer, 2)

	authorize := func() {
		result, err := fixture.call(t, AuthAuthorizeMethod, request)
		presentation, _ := result.(authAuthorizeResult)

		answers <- answer{presentation: presentation, err: err}
	}

	go authorize()

	<-minting

	// The first leg is parked inside its native mint and has already taken its
	// own flow id, so from here a flow id being minted is the second leg
	// deciding to start a login of its own rather than to be answered from the
	// first. Waiting for that decision is what makes the interleaving this test
	// describes the one it actually runs.
	deciding := make(chan struct{}, 1)
	entropy := authRandRead

	t.Cleanup(func() { authRandRead = entropy })

	authRandRead = func(value []byte) (int, error) {
		select {
		case deciding <- struct{}{}:
		default:
		}

		return entropy(value)
	}

	go authorize()

	select {
	case <-deciding:
	case <-time.After(authAdmissionSettle):
	}

	close(release)

	first, second := <-answers, <-answers

	if first.err != nil || second.err != nil {
		t.Fatalf("authorize: %v / %v", first.err, second.err)
	}

	if first.presentation.FlowID != second.presentation.FlowID {
		t.Fatalf("one idempotency key minted %q and %q", first.presentation.FlowID, second.presentation.FlowID)
	}

	if fixture.client.deviceCalls != 1 {
		t.Fatalf("one idempotency key drove %d native mints", fixture.client.deviceCalls)
	}

	record, ok, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil || !ok || record.Revision != 1 || record.FlowID != first.presentation.FlowID {
		t.Fatalf("ledger record = %+v ok=%v err=%v", record, ok, err)
	}
}

// TestAuthorizeCannotPublishIntoAClosedSession holds an authorize between the
// session lookup that admitted it and the publication that makes its flow
// addressable, and closes the session in that window. Close sweeps what is
// published, so an authorize that publishes afterwards escapes the sweep
// entirely: it goes on to mint a device login against a session the host has
// already torn down, and arms a completer nothing will ever cancel.
func TestAuthorizeCannotPublishIntoAClosedSession(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	request := map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  fixture.mintGeneration(t),
		"method":             authMethodDeviceCode,
		"authorizeRequestId": "request-1",
	}

	admitted := make(chan struct{})
	release := make(chan struct{})
	entropy := authRandRead

	t.Cleanup(func() { authRandRead = entropy })

	// The hold is a buffered slot rather than a sync.Once because Once parks
	// every later caller behind the one holding it: a second leg reaching for a
	// flow id would queue inside the hook instead of running the interleaving
	// this test is about.
	first := make(chan struct{}, 1)
	first <- struct{}{}

	authRandRead = func(value []byte) (int, error) {
		select {
		case <-first:
			close(admitted)
			<-release
		default:
		}

		return entropy(value)
	}

	answered := make(chan error, 1)

	go func() {
		_, err := fixture.call(t, AuthAuthorizeMethod, request)
		answered <- err
	}()

	<-admitted

	fixture.broker.closeSession(acp.SessionId(fixture.sessionID))

	close(release)

	err := <-answered
	if err == nil {
		t.Fatal("an authorize published a flow into a session that had already been swept")
	}

	requireInvalidField(t, err, "sessionId")

	if fixture.client.deviceCalls != 0 {
		t.Fatalf("a closed session still drove %d native mints", fixture.client.deviceCalls)
	}

	fixture.broker.mu.Lock()
	defer fixture.broker.mu.Unlock()

	if len(fixture.broker.flows) != 0 || len(fixture.broker.byID) != 0 {
		t.Fatal("a swept session kept an addressable flow")
	}
}

// holdSlot takes the provider's credential-slot gate the way a leg in the
// middle of a native mutation holds it, so the next leg to name that provider
// meets a gate it cannot have.
func (f *providerAuthFixture) holdSlot(t *testing.T) func() {
	t.Helper()

	release, held := authAdmitGate(context.Background(), f.broker, f.broker.slots, authProviderOpenAI)
	if !held {
		t.Fatal("the credential slot gate was not free")
	}

	return release
}

// TestAContendedGateAnswersRatherThanWaitForever pins what a queued leg does
// when its own request is abandoned. The leg ahead of it is inside a native call
// this adapter cannot bound, so waiting for the gate is waiting on the harness:
// every queued leg drops out with the request that owns it and reports a timeout
// it can honestly claim.
func TestAContendedGateAnswersRatherThanWaitForever(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	flow := fixture.authorize(t, authMethodAPIKey, "request-1")

	abandoned, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("authorize", func(t *testing.T) {
		key := authFlowKey{sessionID: acp.SessionId(fixture.sessionID), providerID: authProviderOpenAI}

		release, held := authAdmitGate(context.Background(), fixture.broker, fixture.broker.admissions, key)
		if !held {
			t.Fatal("the admission gate was not free")
		}

		defer release()

		_, err := fixture.broker.authorize(abandoned, mustAuthParams(t, map[string]any{
			"sessionId":          fixture.sessionID,
			"providerId":         authProviderOpenAI,
			"connectionId":       "connection-1",
			"methodsGeneration":  fixture.mintGeneration(t),
			"method":             authMethodDeviceCode,
			"authorizeRequestId": "request-2",
		}))
		requireAuthCause(t, err, authCauseTimeout)
	})

	t.Run("callback", func(t *testing.T) {
		defer fixture.holdSlot(t)()

		_, err := fixture.broker.callback(abandoned, mustAuthParams(t, map[string]any{
			"sessionId":  fixture.sessionID,
			"providerId": authProviderOpenAI,
			"method":     authMethodAPIKey,
			"flowId":     flow.FlowID,
			"input":      "sk-canary",
		}))
		requireAuthCause(t, err, authCauseTimeout)

		if fixture.client.apiKeyCalls != 0 {
			t.Fatal("a leg that never held the slot still wrote to it")
		}

		if state := fixture.status(t, flow.FlowID).State; state != authStatePending {
			t.Fatalf("state = %q, want a flow still open to a resubmission", state)
		}
	})

	t.Run("disconnect", func(t *testing.T) {
		defer fixture.holdSlot(t)()

		_, err := fixture.broker.disconnect(abandoned, mustAuthParams(t, map[string]any{
			"sessionId":         fixture.sessionID,
			"providerId":        authProviderOpenAI,
			"connectionId":      "connection-1",
			"bindingGeneration": 1,
		}))
		requireAuthCause(t, err, authCauseTimeout)

		if fixture.client.logoutCalls != 0 {
			t.Fatal("a leg that never held the slot still cleared it")
		}
	})

	t.Run("intent", func(t *testing.T) {
		generation := fixture.mintGeneration(t)

		abandonable, cancel := context.WithCancel(context.Background())
		defer cancel()

		entropy := authRandRead

		t.Cleanup(func() { authRandRead = entropy })

		// The key gate is free and has to be taken cleanly, so the request is
		// abandoned only once this leg is past it and reaching for the slot.
		authRandRead = func(value []byte) (int, error) {
			cancel()

			return entropy(value)
		}

		defer fixture.holdSlot(t)()

		_, err := fixture.broker.authorize(abandonable, mustAuthParams(t, map[string]any{
			"sessionId":          fixture.sessionID,
			"providerId":         authProviderOpenAI,
			"connectionId":       "connection-1",
			"methodsGeneration":  generation,
			"method":             authMethodDeviceCode,
			"authorizeRequestId": "request-4",
		}))
		requireAuthCause(t, err, authCauseTimeout)

		if record, ok, readErr := fixture.broker.ledger.read(authProviderOpenAI); readErr != nil || !ok || record.FlowID == "" {
			t.Fatalf("a leg that never held the slot rewrote the entry: %+v ok=%v err=%v", record, ok, readErr)
		}
	})

	t.Run("probe", func(t *testing.T) {
		device := fixture.authorize(t, authMethodDeviceCode, "request-3")

		session, err := fixture.agent.session(acp.SessionId(fixture.sessionID))
		if err != nil {
			t.Fatalf("session: %v", err)
		}

		reads := fixture.client.accountRead

		defer fixture.holdSlot(t)()

		fixture.broker.probe(abandoned, session, fixture.broker.byID[device.FlowID])

		if fixture.client.accountRead != reads {
			t.Fatal("a backstop that never held the slot still read the account")
		}
	})
}

// TestAuthorizeCannotWriteItsIntentAcrossARemoval holds an authorize between the
// ledger read that derives its lineage and the write that records it, and runs
// the disconnect that races it. Both rewrite the provider's one entry: an intent
// written from a generation read before the removal bumped it puts the entry
// back at a binding the removal has already released, over the removed record
// that was the only thing naming it as released.
func TestAuthorizeCannotWriteItsIntentAcrossARemoval(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	seed := authLedgerRecord{
		ProviderID:         authProviderOpenAI,
		ConnectionID:       "connection-1",
		Revision:           1,
		BindingGeneration:  1,
		FlowID:             "seed-flow",
		AuthorizeRequestID: "seed-request",
		State:              authLedgerConfirmed,
		CreatedAt:          1,
		UpdatedAt:          1,
	}
	if err := fixture.broker.ledger.write(seed); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	request := map[string]any{
		"sessionId":          fixture.sessionID,
		"providerId":         authProviderOpenAI,
		"connectionId":       "connection-1",
		"methodsGeneration":  fixture.mintGeneration(t),
		"method":             authMethodAPIKey,
		"authorizeRequestId": "request-1",
	}

	recording := make(chan struct{})
	release := make(chan struct{})

	restoreLedgerHooks(t)

	entry := ledgerCreateTemp

	// The claim is a buffered slot rather than a sync.Once because every other
	// ledger write in this test — the removal's included — runs while the first
	// one is held, and Once would park them all behind it.
	first := make(chan struct{}, 1)
	first <- struct{}{}

	ledgerCreateTemp = func(dir string, pattern string) (ledgerFile, error) {
		select {
		case <-first:
			close(recording)
			<-release
		default:
		}

		return entry(dir, pattern)
	}

	authorized := make(chan error, 1)

	go func() {
		_, err := fixture.call(t, AuthAuthorizeMethod, request)
		authorized <- err
	}()

	<-recording

	var disconnectErr error

	finished := make(chan struct{})

	go func() {
		defer close(finished)

		_, disconnectErr = fixture.call(t, AuthDisconnectMethod, map[string]any{
			"sessionId":         fixture.sessionID,
			"providerId":        authProviderOpenAI,
			"connectionId":      "connection-1",
			"bindingGeneration": 1,
		})
	}()

	select {
	case <-finished:
	case <-time.After(authAdmissionSettle):
	}

	close(release)

	<-finished

	if err := <-authorized; err != nil {
		t.Fatalf("authorize: %v", err)
	}

	if disconnectErr != nil {
		t.Fatalf("disconnect: %v", disconnectErr)
	}

	record, ok, err := fixture.broker.ledger.read(authProviderOpenAI)
	if err != nil || !ok {
		t.Fatalf("ledger read: %v ok=%v", err, ok)
	}

	if record.BindingGeneration != seed.BindingGeneration+1 {
		t.Fatalf("the entry names generation %d after a removal bumped it to %d",
			record.BindingGeneration, seed.BindingGeneration+1)
	}
}
