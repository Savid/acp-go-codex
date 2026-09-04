package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/savid/acp-go-codex/internal/homelock"
)

// newProcessPipe is the seam the pipe-exhaustion refusals are proven through.
var newProcessPipe = os.Pipe

type ordinaryNativeProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	lock   *homelock.Lock

	waitOnce sync.Once
	done     chan struct{}
	result   NativeResult
	waitErr  error
	revoked  bool
	mu       sync.Mutex
}

func startOrdinaryNative(ctx context.Context, request NativeRequest, options Options) (NativeProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cmd := ordinaryExecCommand(request.Executable, request.Arguments...)
	cmd.Env = append([]string(nil), request.Environment...)
	cmd.Dir = request.WorkingDirectory
	configureProcess(cmd)

	pipes, err := ordinaryProcessPipes(cmd)
	if err != nil {
		return nil, err
	}

	var lock *homelock.Lock
	if !options.skipHomeLock {
		lockRoot, lockErr := HomeLockRoot(options.ScratchParent, firstNonEmpty(options.WritableHome, options.CodexHome))
		if lockErr != nil {
			pipes.closeEverything()

			return nil, lockErr
		}

		lock, err = homelock.Acquire(lockRoot)
		if err != nil {
			pipes.closeEverything()

			return nil, err
		}
	}

	startErr := cmd.Start()

	// From Start onward the child holds its own ends of all three pipes. This
	// process must drop its copies or the child never reads EOF on stdin and
	// its stdout and stderr never reach one either.
	pipes.closeChildEnds()

	if startErr != nil {
		pipes.closeParentEnds()

		_ = lock.Release()

		return nil, startErr
	}

	return &ordinaryNativeProcess{
		cmd: cmd, stdin: pipes.stdin, stdout: pipes.stdout, stderr: pipes.stderr,
		lock: lock, done: make(chan struct{}),
	}, nil
}

// ordinaryPipes holds both ends of the child's three standard streams while the
// process is being started, so a refusal at any point releases every descriptor
// it already claimed.
type ordinaryPipes struct {
	stdin  *os.File
	stdout *os.File
	stderr *os.File

	childStdin  *os.File
	childStdout *os.File
	childStderr *os.File
}

func (p *ordinaryPipes) closeChildEnds() {
	_ = p.childStdin.Close()
	_ = p.childStdout.Close()
	_ = p.childStderr.Close()
}

func (p *ordinaryPipes) closeParentEnds() {
	_ = p.stdin.Close()
	_ = p.stdout.Close()
	_ = p.stderr.Close()
}

func (p *ordinaryPipes) closeEverything() {
	p.closeChildEnds()
	p.closeParentEnds()
}

// ordinaryProcessPipes wires the child's three standard streams as ordinary OS
// pipes this process owns outright.
//
// exec.Cmd's own StdinPipe/StdoutPipe/StderrPipe hand their parent ends to
// Cmd.Wait, which closes them the moment the child exits — a close that races
// whoever is still draining what the child already wrote. This package calls
// Wait concurrently with those readers on every path, and each one-shot Codex
// command writes its whole answer just before exiting, so on a busy machine the
// race is lost routinely: the version probe reads an empty version, an account
// command loses the stderr line its diagnosis needs, and the app-server reader
// sees a closed descriptor instead of the frame the child had already written.
// Owning both ends here keeps each parent end open until its reader sees EOF,
// so the child's bytes survive whatever the scheduler does with the exit.
func ordinaryProcessPipes(cmd *exec.Cmd) (_ *ordinaryPipes, err error) {
	pipes := &ordinaryPipes{}

	defer func() {
		if err != nil {
			pipes.closeEverything()
		}
	}()

	if cmd.Stdin != nil {
		return nil, errors.New("create native stdin: stdin already set")
	}

	pipes.childStdin, pipes.stdin, err = newProcessPipe()
	if err != nil {
		return nil, fmt.Errorf("create native stdin: %w", err)
	}

	if cmd.Stdout != nil {
		return nil, errors.New("create native stdout: stdout already set")
	}

	pipes.stdout, pipes.childStdout, err = newProcessPipe()
	if err != nil {
		return nil, fmt.Errorf("create native stdout: %w", err)
	}

	if cmd.Stderr != nil {
		return nil, errors.New("create native stderr: stderr already set")
	}

	pipes.stderr, pipes.childStderr, err = newProcessPipe()
	if err != nil {
		return nil, fmt.Errorf("create native stderr: %w", err)
	}

	cmd.Stdin = pipes.childStdin
	cmd.Stdout = pipes.childStdout
	cmd.Stderr = pipes.childStderr

	return pipes, nil
}

func (p *ordinaryNativeProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *ordinaryNativeProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *ordinaryNativeProcess) Stderr() io.ReadCloser { return p.stderr }

func (p *ordinaryNativeProcess) beginWait() {
	p.waitOnce.Do(func() {
		go func() {
			err := p.cmd.Wait()

			result := NativeResult{}
			if p.cmd.ProcessState != nil {
				result.ExitCode = p.cmd.ProcessState.ExitCode()
				if status, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					result.Signal = int(status.Signal())
				}

				err = nil
			}

			p.mu.Lock()
			result.Revoked = p.revoked
			p.result = result
			p.waitErr = err
			p.mu.Unlock()
			_ = p.lock.Release()
			close(p.done)
		}()
	})
}

func (p *ordinaryNativeProcess) Wait(ctx context.Context) (NativeResult, error) {
	p.beginWait()

	select {
	case <-ctx.Done():
		return NativeResult{}, ctx.Err()
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()

		return p.result, p.waitErr
	}
}

func (p *ordinaryNativeProcess) Revoke(ctx context.Context) error {
	p.mu.Lock()
	select {
	case <-p.done:
		p.mu.Unlock()

		return nil
	default:
		p.revoked = true
	}
	p.mu.Unlock()

	p.beginWait()

	_ = terminateProcess(p.cmd)
	select {
	case <-p.done:
		return nil
	case <-time.After(processCloseGrace):
		_ = killProcess(p.cmd)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return nil
	}
}
