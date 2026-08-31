package codex

import (
	"context"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/savid/acp-go-codex/internal/homelock"
)

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

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()

		return nil, err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()

		return nil, err
	}

	var lock *homelock.Lock
	if !options.skipHomeLock {
		lockRoot, lockErr := HomeLockRoot(options.ScratchParent, firstNonEmpty(options.WritableHome, options.CodexHome))
		if lockErr != nil {
			return nil, lockErr
		}

		lock, err = homelock.Acquire(lockRoot)
		if err != nil {
			return nil, err
		}
	}

	if err := cmd.Start(); err != nil {
		_ = lock.Release()

		return nil, err
	}

	return &ordinaryNativeProcess{
		cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr, lock: lock, done: make(chan struct{}),
	}, nil
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
