package codexacp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

const (
	runtimeReadyAttempts       = 5
	runtimeReadyApprovalNever  = "never"
	retainedRuntimeThreadLimit = 1024
	retiredResidenceCountLimit = 64
	retiredResidenceByteLimit  = 64 << 20
)

var runtimeNativeTreeTimeout = 5 * time.Second

var runtimeReadyDeadline = 2 * time.Minute
var runtimeRandRead = rand.Read
var runtimeUserHomeDir = os.UserHomeDir
var runtimeRemoveAll = os.RemoveAll
var runtimeStat = os.Stat
var runtimeProbeCodexVersion = codex.ProbeVersion
var errNoRetainedRuntimeThread = errors.New("no retained Codex runtime thread")
var errSharedRuntimeHasPeers = errors.Join(
	ErrContainmentIncomplete,
	codex.ErrContainmentIncomplete,
	errors.New("cannot retire shared Codex runtime while peer sessions still own it"),
)

// retainedRuntimeThread is a native thread reference that outlives an ACP
// session/close but not the app-server generation that still owns the thread.
// The ACP session ID and native thread ID form one inseparable identity; the
// canonical rollout path is never selected from host-provided store data.
type retainedRuntimeThread struct {
	sessionID acp.SessionId
	threadID  string
	path      string
	client    codex.Client
	epoch     uint64
	claimed   bool

	materializedPath    string
	materializedRelease func()
	materializedBytes   int64
	nativeEnded         bool
}

type retiredNativeResidence struct {
	epoch     uint64
	tree      string
	path      string
	bytes     int64
	release   func()
	remove    func(string) error
	reclaimed bool
}

func (a *Agent) runtimeEpochIsCurrent(epoch uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return !a.closed && a.runtimeEpoch == epoch
}

func (a *Agent) handleCodexServerRequestForEpoch(ctx context.Context, epoch uint64, req codex.ServerRequest) (any, error) {
	if !a.runtimeEpochIsCurrent(epoch) {
		return nil, fmt.Errorf("codex server request belongs to a fenced runtime epoch")
	}

	return a.handleCodexServerRequest(ctx, req)
}

func (a *Agent) reserveNativeResidenceCapacity(ctx context.Context, additionalBytes int64) (func(), error) {
	if a.options.HostAuthority == nil {
		return func() {}, nil
	}

	if additionalBytes > retiredResidenceByteLimit {
		return nil, acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "materialized Codex rollout exceeds the managed residence byte limit",
			jsonFieldLimit: "retired_residence_bytes",
		})
	}

	for {
		a.mu.Lock()
		if a.runtimeCleanupErr != nil {
			cleanupErr := a.runtimeCleanupErr
			a.mu.Unlock()

			return nil, toPublicAuthorityError(cleanupErr)
		}

		withinLimit := a.nativeResidenceCount < retiredResidenceCountLimit &&
			a.retiredResidenceBytes+additionalBytes <= retiredResidenceByteLimit
		if withinLimit {
			a.nativeResidenceCount++
			a.retiredResidenceBytes += additionalBytes
		}

		client := a.runtimeClient
		starting := a.runtimeStarting
		a.mu.Unlock()

		if withinLimit {
			return sync.OnceFunc(func() {
				a.mu.Lock()
				a.nativeResidenceCount--
				a.retiredResidenceBytes -= additionalBytes
				a.mu.Unlock()
			}), nil
		}

		if starting != nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-starting:
				continue
			}
		}

		if client == nil {
			return nil, errors.Join(
				ErrContainmentIncomplete,
				fmt.Errorf("%w: retired native residences cannot be reclaimed without a terminal runtime generation", codex.ErrContainmentIncomplete),
			)
		}

		if err := a.retireRuntimeGeneration(context.WithoutCancel(ctx), client); err != nil {
			return nil, err
		}
	}
}

func (a *Agent) retireMaterializedRolloutAtEpoch(path string, bytes int64, release func(), epoch uint64) error {
	return a.retireNativeResidenceAtEpoch(filepath.Dir(path), path, bytes, release, epoch, removeMaterializedRollout)
}

func (a *Agent) retireNativeResidenceAtEpoch(
	tree string,
	path string,
	bytes int64,
	release func(),
	epoch uint64,
	remove func(string) error,
) error {
	if path == "" {
		if release != nil {
			release()
		}

		return nil
	}

	if a.options.HostAuthority == nil {
		err := remove(path)
		if err == nil && release != nil {
			release()
		}

		return err
	}

	a.mu.Lock()
	a.retiredResidences = append(a.retiredResidences, retiredNativeResidence{
		epoch: epoch, tree: tree, path: path, bytes: bytes, release: release, remove: remove,
	})
	a.mu.Unlock()

	return nil
}

func (a *Agent) reclaimRetiredResidences(ctx context.Context, epoch uint64) error {
	if a.options.HostAuthority == nil {
		return nil
	}

	a.mu.Lock()

	candidates := make([]retiredNativeResidence, 0, len(a.retiredResidences))
	for _, residence := range a.retiredResidences {
		if residence.epoch == epoch {
			candidates = append(candidates, residence)
		}
	}
	a.mu.Unlock()

	var result error

	reclaimCtx, cancelReclaim := context.WithTimeout(context.WithoutCancel(ctx), runtimeNativeTreeTimeout)
	defer cancelReclaim()

	for _, residence := range candidates {
		if !residence.reclaimed {
			if err := a.options.HostAuthority.ReclaimNativeTree(reclaimCtx, residence.tree); err != nil {
				if errors.Is(err, ErrNativeTreeBusy) {
					result = errors.Join(result, err)

					continue
				}

				result = errors.Join(result, ErrContainmentIncomplete,
					fmt.Errorf("%w: reclaim native rollout tree: %w", codex.ErrContainmentIncomplete, err))

				continue
			}

			a.mu.Lock()
			for index := range a.retiredResidences {
				if a.retiredResidences[index].path == residence.path && a.retiredResidences[index].epoch == residence.epoch {
					a.retiredResidences[index].reclaimed = true

					break
				}
			}
			a.mu.Unlock()
		}

		if err := residence.remove(residence.path); err != nil {
			result = errors.Join(result, err)

			continue
		}

		if residence.release != nil {
			residence.release()
		}

		a.mu.Lock()
		for index := range a.retiredResidences {
			if a.retiredResidences[index].path == residence.path && a.retiredResidences[index].epoch == residence.epoch {
				a.retiredResidences = append(a.retiredResidences[:index], a.retiredResidences[index+1:]...)

				break
			}
		}
		a.mu.Unlock()
	}

	return result
}

// finalizeRuntimeResources releases admissions only after their corresponding
// resource is gone. An incomplete containment boundary retains both
// reservations because native work may still be using the private runtime root. Scratch deletion failure
// likewise retains the scratch reservation so the worker-wide bound remains
// truthful.
func finalizeRuntimeResources(
	runtimeErr error,
	nativeRelease func() error,
	scratchRoot string,
	scratchRelease func(),
) error {
	if errors.Is(runtimeErr, ErrContainmentIncomplete) || errors.Is(runtimeErr, codex.ErrContainmentIncomplete) {
		return runtimeErr
	}

	if nativeRelease != nil {
		runtimeErr = errors.Join(runtimeErr, nativeRelease())
	}

	var removeErr error
	if scratchRoot != "" {
		removeErr = runtimeRemoveAll(scratchRoot)
	}

	if removeErr == nil && scratchRelease != nil {
		scratchRelease()
	}

	return errors.Join(runtimeErr, removeErr)
}

func latchRuntimeCleanup(err error) bool {
	return errors.Is(err, ErrContainmentIncomplete) || errors.Is(err, ErrHostAuthorityUnavailable) ||
		errors.Is(err, codex.ErrContainmentIncomplete) || errors.Is(err, codex.ErrHostAuthorityUnavailable)
}

func retainRuntimeResources(err error) bool {
	return errors.Is(err, ErrContainmentIncomplete) || errors.Is(err, codex.ErrContainmentIncomplete) ||
		errors.Is(err, ErrNativeTreeBusy) || errors.Is(err, codex.ErrNativeTreeBusy)
}

func (a *Agent) retainOpaqueNativeTree(err error) error {
	return a.retainOpaqueNativeTreeFromNativePump(err, nil)
}

func (a *Agent) retainOpaqueNativeTreeFromNativePump(err error, current *session) error {
	if err == nil {
		return nil
	}

	opaqueErr := errors.Join(err, ErrContainmentIncomplete, codex.ErrContainmentIncomplete)

	a.mu.Lock()
	if a.runtimeCleanupErr == nil {
		a.runtimeCleanupErr = opaqueErr
	}

	a.runtimeDead = true
	fanoutOwner := !a.opaqueNativeFanout
	a.opaqueNativeFanout = true

	fenced := make([]*session, 0, len(a.sessions))
	for _, active := range a.sessions {
		active.setClientDead(true)
		fenced = append(fenced, active)
	}
	a.mu.Unlock()

	if !fanoutOwner {
		return opaqueErr
	}

	for _, active := range fenced {
		if active == current {
			active.deferFenceUntilNativePumpStops()

			continue
		}

		active.fenceSession()
	}

	return opaqueErr
}

func (a *Agent) claimRetainedRuntimeThreadForStore(
	sessionID acp.SessionId,
	storedThreadID string,
) (*retainedRuntimeThread, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	current := func(retained *retainedRuntimeThread) bool {
		return retained != nil &&
			!retained.nativeEnded &&
			!a.closed && !a.runtimeDead &&
			retained.client == a.runtimeClient && retained.epoch == a.runtimeEpoch
	}

	if retained := a.retainedThreads[sessionID]; current(retained) {
		switch {
		case retained.claimed:
			return nil, acp.NewInvalidRequest(map[string]any{
				jsonFieldError: "retained Codex thread lifecycle is already in progress",
			})
		case storedThreadID == "":
			return nil, acp.NewInvalidRequest(map[string]any{
				jsonFieldError: "stored Codex thread identity is required for the retained session",
			})
		case storedThreadID != retained.threadID:
			return nil, acp.NewInvalidRequest(map[string]any{
				jsonFieldError: "stored Codex thread does not match the retained session",
			})
		default:
			retained.claimed = true

			return retained, nil
		}
	}

	if storedThreadID == "" {
		return nil, errNoRetainedRuntimeThread
	}

	for otherID, retained := range a.retainedThreads {
		if otherID != sessionID && current(retained) && retained.threadID == storedThreadID {
			return nil, acp.NewInvalidRequest(map[string]any{
				jsonFieldError: "stored Codex thread is retained by another session",
			})
		}
	}

	for otherID, active := range a.sessions {
		if otherID == sessionID {
			continue
		}

		active.mu.Lock()
		ownedByPeer := active.client == a.runtimeClient && !active.clientDead && active.codexThreadID == storedThreadID
		active.mu.Unlock()

		if ownedByPeer {
			return nil, acp.NewInvalidRequest(map[string]any{
				jsonFieldError: "stored Codex thread is active in another session",
			})
		}
	}

	return nil, errNoRetainedRuntimeThread
}

func (a *Agent) storeRetainedRuntimeSession(session *session, retained *retainedRuntimeThread) error {
	if a.negotiatedLifecycle().Present() {
		if err := session.attachNativeEvents(); err != nil {
			return err
		}
	}

	a.mu.Lock()

	switch {
	case a.closed:
		a.mu.Unlock()

		return newAgentClosedError()

	// A retained resume and a delete of the same id already exclude each other
	// through the retained-thread claim, so this is belt-and-braces: the fence is
	// re-read where the wrapper would become reachable, exactly as the started
	// path does, so no future ordering can register a wrapper behind a delete.
	case a.deleteFencedLocked(session.id):
		committed := a.deleteCommittedLocked(session.id)
		a.mu.Unlock()

		if committed {
			return newUnknownSession()
		}

		return newSessionDeleteInProgress()
	case retained == nil || retained.nativeEnded || !retained.claimed || a.retainedThreads[session.id] != retained:
		a.mu.Unlock()

		return acp.NewInvalidRequest(map[string]any{jsonFieldError: "retained Codex thread ownership changed during resume"})
	case a.runtimeDead || a.runtimeClient != retained.client || a.runtimeEpoch != retained.epoch:
		a.mu.Unlock()

		return acp.NewInvalidRequest(map[string]any{jsonFieldError: "retained Codex runtime ownership changed during resume"})
	case a.sessions[session.id] != nil:
		a.mu.Unlock()

		return acp.NewInvalidRequest(map[string]any{jsonFieldError: "Codex session became active during retained resume"})
	case len(a.sessions) >= a.options.ConcurrencyLimits.MaxActiveSessions:
		a.mu.Unlock()

		return acp.NewInvalidRequest(map[string]any{jsonFieldError: valueBackpressure, jsonFieldLimit: "active_sessions"})
	}

	session.mu.Lock()
	session.materializedPath = retained.materializedPath
	session.materializedRelease = retained.materializedRelease
	session.materializedBytes = retained.materializedBytes
	session.materializedEpoch = retained.epoch
	session.mu.Unlock()

	retained.materializedPath = ""
	retained.materializedRelease = nil
	retained.materializedBytes = 0

	delete(a.retainedThreads, session.id)
	a.sessions[session.id] = session
	a.mu.Unlock()

	a.readmitProviderAuth(session.id)

	a.observe.AddActiveSession(context.Background(), 1)

	return nil
}

func (a *Agent) releaseRetainedRuntimeThreadClaim(retained *retainedRuntimeThread) {
	if retained == nil {
		return
	}

	a.mu.Lock()
	if a.retainedThreads[retained.sessionID] == retained {
		retained.claimed = false
	}
	a.mu.Unlock()
}

func (a *Agent) claimRetainedRuntimeThreadForDelete(sessionID acp.SessionId) (*retainedRuntimeThread, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	retained := a.retainedThreads[sessionID]
	if retained == nil {
		return nil, errNoRetainedRuntimeThread
	}

	if retained.claimed {
		return nil, acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "retained Codex thread lifecycle is already in progress",
		})
	}

	retained.claimed = true

	return retained, nil
}

func (a *Agent) cleanupRetainedRuntimeThread(retained *retainedRuntimeThread) error {
	if retained == nil {
		return nil
	}

	if err := a.retireMaterializedRolloutAtEpoch(
		retained.materializedPath,
		retained.materializedBytes,
		retained.materializedRelease,
		retained.epoch,
	); err != nil {
		return err
	}

	retained.materializedPath = ""
	retained.materializedRelease = nil
	retained.materializedBytes = 0

	return nil
}

func (a *Agent) endRetainedRuntimeThread(retained *retainedRuntimeThread) error {
	if retained == nil {
		return nil
	}

	a.mu.Lock()
	if a.retainedThreads[retained.sessionID] != retained {
		a.mu.Unlock()

		return nil
	}

	retained.nativeEnded = true
	a.mu.Unlock()

	if err := a.cleanupRetainedRuntimeThread(retained); err != nil {
		return err
	}

	a.mu.Lock()
	if a.retainedThreads[retained.sessionID] == retained {
		delete(a.retainedThreads, retained.sessionID)
	}
	a.mu.Unlock()

	return nil
}

func (a *Agent) releaseRetainedRuntimeThreads(client codex.Client, epoch uint64) error {
	a.mu.Lock()

	retained := make([]*retainedRuntimeThread, 0, len(a.retainedThreads))
	for _, candidate := range a.retainedThreads {
		if candidate.client == client && candidate.epoch == epoch {
			retained = append(retained, candidate)
		}
	}
	a.mu.Unlock()

	var cleanupErr error

	for _, candidate := range retained {
		if err := a.cleanupRetainedRuntimeThread(candidate); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)

			continue
		}

		a.mu.Lock()
		if a.retainedThreads[candidate.sessionID] == candidate {
			delete(a.retainedThreads, candidate.sessionID)
		}
		a.mu.Unlock()
	}

	return cleanupErr
}

// sharedRuntime returns the one app-server owned by this Agent. A dead runtime
// is replaced as one atomic generation. Logical sessions remain marked dead
// and resume independently on first use, so one stale peer cannot prevent an
// unrelated session from loading or starting on the replacement generation.
func (a *Agent) sharedRuntime(ctx context.Context) (codex.Client, error) {
	for {
		a.mu.Lock()
		if a.closed {
			a.mu.Unlock()

			return nil, newAgentClosedError()
		}

		if a.runtimeCleanupErr != nil {
			cleanupErr := a.runtimeCleanupErr
			a.mu.Unlock()

			return nil, toPublicAuthorityError(cleanupErr)
		}

		if closing := a.runtimeClosing; closing != nil {
			a.mu.Unlock()

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-closing:
				continue
			}
		}

		if a.runtimeClient != nil && !a.runtimeDead {
			client := a.runtimeClient
			a.mu.Unlock()

			return client, nil
		}

		if wait := a.runtimeStarting; wait != nil {
			a.mu.Unlock()

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-wait:
				continue
			}
		}

		wait := make(chan struct{})
		startCtx, cancelStart := context.WithCancel(ctx)
		a.runtimeStarting = wait
		a.runtimeStartCancel = cancelStart
		oldEpoch := a.runtimeEpoch
		a.runtimeEpoch++
		epoch := a.runtimeEpoch
		oldClient := a.runtimeClient
		oldRelease := a.runtimeNativeRelease
		oldScratchRoot := a.runtimeScratchRoot
		oldScratchRelease := a.runtimeScratchRelease
		a.runtimeClient = nil
		a.runtimeNativeRelease = nil
		a.runtimeScratchRoot = ""
		a.runtimeScratchRelease = nil
		a.runtimeDead = true
		a.mu.Unlock()

		cleanupErr := a.closeRuntimeGeneration(
			context.Background(), oldClient, oldRelease, oldScratchRoot, oldScratchRelease,
			oldEpoch,
		)
		if cleanupErr != nil {
			cancelStart()

			a.mu.Lock()
			a.runtimeClient = oldClient
			a.runtimeNativeRelease = oldRelease
			a.runtimeScratchRoot = oldScratchRoot
			a.runtimeScratchRelease = oldScratchRelease
			a.runtimeEpoch = oldEpoch

			a.runtimeDead = true
			if latchRuntimeCleanup(cleanupErr) {
				a.runtimeCleanupErr = cleanupErr
			}

			a.runtimeStarting = nil
			a.runtimeStartCancel = nil

			close(wait)
			a.mu.Unlock()

			return nil, toPublicAuthorityError(cleanupErr)
		}

		resources, err := a.startRuntimeGeneration(startCtx, epoch)

		cancelStart()

		a.mu.Lock()

		switch {
		case err == nil && !a.closed && a.runtimeEpoch == epoch:
			a.runtimeClient = resources.client
			a.runtimeNativeRelease = resources.nativeRelease
			a.runtimeScratchRoot = resources.scratchRoot
			a.runtimeScratchRelease = resources.scratchRelease
			a.runtimeDead = false
		case err == nil:
			closeErr := resources.client.Close(context.Background())
			cleanupErr := finalizeRuntimeResources(
				closeErr, resources.nativeRelease, resources.scratchRoot, resources.scratchRelease,
			)
			err = errors.Join(newAgentClosedError(), cleanupErr)
		case retainRuntimeResources(err):
			a.runtimeNativeRelease = resources.nativeRelease
			a.runtimeScratchRoot = resources.scratchRoot
			a.runtimeScratchRelease = resources.scratchRelease
			a.runtimeEpoch = epoch
			a.runtimeDead = true
		}

		if latchRuntimeCleanup(err) {
			a.runtimeCleanupErr = err
		}

		a.runtimeStarting = nil
		a.runtimeStartCancel = nil

		close(wait)
		a.mu.Unlock()

		if err != nil {
			return nil, toPublicAuthorityError(err)
		}

		return resources.client, nil
	}
}

type runtimeGenerationResources struct {
	client         codex.Client
	nativeRelease  func() error
	scratchRoot    string
	scratchRelease func()
}

func (a *Agent) startRuntimeGeneration(ctx context.Context, epoch uint64) (runtimeGenerationResources, error) {
	resources := runtimeGenerationResources{scratchRelease: func() {}}

	scratchRoot, err := createPrivateTempDir(a.scratchDir, "acp-go-codex-runtime-")
	if err != nil {
		resources.scratchRelease()

		return resources, err
	}

	resources.scratchRoot = scratchRoot

	resources.nativeRelease, err = a.prepareRuntimeHome(ctx)
	if err != nil {
		removeErr := runtimeRemoveAll(resources.scratchRoot)
		if removeErr == nil {
			resources.scratchRelease()
			resources.scratchRoot = ""
			resources.scratchRelease = nil
		}

		return resources, errors.Join(err, removeErr)
	}

	nativeVersion := minSupportedCodexVersion
	if !a.options.customClientFactory {
		nativeVersion, err = a.probeRuntimeVersion(ctx)
		if err != nil {
			err = finalizeRuntimeResources(
				err, resources.nativeRelease, resources.scratchRoot, resources.scratchRelease,
			)
			if !retainRuntimeResources(err) {
				resources.nativeRelease = nil
				resources.scratchRoot = ""
				resources.scratchRelease = nil
			}

			return resources, err
		}
	}

	resources.client, err = a.launchRuntimeClient(ctx, epoch, resources.scratchRoot, nativeVersion)
	if err != nil {
		err = finalizeRuntimeResources(
			err, resources.nativeRelease, resources.scratchRoot, resources.scratchRelease,
		)
	}

	return resources, err
}

func (a *Agent) prepareRuntimeHome(ctx context.Context) (func() error, error) {
	home := a.resolvedCodexHomeForEnv(a.staticRuntimeEnv())
	if home == "" || !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return nil, errors.New("codex home must resolve to a canonical absolute path")
	}

	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("create Codex home before native preparation: %w", err)
	}

	if err := writeSeedFiles(home, a.options.SeedFiles); err != nil {
		return nil, err
	}

	info, err := runtimeStat(home)
	if err != nil {
		return nil, fmt.Errorf("validate Codex home before native preparation: %w", err)
	}

	if !info.IsDir() {
		return nil, errors.New("codex home must be a directory")
	}

	if a.options.HostAuthority == nil {
		return func() error { return nil }, nil
	}

	if err := a.options.HostAuthority.PrepareNativeTree(ctx, home); err != nil {
		return nil, a.retainOpaqueNativeTree(err)
	}

	return runtimeHomeReleaser(a.options.HostAuthority, home), nil
}

func runtimeHomeReleaser(authority HostAuthority, home string) func() error {
	var (
		reclaimMu  sync.Mutex
		reclaimed  bool
		reclaimErr error
	)

	return func() error {
		reclaimMu.Lock()
		defer reclaimMu.Unlock()

		if reclaimed || reclaimErr != nil {
			return reclaimErr
		}

		reclaimCtx, cancelReclaim := context.WithTimeout(context.Background(), runtimeNativeTreeTimeout)
		err := authority.ReclaimNativeTree(reclaimCtx, home)

		cancelReclaim()

		if err != nil {
			if errors.Is(err, ErrNativeTreeBusy) {
				return err
			}

			reclaimErr = errors.Join(ErrContainmentIncomplete,
				fmt.Errorf("%w: reclaim Codex home: %w", codex.ErrContainmentIncomplete, err))

			return reclaimErr
		}

		reclaimed = true

		return nil
	}
}

func (a *Agent) probeRuntimeVersion(ctx context.Context) (string, error) {
	env := a.staticRuntimeEnv()
	home := a.resolvedCodexHomeForEnv(env)

	scratchRelease := func() {}

	scratchRoot, err := createPrivateTempDir(a.scratchDir, "acp-go-codex-runtime-discovery-")
	if err != nil {
		scratchRelease()

		return "", err
	}

	version, probeErr := runtimeProbeCodexVersion(ctx, codex.VersionProbeOptions{
		CLIPath:             a.options.ExecutablePath,
		CodexHome:           home,
		Scratch:             scratchRoot,
		ScratchParent:       filepath.Dir(scratchRoot),
		Env:                 env,
		ImplicitEnvironment: cloneStringMap(a.options.implicitEnvironment),
		HostAuthority:       adaptHostAuthority(a.options.HostAuthority),
	})
	probeErr = finalizeRuntimeResources(probeErr, nil, scratchRoot, scratchRelease)

	return version, probeErr
}

// staticRuntimeEnv is the app-server process environment: the operator's
// top-level WithEnv values and nothing else. Session environment and session
// path directories are thread scoped and never reach this Agent-wide surface,
// so executable lookup, home resolution, version probing, telemetry, and
// app-server launch cannot observe one session's credential.
func (a *Agent) staticRuntimeEnv() map[string]string {
	if len(a.options.Env) == 0 {
		return nil
	}

	return cloneStringMap(a.options.Env)
}

func (a *Agent) sessionMetaForLifecycle(values map[string]any) (sessionMeta, error) {
	return sessionMetaFromLifecycle(values)
}

func (a *Agent) resolvedCodexHome() string {
	return a.resolvedCodexHomeForEnv(a.options.Env)
}

func (a *Agent) resolvedCodexHomeForEnv(env map[string]string) string {
	if a.options.Home != "" {
		return filepath.Clean(a.options.Home)
	}

	if value := env["CODEX_HOME"]; value != "" {
		return filepath.Clean(value)
	}

	if a.options.HostAuthority != nil {
		native := a.options.HostAuthority.NativeEnvironment()
		if value := native["CODEX_HOME"]; value != "" {
			return filepath.Clean(value)
		}

		if home := native["HOME"]; home != "" {
			return filepath.Join(home, ".codex")
		}
	}

	if value := a.options.implicitEnvironment["CODEX_HOME"]; value != "" {
		return filepath.Clean(value)
	}

	if home := a.options.implicitEnvironment["HOME"]; home != "" {
		return filepath.Join(home, ".codex")
	}

	return ""
}

func (a *Agent) markRuntimeDead(client codex.Client) {
	a.mu.Lock()
	if a.runtimeClient == client {
		a.runtimeDead = true
		for _, session := range a.sessions {
			session.setClientDead(true)
		}
	}
	a.mu.Unlock()
}

// retireRuntimeGeneration fences admission, shuts down the protocol transport,
// waits for the native lease to become terminal, then reclaims its residences.
func (a *Agent) retireRuntimeGeneration(ctx context.Context, expected codex.Client) error {
	return a.retireRuntimeGenerationOwned(ctx, expected, nil)
}

func (a *Agent) retireRuntimeGenerationOwned(ctx context.Context, expected codex.Client, owner *session) error {
	a.mu.Lock()
	if closing := a.runtimeClosing; closing != nil {
		a.mu.Unlock()
		<-closing

		a.mu.Lock()
		cleanupErr := a.runtimeCleanupErr
		a.mu.Unlock()

		return cleanupErr
	}

	if owner != nil && expected != nil && a.runtimeClient == expected {
		for _, candidate := range a.sessions {
			if candidate == owner || candidate.id == owner.id {
				continue
			}

			candidate.mu.Lock()
			peerOwnsGeneration := candidate.client == expected && !candidate.clientDead
			candidate.mu.Unlock()

			if peerOwnsGeneration {
				a.mu.Unlock()

				return errSharedRuntimeHasPeers
			}
		}

		for _, retained := range a.retainedThreads {
			if retained.sessionID != owner.id && retained.client == expected && !retained.nativeEnded {
				a.mu.Unlock()

				return errSharedRuntimeHasPeers
			}
		}
	}

	if expected == nil || a.runtimeClient != expected {
		wait := a.runtimeStarting
		a.mu.Unlock()

		if wait != nil {
			// Another generation transition already owns cleanup. Its completion
			// is the proof that the expected tree cannot survive this return.
			<-wait
		}

		a.mu.Lock()
		cleanupErr := a.runtimeCleanupErr
		a.mu.Unlock()

		return cleanupErr
	}

	wait := make(chan struct{})
	a.runtimeStarting = wait

	client := a.runtimeClient
	epoch := a.runtimeEpoch
	release := a.runtimeNativeRelease
	scratchRoot := a.runtimeScratchRoot
	scratchRelease := a.runtimeScratchRelease

	a.runtimeClient = nil
	a.runtimeNativeRelease = nil
	a.runtimeScratchRoot = ""
	a.runtimeScratchRelease = nil
	a.runtimeDead = true
	a.runtimeEpoch++

	fenced := make([]*session, 0, len(a.sessions))

	for _, session := range a.sessions {
		session.setClientDead(true)

		fenced = append(fenced, session)
	}

	a.mu.Unlock()

	// The generation every peer's incarnation was reading is gone, so each of
	// those incarnations is lost. Fencing them explicitly is what keeps a peer
	// from continuing an ordered stream against a source that can no longer
	// produce its terminal; the peer's next prompt opens a fresh incarnation.
	for _, session := range fenced {
		session.fenceSession()
	}

	cleanupErr := a.closeRuntimeGeneration(
		context.WithoutCancel(ctx), client, release, scratchRoot, scratchRelease, epoch,
	)

	a.mu.Lock()
	if cleanupErr != nil {
		a.runtimeClient = client
		a.runtimeNativeRelease = release
		a.runtimeScratchRoot = scratchRoot
		a.runtimeScratchRelease = scratchRelease
		a.runtimeEpoch = epoch
		a.runtimeDead = true
	}

	if latchRuntimeCleanup(cleanupErr) {
		a.runtimeCleanupErr = cleanupErr
	}

	if a.runtimeStarting == wait {
		a.runtimeStarting = nil

		close(wait)
	}
	a.mu.Unlock()

	return cleanupErr
}

func (a *Agent) resumeRuntimeSession(ctx context.Context, client codex.Client, session *session) (codex.Thread, error) {
	request := session.resumeRequest()

	thread, err := client.ResumeThread(ctx, request)
	if err != nil {
		return codex.Thread{}, codexThreadACPError(err, session.accountMetaSnapshot())
	}

	if thread.ID != "" && thread.ID != request.ThreadID {
		return codex.Thread{}, acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "Codex resumed a different thread during runtime recovery",
		})
	}

	return thread, nil
}

func (a *Agent) runtimeReadyCanary(parent context.Context, client codex.Client, session *session) error {
	return a.runtimeReadyCanaryWithConfig(parent, client, session, session.threadConfig())
}

func (a *Agent) runtimeReadyCanaryWithConfig(
	parent context.Context,
	client codex.Client,
	session *session,
	config map[string]any,
) error {
	if len(config) == 0 {
		return nil
	}

	session.lifecycleMu.Lock()
	brokerAlreadyAttached := session.nativeEventSource && !session.nativeEventStopping
	session.lifecycleMu.Unlock()

	if err := session.attachNativeEvents(); err != nil {
		return fmt.Errorf("attach runtime_ready thread broker: %w", err)
	}

	if !brokerAlreadyAttached && !a.negotiatedLifecycle().Present() {
		defer func() { _ = session.prepareNativeEventRebind() }()
	}

	var nonceBytes [16]byte
	if _, err := runtimeRandRead(nonceBytes[:]); err != nil {
		return fmt.Errorf("create runtime_ready nonce: %w", err)
	}

	nonce := hex.EncodeToString(nonceBytes[:])
	threadID := session.snapshot().codexThreadID

	deadlineCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), runtimeReadyDeadline)
	defer cancel()

	for attempt := 0; attempt < runtimeReadyAttempts; attempt++ {
		// Diagnostic only. It must never decide readiness.
		_, _ = client.MCPServerStatusList(deadlineCtx, threadID)

		canary, canaryErr := session.beginNativeCanary()
		if canaryErr != nil {
			return fmt.Errorf("begin runtime_ready turn: %w", canaryErr)
		}

		turn, runErr := client.RunTurn(deadlineCtx, codex.TurnStartRequest{
			ThreadID: threadID,
			Prompt: []codex.UserInput{{
				jsonFieldType: jsonFieldText,
				jsonFieldText: "Call the side-effect-free MCP tool named runtime_ready exactly once with nonce " + nonce + ". Do not call any other tool. Reply only after the tool returns.",
			}},
			ApprovalPolicy: runtimeReadyApprovalNever,
		})
		if runErr != nil {
			turnID, rejected := session.rejectNativeCanaryAck(canary, runErr)
			if turnID != "" {
				interruptCtx, interruptCancel := context.WithTimeout(context.WithoutCancel(parent), closeTimeout)
				rejected = errors.Join(rejected, client.CancelTurn(interruptCtx, threadID, turnID))

				interruptCancel()
			}

			session.endNativeCanary(canary)

			if deadlineCtx.Err() != nil {
				break
			}

			if turnID != "" {
				return fmt.Errorf("codex thread %s failed runtime_ready acknowledgement: %w", threadID, rejected)
			}

			continue
		}

		if bindErr := session.bindNativeCanary(canary, turn.ID); bindErr != nil {
			session.endNativeCanary(canary)

			return fmt.Errorf("bind runtime_ready turn: %w", bindErr)
		}

		ready := false
		terminal := false
		completed := false

		for !terminal {
			select {
			case event, open := <-canary.events:
				if !open {
					terminal = true

					continue
				}

				if runtimeReadyEvent(event, nonce) {
					ready = true
				}

				completed = event.Kind == codex.EventCompleted && event.StopReason == codex.StopReasonEndTurn
				terminal = event.Kind == codex.EventCompleted || event.Kind == codex.EventError
			case <-deadlineCtx.Done():
				interruptCtx, interruptCancel := context.WithTimeout(context.WithoutCancel(parent), closeTimeout)
				_ = client.CancelTurn(interruptCtx, threadID, turn.ID)

				interruptCancel()

				terminal = true
			}
		}

		session.endNativeCanary(canary)

		if ready && completed {
			return nil
		}

		if deadlineCtx.Err() != nil {
			break
		}
	}

	cause := deadlineCtx.Err()
	if cause == nil {
		cause = errors.New("marker was not observed")
	}

	return fmt.Errorf("codex thread %s failed runtime_ready after %d attempts: %w", threadID, runtimeReadyAttempts, cause)
}

func runtimeReadyEvent(event codex.Event, nonce string) bool {
	if event.Kind != codex.EventToolStarted && event.Kind != codex.EventToolDelta && event.Kind != codex.EventToolCompleted {
		return false
	}

	name := strings.ToLower(event.Tool.Title + " " + event.Tool.Kind)
	if !strings.Contains(name, "runtime_ready") {
		return false
	}

	if strings.Contains(event.Tool.Content, nonce) {
		return true
	}

	encoded, err := json.Marshal(event.Tool.Raw)

	return err == nil && strings.Contains(string(encoded), nonce)
}

func (a *Agent) closeSharedRuntime(ctx context.Context) error {
	closing := make(chan struct{})

	for {
		a.mu.Lock()
		if currentClosing := a.runtimeClosing; currentClosing != nil {
			a.mu.Unlock()
			<-currentClosing

			continue
		}

		a.runtimeClosing = closing
		starting := a.runtimeStarting
		cancelStart := a.runtimeStartCancel
		a.mu.Unlock()

		if cancelStart != nil {
			cancelStart()
		}

		if starting != nil {
			<-starting
		}

		break
	}

	a.mu.Lock()
	client := a.runtimeClient
	epoch := a.runtimeEpoch
	release := a.runtimeNativeRelease
	scratchRoot := a.runtimeScratchRoot
	scratchRelease := a.runtimeScratchRelease
	latchedCleanupErr := a.runtimeCleanupErr
	a.runtimeClient = nil
	a.runtimeNativeRelease = nil
	a.runtimeScratchRoot = ""
	a.runtimeScratchRelease = nil
	a.runtimeDead = true
	a.runtimeEpoch++
	a.mu.Unlock()

	cleanupErr := latchedCleanupErr
	if !retainRuntimeResources(cleanupErr) {
		cleanupErr = errors.Join(
			cleanupErr,
			a.closeRuntimeGeneration(ctx, client, release, scratchRoot, scratchRelease, epoch),
		)
	}

	a.mu.Lock()
	if cleanupErr != nil {
		a.runtimeClient = client
		a.runtimeNativeRelease = release
		a.runtimeScratchRoot = scratchRoot
		a.runtimeScratchRelease = scratchRelease
		a.runtimeEpoch = epoch
	}

	if latchRuntimeCleanup(cleanupErr) {
		a.runtimeCleanupErr = cleanupErr
	}

	if a.runtimeClosing == closing {
		a.runtimeClosing = nil

		close(closing)
	}
	a.mu.Unlock()

	return cleanupErr
}

func (a *Agent) closeRuntimeGeneration(
	ctx context.Context,
	client codex.Client,
	release func() error,
	scratchRoot string,
	scratchRelease func(),
	epoch uint64,
) error {
	var runtimeCloseErr error
	if client != nil {
		runtimeCloseErr = client.Close(ctx)
	}

	if runtimeCloseErr != nil {
		return runtimeCloseErr
	}

	residenceErr := errors.Join(
		a.releaseRetainedRuntimeThreads(client, epoch),
		a.reclaimRetiredResidences(ctx, epoch),
	)
	if residenceErr != nil {
		return residenceErr
	}

	return finalizeRuntimeResources(nil, release, scratchRoot, scratchRelease)
}
