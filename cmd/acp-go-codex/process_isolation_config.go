package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

const processIsolationConfigFlag = "process-isolation-config"

type processIsolationConfig struct {
	UID                 uint32            `json:"uid"`
	GID                 uint32            `json:"gid"`
	BaseEnvironment     map[string]string `json:"baseEnvironment"`
	InheritEnvironment  []string          `json:"inheritEnvironment"`
	StandaloneOwnerID   string            `json:"standaloneOwnerId"`
	StandaloneStateRoot string            `json:"standaloneStateRoot"`
}

var processIsolationConfigLoader = loadProcessIsolationConfig

func decodeProcessIsolationConfig(data []byte) (processIsolationConfig, error) {
	if !utf8.Valid(data) {
		return processIsolationConfig{}, fmt.Errorf("decode policy: invalid UTF-8")
	}
	// The token walk covers the whole document, so it rejects both the duplicate
	// keys the struct decoder would resolve last-write-wins and anything trailing
	// the policy value.
	if err := rejectDuplicateJSONKeys(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return processIsolationConfig{}, fmt.Errorf("decode policy: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var config processIsolationConfig
	if err := decoder.Decode(&config); err != nil {
		return processIsolationConfig{}, fmt.Errorf("decode policy: %w", err)
	}

	return config, nil
}

//nolint:wsl_v5 // The token parser keeps each structural check adjacent.
func rejectDuplicateJSONKeys(decoder *json.Decoder) error {
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}

		return err
	}

	return nil
}

//nolint:wsl_v5 // The token parser keeps each structural check adjacent.
func scanJSONValue(decoder *json.Decoder) error {
	token, tokenErr := decoder.Token()
	if tokenErr != nil {
		return tokenErr
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}

	// A closing delimiter is a syntax error wherever a value may begin, so the
	// decoder never hands one to this function; the container loops below
	// consume their own.
	if delimiter == '{' {
		return scanJSONObject(decoder)
	}

	return scanJSONArray(decoder)
}

//nolint:wsl_v5 // The token parser keeps each structural check adjacent.
func scanJSONObject(decoder *json.Decoder) error {
	seen := make(map[any]struct{})
	for decoder.More() {
		// The decoder rejects a non-string object key before returning it, so the
		// raw token doubles as the duplicate-detection key.
		keyToken, keyErr := decoder.Token()
		if keyErr != nil {
			return keyErr
		}
		if _, exists := seen[keyToken]; exists {
			return fmt.Errorf("duplicate object key %q", keyToken)
		}
		seen[keyToken] = struct{}{}
		if valueErr := scanJSONValue(decoder); valueErr != nil {
			return valueErr
		}
	}
	_, endErr := decoder.Token()

	return endErr
}

//nolint:wsl_v5 // The token parser keeps each structural check adjacent.
func scanJSONArray(decoder *json.Decoder) error {
	for decoder.More() {
		if valueErr := scanJSONValue(decoder); valueErr != nil {
			return valueErr
		}
	}
	_, endErr := decoder.Token()

	return endErr
}
