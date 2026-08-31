package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProbeVersionOrdinaryBackend(t *testing.T) {
	script := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\necho codex-cli 0.144.1\n"), 0o700))
	version, err := ProbeVersion(t.Context(), VersionProbeOptions{
		CLIPath: script, ScratchParent: t.TempDir(), ImplicitEnvironment: map[string]string{"PATH": "/bin"},
	})
	require.NoError(t, err)
	require.Equal(t, "0.144.1", version)
}

func TestProbeVersionFailureBranches(t *testing.T) {
	_, err := ProbeVersion(t.Context(), VersionProbeOptions{
		CLIPath: filepath.Join(t.TempDir(), "missing"), ImplicitEnvironment: map[string]string{"PATH": "/bin"},
	})
	require.Error(t, err)

	host := &authorityTestHost{environment: map[string]string{"PATH": "/host/bin"}, process: newAuthorityTestProcess("bad output")}
	_, err = ProbeVersion(t.Context(), VersionProbeOptions{CLIPath: "host-codex", HostAuthority: host})
	require.Error(t, err)

	host.process = nil
	_, err = ProbeVersion(t.Context(), VersionProbeOptions{CLIPath: "host-codex", HostAuthority: host})
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
}

type cancelledVersionProcess struct{ *authorityTestProcess }

func (p *cancelledVersionProcess) Wait(ctx context.Context) (NativeResult, error) {
	if ctx.Err() != nil {
		return NativeResult{}, ctx.Err()
	}

	return p.authorityTestProcess.Wait(ctx)
}

func TestProbeVersionCancellationRevokesAndWaits(t *testing.T) {
	base := newAuthorityTestProcess("")
	process := &cancelledVersionProcess{authorityTestProcess: base}
	host := &authorityTestHost{environment: map[string]string{"PATH": "/host/bin"}, process: process}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ProbeVersion(ctx, VersionProbeOptions{CLIPath: "host-codex", HostAuthority: host})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, base.revokes)
}

func TestProbeVersionWaitFailure(t *testing.T) {
	waitErr := errors.New("wait failed")
	process := &errorWaitVersionProcess{authorityTestProcess: newAuthorityTestProcess(""), err: waitErr}
	host := &authorityTestHost{environment: map[string]string{"PATH": "/host/bin"}, process: process}
	_, err := ProbeVersion(t.Context(), VersionProbeOptions{CLIPath: "host-codex", HostAuthority: host})
	require.ErrorIs(t, err, waitErr)
}

type errorWaitVersionProcess struct {
	*authorityTestProcess
	err error
}

func (p *errorWaitVersionProcess) Wait(context.Context) (NativeResult, error) {
	if p.err == nil {
		p.err = errors.New("wait failed")
	}

	return NativeResult{}, p.err
}
