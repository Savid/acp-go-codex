package codexacp

import (
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

// Each client capability shape decides which elicitation modes reach the
// client: a tool question travels only as a form, an MCP url elicitation only
// as a url, and everything the client cannot take is answered natively with
// no client call.
func TestElicitationCapabilityGating(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, json string
		form, url  bool
	}{
		{"omitted", `{}`, false, false},
		{"empty", `{"elicitation":{}}`, false, false},
		{"form", `{"elicitation":{"form":{}}}`, true, false},
		{"url", `{"elicitation":{"url":{}}}`, false, true},
		{"both", `{"elicitation":{"form":{},"url":{}}}`, true, true},
		{"null", `{"elicitation":{"form":null,"url":null}}`, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)

			var calls atomic.Int32

			h.rec.mu.Lock()
			h.rec.elicit = func(acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
				calls.Add(1)

				return acp.UnstableCreateElicitationResponse{Accept: &acp.UnstableCreateElicitationAccept{Action: "accept", Content: map[string]any{"name": "Ada"}}}, nil
			}
			h.rec.mu.Unlock()

			h.initialize(withLifecycle(), func(request *acp.InitializeRequest) {
				require.NoError(t, json.Unmarshal([]byte(tc.json), &request.ClientCapabilities))
			})
			session := h.newSession()

			for index, step := range []struct {
				prompt, supported, unsupported string
				expected                       bool
			}{
				{"ASK", "hi Ada", "declined", tc.form},
				{"MCPELICIT_URL", "accept", "cancel", tc.url},
			} {
				before := len(h.rec.snapshot())
				_, err := h.prompt(session.SessionId, step.prompt, promptMeta(index+1))
				require.NoError(t, err)

				expected := step.unsupported
				if step.expected {
					expected = step.supported
				}

				require.Equal(t, expected, agentText(h.rec.snapshot()[before:]))
			}

			expectedCalls := int32(0)
			if tc.form {
				expectedCalls++
			}

			if tc.url {
				expectedCalls++
			}

			require.Equal(t, expectedCalls, calls.Load())
		})
	}
}

// Every server request the app-server can raise for a thread is answered
// through the client and mapped back onto the native decision.
func TestServerRequestBranches(t *testing.T) {
	t.Parallel()

	decline := func(acp.RequestPermissionRequest) acp.RequestPermissionResponse {
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected("decline")}
	}
	second := func(request acp.RequestPermissionRequest) acp.RequestPermissionResponse {
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(request.Options[1].OptionId)}
	}

	cases := []struct {
		name, prompt, expected string
		answer                 func(acp.RequestPermissionRequest) acp.RequestPermissionResponse
		title                  string
		kind                   acp.ToolKind
	}{
		{"file change allowed", "FILECHANGE", "edited", nil, "Edit files", acp.ToolKindEdit},
		{"file change declined", "FILECHANGE", "declined", decline, "Edit files", acp.ToolKindEdit},
		{"permissions for the turn", "PERMISSIONS", "granted turn", nil, "Codex permission request", acp.ToolKindOther},
		{"permissions for the session", "PERMISSIONS", "granted session", second, "Codex permission request", acp.ToolKindOther},
		{"permissions declined", "PERMISSIONS", "declined", decline, "Codex permission request", acp.ToolKindOther},
		{"mcp tool allowed", "MCPTOOL", "accept", nil, "fetch", acp.ToolKindOther},
		{"mcp tool declined", "MCPTOOL", "decline", decline, "fetch", acp.ToolKindOther},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)

			if tc.answer != nil {
				h.rec.mu.Lock()
				h.rec.answer = tc.answer
				h.rec.mu.Unlock()
			}

			h.initialize(withLifecycle())
			session := h.newSession()

			_, err := h.prompt(session.SessionId, tc.prompt, promptMeta(1))
			require.NoError(t, err)
			require.Equal(t, tc.expected, agentText(h.rec.snapshot()))

			h.rec.mu.Lock()
			require.Len(t, h.rec.permissions, 1)
			permission := h.rec.permissions[0]
			h.rec.mu.Unlock()

			require.Equal(t, tc.title, *permission.ToolCall.Title)
			require.Equal(t, tc.kind, *permission.ToolCall.Kind)
		})
	}

	t.Run("mcp form elicitation", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)

		h.rec.mu.Lock()
		h.rec.elicit = func(request acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
			require.NotNil(t, request.Form)
			require.Contains(t, request.Form.RequestedSchema.Properties, "color")

			return acp.UnstableCreateElicitationResponse{Accept: &acp.UnstableCreateElicitationAccept{Action: "accept", Content: map[string]any{"color": "blue"}}}, nil
		}
		h.rec.mu.Unlock()

		h.initialize(withLifecycle(), withFormElicitation())
		session := h.newSession()

		_, err := h.prompt(session.SessionId, "MCPELICIT", promptMeta(1))
		require.NoError(t, err)
		require.Equal(t, "accept blue", agentText(h.rec.snapshot()))
	})
}
