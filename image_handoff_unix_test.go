//go:build unix

package codexacp

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestHandoffFIFOInsideTheRootDoesNotBlock(t *testing.T) {
	root := t.TempDir()

	// A real FIFO with no writer attached. Opened without the non-blocking flag
	// this parks in the kernel and never returns, so the assertion runs on
	// another goroutine and a regression fails the test instead of wedging the
	// whole suite.
	path := filepath.Join(root, "pipe.png")
	require.NoError(t, syscall.Mkfifo(path, 0o600))

	block := handoffBlock(path, mimeImagePNG, handoffEnvelopeFor(syntheticPNG(t, 64)))

	type outcome struct {
		code    string
		message string
	}

	answered := make(chan outcome, 1)

	go func() {
		_, imageErr, err := validatePromptImages(t.Context(), []acp.ContentBlock{block}, defaultImageLimits(), root)
		if err != nil || imageErr == nil {
			close(answered)

			return
		}

		answered <- outcome{code: imageErr.code, message: imageErr.message}
	}()

	select {
	case got, ok := <-answered:
		require.True(t, ok, "the read must produce a verdict rather than an abort")
		require.Equal(t, imageErrorPathNotAllowed, got.code)
		require.Equal(t, handoffCauseNotRegular, got.message)
	case <-time.After(10 * time.Second):
		t.Fatal("opening a FIFO inside the handoff root blocked instead of being refused")
	}
}
