package codexacp

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/lifecycle"
)

type retainedWireMetadata struct {
	route    json.RawMessage
	handoffs []json.RawMessage
}

// preserveWireMetadata shields owned values from the SDK's float64 decoding.
// Replacing only value spans preserves its handling of all other fields,
// including repeated struct fields and case-folded metadata aliases.
func preserveWireMetadata(params json.RawMessage, prompt, route bool) (json.RawMessage, retainedWireMetadata) {
	retained := retainedWireMetadata{}
	if !json.Valid(params) {
		return params, retained
	}

	sanitized := rewriteWireObject(params, func(field string, value json.RawMessage) json.RawMessage {
		switch {
		case strings.EqualFold(field, "_meta"):
			return rewriteWireObject(value, func(key string, raw json.RawMessage) json.RawMessage {
				switch key {
				case lifecycle.MetaKey:
					return json.RawMessage("null")
				case routeMetaKey:
					if !route {
						return raw
					}

					retained.route = raw

					return json.RawMessage("null")
				default:
					return raw
				}
			})
		case prompt && strings.EqualFold(field, jsonFieldPrompt):
			var blocks []json.RawMessage
			if json.Unmarshal(value, &blocks) != nil {
				return value
			}

			retained.handoffs = make([]json.RawMessage, len(blocks))
			changed := false

			for index, block := range blocks {
				candidate := rewriteWireObject(block, func(key string, raw json.RawMessage) json.RawMessage {
					if !strings.EqualFold(key, "_meta") {
						return raw
					}

					return rewriteWireObject(raw, func(key string, raw json.RawMessage) json.RawMessage {
						if key != handoffMetaKey {
							return raw
						}

						retained.handoffs[index] = raw

						return json.RawMessage("null")
					})
				})

				if retained.handoffs[index] == nil {
					continue
				}

				// Use the SDK's own union selection, including its fallback and
				// escaped discriminator behavior. Other block metadata is foreign.
				var decoded acp.ContentBlock
				if json.Unmarshal(candidate, &decoded) == nil && decoded.Image != nil {
					blocks[index] = candidate
					changed = true
				}
			}

			if !changed {
				return value
			}

			encoded, _ := json.Marshal(blocks)

			return encoded
		default:
			return value
		}
	})

	return sanitized, retained
}

// rewriteWireObject receives valid JSON and changes only selected value bytes.
func rewriteWireObject(raw json.RawMessage, rewrite func(string, json.RawMessage) json.RawMessage) json.RawMessage {
	decoder := json.NewDecoder(bytes.NewReader(raw))

	opening, _ := decoder.Token()
	if opening != json.Delim('{') {
		return raw
	}

	var result bytes.Buffer

	previous := 0

	for decoder.More() {
		token, _ := decoder.Token()
		field, _ := token.(string) // Valid object keys are strings.

		var value json.RawMessage

		_ = decoder.Decode(&value)
		end := int(decoder.InputOffset())

		replacement := rewrite(field, value)
		if bytes.Equal(value, replacement) {
			continue
		}

		result.Write(raw[previous : end-len(value)])
		result.Write(replacement)

		previous = end
	}

	if previous == 0 {
		return raw
	}

	result.Write(raw[previous:])

	return result.Bytes()
}

func restoreWireNumberValue(meta map[string]any, key string, raw json.RawMessage) {
	if _, present := meta[key]; !present || len(raw) == 0 {
		return
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var value any

	_ = decoder.Decode(&value) // The retained value came from valid request JSON.
	meta[key] = value
}

func (r retainedWireMetadata) restorePrompt(request *acp.PromptRequest) {
	for index, block := range request.Prompt {
		if block.Image != nil {
			restoreWireNumberValue(block.Image.Meta, handoffMetaKey, r.handoffs[index])
		}
	}
}

// exactMetadataInteger recognizes mathematical integers without float rounding
// or allocating the potentially enormous number described by an exponent.
func exactMetadataInteger(number json.Number) (int64, bool) {
	lexeme := string(number)
	if !json.Valid([]byte(lexeme)) {
		return 0, false
	}

	mantissa, exponent := lexeme, "0"
	if index := strings.IndexAny(lexeme, "eE"); index >= 0 {
		mantissa, exponent = lexeme[:index], lexeme[index+1:]
	}

	negative := strings.HasPrefix(mantissa, "-")
	mantissa = strings.TrimPrefix(mantissa, "-")
	integral, fraction, _ := strings.Cut(mantissa, ".")
	coefficient := strings.TrimLeft(integral+fraction, "0")

	digits := strings.TrimRight(coefficient, "0")
	if digits == "" {
		return 0, true
	}

	if negative {
		return 0, false
	}

	scale, err := strconv.ParseInt(exponent, 10, 64)
	if err != nil || scale < -int64(len(lexeme)) || scale > int64(len(lexeme))+19 {
		return 0, false
	}

	scale += int64(len(coefficient) - len(digits) - len(fraction))
	if scale < 0 || int64(len(digits))+scale > 19 {
		return 0, false
	}

	value, err := strconv.ParseInt(digits+strings.Repeat("0", int(scale)), 10, 64)

	return value, err == nil
}
