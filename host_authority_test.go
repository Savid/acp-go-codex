package codexacp

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

type unavailableAuthority struct{}

func (*unavailableAuthority) NativeEnvironment() map[string]string {
	return map[string]string{"PATH": "/bin"}
}
func (*unavailableAuthority) PrepareNativeTree(context.Context, string) error { return nil }
func (*unavailableAuthority) ReclaimNativeTree(context.Context, string) error { return nil }
func (*unavailableAuthority) StartNative(context.Context, NativeRequest) (NativeProcess, error) {
	return nil, ErrHostAuthorityUnavailable
}

type invalidEnvironmentAuthority struct{ starts *int }

func (a invalidEnvironmentAuthority) NativeEnvironment() map[string]string { return nil }
func (a invalidEnvironmentAuthority) PrepareNativeTree(context.Context, string) error {
	return errors.New("unexpected prepare")
}
func (a invalidEnvironmentAuthority) ReclaimNativeTree(context.Context, string) error { return nil }
func (a invalidEnvironmentAuthority) StartNative(context.Context, NativeRequest) (NativeProcess, error) {
	(*a.starts)++

	return nil, errors.New("unexpected launch")
}

func TestHostAuthoritySurfaceAndConstructionGate(t *testing.T) {
	require.EqualError(t, ErrHostAuthorityUnavailable, "host authority unavailable")
	require.EqualError(t, ErrContainmentIncomplete, "native containment incomplete")
	require.EqualError(t, ErrNativeTreeBusy, "native tree has live lease processes")

	var typedNil *unavailableAuthority
	for name, authority := range map[string]HostAuthority{
		"typed nil":           typedNil,
		"missing environment": invalidEnvironmentAuthority{starts: new(int)},
	} {
		t.Run(name, func(t *testing.T) {
			agent := NewAgent(WithHostAuthority(authority))
			_, err := agent.Initialize(t.Context(), acp.InitializeRequest{})
			require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
		})
	}
}

type publicNativeProcess struct{}

func (publicNativeProcess) Stdin() io.WriteCloser { return nil }
func (publicNativeProcess) Stdout() io.ReadCloser { return nil }
func (publicNativeProcess) Stderr() io.ReadCloser { return nil }
func (publicNativeProcess) Wait(context.Context) (NativeResult, error) {
	return NativeResult{}, nil
}
func (publicNativeProcess) Revoke(context.Context) error { return nil }

var _ HostAuthority = (*unavailableAuthority)(nil)
var _ NativeProcess = publicNativeProcess{}

type orderingAuthority struct{ calls *[]string }

func (*orderingAuthority) NativeEnvironment() map[string]string {
	return map[string]string{"PATH": "/bin", "HOME": "/tmp"}
}
func (*orderingAuthority) PrepareNativeTree(context.Context, string) error { return nil }
func (a *orderingAuthority) ReclaimNativeTree(context.Context, string) error {
	*a.calls = append(*a.calls, "reclaim")

	return nil
}
func (*orderingAuthority) StartNative(context.Context, NativeRequest) (NativeProcess, error) {
	return nil, ErrHostAuthorityUnavailable
}

type orderingRuntimeClient struct {
	*spyCodexClient
	calls *[]string
}

func (c *orderingRuntimeClient) Close(context.Context) error {
	*c.calls = append(*c.calls, "close")

	return nil
}

func TestRuntimeRetirementWaitsBeforeResidenceReclaim(t *testing.T) {
	calls := []string{}
	authority := &orderingAuthority{calls: &calls}
	agent := NewAgent(WithHostAuthority(authority))
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	agent.retiredResidences = []retiredNativeResidence{{
		epoch: 1,
		tree:  filepath.Dir(path),
		path:  path,
		remove: func(string) error {
			calls = append(calls, "remove")

			return nil
		},
		release: func() { calls = append(calls, "release-residence") },
	}}
	client := &orderingRuntimeClient{spyCodexClient: newSpyCodexClient(), calls: &calls}
	err := agent.closeRuntimeGeneration(t.Context(), client, func() error {
		calls = append(calls, "release-home")

		return nil
	}, "", nil, 1)
	require.NoError(t, err)
	require.Equal(t, []string{"close", "reclaim", "remove", "release-residence", "release-home"}, calls)
}

func TestRetiredResidenceBoundsForceRuntimeRetirementBeforeAdmission(t *testing.T) {
	for name, residences := range map[string][]retiredNativeResidence{
		"count": make([]retiredNativeResidence, retiredResidenceCountLimit),
		"bytes": {{epoch: 1, path: "residence", tree: "tree", bytes: retiredResidenceByteLimit}},
	} {
		t.Run(name, func(t *testing.T) {
			calls := []string{}
			authority := &orderingAuthority{calls: &calls}
			client := &orderingRuntimeClient{spyCodexClient: newSpyCodexClient(), calls: &calls}
			agent := NewAgent(WithHostAuthority(authority))
			agent.runtimeClient = client
			agent.runtimeEpoch = 1
			agent.runtimeDead = false

			for index := range residences {
				residences[index].epoch = 1
				residences[index].path = filepath.Join(t.TempDir(), "residence")
				residences[index].tree = filepath.Dir(residences[index].path)
				residences[index].remove = func(string) error { return nil }
				agent.retiredResidenceBytes += residences[index].bytes
			}
			agent.retiredResidences = residences

			require.NoError(t, agent.ensureRetiredResidenceCapacity(t.Context(), 1))
			require.Nil(t, agent.runtimeClient)
			require.Empty(t, agent.retiredResidences)
			require.Equal(t, "close", calls[0])
		})
	}
}

var _ codex.Client = (*orderingRuntimeClient)(nil)
