package codex

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOrdinaryNativeKeepsTheChildTailAfterWait pins who owns the bytes a
// one-shot native command wrote just before exiting.
//
// Wait runs here before either stream is drained, on purpose: what the child
// wrote belongs to whoever holds the pipe, not to whoever the scheduler happened
// to run before the exit. A backend that hands its parent ends to
// exec.Cmd.Wait has them closed underneath it the moment the child is reaped,
// and loses both payloads here every time.
func TestOrdinaryNativeKeepsTheChildTailAfterWait(t *testing.T) {
	executable := writeFakeCLI(t, t.TempDir(), "tail-streams", fakeCLITailStreams)

	native, err := startOrdinaryNative(t.Context(), NativeRequest{
		Executable: executable, Environment: os.Environ(), WorkingDirectory: t.TempDir(),
	}, Options{skipHomeLock: true})
	require.NoError(t, err)

	require.NoError(t, native.Stdin().Close())

	result, err := native.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, NativeResult{}, result)

	stdout, err := io.ReadAll(native.Stdout())
	require.NoError(t, err)
	stderr, err := io.ReadAll(native.Stderr())
	require.NoError(t, err)

	require.Equal(t, "codex stdout tail", strings.TrimSpace(string(stdout)))
	require.Equal(t, "codex stderr tail", strings.TrimSpace(string(stderr)))

	require.NoError(t, closeNativePipes(native.Stdin(), native.Stdout(), native.Stderr()))
}

// TestOrdinaryNativePipeExhaustionReleasesClaimedDescriptors covers each pipe
// this backend claims: a host that cannot hand out the next one refuses the
// start naming that stream, and every descriptor already claimed is released
// rather than leaked into the refusal.
func TestOrdinaryNativePipeExhaustionReleasesClaimedDescriptors(t *testing.T) {
	original := newProcessPipe
	t.Cleanup(func() { newProcessPipe = original })

	want := errors.New("no descriptors left")
	for _, test := range []struct {
		stream string
		allow  int
	}{
		{stream: "stdin", allow: 0},
		{stream: "stdout", allow: 1},
		{stream: "stderr", allow: 2},
	} {
		t.Run(test.stream, func(t *testing.T) {
			remaining := test.allow
			newProcessPipe = func() (*os.File, *os.File, error) {
				if remaining == 0 {
					return nil, nil, want
				}

				remaining--

				return original()
			}

			_, err := startOrdinaryNative(t.Context(), NativeRequest{Executable: "unused"},
				Options{skipHomeLock: true})
			require.ErrorIs(t, err, want)
			require.ErrorContains(t, err, "create native "+test.stream)
		})
	}
}
