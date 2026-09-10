package codexacp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWithAmbientEnvironmentReplacesTheAdapterEnvironment(t *testing.T) {
	original := captureAmbientEnvironment
	t.Cleanup(func() { captureAmbientEnvironment = original })

	captureAmbientEnvironment = func() []string {
		t.Error("the adapter environment was read despite a supplied ambient block")

		return []string{"PATH=/adapter/bin"}
	}

	agent := NewAgent(WithAmbientEnvironment(map[string]string{"HOME": "/host/home", "PATH": "/host/bin"}))
	require.NoError(t, agent.optionsErr)
	require.Equal(t, map[string]string{"HOME": "/host/home", "PATH": "/host/bin"}, agent.options.implicitEnvironment)
}

func TestWithAmbientEnvironmentEmptyBlockInheritsNothing(t *testing.T) {
	original := adapterHomeDir
	adapterHomeDir = func() (string, error) { return "/fallback/home", nil }
	t.Cleanup(func() { adapterHomeDir = original })

	agent := NewAgent(WithAmbientEnvironment(map[string]string{}))
	require.NoError(t, agent.optionsErr)
	require.Empty(t, agent.options.implicitEnvironment, "a supplied block never gains the adapter account's home")
	require.Empty(t, agent.resolvedCodexHomeForEnv(nil))
}

func TestWithAmbientEnvironmentRefusesMalformedEntries(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"empty key":  {env: map[string]string{"": "x"}, want: `key "" is not a variable name`},
		"equals key": {env: map[string]string{"A=B": "x"}, want: `key "A=B" is not a variable name`},
		"nul key":    {env: map[string]string{"A\x00B": "x"}, want: "is not a variable name"},
		"nul value":  {env: map[string]string{"A": "x\x00y"}, want: `value for "A" contains NUL`},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, NewAgent(WithAmbientEnvironment(tc.env)).optionsErr, tc.want)
		})
	}
}
