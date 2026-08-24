package lifecycle

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLifecycleValueEqualityComparesNumbersAsExactValues pins the predicate the
// whole extension compares with. The battery drives it through duplicate frames
// and terminal restatements; these cases reach the spellings a wire vector cannot
// state and the structures a lifecycle frame does not happen to nest.
func TestLifecycleValueEqualityComparesNumbersAsExactValues(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		left  string
		right string
		equal bool
	}{
		{name: "an integer against the same integer written with a fraction", left: `1`, right: `1.0`, equal: true},
		{name: "negative zero against zero", left: `-0`, right: `0`, equal: true},
		{name: "a zero written every way", left: `-0.000e9`, right: `0e-9`, equal: true},
		{name: "a scaled integer against its exponent form", left: `100`, right: `1e2`, equal: true},
		{name: "a fraction against its exponent form", left: `0.5`, right: `5e-1`, equal: true},
		{name: "a signed exponent against an unsigned one", left: `1e+3`, right: `1e3`, equal: true},
		{name: "a negative against its positive", left: `-1`, right: `1`, equal: false},
		{name: "two negatives with the same magnitude", left: `-2.50`, right: `-25e-1`, equal: true},
		{
			name:  "integers past double precision differing in the last digit",
			left:  `1234567890123456788`,
			right: `1234567890123456789`,
		},
		{name: "an exponent no expansion could hold", left: `1e999999999`, right: `1e999999999`, equal: true},
		{name: "two huge exponents one apart", left: `1e999999999`, right: `1e999999998`},
		{name: "a number against a string", left: `1`, right: `"1"`},
		{name: "arrays of equal numbers", left: `[1, 2.0, 3e0]`, right: `[1.0, 2, 3]`, equal: true},
		{name: "arrays differing in one element", left: `[1, 2]`, right: `[1, 3]`},
		{name: "arrays of different lengths", left: `[1]`, right: `[1, 2]`},
		{name: "an array against an object", left: `[1]`, right: `{"a": 1}`},
		{name: "objects differing only in key order", left: `{"a": 1, "b": 2}`, right: `{"b": 2, "a": 1}`, equal: true},
		{name: "objects with different key sets", left: `{"a": 1}`, right: `{"b": 1}`},
		{name: "objects of different sizes", left: `{"a": 1}`, right: `{"a": 1, "b": 2}`},
		{name: "an object against a number", left: `{"a": 1}`, right: `1`},
		{name: "nested structures", left: `{"a": [{"b": 1.00}]}`, right: `{"a": [{"b": 1}]}`, equal: true},
		{name: "booleans", left: `true`, right: `true`, equal: true},
		{name: "null against a number", left: `null`, right: `0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			left, ok := decodeValue(json.RawMessage(tc.left))
			require.True(t, ok)

			right, ok := decodeValue(json.RawMessage(tc.right))
			require.True(t, ok)

			require.Equal(t, tc.equal, valueEqual(left, right))
			require.Equal(t, tc.equal, valueEqual(right, left))
		})
	}
}

// TestUndecodableValuesAreNotContent proves what an absent member compares as. A
// member nothing delivered decodes to nothing, so two absences are equal and an
// absence never equals a stated value.
func TestUndecodableValuesAreNotContent(t *testing.T) {
	t.Parallel()

	_, ok := decodeValue(nil)
	require.False(t, ok)

	require.True(t, rawEqual(nil, nil))
	require.False(t, rawEqual(json.RawMessage(`{"a":1}`), nil))
	require.True(t, rawEqual(json.RawMessage(`{"a":1}`), json.RawMessage(`{"a":1.0}`)))
}
