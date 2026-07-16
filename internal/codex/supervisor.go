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
	supervisorConfigEnv     = "ACP_GO_CODEX_INTERNAL_HELPER"
	supervisorModeGuardian  = "guardian"
	supervisorModeLiveness  = "liveness"
	supervisorReadyPrefix   = "acp-go-codex supervisor-ready "
	supervisorConfigPrefix  = "supervisor-config-"
	supervisorQuiesceWindow = 5 * time.Second
)

type supervisorConfig struct {
	NativePath       string   `json:"nativePath"`
	NativeArgs       []string `json:"nativeArgs"`
	NativeEnv        []string `json:"nativeEnv"`
	Home             string   `json:"home"`
	Scratch          string   `json:"scratch"`
	JobName          string   `json:"jobName,omitempty"`
	Started          string   `json:"started"`
	Completion       string   `json:"completion"`
	NativePIDFile    string   `json:"nativePidFile"`
	ProviderSnapshot string   `json:"providerSnapshot"`
	FramedInput      bool     `json:"framedInput"`
}

type supervisorReady struct {
	NativePID int `json:"nativePid"`
}

var supervisorExecutable = os.Executable
var supervisorExecCommand = exec.Command
var supervisorRandRead = rand.Read
var supervisorChmod = os.Chmod
var supervisorOpenFile = os.OpenFile
var supervisorEncodeConfig = func(writer io.Writer, config supervisorConfig) error {
	return json.NewEncoder(writer).Encode(config)
}
var supervisorNewGuardianContainment = newGuardianContainment
var supervisorOpenLivenessContainment = openLivenessContainment
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
	completionWait   time.Duration
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

	err := runSupervisor(mode, os.Getenv(supervisorConfigEnv))
	if err != nil {
		_, _ = fmt.Fprintln(supervisorError, "acp-go-codex runtime supervisor:", err)

		supervisorExit(1)

		return
	}

	supervisorExit(0)
}

func runSupervisor(mode string, configPath string) error {
	config, err := readSupervisorConfig(configPath)
	if err != nil {
		return err
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

func readSupervisorConfig(path string) (supervisorConfig, error) {
	if path == "" {
		return supervisorConfig{}, errors.New("missing private supervisor config")
	}
	// #nosec G703 -- the path is a nonce-named file inside the reserved private
	// runtime scratch root and is handed only to the embedded child process.
	file, err := os.Open(path)
	if err != nil {
		return supervisorConfig{}, fmt.Errorf("open private supervisor config: %w", err)
	}
	defer file.Close()
	defer os.Remove(path)

	var config supervisorConfig
	if err := json.NewDecoder(io.LimitReader(file, 8<<20)).Decode(&config); err != nil {
		return supervisorConfig{}, fmt.Errorf("decode private supervisor config: %w", err)
	}

	if config.NativePath == "" || config.Home == "" || config.Scratch == "" {
		return supervisorConfig{}, errors.New("private supervisor config is incomplete")
	}

	return config, nil
}

func writeSupervisorConfig(root string, config supervisorConfig) (string, error) {
	if root == "" {
		return "", errors.New("private supervisor scratch root is required")
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create private supervisor scratch root: %w", err)
	}

	if err := supervisorChmod(root, 0o700); err != nil {
		return "", fmt.Errorf("chmod private supervisor scratch root: %w", err)
	}

	var nonce [16]byte
	if _, err := supervisorRandRead(nonce[:]); err != nil {
		return "", fmt.Errorf("create private supervisor config nonce: %w", err)
	}

	path := filepath.Join(root, supervisorConfigPrefix+hex.EncodeToString(nonce[:])+".json")

	file, err := supervisorOpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create private supervisor config: %w", err)
	}

	encodeErr := supervisorEncodeConfig(file, config)

	closeErr := file.Close()
	if err := errors.Join(encodeErr, closeErr); err != nil {
		_ = os.Remove(path)

		return "", fmt.Errorf("write private supervisor config: %w", err)
	}

	return path, nil
}

func supervisorCommand(ctx context.Context, config supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
	markerNonce, err := supervisorNonce()
	if err != nil {
		return nil, nil, err
	}

	config.Started = filepath.Join(config.Scratch, "supervisor-started-"+markerNonce)
	config.Completion = filepath.Join(config.Scratch, "supervisor-complete-"+markerNonce)
	config.NativePIDFile = filepath.Join(config.Scratch, "supervisor-native-pid-"+markerNonce)
	config.ProviderSnapshot = filepath.Join(config.Scratch, "supervisor-provider-snapshot-"+markerNonce)

	path, err := writeSupervisorConfig(config.Scratch, config)
	if err != nil {
		return nil, nil, err
	}

	executable, err := supervisorExecutable()
	if err != nil {
		_ = os.Remove(path)

		return nil, nil, fmt.Errorf("resolve embedded runtime supervisor: %w", err)
	}

	cmd := execCommandContext(ctx, executable)
	cmd.Env = supervisorEnv(supervisorModeGuardian, path)

	return cmd, &supervisorProof{
		started:          config.Started,
		completion:       config.Completion,
		providerSnapshot: config.ProviderSnapshot,
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
// native tree empty. If no liveness process ever started, the guardian could
// not have launched a native root and the short startup observation expires.
func (p *supervisorProof) awaitCompletion() error {
	if p == nil || (p.started == "" && p.completion == "") {
		return nil
	}

	startupDeadline := time.Now().Add(time.Second)

	for {
		if _, err := os.Stat(p.completion); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: stat liveness completion proof: %v", ErrProcessTreeUnproven, err)
		}

		if _, err := os.Stat(p.started); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: stat liveness start proof: %v", ErrProcessTreeUnproven, err)
		}

		if time.Now().After(startupDeadline) {
			return nil
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
			return fmt.Errorf("%w: stat liveness completion proof: %v", ErrProcessTreeUnproven, err)
		}

		if time.Now().After(completionDeadline) {
			return fmt.Errorf("%w: liveness supervisor started but did not publish completion within %s", ErrProcessTreeUnproven, completionWait)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func supervisorEnv(mode string, configPath string) []string {
	env := make([]string, 0, len(os.Environ())+2)

	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, supervisorModeEnv+"=") || strings.HasPrefix(entry, supervisorConfigEnv+"=") {
			continue
		}

		env = append(env, entry)
	}

	return append(env, supervisorModeEnv+"="+mode, supervisorConfigEnv+"="+configPath)
}

func runGuardian(config supervisorConfig) error {
	input, output, errorOutput := supervisorInput, supervisorOutput, supervisorError

	claim, err := homelock.AcquireClaim(config.Home)
	if err != nil {
		return err
	}
	defer func() { _ = claim.Release() }()

	containment, err := supervisorNewGuardianContainment()
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
		_ = os.Remove(livenessConfig)

		return fmt.Errorf("resolve liveness supervisor executable: %w", err)
	}

	cmd := supervisorExecCommand(executable)
	cmd.Env = supervisorEnv(supervisorModeLiveness, livenessConfig)
	configureIndependentSupervisor(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = os.Remove(livenessConfig)

		return fmt.Errorf("open liveness control input: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		_ = os.Remove(livenessConfig)

		return fmt.Errorf("open liveness data output: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = os.Remove(livenessConfig)

		return fmt.Errorf("open liveness control output: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		_ = os.Remove(livenessConfig)

		return fmt.Errorf("start liveness supervisor: %w", err)
	}

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
					return containment.Quiesce(nativePID, supervisorQuiesceWindow)
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

	copyDone := make(chan struct{}, 3)
	if config.FramedInput {
		go copySupervisorFramedInput(stdin, input, copyDone)
	} else {
		go copySupervisorStream(stdin, input, copyDone)
	}

	go copySupervisorStream(output, stdout, copyDone)
	go copySupervisorStream(errorOutput, control, copyDone)

	waitErr := cmd.Wait()
	_ = stdin.Close()

	proofErr := awaitQuiescence(func() error {
		return containment.Quiesce(ready.NativePID, supervisorQuiesceWindow)
	})
	if proofErr != nil {
		return errors.Join(waitErr, proofErr)
	}

	if err := writeSupervisorMarker(config.Completion); err != nil {
		return err
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

	containment, err := supervisorOpenLivenessContainment(config.JobName)
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

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()

		return fmt.Errorf("open native data output: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()

		return fmt.Errorf("open native error output: %w", err)
	}

	if startErr := containment.Start(cmd); startErr != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()

		return fmt.Errorf("start contained native root: %w", startErr)
	}

	publishProviderProcessSnapshot(config.ProviderSnapshot, containment)

	// Reap concurrently with containment. On Unix a killed, unreaped process
	// remains visible to kill(-pgid, 0), so waiting only after the quiescence
	// probe would deadlock every post-start error path on its own zombie.
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

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

	go func() { _, _ = io.Copy(output, stdout) }()
	go func() { _, _ = io.Copy(errorOutput, stderr) }()

	select {
	case waitErr := <-waitDone:
		proofErr := awaitQuiescence(func() error {
			// The root has already been reaped, so do not signal its numeric
			// process-group ID after it may have become reusable.
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
	if err == nil || errors.Is(err, ErrProcessTreeUnproven) {
		return err
	}

	return fmt.Errorf("%w: %v", ErrProcessTreeUnproven, err)
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
