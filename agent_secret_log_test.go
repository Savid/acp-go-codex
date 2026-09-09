package codexacp

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestSecretSafeLoggerClassifiesNotificationsWithoutExposingPayloads(t *testing.T) {
	const secret = "transport-log-secret-sentinel"

	for _, tc := range []struct {
		name       string
		method     string
		err        error
		wantMethod string
		wantError  string
	}{
		{
			name: "classified", method: acp.AgentMethodSessionCancel,
			err:        &acp.RequestError{Code: -32602, Message: secret, Data: map[string]any{"token": secret}},
			wantMethod: acp.AgentMethodSessionCancel, wantError: "invalid_params",
		},
		{
			name: "opaque", method: acp.AgentMethodSessionCancel + secret, err: errors.New(secret),
			wantMethod: valueInternalFailure, wantError: valueInternalFailure,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := secretSafeLogger(slog.New(slog.NewJSONHandler(&output, nil)))
			logger.Error("failed to handle notification",
				slog.String("method", tc.method), slog.Any("err", tc.err))

			require.NotContains(t, output.String(), secret)
			var record map[string]any
			require.NoError(t, json.Unmarshal(output.Bytes(), &record))
			require.Equal(t, tc.wantMethod, record["method"])
			require.Equal(t, tc.wantError, record["err"])
		})
	}
}
