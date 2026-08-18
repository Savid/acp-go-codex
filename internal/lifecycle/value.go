package lifecycle

import (
	"bytes"
	"encoding/json"
	"math/big"
	"strings"
)

// bigTen is the radix the exponent reader accumulates in.
var bigTen = big.NewInt(10)

// decodeValue reads one JSON document as the value form every comparison in this
// extension runs on. Numbers are retained as their literals rather than collapsed
// into binary floating point: lifecycle value equality compares them as exact
// mathematical values, and a float64 cannot hold one. It reports failure for a
// document it cannot read at all.
func decodeValue(raw json.RawMessage) (any, bool) {
	reader := json.NewDecoder(bytes.NewReader(raw))
	reader.UseNumber()

	var value any
	if err := reader.Decode(&value); err != nil {
		return nil, false
	}

	return value, true
}

// valueEqual reports lifecycle value equality: two decoded values are equal when
// they are deeply equal, key order and insignificant whitespace are not
// differences, and numbers compare as exact mathematical values rather than as
// lexemes or as doubles. It is the one equality this extension uses, at every
// comparison site.
func valueEqual(left, right any) bool {
	switch value := left.(type) {
	case map[string]any:
		return objectEqual(value, right)
	case []any:
		return arrayEqual(value, right)
	case json.Number:
		other, ok := right.(json.Number)

		return ok && normalizedNumber(string(value)) == normalizedNumber(string(other))
	default:
		return left == right
	}
}

func objectEqual(left map[string]any, right any) bool {
	other, ok := right.(map[string]any)
	if !ok || len(left) != len(other) {
		return false
	}

	for key, value := range left {
		counterpart, present := other[key]
		if !present || !valueEqual(value, counterpart) {
			return false
		}
	}

	return true
}

func arrayEqual(left []any, right any) bool {
	other, ok := right.([]any)
	if !ok || len(left) != len(other) {
		return false
	}

	for index := range left {
		if !valueEqual(left[index], other[index]) {
			return false
		}
	}

	return true
}

// rawEqual compares two undecoded members under the same equality. A member
// nothing delivered decodes to nothing, so an absent member equals an absent one
// and differs from every present value.
func rawEqual(left, right json.RawMessage) bool {
	leftValue, _ := decodeValue(left)
	rightValue, _ := decodeValue(right)

	return valueEqual(leftValue, rightValue)
}

// normalizedNumber renders one JSON number lexeme as its normalized decimal
// form: the sign, the coefficient with leading and trailing zeros stripped, and
// the adjusted exponent that stripping left. Every zero normalizes to the same
// form, so -0 and 0 are one value.
//
// The rendering never materializes the number. A twelve-byte literal with a
// nine-digit exponent costs the digits it was written with and nothing more,
// which is what makes an exact comparison affordable on a member whose interior
// this contract does not fix.
func normalizedNumber(lexeme string) string {
	mantissa, exponent := lexeme, ""
	if index := strings.IndexAny(lexeme, "eE"); index >= 0 {
		mantissa, exponent = lexeme[:index], lexeme[index+1:]
	}

	sign := ""
	if strings.HasPrefix(mantissa, "-") {
		sign, mantissa = "-", mantissa[1:]
	}

	integral, fraction := mantissa, ""
	if index := strings.IndexByte(mantissa, '.'); index >= 0 {
		integral, fraction = mantissa[:index], mantissa[index+1:]
	}

	coefficient := strings.TrimLeft(integral+fraction, "0")

	digits := strings.TrimRight(coefficient, "0")
	if digits == "" {
		return "0"
	}

	scale := exponentValue(exponent)
	scale.Sub(scale, big.NewInt(int64(len(fraction)-(len(coefficient)-len(digits)))))

	return sign + digits + "e" + scale.String()
}

// exponentValue reads a lexeme's exponent part as an exact integer. The lexeme
// comes from the JSON decoder, so its digits are already validated; accumulating
// them directly keeps the exponent exact however long it was written.
func exponentValue(exponent string) *big.Int {
	value := new(big.Int)
	if exponent == "" {
		return value
	}

	negative := exponent[0] == '-'
	if negative || exponent[0] == '+' {
		exponent = exponent[1:]
	}

	for index := range len(exponent) {
		value.Mul(value, bigTen)
		value.Add(value, big.NewInt(int64(exponent[index]-'0')))
	}

	if negative {
		value.Neg(value)
	}

	return value
}
