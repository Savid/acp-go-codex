package codex

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/savid/acp-go-codex/internal/homelock"
)

const (
	supervisorModeEnv      = "ACP_GO_CODEX_INTERNAL_MODE"
	supervisorModeGuardian = "guardian"
	supervisorModeLiveness = "liveness"
	supervisorReadyPrefix  = "acp-go-codex supervisor-ready "
	// supervisorRefusedPrefix frames the terminal readiness line a supervisor
	// publishes in place of the readiness it never reached. Readiness travels
	// the child's stderr, so a refusal reason that is only printed there is
	// forwarded to a stream nobody correlates while the reader reports the same
	// wordless verdict whatever the cause. Framing the refusal alongside the
	// readiness lets it name itself. It changes nothing about the verdict — a
	// refusal is still a refusal — and a supervisor that dies without writing
	// the frame still closes the pipe with nothing to say.
	supervisorRefusedPrefix = "acp-go-codex supervisor-refused "
	supervisorConfigPrefix  = "supervisor-config-"
	supervisorQuiesceWindow = 5 * time.Second
	// supervisorStartProofWait bounds the wait for the liveness start proof. It
	// is not one of the budgets that must clear the standalone-claim maximum:
	// it is armed only once the guardian has exited or has already quarantined,
	// so it can never cancel a claim that is still walking /proc. It matches the
	// window the sibling packages give a trusted helper to publish a marker.
	supervisorStartProofWait = 5 * time.Second
	lifecycleRuntime         = "runtime"
	lifecycleDiscovery       = "discovery"
)

type supervisorConfig struct {
	NativePath          string            `json:"nativePath"`
	NativeArgs          []string          `json:"nativeArgs"`
	NativeEnv           []string          `json:"nativeEnv"`
	Home                string            `json:"home"`
	Scratch             string            `json:"scratch"`
	ScratchParent       string            `json:"scratchParent"`
	LifecycleKind       string            `json:"lifecycleKind"`
	DarwinBestEffort    bool              `json:"darwinBestEffort"`
	JobName             string            `json:"jobName,omitempty"`
	Started             string            `json:"started"`
	Completion          string            `json:"completion"`
	Quarantine          string            `json:"quarantine"`
	NativePIDFile       string            `json:"nativePidFile"`
	ProviderSnapshot    string            `json:"providerSnapshot"`
	FramedInput         bool              `json:"framedInput"`
	IsolationUID        uint32            `json:"isolationUid"`
	IsolationGID        uint32            `json:"isolationGid"`
	StandaloneOwnerID   string            `json:"standaloneOwnerId"`
	StandaloneStateRoot string            `json:"standaloneStateRoot"`
	IdentityLock        bool              `json:"identityLock"`
	AuthorityDomain     bool              `json:"authorityDomain"`
	StandaloneAuthority bool              `json:"standaloneAuthority"`
	OrdinaryExecution   bool              `json:"ordinaryExecution"`
	Isolation           *ProcessIsolation `json:"-"`
}

type supervisorReady struct {
	NativePID int `json:"nativePid"`
}

var supervisorExecutable = os.Executable
var supervisorExecCommand = exec.Command
var supervisorRandRead = rand.Read
var supervisorChmod = os.Chmod
var supervisorOpenFile = os.OpenFile
var supervisorCreateTemp = os.CreateTemp
var supervisorInheritedFile = os.NewFile
var supervisorPipe = os.Pipe
var supervisorEncodeConfig = func(writer io.Writer, config supervisorConfig) error {
	return json.NewEncoder(writer).Encode(config)
}
var supervisorNewGuardianContainment = newGuardianContainment
var supervisorOpenLivenessContainment = openLivenessContainment
var supervisorGuardianQuiesce = func(containment *guardianContainment, nativePID int, timeout time.Duration) error {
	return containment.Quiesce(nativePID, timeout)
}
var supervisorLivenessQuiesce = func(containment *livenessContainment, nativePID int, timeout time.Duration) error {
	return containment.Quiesce(nativePID, timeout)
}
var supervisorQuarantineRetry func(*livenessContainment) error
var supervisorGuardianQuarantineRetry func(*guardianContainment) error
var supervisorDescendantCount = func(containment *livenessContainment) (int, bool) {
	return containment.DescendantCount()
}
var supervisorInput io.Reader = os.Stdin
var supervisorOutput io.Writer = os.Stdout
var supervisorError io.Writer = os.Stderr
var supervisorExit = os.Exit
var supervisorWriteConfig = writeSupervisorConfig

// The identity seams below carry no initializer: every platform installs its
// own through configureSupervisorPlatform, because the answers they stand for
// are what differs between platforms. Linux binds the real agent identity
// registry; everywhere else the supervisor holds no identity to bind.
var supervisorMarkerRoot func(config supervisorConfig) (string, error)
var supervisorAcquireIdentityAuthority func(
	uint32, uint32, string, string, io.Reader,
) (supervisorIdentityLock, supervisorIdentityLock, error)
var supervisorVerifyTrustedIdentity func(uint32) error
var supervisorAdoptIdentityLock func(uint32) (supervisorIdentityLock, error)
var supervisorAdoptAuthorityDomain func(uint32) (supervisorIdentityLock, error)
var supervisorValidateAdoptedAuthority func(supervisorConfig) error
var supervisorOpenIdentityPlaceholder = func() (*os.File, error) { return os.Open(os.DevNull) }
var supervisorGuardianPeer *os.File
var supervisorValidateGuardianPeer func(*os.File, <-chan struct{}) error
var supervisorNotifySignals = signal.Notify
var supervisorStopSignals = signal.Stop

type supervisorIdentityLock interface {
	io.Closer
	InheritedFile() *os.File
}

type noopSupervisorIdentityLock struct{}

func (noopSupervisorIdentityLock) Close() error            { return nil }
func (noopSupervisorIdentityLock) InheritedFile() *os.File { return nil }

type supervisorProof struct {
	abandoned           chan struct{}
	abandonOnce         sync.Once
	started             string
	completion          string
	quarantine          string
	nativePIDFile       string
	providerSnapshot    string
	startupWait         time.Duration
	completionWait      time.Duration
	inherited           []*os.File
	ordinaryHomeLock    *homelock.Lock
	homeLockReleaseOnce sync.Once
	homeLockReleaseErr  error
}

func (p *supervisorProof) closeInherited() error {
	if p == nil {
		return nil
	}

	var result error
	for _, file := range p.inherited {
		result = errors.Join(result, file.Close())
	}

	p.inherited = nil

	return result
}

func (p *supervisorProof) releaseOrdinaryHomeLock() error {
	if p == nil || p.ordinaryHomeLock == nil {
		return nil
	}

	p.homeLockReleaseOnce.Do(func() { p.homeLockReleaseErr = p.ordinaryHomeLock.Release() })

	return p.homeLockReleaseErr
}

// init turns the embedding command itself into either member of the
// supervisor pair. No separate helper binary is installed, and these modes
// run before the host's main package can open any adapter state.
func init() {
	configureSupervisorPlatform()
	supervisorBootstrap()
}

// holdSupervisorHangup keeps SIGHUP from terminating a trusted supervisor
// role. When the guardian dies, the liveness supervisor reparents away and its
// process group becomes orphaned; the kernel then sends SIGHUP and SIGCONT to
// every member of a newly orphaned group that still holds a stopped job. At the
// default disposition that hangup kills the survivor in the middle of
// quiescence, before it can prove the tree gone or publish its completion.
//
// The disposition is a drained handler rather than SIG_IGN on purpose: SIG_IGN
// survives execve and would leak into the native child, while a Go signal
// handler is reset to the default for the executed image. Nothing reads the
// hangup as intent — quiescence stays driven only by the authenticated guardian
// peer descriptor and the guardian control EOF — so a spurious orphan hangup
// can never be mistaken for an operator stop request. The returned release
// retires both the notification and the drain.
func holdSupervisorHangup() func() {
	hangups := make(chan os.Signal, 1)

	supervisorNotifySignals(hangups, syscall.SIGHUP)

	go drainSupervisorHangups(hangups)

	return func() {
		supervisorStopSignals(hangups)
		close(hangups)
	}
}

func drainSupervisorHangups(hangups <-chan os.Signal) {
	for {
		_, held := <-hangups
		if !held {
			return
		}
	}
}

func supervisorBootstrap() {
	mode := os.Getenv(supervisorModeEnv)
	if mode == "" {
		return
	}

	// The hold is scoped to the role's run and released when that run ends.
	releaseHangupHold := holdSupervisorHangup()
	defer releaseHangupHold()

	var err error

	configFile := supervisorInheritedFile(3, "acp-go-codex-supervisor-config")
	if err == nil && configFile == nil {
		err = errors.New("process supervisor inherited config descriptor is unavailable")
	}

	if err == nil {
		err = closeInheritedOnExec(configFile)
	}

	var guardianPeer *os.File
	if err == nil && mode == supervisorModeLiveness {
		guardianPeer = supervisorInheritedFile(6, "acp-go-codex-supervisor-guardian-peer")
		if guardianPeer == nil {
			err = errors.New("liveness supervisor inherited guardian peer descriptor is unavailable")
		} else {
			err = closeInheritedOnExec(guardianPeer)
		}
	}

	if err == nil {
		supervisorGuardianPeer = guardianPeer
		err = runSupervisor(mode, configFile)
	}

	if configFile != nil {
		_ = configFile.Close()
	}

	if guardianPeer != nil {
		_ = guardianPeer.Close()
	}

	if err != nil {
		_, _ = fmt.Fprintln(supervisorError, supervisorRefusedPrefix+err.Error())

		supervisorExit(1)

		return
	}

	supervisorExit(0)
}

func runSupervisor(mode string, configInput io.Reader) (runErr error) {
	config, err := readSupervisorConfig(configInput)
	if err != nil {
		return err
	}

	if config.IdentityLock != config.AuthorityDomain {
		return errors.New("supervisor UID lock and authority domain are inconsistent")
	}

	if err := validateSupervisorIdentityDisposition(config); err != nil {
		return err
	}

	if mode == supervisorModeLiveness && config.IdentityLock {
		lock, lockErr := supervisorAdoptIdentityLock(config.IsolationUID)
		if lockErr != nil {
			return lockErr
		}
		defer func() { runErr = errors.Join(runErr, lock.Close()) }()

		domain, domainErr := supervisorAdoptAuthorityDomain(config.IsolationUID)
		if domainErr != nil {
			return domainErr
		}

		defer func() { runErr = errors.Join(runErr, domain.Close()) }()

		if validationErr := supervisorValidateAdoptedAuthority(config); validationErr != nil {
			return validationErr
		}
	}

	switch mode {
	case supervisorModeGuardian:
		return runGuardian(config)
	case supervisorModeLiveness:
		return runLiveness(config)
	default:
		return fmt.Errorf("unknown internal mode %q", mode)
	}
}

func readSupervisorConfig(reader io.Reader) (supervisorConfig, error) {
	if reader == nil {
		return supervisorConfig{}, errors.New("missing private supervisor config descriptor")
	}

	var config supervisorConfig
	if err := json.NewDecoder(io.LimitReader(reader, 8<<20)).Decode(&config); err != nil {
		return supervisorConfig{}, fmt.Errorf("decode private supervisor config: %w", err)
	}

	if config.NativePath == "" || config.Home == "" || config.Scratch == "" ||
		(!config.OrdinaryExecution && (config.IsolationUID == 0 || config.IsolationGID == 0)) {
		return supervisorConfig{}, errors.New("private supervisor config is incomplete")
	}

	return config, nil
}

func writeSupervisorConfig(root string, config supervisorConfig) (*os.File, error) {
	if root == "" {
		return nil, errors.New("private supervisor scratch root is required")
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create private supervisor scratch root: %w", err)
	}

	if err := supervisorChmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("chmod private supervisor scratch root: %w", err)
	}

	file, err := supervisorCreateTemp(root, supervisorConfigPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("create private supervisor config: %w", err)
	}

	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)

		return nil, fmt.Errorf("secure private supervisor config: %w", err)
	}

	encodeErr := supervisorEncodeConfig(file, config)
	if encodeErr != nil {
		_ = file.Close()
		_ = os.Remove(path)

		return nil, fmt.Errorf("write private supervisor config: %w", encodeErr)
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		_ = os.Remove(path)

		return nil, fmt.Errorf("rewind private supervisor config: %w", err)
	}

	if err := os.Remove(path); err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("unlink private supervisor config: %w", err)
	}

	return file, nil
}

func supervisorCommand(ctx context.Context, config supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	if config.DarwinBestEffort && config.Isolation != nil {
		return nil, nil, errors.New("darwin best-effort containment and explicit process isolation are mutually exclusive")
	}

	if config.DarwinBestEffort && processIsolationGOOS != processIsolationDarwin {
		return nil, nil, errors.New("darwin best-effort containment is supported only on darwin")
	}

	if ordinaryProcessBackend(config.Isolation, config.DarwinBestEffort) {
		homeLock, err := homelock.Acquire(config.Home)
		if err != nil {
			return nil, nil, err
		}

		cmd := execCommandContext(ctx, config.NativePath, config.NativeArgs...)
		cmd.Env = append([]string(nil), config.NativeEnv...)

		return cmd, &supervisorProof{ordinaryHomeLock: homeLock}, nil
	}

	if config.Isolation == nil {
		uid, gid, err := currentProcessIdentity()
		if err != nil {
			return nil, nil, err
		}

		config.IsolationUID = uid
		config.IsolationGID = gid
		config.OrdinaryExecution = true
	} else {
		if err := validateProcessIsolation(config.Isolation); err != nil {
			return nil, nil, err
		}

		if (config.Isolation.IdentityLock == nil) != (config.Isolation.AuthorityDomain == nil) {
			return nil, nil, errors.New("codex supervisor requires the UID lock and authority domain together")
		}

		if err := supervisorVerifyTrustedIdentity(config.Isolation.UID); err != nil {
			return nil, nil, err
		}

		config.IsolationUID = config.Isolation.UID
		config.IsolationGID = config.Isolation.GID
		config.StandaloneOwnerID = config.Isolation.StandaloneOwnerID
		config.StandaloneStateRoot = config.Isolation.StandaloneStateRoot
		config.IdentityLock = config.Isolation.IdentityLock != nil
		config.AuthorityDomain = config.Isolation.AuthorityDomain != nil
		config.StandaloneAuthority = config.Isolation.IdentityLock == nil
	}

	if config.ScratchParent == "" && config.Scratch != "" {
		config.ScratchParent = filepath.Dir(config.Scratch)
	}

	if config.LifecycleKind == "" {
		config.LifecycleKind = lifecycleRuntime
	}

	markerNonce, err := supervisorNonce()
	if err != nil {
		return nil, nil, err
	}

	markerRoot, err := supervisorMarkerRoot(config)
	if err != nil {
		return nil, nil, err
	}

	config.Started = filepath.Join(markerRoot, "supervisor-started-"+markerNonce)
	config.Completion = filepath.Join(markerRoot, "supervisor-complete-"+markerNonce)
	config.Quarantine = filepath.Join(markerRoot, "supervisor-quarantine-"+markerNonce)
	config.NativePIDFile = filepath.Join(markerRoot, "supervisor-native-pid-"+markerNonce)

	if !config.OrdinaryExecution {
		config.ProviderSnapshot = filepath.Join(markerRoot, "supervisor-provider-snapshot-"+markerNonce)
	}

	configFile, err := supervisorWriteConfig(config.Scratch, config)
	if err != nil {
		return nil, nil, err
	}

	executable, err := supervisorExecutable()
	if err != nil {
		_ = configFile.Close()

		return nil, nil, fmt.Errorf("resolve embedded runtime supervisor: %w", err)
	}

	helperEnv := []string{supervisorModeEnv + "=" + supervisorModeGuardian}

	executable, err = resolveProcessExecutable(executable, helperEnv)
	if err != nil {
		_ = configFile.Close()

		return nil, nil, fmt.Errorf("resolve embedded runtime supervisor through process policy: %w", err)
	}

	cmd := execCommandContext(ctx, executable)
	cmd.Cancel = nil
	cmd.WaitDelay = 0
	cmd.Env = helperEnv
	cmd.Dir = "/"
	cmd.ExtraFiles = []*os.File{configFile}
	inherited := []*os.File{configFile}

	if config.Isolation != nil && config.Isolation.IdentityLock != nil {
		identityLock, duplicateErr := config.Isolation.IdentityLock.Duplicate()
		if duplicateErr != nil {
			_ = configFile.Close()

			return nil, nil, fmt.Errorf("duplicate Codex agent identity lock: %w", duplicateErr)
		}

		cmd.ExtraFiles = append(cmd.ExtraFiles, identityLock)
		inherited = append(inherited, identityLock)

		authorityDomain, duplicateErr := config.Isolation.AuthorityDomain.Duplicate()
		if duplicateErr != nil {
			_ = identityLock.Close()
			_ = configFile.Close()

			return nil, nil, fmt.Errorf("duplicate Codex agent authority domain: %w", duplicateErr)
		}

		cmd.ExtraFiles = append(cmd.ExtraFiles, authorityDomain)
		inherited = append(inherited, authorityDomain)
	}

	return cmd, &supervisorProof{
		abandoned:        make(chan struct{}),
		started:          config.Started,
		completion:       config.Completion,
		quarantine:       config.Quarantine,
		nativePIDFile:    config.NativePIDFile,
		providerSnapshot: config.ProviderSnapshot,
		inherited:        inherited,
	}, nil
}

func (p *supervisorProof) readProviderSnapshot() (int, bool) {
	if p == nil || p.providerSnapshot == "" {
		return 0, false
	}

	raw, err := os.ReadFile(p.providerSnapshot)
	if err != nil {
		return 0, false
	}

	var count int
	if _, err := fmt.Sscanf(string(raw), "%d", &count); err != nil || count < 0 {
		return 0, false
	}

	return count, true
}

func supervisorNonce() (string, error) {
	var nonce [16]byte
	if _, err := supervisorRandRead(nonce[:]); err != nil {
		return "", fmt.Errorf("create private supervisor marker nonce: %w", err)
	}

	return hex.EncodeToString(nonce[:]), nil
}

// awaitCompletion closes the guardian-SIGKILL gap for a still-running adapter.
// A liveness supervisor publishes completion only after it has proved its
// selected containment boundary complete. Absence of both markers is not a
// no-start proof: an independently grouped liveness process may still start.
func (p *supervisorProof) awaitCompletion() error {
	if p == nil {
		return nil
	}

	if p.started == "" && p.completion == "" {
		return p.releaseOrdinaryHomeLock()
	}

	startupWait := p.startupWait
	if startupWait <= 0 {
		startupWait = supervisorStartProofWait
	}

	startupDeadline := time.Now().Add(startupWait)

	for {
		if _, err := os.Stat(p.completion); err == nil {
			p.removeTerminalMarkers()

			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: stat liveness completion proof: %v", ErrProcessContainmentIncomplete, err)
		}

		if quarantined, err := supervisorMarkerExists(p.quarantine); err != nil {
			return fmt.Errorf("%w: stat liveness quarantine proof: %v", ErrProcessContainmentIncomplete, err)
		} else if quarantined {
			return fmt.Errorf("%w: liveness supervisor retained the identity lock while quarantining descendants", ErrProcessContainmentIncomplete)
		}

		if _, err := os.Stat(p.started); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: stat liveness start proof: %v", ErrProcessContainmentIncomplete, err)
		}

		if time.Now().After(startupDeadline) {
			return fmt.Errorf(
				"%w: liveness supervisor published neither start nor completion within %s",
				ErrProcessContainmentIncomplete,
				startupWait,
			)
		}

		if err := p.pauseUntilAbandoned(); err != nil {
			return err
		}
	}

	completionWait := p.completionWait
	if completionWait <= 0 {
		completionWait = supervisorQuiesceWindow + time.Second
	}

	completionDeadline := time.Now().Add(completionWait)

	for {
		if _, err := os.Stat(p.completion); err == nil {
			p.removeTerminalMarkers()

			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: stat liveness completion proof: %v", ErrProcessContainmentIncomplete, err)
		}

		if quarantined, err := supervisorMarkerExists(p.quarantine); err != nil {
			return fmt.Errorf("%w: stat liveness quarantine proof: %v", ErrProcessContainmentIncomplete, err)
		} else if quarantined {
			return fmt.Errorf("%w: liveness supervisor retained the identity lock while quarantining descendants", ErrProcessContainmentIncomplete)
		}

		if time.Now().After(completionDeadline) {
			return fmt.Errorf("%w: liveness supervisor started but did not publish completion within %s", ErrProcessContainmentIncomplete, completionWait)
		}

		if err := p.pauseUntilAbandoned(); err != nil {
			return err
		}
	}
}

// pauseUntilAbandoned spaces out the marker polls and gives up the moment the
// caller abandons the wait. The trusted supervisor keeps both capabilities
// until the kernel proves ECHILD either way, so abandoning this poll retires
// the waiting goroutine without retiring any proof.
func (p *supervisorProof) pauseUntilAbandoned() error {
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()

	select {
	case <-p.abandoned:
		return fmt.Errorf("%w: containment proof wait abandoned at teardown", ErrProcessContainmentIncomplete)
	case <-timer.C:
		return nil
	}
}

// abandon retires an awaitCompletion that outlived its caller. A nil channel
// never fires, so a proof that was never armed keeps polling to its deadline.
func (p *supervisorProof) abandon() {
	if p == nil || p.abandoned == nil {
		return
	}

	p.abandonOnce.Do(func() { close(p.abandoned) })
}

func (p *supervisorProof) removeTerminalMarkers() {
	if p == nil {
		return
	}

	removeSupervisorMarkers(p.started, p.completion, p.quarantine, p.nativePIDFile, p.providerSnapshot)
}

func (p *supervisorProof) quarantineDetected() (bool, error) {
	if p == nil {
		return false, nil
	}

	return supervisorMarkerExists(p.quarantine)
}

func (p *supervisorProof) awaitCommand(waitDone <-chan error) (error, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case waitErr := <-waitDone:
			return waitErr, p.awaitCompletion()
		case <-ticker.C:
			quarantined, err := p.quarantineDetected()
			if err != nil {
				return nil, fmt.Errorf("%w: stat liveness quarantine proof: %v", ErrProcessContainmentIncomplete, err)
			}

			if quarantined {
				return nil, p.awaitCompletion()
			}
		}
	}
}

//nolint:gocyclo // Guardian startup coordinates authority, liveness, native I/O, and containment proof ordering.
func runGuardian(config supervisorConfig) (runErr error) {
	input, output, errorOutput := supervisorInput, supervisorOutput, supervisorError

	var (
		err             error
		identityLock    supervisorIdentityLock = noopSupervisorIdentityLock{}
		authorityDomain supervisorIdentityLock = noopSupervisorIdentityLock{}
	)

	if !config.OrdinaryExecution && config.IsolationUID != 0 {
		if verifyErr := supervisorVerifyTrustedIdentity(config.IsolationUID); verifyErr != nil {
			return verifyErr
		}

		if config.IdentityLock {
			identityLock, err = supervisorAdoptIdentityLock(config.IsolationUID)
			if err != nil {
				return err
			}

			authorityDomain, err = supervisorAdoptAuthorityDomain(config.IsolationUID)
			if err != nil {
				return err
			}

			if validationErr := supervisorValidateAdoptedAuthority(config); validationErr != nil {
				return validationErr
			}
		} else {
			identityLock, authorityDomain, err = supervisorAcquireIdentityAuthority(
				config.IsolationUID,
				config.IsolationGID,
				config.StandaloneOwnerID,
				config.StandaloneStateRoot,
				input,
			)
			if err != nil {
				return err
			}
		}
	}

	defer func() { runErr = errors.Join(runErr, identityLock.Close(), authorityDomain.Close()) }()

	claim, err := homelock.AcquireClaim(config.Home)
	if err != nil {
		return err
	}
	defer func() { _ = claim.Release() }()

	containment, err := supervisorNewGuardianContainment(config)
	if err != nil {
		return err
	}
	defer func() { _ = containment.Close() }()

	config.JobName = containment.Name()

	lockFile := identityLock.InheritedFile()
	domainFile := authorityDomain.InheritedFile()
	config.IdentityLock = lockFile != nil
	config.AuthorityDomain = domainFile != nil

	peerRead, peerWrite, err := supervisorPipe()
	if err != nil {
		return fmt.Errorf("open liveness guardian peer: %w", err)
	}
	defer peerRead.Close()
	defer peerWrite.Close()

	identityExtra := lockFile
	if identityExtra == nil {
		identityExtra, err = supervisorOpenIdentityPlaceholder()
		if err != nil {
			return fmt.Errorf("open liveness identity placeholder: %w", err)
		}
		defer identityExtra.Close()
	}

	domainExtra := domainFile
	if domainExtra == nil {
		domainExtra, err = supervisorOpenIdentityPlaceholder()
		if err != nil {
			return fmt.Errorf("open liveness authority placeholder: %w", err)
		}
		defer domainExtra.Close()
	}

	livenessConfig, err := supervisorWriteConfig(config.Scratch, config)
	if err != nil {
		return err
	}

	executable, err := supervisorExecutable()
	if err != nil {
		_ = livenessConfig.Close()

		return fmt.Errorf("resolve liveness supervisor executable: %w", err)
	}

	executable, err = resolveProcessExecutable(executable, config.NativeEnv)
	if err != nil {
		_ = livenessConfig.Close()

		return fmt.Errorf("resolve liveness supervisor executable through process policy: %w", err)
	}

	cmd := supervisorExecCommand(executable)
	cmd.Env = []string{supervisorModeEnv + "=" + supervisorModeLiveness}
	cmd.Dir = "/"
	cmd.ExtraFiles = []*os.File{livenessConfig, identityExtra, domainExtra, peerRead}

	configureIndependentSupervisor(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = livenessConfig.Close()

		return fmt.Errorf("open liveness control input: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = livenessConfig.Close()

		return fmt.Errorf("open liveness control output: %w", err)
	}

	cmd.Stdout = output

	if err := startIndependentSupervisor(cmd); err != nil {
		_ = stdin.Close()
		_ = stderr.Close()
		_ = livenessConfig.Close()

		return fmt.Errorf("start liveness supervisor: %w", err)
	}

	_ = livenessConfig.Close()
	_ = peerRead.Close()

	livenessDone := make(chan error, 1)
	go func() { livenessDone <- cmd.Wait() }()

	control := bufio.NewReader(stderr)
	readyLine, readyErr := control.ReadString('\n')

	ready, parseErr := parseSupervisorReady(readyLine)
	if readyErr != nil || parseErr != nil {
		_ = stdin.Close()
		_ = terminateIndependentSupervisor(cmd)

		waitErr, quarantined, terminalErr := awaitLivenessTerminal(livenessDone, config.Quarantine)
		if quarantined || terminalErr != nil {
			_ = stderr.Close()
			_, _ = io.Copy(errorOutput, control)

			return errors.Join(terminalErr, finishQuarantinedLiveness(livenessDone, config))
		}

		_, _ = io.Copy(errorOutput, control)

		var proofErr error

		if _, completeErr := os.Stat(config.Completion); completeErr != nil {
			if !errors.Is(completeErr, os.ErrNotExist) {
				return errors.Join(waitErr, completeErr)
			}

			if _, startedErr := os.Stat(config.Started); startedErr == nil {
				nativePID, _ := readNativePID(config.NativePIDFile)
				if markerErr := writeGuardianQuarantineMarker(config); markerErr != nil {
					return errors.Join(waitErr, markerErr)
				}

				proofErr = awaitQuiescence(func() error {
					return supervisorGuardianQuiesce(containment, nativePID, supervisorQuiesceWindow)
				})
				proofErr = completeOrQuarantineGuardian(config, containment, proofErr)
			} else if !errors.Is(startedErr, os.ErrNotExist) {
				return errors.Join(waitErr, startedErr)
			}
		}

		return errors.Join(fmt.Errorf("liveness supervisor failed before readiness: %w", errors.Join(readyErr, parseErr)), waitErr, proofErr)
	}

	// The guardian forwards its control input verbatim. Framing is the caller's
	// decision and the liveness supervisor's to undo: a zero-length frame ends
	// native stdin, while loss of this pipe means the caller died and starts
	// containment. Reframing here would erase that distinction.
	inputDone := make(chan struct{}, 1)
	go copySupervisorStream(stdin, input, inputDone)

	streamDone := make(chan struct{}, 1)
	go copySupervisorStream(errorOutput, control, streamDone)

	waitErr, quarantined, terminalErr := awaitLivenessTerminal(livenessDone, config.Quarantine)

	_ = stdin.Close()
	if quarantined || terminalErr != nil {
		_ = stderr.Close()

		<-streamDone

		return errors.Join(terminalErr, finishQuarantinedLiveness(livenessDone, config))
	}

	<-streamDone

	var proofErr error

	if _, completionErr := os.Stat(config.Completion); errors.Is(completionErr, os.ErrNotExist) {
		if markerErr := writeGuardianQuarantineMarker(config); markerErr != nil {
			return errors.Join(waitErr, markerErr)
		}

		proofErr = awaitQuiescence(func() error {
			return supervisorGuardianQuiesce(containment, ready.NativePID, supervisorQuiesceWindow)
		})
		proofErr = completeOrQuarantineGuardian(config, containment, proofErr)
	} else if completionErr != nil {
		proofErr = fmt.Errorf("stat liveness completion: %w", completionErr)
	}

	if proofErr != nil {
		return errors.Join(waitErr, proofErr)
	}

	if waitErr != nil {
		return fmt.Errorf("liveness supervisor exited: %w", waitErr)
	}

	return nil
}

func runLiveness(config supervisorConfig) error {
	input, output, errorOutput := supervisorInput, supervisorOutput, supervisorError

	if !config.OrdinaryExecution && config.IsolationUID != 0 {
		if verifyErr := supervisorVerifyTrustedIdentity(config.IsolationUID); verifyErr != nil {
			return verifyErr
		}
	}

	if err := writeSupervisorMarker(config.Started); err != nil {
		return err
	}

	liveness, err := homelock.AcquireLiveness(config.Home)
	if err != nil {
		return err
	}
	defer func() { _ = liveness.Release() }()

	containment, err := supervisorOpenLivenessContainment(config)
	if err != nil {
		return err
	}
	defer func() { _ = containment.Close() }()

	guardianDone := make(chan struct{})

	if supervisorGuardianPeer != nil {
		go func() {
			_, _ = io.Copy(io.Discard, supervisorGuardianPeer)

			close(guardianDone)
		}()
	}

	// #nosec G204 -- native path and arguments come from adapter construction,
	// cross a mode-0600 file in the private runtime scratch root, and are not
	// accepted from ACP requests.
	cmd := supervisorExecCommand(config.NativePath, config.NativeArgs...)
	cmd.Env = config.NativeEnv

	if !config.OrdinaryExecution && (config.IsolationUID != 0 || config.IsolationGID != 0) {
		if credentialErr := applyProcessCredential(cmd, supervisedNativeIsolation(config)); credentialErr != nil {
			return fmt.Errorf("apply supervised Codex native identity: %w", credentialErr)
		}
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open native data input: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()

		return fmt.Errorf("open native error output: %w", err)
	}

	cmd.Stdout = output

	if peerErr := supervisorValidateGuardianPeer(supervisorGuardianPeer, guardianDone); peerErr != nil {
		_ = stdin.Close()
		_ = stderr.Close()
		proofErr := awaitQuiescence(func() error {
			return supervisorLivenessQuiesce(containment, 0, supervisorQuiesceWindow)
		})
		proofErr = completeOrQuarantineLiveness(config, containment, proofErr)

		return errors.Join(peerErr, proofErr)
	}

	var finalPeerErr error

	containment.beforeStart = func() error {
		finalPeerErr = supervisorValidateGuardianPeer(supervisorGuardianPeer, guardianDone)

		return finalPeerErr
	}

	if startErr := containment.Start(cmd); startErr != nil {
		_ = stdin.Close()
		_ = stderr.Close()

		if finalPeerErr != nil {
			proofErr := awaitQuiescence(func() error {
				return supervisorLivenessQuiesce(containment, 0, supervisorQuiesceWindow)
			})
			proofErr = completeOrQuarantineLiveness(config, containment, proofErr)

			return errors.Join(finalPeerErr, proofErr)
		}

		return fmt.Errorf("start contained native root: %w", startErr)
	}

	waitDone := containment.Wait()

	publishProviderProcessSnapshot(config.ProviderSnapshot, containment)

	if pidErr := writeNativePID(config.NativePIDFile, cmd.Process.Pid); pidErr != nil {
		proofErr := awaitQuiescence(func() error {
			return supervisorLivenessQuiesce(containment, cmd.Process.Pid, supervisorQuiesceWindow)
		})

		<-waitDone

		proofErr = completeOrQuarantineLiveness(config, containment, proofErr)

		return errors.Join(pidErr, proofErr)
	}

	// This fixed, integer-only object has no JSON encoding failure mode.
	ready := fmt.Appendf(nil, "{\"nativePid\":%d}", cmd.Process.Pid)

	if _, err := fmt.Fprintln(errorOutput, supervisorReadyPrefix+string(ready)); err != nil {
		proofErr := awaitQuiescence(func() error {
			return supervisorLivenessQuiesce(containment, cmd.Process.Pid, supervisorQuiesceWindow)
		})

		<-waitDone

		proofErr = completeOrQuarantineLiveness(config, containment, proofErr)

		return errors.Join(fmt.Errorf("publish supervisor readiness: %w", err), proofErr)
	}

	controlDone := make(chan struct{})

	if config.FramedInput {
		go copyFramedSupervisorInput(stdin, input, controlDone)
	} else {
		go func() {
			_, _ = io.Copy(stdin, input)
			_ = stdin.Close()

			close(controlDone)
		}()
	}

	streamDone := make(chan struct{}, 1)

	go func() {
		_, _ = io.Copy(errorOutput, stderr)

		streamDone <- struct{}{}
	}()

	select {
	case waitErr := <-waitDone:
		<-streamDone

		proofErr := awaitQuiescence(func() error {
			// Each backend uses the containment boundary captured at launch.
			// Darwin's boundary is the original process group and therefore keeps
			// the documented best-effort numeric-PGID reuse risk after this wait.
			return supervisorLivenessQuiesce(containment, 0, supervisorQuiesceWindow)
		})

		proofErr = completeOrQuarantineLiveness(config, containment, proofErr)
		if proofErr != nil {
			return errors.Join(waitErr, proofErr)
		}

		if waitErr != nil {
			return fmt.Errorf("native root exited: %w", waitErr)
		}

		return nil
	case <-controlDone:
		proofErr := awaitQuiescence(func() error {
			return supervisorLivenessQuiesce(containment, cmd.Process.Pid, supervisorQuiesceWindow)
		})

		<-waitDone
		<-streamDone

		proofErr = completeOrQuarantineLiveness(config, containment, proofErr)

		return proofErr
	}
}

func supervisedNativeIsolation(config supervisorConfig) *ProcessIsolation {
	isolation := &ProcessIsolation{
		UID:             config.IsolationUID,
		GID:             config.IsolationGID,
		BaseEnvironment: environmentMap(config.NativeEnv),
	}
	if config.IdentityLock && config.AuthorityDomain {
		isolation.identityAuthorityAdopted = true

		return isolation
	}

	isolation.StandaloneOwnerID = config.StandaloneOwnerID
	isolation.StandaloneStateRoot = config.StandaloneStateRoot

	return isolation
}

func completeOrQuarantineLiveness(config supervisorConfig, containment *livenessContainment, proofErr error) error {
	if proofErr == nil {
		return writeSupervisorMarker(config.Completion)
	}

	if supervisorQuarantineRetry == nil || config.Quarantine == "" {
		return proofErr
	}

	if err := writeSupervisorMarker(config.Quarantine); err != nil {
		return errors.Join(proofErr, err)
	}

	closeSupervisorQuarantineStreams()

	retryErr := supervisorQuarantineRetry(containment)

	removeSupervisorMarkers(config.Started, config.Completion, config.NativePIDFile, config.ProviderSnapshot)

	return errors.Join(proofErr, retryErr)
}

func writeGuardianQuarantineMarker(config supervisorConfig) error {
	if config.Quarantine == "" {
		return nil
	}

	return writeSupervisorMarker(config.Quarantine)
}

func completeOrQuarantineGuardian(config supervisorConfig, containment *guardianContainment, proofErr error) error {
	if proofErr == nil {
		return writeSupervisorMarker(config.Completion)
	}

	if supervisorGuardianQuarantineRetry == nil || config.Quarantine == "" {
		return proofErr
	}

	if err := writeSupervisorMarker(config.Quarantine); err != nil {
		return errors.Join(proofErr, err)
	}

	closeSupervisorQuarantineStreams()

	retryErr := supervisorGuardianQuarantineRetry(containment)

	removeSupervisorMarkers(config.Started, config.Completion, config.Quarantine, config.NativePIDFile, config.ProviderSnapshot)

	return errors.Join(proofErr, retryErr)
}

func finishQuarantinedLiveness(done <-chan error, config supervisorConfig) error {
	waitErr := <-done

	removeSupervisorMarkers(config.Started, config.Completion, config.Quarantine, config.NativePIDFile, config.ProviderSnapshot)

	return errors.Join(waitErr, fmt.Errorf("%w: liveness supervisor completed containment quarantine", ErrProcessContainmentIncomplete))
}

func awaitLivenessTerminal(done <-chan error, quarantine string) (error, bool, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case err := <-done:
			return err, false, nil
		case <-ticker.C:
			present, err := supervisorMarkerExists(quarantine)
			if err != nil {
				return nil, false, fmt.Errorf("stat liveness quarantine proof: %w", err)
			}

			if present {
				return nil, true, nil
			}
		}
	}
}

func supervisorMarkerExists(path string) (bool, error) {
	if path == "" {
		return false, nil
	}

	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	return err == nil, err
}

func removeSupervisorMarkers(paths ...string) {
	for _, path := range paths {
		if path != "" {
			_ = os.Remove(path)
		}
	}
}

func closeSupervisorQuarantineStreams() {
	for _, stream := range []any{supervisorInput, supervisorOutput, supervisorError} {
		if file, ok := stream.(*os.File); ok {
			_ = file.Close()
		}
	}
}

func publishProviderProcessSnapshot(path string, containment *livenessContainment) {
	if path == "" {
		return
	}

	if count, available := supervisorDescendantCount(containment); available {
		// Failure to persist the optional observation makes the inventory
		// unavailable; it must not turn into a fabricated zero or fail launch.
		_ = os.WriteFile(path, []byte(fmt.Sprintf("%d\n", count)), 0o600)
	}
}

func awaitQuiescence(probe func() error) error {
	err := probe()
	if err == nil || errors.Is(err, ErrProcessContainmentIncomplete) {
		return err
	}

	return fmt.Errorf("%w: %v", ErrProcessContainmentIncomplete, err)
}

func writeSupervisorMarker(path string) error {
	if path == "" {
		return errors.New("private supervisor proof path is required")
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("write private supervisor proof: %w", err)
	}

	return file.Close()
}

func writeNativePID(path string, pid int) error {
	if path == "" || pid <= 0 {
		return errors.New("private native PID proof is invalid")
	}

	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", pid)), 0o600); err != nil {
		return fmt.Errorf("write private native PID proof: %w", err)
	}

	return nil
}

func readNativePID(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	var pid int
	if _, err := fmt.Sscanf(string(raw), "%d", &pid); err != nil || pid <= 0 {
		return 0, errors.New("private native PID proof is invalid")
	}

	return pid, nil
}

func parseSupervisorReady(line string) (supervisorReady, error) {
	if reason, refused := strings.CutPrefix(strings.TrimSpace(line), supervisorRefusedPrefix); refused {
		return supervisorReady{}, fmt.Errorf("supervisor refused to start: %s", reason)
	}

	if !strings.HasPrefix(line, supervisorReadyPrefix) {
		return supervisorReady{}, fmt.Errorf("invalid readiness frame %q", strings.TrimSpace(line))
	}

	var ready supervisorReady
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, supervisorReadyPrefix))), &ready); err != nil {
		return supervisorReady{}, fmt.Errorf("decode readiness frame: %w", err)
	}

	if ready.NativePID <= 0 {
		return supervisorReady{}, errors.New("readiness frame omitted native PID")
	}

	return ready, nil
}

func copySupervisorStream(dst io.Writer, src io.Reader, done chan<- struct{}) {
	_, _ = io.Copy(dst, src)
	if closer, ok := dst.(io.Closer); ok {
		_ = closer.Close()
	}

	done <- struct{}{}
}

const supervisorInputFrameLimit = 64 << 10

// copySupervisorFramedInput distinguishes a deliberate native-stdin EOF from
// loss of the control pipe. The caller writes a zero-length frame when its own
// input ends, which closes only native stdin; the pipe itself stays open for
// the command's lifetime, so a hangup on it always means the caller is gone.
func copySupervisorFramedInput(dst io.WriteCloser, src io.Reader, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()

	buffer := make([]byte, supervisorInputFrameLimit)
	for {
		count, readErr := src.Read(buffer)
		if count > 0 {
			var header [4]byte
			binary.BigEndian.PutUint32(header[:], uint32(count)) // #nosec G115 -- count is bounded by the 64 KiB buffer.

			if _, err := dst.Write(header[:]); err != nil {
				_ = dst.Close()

				return
			}

			if _, err := dst.Write(buffer[:count]); err != nil {
				_ = dst.Close()

				return
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				var eof [4]byte

				_, _ = dst.Write(eof[:])
			} else {
				_ = dst.Close()
			}

			return
		}
	}
}

func copyFramedSupervisorInput(dst io.WriteCloser, src io.Reader, controlDone chan<- struct{}) {
	defer close(controlDone)

	for {
		var header [4]byte
		if _, err := io.ReadFull(src, header[:]); err != nil {
			_ = dst.Close()

			return
		}

		size := binary.BigEndian.Uint32(header[:])
		if size == 0 {
			_ = dst.Close()
			_, _ = io.Copy(io.Discard, src)

			return
		}

		if size > supervisorInputFrameLimit {
			_ = dst.Close()

			return
		}

		if _, err := io.CopyN(dst, src, int64(size)); err != nil {
			_ = dst.Close()

			return
		}
	}
}
