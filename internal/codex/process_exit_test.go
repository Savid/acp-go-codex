package codex

import (
	"errors"
	"io"
	"os/exec"
	"testing"
	"time"
)

// TestLineTransportCapturesProcessExit drives a real app-server process to death
// and proves its raw stderr never enters the classified error.
func TestLineTransportCapturesProcessExit(t *testing.T) {
	const secret = "native-stderr-secret"
	cmd := exec.Command("/bin/sh", "-c", "printf '"+secret+"\\n' >&2; exit 7")
	cmd.Stderr = io.Discard

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}

	waiter, err := startProcess(cmd)
	if err != nil {
		t.Fatalf("start process: %v", err)
	}

	proc := &process{cmd: cmd, stdin: stdin, stdout: stdout, processWaiter: waiter}
	transport := newLineTransport(stdout, stdin, proc)
	t.Cleanup(func() { _ = transport.Close() })

	_, _, recvErr := transport.Recv()

	var pe *ProcessExitError
	if !errors.As(recvErr, &pe) {
		t.Fatalf("Recv error = %v, want *ProcessExitError", recvErr)
	}
	if pe.Error() != processExitText {
		t.Fatalf("process-exit classification = %q", pe.Error())
	}
	if !errors.Is(recvErr, io.EOF) {
		t.Fatalf("process-exit error does not unwrap to the underlying read error: %v", recvErr)
	}
}

// TestLineTransportReadErrorProcessAlive verifies a read failure while the
// process is still running is not misclassified as a process exit; it stays a
// bare transport error (cause:"transport").
func TestLineTransportReadErrorProcessAlive(t *testing.T) {
	cmd := sleepCommand(t, "10")

	cmd.Stderr = io.Discard

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}

	waiter, err := startProcess(cmd)
	if err != nil {
		t.Fatalf("start process: %v", err)
	}

	proc := &process{cmd: cmd, stdin: stdin, stdout: stdout, processWaiter: waiter}
	transport := newLineTransport(stdout, stdin, proc)
	// Shrink this transport instance's exit grace so the alive-process
	// classification path does not stall the test. The field is written before
	// any other goroutine can observe the transport, so there is no shared
	// mutable state (a package-level grace override here raced with readError
	// in rpcConn read loops leaked past rpcConn.Close by earlier tests).
	transport.grace = 10 * time.Millisecond

	// Force a read failure while the process keeps running.
	_ = stdout.Close()

	_, _, recvErr := transport.Recv()
	if recvErr == nil {
		t.Fatal("Recv returned no error on a closed stream")
	}

	var pe *ProcessExitError
	if errors.As(recvErr, &pe) {
		t.Fatalf("live-process read error misclassified as process exit: %v", recvErr)
	}

	_ = killProcess(cmd)
	<-proc.waitDone
}

// TestProcessBeginWaitNoProcess covers the reaper short-circuit when there is no
// started process to wait on.
func TestProcessBeginWaitNoProcess(t *testing.T) {
	p := &process{}

	if !p.exited(time.Second) {
		t.Fatal("process without a command was not classified as exited")
	}
}

func TestProcessExitErrorFormatting(t *testing.T) {
	if got := (*ProcessExitError)(nil).Error(); got != "" {
		t.Fatalf("nil ProcessExitError.Error() = %q, want empty", got)
	}
	if got := (*ProcessExitError)(nil).Unwrap(); got != nil {
		t.Fatalf("nil ProcessExitError.Unwrap() = %v, want nil", got)
	}

	base := (&ProcessExitError{}).Error()
	if base != processExitText {
		t.Fatalf("bare error = %q", base)
	}

	underlying := errors.New("boom")
	if got := (&ProcessExitError{Err: underlying}).Unwrap(); !errors.Is(got, underlying) {
		t.Fatalf("Unwrap() = %v, want underlying", got)
	}

	if exitStatus(nil) != exitStatusZero {
		t.Fatalf("exitStatus(nil) = %q", exitStatus(nil))
	}
	if exitStatus(errors.New("signal: killed")) != "signal: killed" {
		t.Fatalf("exitStatus(err) mismatch")
	}
}
