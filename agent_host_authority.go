package codexacp

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"

	"github.com/savid/acp-go-codex/internal/codex"
)

const managedCodexHomeEnvironment = "CODEX_HOME"

type guardedHostAuthority struct {
	authority   HostAuthority
	environment map[string]string
}

func normalizeHostAuthority(authority HostAuthority, supplied bool) (HostAuthority, error) {
	if authority == nil {
		if supplied {
			return nil, ErrHostAuthorityUnavailable
		}

		//nolint:nilnil // Omission selects ordinary execution.
		return nil, nil
	}

	value := reflect.ValueOf(authority)
	if nilable(value.Kind()) && value.IsNil() {
		return nil, ErrHostAuthorityUnavailable
	}

	environment, err := nativeEnvironment(authority)
	if err != nil || environment == nil {
		return nil, errors.Join(ErrHostAuthorityUnavailable, err)
	}

	for key, value := range environment {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return nil, ErrHostAuthorityUnavailable
		}
	}

	return &guardedHostAuthority{authority: authority, environment: cloneStringMap(environment)}, nil
}

func nilable(kind reflect.Kind) bool {
	switch kind {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return true
	default:
		return false
	}
}

func nativeEnvironment(authority HostAuthority) (environment map[string]string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = ErrHostAuthorityUnavailable
		}
	}()

	return authority.NativeEnvironment(), nil
}

func (a *guardedHostAuthority) NativeEnvironment() map[string]string {
	return cloneStringMap(a.environment)
}

func (a *guardedHostAuthority) PrepareNativeTree(ctx context.Context, path string) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrHostAuthorityUnavailable
		}
	}()

	return a.authority.PrepareNativeTree(ctx, path)
}

func (a *guardedHostAuthority) ReclaimNativeTree(ctx context.Context, path string) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrHostAuthorityUnavailable
		}
	}()

	return a.authority.ReclaimNativeTree(ctx, path)
}

func (a *guardedHostAuthority) StartNative(ctx context.Context, request NativeRequest) (process NativeProcess, err error) {
	defer func() {
		if recover() != nil {
			err = ErrHostAuthorityUnavailable
		}
	}()

	process, err = a.authority.StartNative(ctx, request)
	if err == nil && interfaceNil(process) {
		err = ErrHostAuthorityUnavailable
	}

	if err == nil {
		process = guardedNativeProcess{NativeProcess: process}
	}

	return process, err
}

func interfaceNil(value any) bool {
	if value == nil {
		return true
	}

	reflected := reflect.ValueOf(value)

	return nilable(reflected.Kind()) && reflected.IsNil()
}

type guardedNativeProcess struct{ NativeProcess }

func (p guardedNativeProcess) Stdin() (stream io.WriteCloser) {
	defer func() { _ = recover() }()

	return p.NativeProcess.Stdin()
}

func (p guardedNativeProcess) Stdout() (stream io.ReadCloser) {
	defer func() { _ = recover() }()

	return p.NativeProcess.Stdout()
}

func (p guardedNativeProcess) Stderr() (stream io.ReadCloser) {
	defer func() { _ = recover() }()

	return p.NativeProcess.Stderr()
}

func (p guardedNativeProcess) Wait(ctx context.Context) (result NativeResult, err error) {
	defer func() {
		if recover() != nil {
			err = ErrHostAuthorityUnavailable
		}
	}()

	return p.NativeProcess.Wait(ctx)
}

func (p guardedNativeProcess) Revoke(ctx context.Context) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrHostAuthorityUnavailable
		}
	}()

	return p.NativeProcess.Revoke(ctx)
}

type hostAuthorityAdapter struct{ HostAuthority }

func adaptHostAuthority(authority HostAuthority) codex.HostAuthority {
	if authority == nil {
		return nil
	}

	return hostAuthorityAdapter{HostAuthority: authority}
}

func (a hostAuthorityAdapter) PrepareNativeTree(ctx context.Context, path string) error {
	return toInternalAuthorityError(a.HostAuthority.PrepareNativeTree(ctx, path))
}

func (a hostAuthorityAdapter) ReclaimNativeTree(ctx context.Context, path string) error {
	return toInternalAuthorityError(a.HostAuthority.ReclaimNativeTree(ctx, path))
}

func (a hostAuthorityAdapter) StartNative(ctx context.Context, request codex.NativeRequest) (codex.NativeProcess, error) {
	process, err := a.HostAuthority.StartNative(ctx, NativeRequest(request))
	if err != nil {
		return nil, toInternalAuthorityError(err)
	}

	if process == nil {
		return nil, codex.ErrHostAuthorityUnavailable
	}

	return nativeProcessAdapter{NativeProcess: process}, nil
}

func toInternalAuthorityError(err error) error {
	switch {
	case errors.Is(err, ErrHostAuthorityUnavailable):
		return errors.Join(codex.ErrHostAuthorityUnavailable, err)
	case errors.Is(err, ErrContainmentIncomplete):
		return errors.Join(codex.ErrContainmentIncomplete, err)
	case errors.Is(err, ErrNativeTreeBusy):
		return errors.Join(codex.ErrNativeTreeBusy, err)
	default:
		return err
	}
}

func toPublicAuthorityError(err error) error {
	switch {
	case errors.Is(err, codex.ErrHostAuthorityUnavailable) && !errors.Is(err, ErrHostAuthorityUnavailable):
		return errors.Join(ErrHostAuthorityUnavailable, err)
	case errors.Is(err, codex.ErrContainmentIncomplete) && !errors.Is(err, ErrContainmentIncomplete):
		return errors.Join(ErrContainmentIncomplete, err)
	case errors.Is(err, codex.ErrNativeTreeBusy) && !errors.Is(err, ErrNativeTreeBusy):
		return errors.Join(ErrNativeTreeBusy, err)
	default:
		return err
	}
}

type nativeProcessAdapter struct{ NativeProcess }

func (p nativeProcessAdapter) Wait(ctx context.Context) (codex.NativeResult, error) {
	result, err := p.NativeProcess.Wait(ctx)

	return codex.NativeResult(result), toInternalAuthorityError(err)
}

func validateRuntimeEnvironment(environment map[string]string) error {
	for key, value := range environment {
		upperKey := strings.ToUpper(key)
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return errors.New("invalid Codex environment entry")
		}

		if strings.HasPrefix(upperKey, "ACP_GO_CODEX_INTERNAL_") || managedCodexRootEnvKey(upperKey) {
			return errors.New("codex environment contains a reserved key")
		}
	}

	return nil
}

func managedCodexRootEnvKey(key string) bool {
	switch strings.ToUpper(key) {
	case managedCodexHomeEnvironment, managedHomeEnv, "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "XDG_STATE_HOME":
		return true
	default:
		return false
	}
}

func reservedCodexEnvKey(key string) bool {
	upperKey := strings.ToUpper(key)

	return strings.HasPrefix(upperKey, "ACP_GO_CODEX_INTERNAL_") || managedCodexRootEnvKey(upperKey)
}

func validateManagedExecutableSelector(selector string) error {
	for _, segment := range strings.Split(strings.ReplaceAll(strings.TrimSpace(selector), `\`, "/"), "/") {
		if segment == "node_modules" {
			return errors.New("managed Codex executable must be staged and pinned by the host before adapter initialization")
		}
	}

	return nil
}
