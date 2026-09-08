package codexacp

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

func TestWireMetadataKeepsForeignSDKSemantics(t *testing.T) {
	raw := json.RawMessage(`{"protocolVersion":1,"_meta":{"first":1},"_META":{"second":2,"acp-go.dev/lifecycle":{"version":1e400}}}`)
	request, err := decodeLocalAgentParams[acp.InitializeRequest, *acp.InitializeRequest](raw)
	require.Nil(t, err)
	require.Equal(t, float64(1), request.Meta["first"])
	require.Equal(t, float64(2), request.Meta["second"])
	_, _, refusal := lifecycle.DecodeOffer(request.Meta)
	require.Equal(t, lifecycle.MetaPath, refusal.Field)
	for _, namespace := range []string{"foreign", routeMetaKey, "ACP-GO.DEV/LIFECYCLE"} {
		_, err = decodeLocalAgentParams[acp.InitializeRequest, *acp.InitializeRequest](json.RawMessage(
			`{"protocolVersion":1,"_meta":{"` + namespace + `":{"version":1e400}}}`,
		))
		require.NotNil(t, err, "foreign overflow keeps its SDK refusal")
	}
}

func TestWireHandoffNumbersAndEmbeddedDominance(t *testing.T) {
	root := t.TempDir()
	originalOpen := openHandoffImage
	defer func() { openHandoffImage = originalOpen }()
	opens := 0
	openHandoffImage = func(string, string) (io.ReadCloser, *handoffVerdict) {
		opens++

		return nil, &handoffVerdict{code: imageErrorPathNotAllowed}
	}
	for _, tc := range []struct{ version, size string }{
		{"1.0000000000000001", "1"},
		{"1", "1.0000000000000001"},
		{"1", "-1e-400"},
		{"1e400", "1"},
	} {
		raw := json.RawMessage(`{"sessionId":"s","prompt":[{"type":"im\u0061ge","data":"","mimeType":"image/png","uri":"file:///missing/image.png","_META":{"acp-go.dev/handoff":{"version":` + tc.version + `,"sizeBytes":` + tc.size + `,"digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}]}`)
		request, err := decodeLocalAgentParams[acp.PromptRequest, *acp.PromptRequest](raw)
		require.Nil(t, err)
		_, refusal, abort := validatePromptImages(t.Context(), request.Prompt, ImageLimits{}, root)
		require.NoError(t, abort)
		require.NotNil(t, refusal)
		require.Equal(t, imageErrorInvalidHandoff, refusal.code)
	}
	data := base64.StdEncoding.EncodeToString(outputFixture(t, "valid.png"))
	request, err := decodeLocalAgentParams[acp.PromptRequest, *acp.PromptRequest](json.RawMessage(
		`{"sessionId":"s","prompt":[{"type":"image","data":"` + data + `","mimeType":"image/png","_meta":{"acp-go.dev/handoff":{"version":1e400}}}]}`,
	))
	require.Nil(t, err)
	images, refusal, abort := validatePromptImages(t.Context(), request.Prompt, ImageLimits{}, "")
	require.NoError(t, abort)
	require.Nil(t, refusal)
	require.Len(t, images, 1)
	require.Zero(t, opens, "owned malformed numbers and ignored handoff never open a file")
}

func TestExactMetadataIntegersPreserveMathematicalValues(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		value int64
		valid bool
	}{
		{"1.0", 1, true}, {"1e0", 1, true}, {"1000e-3", 1, true},
		{"1.00e2", 100, true}, {"-0e-999999999999999999999999999", 0, true},
		{strconv.FormatInt(math.MaxInt64, 10), math.MaxInt64, true},
		{"1.0000000000000001", 0, false}, {"-1e-400", 0, false},
		{"9223372036854775808", 0, false}, {"1e400", 0, false},
		{"1e-9999999999999999999999999", 0, false}, {"10e-2", 0, false},
		{"not-a-number", 0, false},
	} {
		value, valid := exactMetadataInteger(json.Number(tc.raw))
		require.Equal(t, tc.valid, valid, tc.raw)
		if valid {
			require.Equal(t, tc.value, value, tc.raw)
		}
	}
	for _, version := range []string{"1.0", "1e0", "2,\"version\":1"} {
		request, err := decodeLocalAgentParams[acp.PromptRequest, *acp.PromptRequest](json.RawMessage(
			`{"sessionId":"s","prompt":[],"_meta":{"acp-go.dev/route":{"version":` + version + `,"turnNonce":"nonce"}}}`,
		))
		require.Nil(t, err)
		route, refusal := parseInboundRoute(request.Meta)
		require.NoError(t, refusal)
		require.Equal(t, "nonce", route.TurnNonce)
	}
}
