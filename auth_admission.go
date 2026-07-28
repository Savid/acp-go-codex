package codexacp

import (
	"context"
	"time"

	"github.com/coder/acp-go-sdk"
)

// authAdmitGate serializes every caller that names one key. The gate is a
// one-slot channel rather than a mutex because a caller that cannot have the
// key must still be able to give its own answer: the leg ahead of it is inside
// an unbounded native call, and the ACP request context is the only thing that
// bounds the wait. A false report means the caller never held the key and owes
// no release.
//
// The gates map is only ever read and written under p.mu; the channel it hands
// back is what the caller waits on, so the wait itself holds no lock.
func authAdmitGate[K comparable](ctx context.Context, p *providerAuth, gates map[K]chan struct{}, key K) (func(), bool) {
	p.mu.Lock()

	gate, ok := gates[key]
	if !ok {
		gate = make(chan struct{}, 1)
		gates[key] = gate
	}
	p.mu.Unlock()

	select {
	case gate <- struct{}{}:
		return func() { <-gate }, true
	case <-ctx.Done():
		return nil, false
	}
}

// publishFlow makes a minted flow addressable, and is the authoritative check
// that the session admitting the leg is still open. closeSession marks the id
// and takes its sweep set in one critical section, so a flow published after
// that mark was never in the set: it would keep an armed completer, a native
// login, and a replayable idempotency key alive against a session the host has
// already torn down. Refusing here is what keeps the published set and the
// swept set the same set.
//
// There is deliberately no drain. Making session/close wait for legs already
// past their session lookup would block the close for the length of a native
// call this adapter does not bound, and refusing publication reaches the same
// invariant without it.
func (p *providerAuth) publishFlow(key authFlowKey, flow *authFlow) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.sessionAdmitted(key.sessionID) {
		return false
	}

	p.flows[key] = flow
	p.byID[flow.id] = flow

	return true
}

// sessionAdmitted reports whether the broker still answers for a session. The
// caller holds the mutex.
func (p *providerAuth) sessionAdmitted(sessionID acp.SessionId) bool {
	if p.shutdown {
		return false
	}

	_, closed := p.closedSessions[sessionID]

	return !closed
}

// sessionClosed is the cheap refusal at leg entry. It answers the same question
// publishFlow does, one native call earlier, so an ordinary late leg is turned
// away before it starts anything.
func (p *providerAuth) sessionClosed(sessionID acp.SessionId) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return !p.sessionAdmitted(sessionID)
}

// openSession readmits an id the broker had marked closed. Codex names a
// session by the thread it drives, so session/load can bring an id back after
// its close; without this the mark would refuse that id for the agent's whole
// life.
func (p *providerAuth) openSession(sessionID acp.SessionId) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.closedSessions, sessionID)
}

// claimFlow admits exactly one leg to the flow's native mutation. The terminal
// check and the claim are one critical section because they are one decision: a
// leg that reads the state, releases the lock, and then spends an entire native
// call acting on what it read hands a second leg the same live flow, and every
// individual field access is itself locked, so there is no data race for -race
// to find. What is lost is the write, not the lock.
func (p *providerAuth) claimFlow(flow *authFlow) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if authTerminal(flow.state) || flow.claimed {
		return authFailed(authCauseFlowState, flow.providerID, flow.method.ID, flow.id)
	}

	flow.claimed = true

	return nil
}

// releaseFlow hands the flow back. Every claimant defers this: a flow that
// terminalized rejects a later claim on its own, so releasing after a success
// costs nothing and keeps every path the same shape.
func (p *providerAuth) releaseFlow(flow *authFlow) {
	p.mu.Lock()
	defer p.mu.Unlock()

	flow.claimed = false
}

// authProbeAdmission is what one admitted backstop pass reads out of the flow
// it claimed.
type authProbeAdmission struct {
	baseline   authAccountIdentity
	correlated bool
}

// admitProbe claims the flow for one backstop pass. The interval floor, the
// terminal check, and the claim are one critical section because they are one
// decision: two status polls that each read the floor before either advanced it
// would both reach the native account read, and both would go on to confirm one
// login. A pass that cannot have the flow is skipped rather than queued —
// status reports the flow it already holds and never waits on a peer.
func (p *providerAuth) admitProbe(flow *authFlow, now time.Time) (authProbeAdmission, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if authTerminal(flow.state) || flow.claimed || flow.loginID == "" || now.Before(flow.nextProbeAt) {
		return authProbeAdmission{}, false
	}

	flow.claimed = true
	flow.nextProbeAt = now.Add(flow.probeInterval)

	return authProbeAdmission{baseline: flow.baseline, correlated: flow.nativeCompleted}, true
}

// sweepFlows drops and cancels every flow whose key matches, and forgets the
// keys and admission gates that belong to it. The caller holds the mutex,
// because the mark that refuses a later publication and the set this takes are
// one decision.
func (p *providerAuth) sweepFlows(match func(authFlowKey) bool) {
	for key := range p.retired {
		if match(key) {
			delete(p.retired, key)
		}
	}

	for key := range p.admissions {
		if match(key) {
			delete(p.admissions, key)
		}
	}

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
