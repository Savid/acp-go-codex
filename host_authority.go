package codexacp

import (
	"context"
	"io"

	"github.com/savid/acp-go-codex/internal/codex"
)

var (
	ErrHostAuthorityUnavailable = codex.ErrHostAuthorityUnavailable
	ErrContainmentIncomplete    = codex.ErrContainmentIncomplete
	ErrNativeTreeBusy           = codex.ErrNativeTreeBusy
)

type HostAuthority interface {
	NativeEnvironment() map[string]string
	PrepareNativeTree(context.Context, string) error
	ReclaimNativeTree(context.Context, string) error
	StartNative(context.Context, NativeRequest) (NativeProcess, error)
}

type NativeRequest struct {
	Executable       string
	Arguments        []string
	Environment      []string
	WorkingDirectory string
}

type NativeProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Wait(context.Context) (NativeResult, error)
	Revoke(context.Context) error
}

type NativeResult struct {
	ExitCode int
	Signal   int
	Revoked  bool
}

type hostAuthorityAdapter struct{ HostAuthority }

func adaptHostAuthority(authority HostAuthority) codex.HostAuthority {
	if authority == nil {
		return nil
	}

	return hostAuthorityAdapter{HostAuthority: authority}
}

func (a hostAuthorityAdapter) StartNative(ctx context.Context, request codex.NativeRequest) (codex.NativeProcess, error) {
	process, err := a.HostAuthority.StartNative(ctx, NativeRequest(request))
	if err != nil {
		return nil, err
	}

	if process == nil {
		return nil, ErrHostAuthorityUnavailable
	}

	return nativeProcessAdapter{NativeProcess: process}, nil
}

type nativeProcessAdapter struct{ NativeProcess }

func (p nativeProcessAdapter) Wait(ctx context.Context) (codex.NativeResult, error) {
	result, err := p.NativeProcess.Wait(ctx)

	return codex.NativeResult(result), err
}
