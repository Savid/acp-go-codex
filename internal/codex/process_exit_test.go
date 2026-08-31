package codex

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type gatedWaitProcess struct {
	stdout io.ReadCloser
	wait   <-chan struct{}
}

func (*gatedWaitProcess) Stdin() io.WriteCloser   { return &authorityTestWriteCloser{} }
func (p *gatedWaitProcess) Stdout() io.ReadCloser { return p.stdout }
func (*gatedWaitProcess) Stderr() io.ReadCloser   { return io.NopCloser(bytes.NewReader(nil)) }
func (p *gatedWaitProcess) Wait(context.Context) (NativeResult, error) {
	<-p.wait

	return NativeResult{}, nil
}
func (*gatedWaitProcess) Revoke(context.Context) error { return nil }

func TestLineTransportEOFAwaitsNativeWait(t *testing.T) {
	reader, writer := io.Pipe()
	wait := make(chan struct{})
	native := &gatedWaitProcess{stdout: reader, wait: wait}
	transport := newLineTransport(reader, io.Discard, &process{native: native, stdout: reader})

	require.NoError(t, writer.Close())
	done := make(chan error, 1)
	go func() {
		_, _, err := transport.Recv()
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("EOF published before NativeProcess.Wait completed: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(wait)
	select {
	case err := <-done:
		var exit *ProcessExitError
		require.ErrorAs(t, err, &exit)
		require.ErrorIs(t, err, io.EOF)
	case <-time.After(time.Second):
		t.Fatal("EOF was not published after NativeProcess.Wait completed")
	}
}

func TestLineTransportDeliversTrailingFrameBeforeWaitSettledEOF(t *testing.T) {
	reader, writer := io.Pipe()
	wait := make(chan struct{})
	native := &gatedWaitProcess{stdout: reader, wait: wait}
	transport := newLineTransport(reader, io.Discard, &process{native: native, stdout: reader})

	writeDone := make(chan error, 1)
	go func() {
		_, err := writer.Write([]byte(`{"jsonrpc":"2.0","method":"tail"}` + "\n"))
		writeDone <- errors.Join(err, writer.Close())
	}()

	message, raw, err := transport.Recv()
	require.NoError(t, err)
	require.Equal(t, "tail", message.Method)
	require.Contains(t, raw, "tail")
	require.NoError(t, <-writeDone)

	done := make(chan error, 1)
	go func() {
		_, _, recvErr := transport.Recv()
		done <- recvErr
	}()
	select {
	case err := <-done:
		t.Fatalf("terminal EOF beat NativeProcess.Wait: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(wait)
	require.Error(t, <-done)
}

func TestProcessExitErrorFormatting(t *testing.T) {
	cause := errors.New("cause")
	err := &ProcessExitError{Err: cause}
	require.Equal(t, processExitText, err.Error())
	require.ErrorIs(t, err, cause)
	var nilError *ProcessExitError
	require.Empty(t, nilError.Error())
	require.NoError(t, nilError.Unwrap())
}
