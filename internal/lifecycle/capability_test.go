package lifecycle

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLifecycleCapabilityStrictScalar(t *testing.T) {
	t.Parallel()

	var decoded Negotiated
	require.NoError(t, json.Unmarshal([]byte(`{"version":1}`), &decoded))
	require.Equal(t, Version, decoded.Version)

	for _, test := range []struct {
		name string
		data string
	}{
		{"missing", `{}`},
		{"missing with fields", `{"updatesOutsidePrompt":true}`},
		{"other integer", `{"version":2}`},
		{"fractional", `{"version":1.0}`},
		{"string", `{"version":"1"}`},
		{"boolean", `{"version":true}`},
		{"duplicate", `{"version":1,"version":1}`},
		{"unknown", `{"version":1,"unknown":true}`},
		{"empty input", ``},
		{"array", `[]`},
		{"truncated member", `{"version":1,`},
		{"truncated object", `{"version":1`},
		{"updates type", `{"version":1,"updatesOutsidePrompt":1}`},
		{"quiescence type", `{"version":1,"authoritativeQuiescence":1}`},
		{"source type", `{"version":1,"quiescenceSource":1}`},
		{"activity kinds type", `{"version":1,"activityKinds":1}`},
		{"trailing", `{"version":1} {}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var value Negotiated
			require.Error(t, json.Unmarshal([]byte(test.data), &value))
		})
	}
}

func TestLifecycleCapabilityDirectMalformedInput(t *testing.T) {
	t.Parallel()

	for _, data := range []string{
		`{"version":1,`,
		`{"version":1`,
		`{"version":1} {}`,
	} {
		var value Negotiated
		require.Error(t, value.UnmarshalJSON([]byte(data)))
	}
}
