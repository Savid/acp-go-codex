package codex

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestLineTransportCapturesProcessExit drives a real app-server process to death
// mid-stream and asserts the transport surfaces the exit status and stderr tail
// as a *ProcessExitError instead of a bare EOF.
func TestLineTransportCapturesProcessExit(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "printf 'fatal: out of memory\\n' >&2; exit 7")

	stderr := codexStderrWriter(nil)
	cmd.Stderr = stderr

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

	proc := &process{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr, processWaiter: waiter}
	transport := newLineTransport(stdout, stdin, proc)
	t.Cleanup(func() { _ = transport.Close() })

	_, _, recvErr := transport.Recv()

	var pe *ProcessExitError
	if !errors.As(recvErr, &pe) {
		t.Fatalf("Recv error = %v, want *ProcessExitError", recvErr)
	}
	if !strings.Contains(pe.Status, "exit status 7") {
		t.Fatalf("exit status = %q, want exit status 7", pe.Status)
	}
	if !strings.Contains(pe.StderrTail, "out of memory") {
		t.Fatalf("stderr tail = %q, want stderr detail", pe.StderrTail)
	}

	msg := pe.Error()
	if !strings.Contains(msg, "exit status 7") || !strings.Contains(msg, "out of memory") {
		t.Fatalf("error message = %q, want exit/stderr detail", msg)
	}
	if msg == "EOF" {
		t.Fatal("process-exit message is a bare EOF")
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

	stderr := codexStderrWriter(nil)
	cmd.Stderr = stderr

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

	proc := &process{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr, processWaiter: waiter}
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

	status, tail, ok := p.exited(time.Second)
	if !ok || status != exitStatusZero || tail != "" {
		t.Fatalf("exited() = (%q, %q, %v), want (%s, \"\", true)", status, tail, ok, exitStatusZero)
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
	if base != "codex app-server process exited" {
		t.Fatalf("bare error = %q", base)
	}

	withStatus := (&ProcessExitError{Status: exitStatusZero}).Error()
	if withStatus != "codex app-server process exited ("+exitStatusZero+")" {
		t.Fatalf("status error = %q", withStatus)
	}

	withStderr := (&ProcessExitError{StderrTail: "boom"}).Error()
	if withStderr != "codex app-server process exited: boom" {
		t.Fatalf("stderr error = %q", withStderr)
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

func TestStderrTailBounded(t *testing.T) {
	if got := (*stderrTail)(nil).tail(); got != "" {
		t.Fatalf("nil stderrTail.tail() = %q, want empty", got)
	}

	w := codexStderrWriter(nil)

	oversize := bytes.Repeat([]byte("a"), stderrTailLimit+512)
	if n, err := w.Write(oversize); n != len(oversize) || err != nil {
		t.Fatalf("Write oversize = (%d, %v)", n, err)
	}

	tail := w.tail()
	if len(tail) > stderrTailLimit {
		t.Fatalf("retained tail = %d bytes, want <= %d", len(tail), stderrTailLimit)
	}
	if tail == "" {
		t.Fatal("retained tail is empty")
	}
}
