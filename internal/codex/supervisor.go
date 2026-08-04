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
	"path/filepath"
	"strings"
	"time"

	"github.com/savid/acp-go-codex/internal/homelock"
)

const (
	supervisorModeEnv       = "ACP_GO_CODEX_INTERNAL_MODE"
	supervisorModeGuardian  = "guardian"
	supervisorModeLiveness  = "liveness"
	supervisorReadyPrefix   = "acp-go-codex supervisor-ready "
	supervisorConfigPrefix  = "supervisor-config-"
	supervisorQuiesceWindow = 5 * time.Second
	lifecycleRuntime        = "runtime"
	lifecycleDiscovery      = "discovery"
)

type supervisorConfig struct {
	NativePath       string            `json:"nativePath"`
	NativeArgs       []string          `json:"nativeArgs"`
	NativeEnv        []string          `json:"nativeEnv"`
	Home             string            `json:"home"`
	Scratch          string            `json:"scratch"`
	ScratchParent    string            `json:"scratchParent"`
	LifecycleKind    string            `json:"lifecycleKind"`
	DarwinBestEffort bool              `json:"darwinBestEffort"`
	JobName          string            `json:"jobName,omitempty"`
	Started          string            `json:"started"`
	Completion       string            `json:"completion"`
	NativePIDFile    string            `json:"nativePidFile"`
	ProviderSnapshot string            `json:"providerSnapshot"`
	FramedInput      bool              `json:"framedInput"`
	IsolationUID     uint32            `json:"isolationUid"`
	IsolationGID     uint32            `json:"isolationGid"`
	Isolation        *ProcessIsolation `json:"-"`
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
var supervisorEncodeConfig = func(writer io.Writer, config supervisorConfig) error {
	return json.NewEncoder(writer).Encode(config)
}
var supervisorNewGuardianContainment = newGuardianContainment
var supervisorOpenLivenessContainment = openLivenessContainment
var supervisorGuardianQuiesce = func(containment *guardianContainment, nativePID int, timeout time.Duration) error {
	return containment.Quiesce(nativePID, timeout)
}
var supervisorDescendantCount = func(containment *livenessContainment) (int, bool) {
	return containment.DescendantCount()
}
var supervisorInput io.Reader = os.Stdin
var supervisorOutput io.Writer = os.Stdout
var supervisorError io.Writer = os.Stderr
var supervisorExit = os.Exit

type supervisorProof struct {
	started          string
	completion       string
	providerSnapshot string
	startupWait      time.Duration
	completionWait   time.Duration
	inherited        []*os.File
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

// init turns the embedding command itself into either member of the
// supervisor pair. No separate helper binary is installed, and these modes
// run before the host's main package can open any adapter state.
func init() {
	supervisorBootstrap()
}

func supervisorBootstrap() {
	mode := os.Getenv(supervisorModeEnv)
	if mode == "" {
		return
	}

	err := verifySupervisorIdentity()

	configFile := supervisorInheritedFile(3, "acp-go-codex-supervisor-config")
	if err == nil && configFile == nil {
		err = errors.New("process supervisor inherited config descriptor is unavailable")
	}

	if err == nil {
		err = closeInheritedOnExec(configFile)
	}

	if err == nil {
		err = runSupervisor(mode, configFile)
	}

	if configFile != nil {
		_ = configFile.Close()
	}

	if err != nil {
		_, _ = fmt.Fprintln(supervisorError, "acp-go-codex runtime supervisor:", err)

		supervisorExit(1)

		return
	}

	supervisorExit(0)
}

func runSupervisor(mode string, configInput io.Reader) error {
	config, err := readSupervisorConfig(configInput)
	if err != nil {
		return err
	}

	uid, gid, err := expectedSupervisorIdentity()
	if err != nil || config.IsolationUID != uid || config.IsolationGID != gid {
		return errors.Join(errors.New("private supervisor config identity does not match the verified process identity"), err)
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

	if config.NativePath == "" || config.Home == "" || config.Scratch == "" || config.IsolationUID == 0 || config.IsolationGID == 0 {
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
	if err := validateProcessIsolation(config.Isolation); err != nil {
		return nil, nil, err
	}

	config.IsolationUID = config.Isolation.UID

	config.IsolationGID = config.Isolation.GID
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

	config.Started = filepath.Join(config.Scratch, "supervisor-started-"+markerNonce)
	config.Completion = filepath.Join(config.Scratch, "supervisor-complete-"+markerNonce)
	config.NativePIDFile = filepath.Join(config.Scratch, "supervisor-native-pid-"+markerNonce)
	config.ProviderSnapshot = filepath.Join(config.Scratch, "supervisor-provider-snapshot-"+markerNonce)

	configFile, err := writeSupervisorConfig(config.Scratch, config)
	if err != nil {
		return nil, nil, err
	}

	executable, err := supervisorExecutable()
	if err != nil {
		_ = configFile.Close()

		return nil, nil, fmt.Errorf("resolve embedded runtime supervisor: %w", err)
	}

	helperEnv := supervisorIdentityEnvironment(config.NativeEnv, supervisorModeGuardian, *config.Isolation)

	executable, err = resolveProcessExecutable(executable, helperEnv)
	if err != nil {
		_ = configFile.Close()

		return nil, nil, fmt.Errorf("resolve embedded runtime supervisor through process policy: %w", err)
	}

	cmd := execCommandContext(ctx, executable)
	cmd.Env = helperEnv
	cmd.ExtraFiles = []*os.File{configFile}

	cmd.WaitDelay = supervisorQuiesceWindow + time.Second
	if err := applyProcessCredential(cmd, config.Isolation); err != nil {
		_ = configFile.Close()

		return nil, nil, err
	}

	return cmd, &supervisorProof{
		started:          config.Started,
		completion:       config.Completion,
		providerSnapshot: config.ProviderSnapshot,
		inherited:        []*os.File{configFile},
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
	if p == nil || (p.started == "" && p.completion == "") {
		return nil
	}

	startupWait := p.startupWait
	if startupWait <= 0 {
		startupWait = time.Second
	}

	startupDeadline := time.Now().Add(startupWait)

	for {
		if _, err := os.Stat(p.completion); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: stat liveness completion proof: %v", ErrProcessContainmentIncomplete, err)
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

		time.Sleep(10 * time.Millisecond)
	}

	completionWait := p.completionWait
	if completionWait <= 0 {
		completionWait = supervisorQuiesceWindow + time.Second
	}

	completionDeadline := time.Now().Add(completionWait)

	for {
		if _, err := os.Stat(p.completion); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: stat liveness completion proof: %v", ErrProcessContainmentIncomplete, err)
		}

		if time.Now().After(completionDeadline) {
			return fmt.Errorf("%w: liveness supervisor started but did not publish completion within %s", ErrProcessContainmentIncomplete, completionWait)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func runGuardian(config supervisorConfig) error {
	input, output, errorOutput := supervisorInput, supervisorOutput, supervisorError

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

	livenessConfig, err := writeSupervisorConfig(config.Scratch, config)
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
	cmd.Env = supervisorIdentityEnvironment(config.NativeEnv, supervisorModeLiveness, ProcessIsolation{UID: config.IsolationUID, GID: config.IsolationGID})
	cmd.ExtraFiles = []*os.File{livenessConfig}
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

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stderr.Close()
		_ = livenessConfig.Close()

		return fmt.Errorf("start liveness supervisor: %w", err)
	}

	_ = livenessConfig.Close()

	control := bufio.NewReader(stderr)
	readyLine, readyErr := control.ReadString('\n')

	ready, parseErr := parseSupervisorReady(readyLine)
	if readyErr != nil || parseErr != nil {
		_ = stdin.Close()
		_ = terminateIndependentSupervisor(cmd)
		waitErr := cmd.Wait()
		_, _ = io.Copy(errorOutput, control)

		var proofErr error

		if _, completeErr := os.Stat(config.Completion); completeErr != nil {
			if !errors.Is(completeErr, os.ErrNotExist) {
				return errors.Join(waitErr, completeErr)
			}

			if _, startedErr := os.Stat(config.Started); startedErr == nil {
				nativePID, _ := readNativePID(config.NativePIDFile)

				proofErr = awaitQuiescence(func() error {
					return supervisorGuardianQuiesce(containment, nativePID, supervisorQuiesceWindow)
				})
				if proofErr == nil {
					proofErr = writeSupervisorMarker(config.Completion)
				}
			} else if !errors.Is(startedErr, os.ErrNotExist) {
				return errors.Join(waitErr, startedErr)
			}
		}

		return errors.Join(fmt.Errorf("liveness supervisor failed before readiness: %w", errors.Join(readyErr, parseErr)), waitErr, proofErr)
	}

	inputDone := make(chan struct{}, 1)
	if config.FramedInput {
		go copySupervisorFramedInput(stdin, input, inputDone)
	} else {
		go copySupervisorStream(stdin, input, inputDone)
	}

	streamDone := make(chan struct{}, 1)
	go copySupervisorStream(errorOutput, control, streamDone)

	waitErr := cmd.Wait()
	_ = stdin.Close()

	<-streamDone

	var proofErr error
	if _, completionErr := os.Stat(config.Completion); errors.Is(completionErr, os.ErrNotExist) {
		proofErr = awaitQuiescence(func() error {
			return supervisorGuardianQuiesce(containment, ready.NativePID, supervisorQuiesceWindow)
		})
		if proofErr == nil {
			proofErr = writeSupervisorMarker(config.Completion)
		}
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

	// #nosec G204 -- native path and arguments come from adapter construction,
	// cross a mode-0600 file in the private runtime scratch root, and are not
	// accepted from ACP requests.
	cmd := supervisorExecCommand(config.NativePath, config.NativeArgs...)
	cmd.Env = config.NativeEnv

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

	if startErr := containment.Start(cmd); startErr != nil {
		_ = stdin.Close()
		_ = stderr.Close()

		return fmt.Errorf("start contained native root: %w", startErr)
	}

	waitDone := containment.Wait()

	publishProviderProcessSnapshot(config.ProviderSnapshot, containment)

	if pidErr := writeNativePID(config.NativePIDFile, cmd.Process.Pid); pidErr != nil {
		proofErr := awaitQuiescence(func() error {
			return containment.Quiesce(cmd.Process.Pid, supervisorQuiesceWindow)
		})

		<-waitDone

		if proofErr == nil {
			proofErr = writeSupervisorMarker(config.Completion)
		}

		return errors.Join(pidErr, proofErr)
	}

	// This fixed, integer-only object has no JSON encoding failure mode.
	ready := fmt.Appendf(nil, "{\"nativePid\":%d}", cmd.Process.Pid)

	if _, err := fmt.Fprintln(errorOutput, supervisorReadyPrefix+string(ready)); err != nil {
		proofErr := awaitQuiescence(func() error {
			return containment.Quiesce(cmd.Process.Pid, supervisorQuiesceWindow)
		})

		<-waitDone

		if proofErr == nil {
			proofErr = writeSupervisorMarker(config.Completion)
		}

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
			return containment.Quiesce(0, supervisorQuiesceWindow)
		})
		if proofErr == nil {
			proofErr = writeSupervisorMarker(config.Completion)
		}

		if proofErr != nil {
			return errors.Join(waitErr, proofErr)
		}

		if waitErr != nil {
			return fmt.Errorf("native root exited: %w", waitErr)
		}

		return nil
	case <-controlDone:
		proofErr := awaitQuiescence(func() error {
			return containment.Quiesce(cmd.Process.Pid, supervisorQuiesceWindow)
		})

		<-waitDone
		<-streamDone

		if proofErr == nil {
			proofErr = writeSupervisorMarker(config.Completion)
		}

		return proofErr
	}
}

func publishProviderProcessSnapshot(path string, containment *livenessContainment) {
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
// loss of the guardian control pipe. A zero-length frame closes only native
// stdin; the guardian keeps the outer pipe open until the native command exits.
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
