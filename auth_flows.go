package codexacp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

// Closed flow states.
const (
	authStatePending       = "pending"
	authStateAuthenticated = "authenticated"
	authStateSaved         = "saved"
	authStateFailed        = "failed"
	authStateCancelled     = "cancelled"
	authStateExpired       = "expired"
)

// Closed flow reasons, legal only against the state each pairs with.
const (
	authReasonProviderRefused   = "provider_refused"
	authReasonNativeVeto        = "native_veto"
	authReasonTransport         = "transport"
	authReasonProcess           = "process"
	authReasonAcceptanceUnknown = "acceptance_unknown"
	authReasonHarvestFailed     = "harvest_failed"
	authReasonOwnerCancel       = "owner_cancel"
	authReasonSuperseded        = "superseded"
	authReasonSessionClosed     = "session_closed"
	authReasonDeadline          = "deadline"
)

// Closed interaction discriminator. Codex has no paste-back leg, so it never
// mints `callback`: its device login is `wait` and its API-key login `secret`.
const (
	authInteractionWait   = "wait"
	authInteractionSecret = "secret"
)

const (
	// authSafetyDeadline bounds a flow independently of the harness, which
	// supplies no expiry of its own on this surface.
	authSafetyDeadline = 15 * time.Minute
	// authPollFloor is the fastest cadence a status call may drive the
	// account/read backstop at, so consumer poll cadence never propagates into
	// the provider.
	authPollFloor = 5 * time.Second
	// authNativeCallTimeout bounds one non-blocking native auth call. A
	// ChatGPT-side gating refusal is protocol-silent, so a call that never
	// answers is a first-class outcome rather than a hang.
	authNativeCallTimeout = 30 * time.Second
)

var (
	authRandRead = rand.Read
	authNow      = time.Now
)

// authFlow is the session-scoped record of one login. The presentation it can
// replay lives here and nowhere else: it carries url, message, and userCode,
// which are code-bearing for the flow's life.
type authFlow struct {
	id                 string
	sessionID          acp.SessionId
	providerID         string
	connectionID       string
	revision           int64
	bindingGeneration  int64
	method             authCatalogMethod
	authorizeRequestID string
	presentation       authAuthorizeResult
	// loginID is codex's own handle for a pending device login. It never
	// crosses the boundary; a caller addresses a flow only by flowId.
	loginID string

	createdAt           int64
	state               string
	reason              string
	expiresAt           time.Time
	credentialExpiresAt int64
	harvested           bool
	// presented reports whether the mint published this record's presentation.
	// A record that never reached one has no verbatim answer to replay.
	presented bool

	nextProbeAt   time.Time
	probeInterval time.Duration

	disarm chan struct{}
}

type authAuthorizeResult struct {
	Interaction    string `json:"interaction"`
	URL            string `json:"url,omitempty"`
	Message        string `json:"message"`
	UserCode       string `json:"userCode,omitempty"`
	CallbackInput  string `json:"callbackInput,omitempty"`
	FlowID         string `json:"flowId"`
	FlowExpiresAt  int64  `json:"flowExpiresAt"`
	PollIntervalMs int64  `json:"pollIntervalMs,omitempty"`
}

type authFlowIDResult struct {
	FlowID string `json:"flowId"`
}

type authStatusResult struct {
	FlowID    string `json:"flowId"`
	State     string `json:"state"`
	ExpiresAt int64  `json:"expiresAt,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func authTerminal(state string) bool {
	return state != authStatePending
}

// newAuthToken mints an opaque adapter-owned identifier from 16 CSPRNG bytes,
// encoded unpadded base64url.
func newAuthToken() (string, error) {
	var value [16]byte
	if _, err := authRandRead(value[:]); err != nil {
		return "", fmt.Errorf("create provider auth token: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}

// authorize starts exactly one flow per (sessionId, providerId). It records the
// idempotency key before any native mint and has persisted the flow's slot
// binding before it returns.
func (p *providerAuth) authorize(ctx context.Context, params json.RawMessage) (any, error) {
	fields, err := authParamFields(params,
		authFieldSessionID, authFieldProviderID, authFieldConnectionID,
		authFieldMethodsGeneration, authFieldMethod, authFieldAuthorizeRequestID, authFieldInputs)
	if err != nil {
		return nil, err
	}

	request, err := decodeAuthorizeRequest(fields)
	if err != nil {
		return nil, err
	}

	session, err := p.authSession(request.sessionID)
	if err != nil {
		return nil, err
	}

	key := authFlowKey{sessionID: session.id, providerID: request.providerID}

	if replay, ok := p.replayAuthorize(key, request.authorizeRequestID); ok {
		return replay, nil
	}

	method, err := p.resolveMethod(request)
	if err != nil {
		return nil, err
	}

	flowID, err := newAuthToken()
	if err != nil {
		return nil, authFailed(authCauseProcess, request.providerID, request.method, "")
	}

	p.supersede(key, authReasonSuperseded)

	now := authNow()
	record := authLedgerRecord{
		ProviderID:         request.providerID,
		ConnectionID:       request.connectionID,
		Revision:           1,
		BindingGeneration:  1,
		FlowID:             flowID,
		AuthorizeRequestID: request.authorizeRequestID,
		State:              authLedgerIntent,
		CreatedAt:          now.UnixMilli(),
		UpdatedAt:          now.UnixMilli(),
	}

	if prior, ok, readErr := p.ledger.read(request.providerID); readErr == nil && ok {
		record.Revision = prior.Revision + 1
		record.BindingGeneration = prior.BindingGeneration
		record.CreatedAt = prior.CreatedAt
	}

	if writeErr := p.ledger.write(record); writeErr != nil {
		return nil, authFailed(authCauseProcess, request.providerID, request.method, "")
	}

	flow := &authFlow{
		id:                 flowID,
		sessionID:          session.id,
		providerID:         request.providerID,
		connectionID:       request.connectionID,
		revision:           record.Revision,
		bindingGeneration:  record.BindingGeneration,
		method:             method,
		authorizeRequestID: request.authorizeRequestID,
		createdAt:          record.CreatedAt,
		state:              authStatePending,
		expiresAt:          now.Add(authSafetyDeadline),
		probeInterval:      authPollFloor,
		disarm:             make(chan struct{}),
	}

	// The flow is registered against the ledger entry that already names it, so
	// the flowId every later answer carries — a mint failure's included —
	// addresses a real record.
	p.mu.Lock()
	p.flows[key] = flow
	p.byID[flowID] = flow
	p.mu.Unlock()

	presentation, cause := p.mintPresentation(ctx, session, flow)
	if cause != "" {
		return nil, p.fail(flow, cause, false)
	}

	p.mu.Lock()
	flow.presentation = presentation
	flow.presented = true
	p.mu.Unlock()

	p.armCompleter(flow)

	return presentation, nil
}

type authorizeRequest struct {
	sessionID    string
	providerID   string
	connectionID string
	generation   string
	method       string
	// authorizeRequestID is the caller-minted idempotency key. authorize is the
	// only leg that takes one because it is the most destructive leg here.
	authorizeRequestID string
}

func decodeAuthorizeRequest(fields map[string]json.RawMessage) (authorizeRequest, error) {
	request := authorizeRequest{}

	var err error
	if request.sessionID, err = authRequiredString(fields, authFieldSessionID); err != nil {
		return request, err
	}

	if request.providerID, err = authRequiredString(fields, authFieldProviderID); err != nil {
		return request, err
	}

	if request.connectionID, err = authRequiredString(fields, authFieldConnectionID); err != nil {
		return request, err
	}

	if request.generation, err = authRequiredString(fields, authFieldMethodsGeneration); err != nil {
		return request, err
	}

	if request.method, err = authRequiredString(fields, authFieldMethod); err != nil {
		return request, err
	}

	if request.authorizeRequestID, err = authRequiredString(fields, authFieldAuthorizeRequestID); err != nil {
		return request, err
	}

	// No advertised method carries a prompt, so the visible prompt key set is
	// empty and any supplied answer names a prompt nobody published.
	if raw, ok := fields[authFieldInputs]; ok {
		var inputs map[string]string
		if err := json.Unmarshal(raw, &inputs); err != nil || len(inputs) != 0 {
			return request, invalidAuthField(authFieldInputs)
		}
	}

	return request, nil
}

// replayAuthorize answers a repeated idempotency key verbatim from memory: no
// supersede, no completer disarm, no destruction of flow state, and no native
// call. The record it answers from survives every terminal transition and is
// dropped only when the session closes, so a repeat after completion returns
// what the first call returned instead of driving a second login. A record
// whose mint never published a presentation has nothing to replay.
func (p *providerAuth) replayAuthorize(key authFlowKey, requestID string) (authAuthorizeResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	flow, ok := p.flows[key]
	if !ok || !flow.presented || flow.authorizeRequestID != requestID {
		return authAuthorizeResult{}, false
	}

	return flow.presentation, true
}

// resolveMethod fences a method id against the generation that produced it.
func (p *providerAuth) resolveMethod(request authorizeRequest) (authCatalogMethod, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.generation == "" || p.generation != request.generation {
		return authCatalogMethod{}, invalidAuthField(authFieldMethodsGeneration)
	}

	for _, method := range p.catalog[request.providerID] {
		if method.ID == request.method {
			return method, nil
		}
	}

	return authCatalogMethod{}, invalidAuthField(authFieldMethod)
}

// mintPresentation performs the native mint for the device method and builds
// the wire presentation. The API-key method has nothing to mint: its value is
// submitted through callback and applied natively there. A non-empty cause is
// the leg's failure, and the flow it names owns the transition.
func (p *providerAuth) mintPresentation(ctx context.Context, session *session, flow *authFlow) (authAuthorizeResult, string) {
	result := authAuthorizeResult{
		FlowID:        flow.id,
		FlowExpiresAt: flow.expiresAt.UnixMilli(),
		Message:       flow.method.Label,
	}

	if flow.method.Type == authMethodTypeAPI {
		result.Interaction = authInteractionSecret

		return result, ""
	}

	client := session.nativeAuthClient()
	if client == nil {
		return authAuthorizeResult{}, authCauseTransport
	}

	callCtx, cancel := context.WithTimeout(ctx, authNativeCallTimeout)
	defer cancel()

	login, err := client.StartDeviceCodeLogin(callCtx)
	if err != nil {
		return authAuthorizeResult{}, authNativeCause(callCtx, err)
	}

	if authLoopbackHost(login.VerificationURL) {
		return authAuthorizeResult{}, authCauseUnsupportedVariant
	}

	verificationURL, ok := authDisplayURL(login.VerificationURL)
	if !ok {
		return authAuthorizeResult{}, authCauseNativeVeto
	}

	userCode, ok := authDisplayUserCode(login.UserCode)
	if !ok {
		return authAuthorizeResult{}, authCauseNativeVeto
	}

	p.mu.Lock()
	flow.loginID = login.LoginID
	p.mu.Unlock()

	result.Interaction = authInteractionWait
	result.URL = verificationURL
	result.UserCode = userCode

	return result, ""
}

// armCompleter bounds the flow by its effective deadline. It is armed exactly
// once, at authorize, and status never starts, extends, or rearms it.
func (p *providerAuth) armCompleter(flow *authFlow) {
	deadline := time.Until(flow.expiresAt)
	disarm := flow.disarm

	p.goSafe("provider auth completer", func() {
		timer := time.NewTimer(deadline)
		defer timer.Stop()

		select {
		case <-disarm:
			return
		case <-timer.C:
			p.terminalize(flow, authStateExpired, authReasonDeadline, 0)
		}
	})
}

// supersede terminalizes the flow a new authorize replaces. The superseded
// flowId then addresses nothing. A flow that already terminalized was not
// superseded by anything and keeps both its record and its id.
func (p *providerAuth) supersede(key authFlowKey, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	flow, ok := p.flows[key]
	if !ok || authTerminal(flow.state) {
		return
	}

	delete(p.flows, key)
	delete(p.byID, flow.id)

	flow.state = authStateCancelled
	flow.reason = reason
	flow.stopCompleter()
}

func (f *authFlow) stopCompleter() {
	select {
	case <-f.disarm:
	default:
		close(f.disarm)
	}
}

// callback submits the flow's expected value. Only a secret flow expects one:
// codex has no paste-back leg, so its device login never advertises
// callbackInput and has nothing to submit.
func (p *providerAuth) callback(ctx context.Context, params json.RawMessage) (any, error) {
	fields, err := authParamFields(params, authFieldSessionID, authFieldProviderID, authFieldMethod, authFieldFlowID, authFieldInput)
	if err != nil {
		return nil, err
	}

	sessionID, err := authRequiredString(fields, authFieldSessionID)
	if err != nil {
		return nil, err
	}

	providerID, err := authRequiredString(fields, authFieldProviderID)
	if err != nil {
		return nil, err
	}

	method, err := authRequiredString(fields, authFieldMethod)
	if err != nil {
		return nil, err
	}

	flowID, err := authRequiredString(fields, authFieldFlowID)
	if err != nil {
		return nil, err
	}

	input, err := authString(fields, authFieldInput)
	if err != nil {
		return nil, err
	}

	session, err := p.authSession(sessionID)
	if err != nil {
		return nil, err
	}

	flow, err := p.addressFlow(session.id, providerID, flowID)
	if err != nil {
		return nil, err
	}

	if flow.method.ID != method {
		return nil, invalidAuthField(authFieldMethod)
	}

	if flow.method.Type != authMethodTypeAPI {
		return nil, invalidAuthField(authFieldInput)
	}

	if p.flowState(flow) != authStatePending {
		return nil, authFailed(authCauseFlowState, providerID, method, flowID)
	}

	if err := validateAuthSecret(input); err != nil {
		return nil, err
	}

	return p.applySecret(ctx, session, flow, input)
}

// applySecret applies an operator-supplied API key natively. No harness
// validates a secret at write time, so the flow reaches saved rather than
// authenticated.
func (p *providerAuth) applySecret(ctx context.Context, session *session, flow *authFlow, input string) (any, error) {
	client := session.nativeAuthClient()
	if client == nil {
		return nil, p.fail(flow, authCauseTransport, false)
	}

	callCtx, cancel := context.WithTimeout(ctx, authNativeCallTimeout)
	defer cancel()

	if err := client.StartAPIKeyLogin(callCtx, input); err != nil {
		return nil, p.fail(flow, authNativeCause(callCtx, err), true)
	}

	if err := p.confirmLedger(flow); err != nil {
		return nil, err
	}

	p.terminalize(flow, authStateSaved, "", 0)

	return authFlowIDResult{FlowID: flow.id}, nil
}

// confirmLedger records the post-mutation confirmation that lets inventory
// report residence rather than intent.
func (p *providerAuth) confirmLedger(flow *authFlow) error {
	record := authLedgerRecord{
		ProviderID:         flow.providerID,
		ConnectionID:       flow.connectionID,
		Revision:           flow.revision,
		BindingGeneration:  flow.bindingGeneration,
		FlowID:             flow.id,
		AuthorizeRequestID: flow.authorizeRequestID,
		State:              authLedgerConfirmed,
		CreatedAt:          flow.createdAt,
		UpdatedAt:          authNow().UnixMilli(),
	}

	if err := p.ledger.write(record); err != nil {
		return p.fail(flow, authCauseProcess, true)
	}

	return nil
}

// fail returns the leg's closed error and performs the transition its cause
// pairs with. A cause with no transition consumes nothing.
func (p *providerAuth) fail(flow *authFlow, cause string, materialInFlight bool) error {
	if state, reason := authFlowTransition(cause, materialInFlight); state != "" {
		p.terminalize(flow, state, reason, 0)
	}

	return authFailed(cause, flow.providerID, flow.method.ID, flow.id)
}

func (p *providerAuth) terminalize(flow *authFlow, state string, reason string, credentialExpiresAt int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if authTerminal(flow.state) {
		return
	}

	flow.state = state
	flow.reason = reason
	flow.credentialExpiresAt = credentialExpiresAt

	flow.stopCompleter()
}

func (p *providerAuth) flowState(flow *authFlow) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return flow.state
}

// addressFlow resolves a flowId a caller supplied. A missing, unknown,
// superseded, or cross-session id is a caller addressing failure and never a
// flow failure.
func (p *providerAuth) addressFlow(sessionID acp.SessionId, providerID string, flowID string) (*authFlow, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	flow, ok := p.byID[flowID]
	if !ok || flow.sessionID != sessionID || flow.providerID != providerID {
		return nil, invalidAuthField(authFieldFlowID)
	}

	return flow, nil
}

// status reports the flow, not the connection. Its expiresAt is credential
// expiry and never flow expiry.
func (p *providerAuth) status(ctx context.Context, params json.RawMessage) (any, error) {
	session, flow, err := p.addressedFlowLeg(params)
	if err != nil {
		return nil, err
	}

	p.probe(ctx, session, flow)

	p.mu.Lock()
	defer p.mu.Unlock()

	result := authStatusResult{FlowID: flow.id, State: flow.state, Reason: flow.reason}
	if flow.state == authStateAuthenticated {
		result.ExpiresAt = flow.credentialExpiresAt
	}

	return result, nil
}

// probe drives the account/read backstop behind the adapter's own interval
// floor, serving the cached state in between so a consumer's poll cadence never
// reaches the provider. Codex publishes no poll hint of its own, so the floor is
// the whole interval.
func (p *providerAuth) probe(ctx context.Context, session *session, flow *authFlow) {
	p.mu.Lock()

	now := authNow()
	if authTerminal(flow.state) || flow.loginID == "" || now.Before(flow.nextProbeAt) {
		p.mu.Unlock()

		return
	}

	flow.nextProbeAt = now.Add(flow.probeInterval)
	p.mu.Unlock()

	client := session.nativeAuthClient()
	if client == nil {
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, authNativeCallTimeout)
	defer cancel()

	account, err := client.AccountRead(callCtx)
	if err != nil || account.AuthMode != codex.AuthModeChatGPT {
		return
	}

	p.completeDeviceFlow(flow)
}

// completeDeviceFlow records the confirmation and terminalizes an approved
// device login. The credential expiry is anchored at completion time: codex's
// store records no absolute access-token expiry, and the only exact value lives
// inside a token this surface never decodes.
func (p *providerAuth) completeDeviceFlow(flow *authFlow) {
	if err := p.confirmLedger(flow); err != nil {
		return
	}

	p.terminalize(flow, authStateAuthenticated, "", authNow().Add(codexAccessTokenLifetime).UnixMilli())
}

// loginCompleted applies codex's own completion notification. It is the
// event-driven half of the same confirmation the account/read backstop reaches:
// a success re-probes immediately, and a refusal fails the flow closed.
func (p *providerAuth) loginCompleted(ctx context.Context, completion codex.LoginCompletion) {
	if completion.LoginID == "" {
		return
	}

	p.mu.Lock()

	var pending *authFlow

	for _, flow := range p.flows {
		if flow.loginID == completion.LoginID && !authTerminal(flow.state) {
			pending = flow

			break
		}
	}

	if pending == nil {
		p.mu.Unlock()

		return
	}

	pending.nextProbeAt = time.Time{}
	sessionID := pending.sessionID
	p.mu.Unlock()

	if !completion.Success {
		p.terminalize(pending, authStateFailed, authReasonProviderRefused, 0)

		return
	}

	session, err := p.agent.session(sessionID)
	if err != nil {
		return
	}

	p.probe(ctx, session, pending)
}

// cancel is adapter-owned: codex exposes no native flow-cancel route, so the
// leg does everything the adapter owns and claims nothing about the provider. An
// issued device code stays valid there until it expires.
func (p *providerAuth) cancel(_ context.Context, params json.RawMessage) (any, error) {
	_, flow, err := p.addressedFlowLeg(params)
	if err != nil {
		return nil, err
	}

	p.terminalize(flow, authStateCancelled, authReasonOwnerCancel, 0)

	return authFlowIDResult{FlowID: flow.id}, nil
}

func (p *providerAuth) addressedFlowLeg(params json.RawMessage) (*session, *authFlow, error) {
	fields, err := authParamFields(params, authFieldSessionID, authFieldProviderID, authFieldFlowID)
	if err != nil {
		return nil, nil, err
	}

	sessionID, err := authRequiredString(fields, authFieldSessionID)
	if err != nil {
		return nil, nil, err
	}

	providerID, err := authRequiredString(fields, authFieldProviderID)
	if err != nil {
		return nil, nil, err
	}

	flowID, err := authRequiredString(fields, authFieldFlowID)
	if err != nil {
		return nil, nil, err
	}

	session, err := p.authSession(sessionID)
	if err != nil {
		return nil, nil, err
	}

	flow, err := p.addressFlow(session.id, providerID, flowID)
	if err != nil {
		return nil, nil, err
	}

	return session, flow, nil
}

// disconnect bumps the binding generation before it touches anything else, then
// clears the exactly-fenced account and verifies absence. It removes nothing
// ambient and promises no provider-side revocation: the account stays live at
// OpenAI until the owner revokes it there.
func (p *providerAuth) disconnect(ctx context.Context, params json.RawMessage) (any, error) {
	fields, err := authParamFields(params, authFieldSessionID, authFieldProviderID, authFieldConnectionID, authFieldBindingGeneration)
	if err != nil {
		return nil, err
	}

	sessionID, err := authRequiredString(fields, authFieldSessionID)
	if err != nil {
		return nil, err
	}

	providerID, err := authRequiredString(fields, authFieldProviderID)
	if err != nil {
		return nil, err
	}

	connectionID, err := authRequiredString(fields, authFieldConnectionID)
	if err != nil {
		return nil, err
	}

	bindingGeneration, err := authRequiredInt64(fields, authFieldBindingGeneration)
	if err != nil {
		return nil, err
	}

	session, err := p.authSession(sessionID)
	if err != nil {
		return nil, err
	}

	record, ok, err := p.ledger.read(providerID)
	if err != nil {
		return nil, authFailed(authCauseHarvestFailed, providerID, "", "")
	}

	if !ok || record.ConnectionID != connectionID || record.BindingGeneration != bindingGeneration {
		return nil, authFailed(authCausePolicy, providerID, "", "")
	}

	record.BindingGeneration++
	record.UpdatedAt = authNow().UnixMilli()
	record.State = authLedgerIntent

	if err := p.ledger.write(record); err != nil {
		return nil, authFailed(authCauseProcess, providerID, "", "")
	}

	client := session.nativeAuthClient()
	if client == nil {
		return nil, authFailed(authCauseTransport, providerID, "", "")
	}

	callCtx, cancel := context.WithTimeout(ctx, authNativeCallTimeout)
	defer cancel()

	if err := client.Logout(callCtx); err != nil {
		return nil, authFailed(authNativeCause(callCtx, err), providerID, "", "")
	}

	if account, err := client.AccountRead(callCtx); err != nil || account.AuthMode != "" {
		return nil, authFailed(authCauseHarvestFailed, providerID, "", "")
	}

	record.State = authLedgerRemoved
	record.UpdatedAt = authNow().UnixMilli()

	if err := p.ledger.write(record); err != nil {
		return nil, authFailed(authCauseProcess, providerID, "", "")
	}

	return struct{}{}, nil
}

// closeSession cancels every pending flow the session owns, terminalizing each
// as cancelled/session_closed, and drops every record the session could still
// replay an idempotency key from. It runs before the native interrupt, so a flow
// is never abandoned to a process already being torn down.
func (p *providerAuth) closeSession(sessionID acp.SessionId) {
	p.cancelFlows(func(key authFlowKey) bool { return key.sessionID == sessionID })
}

// closeAll cancels every pending flow when the whole agent shuts down, so no
// completer outlives the process that armed it.
func (p *providerAuth) closeAll() {
	p.cancelFlows(func(authFlowKey) bool { return true })
}

func (p *providerAuth) cancelFlows(match func(authFlowKey) bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for key, flow := range p.flows {
		if !match(key) {
			continue
		}

		delete(p.flows, key)
		delete(p.byID, flow.id)

		if authTerminal(flow.state) {
			continue
		}

		flow.state = authStateCancelled
		flow.reason = authReasonSessionClosed
		flow.stopCompleter()
	}
}

// authNativeCause classifies a native failure without forwarding any of its
// text. A native payload can carry an entire upstream response, headers
// included. A ChatGPT-side gating refusal never answers at all, so a deadline
// the adapter itself imposed is reported as a timeout rather than as transport.
func authNativeCause(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return authCauseTimeout
	}

	if codexRuntimeDied(err) {
		return authCauseProcess
	}

	return authCauseTransport
}
