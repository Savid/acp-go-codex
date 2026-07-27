//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	codexacp "github.com/savid/acp-go-codex"
)

const envRunAttended = "ACP_GO_CODEX_RUN_ATTENDED"

type authMethodsWire struct {
	Providers map[string][]struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Label string `json:"label"`
	} `json:"providers"`
	Generation string `json:"generation"`
}

type authAuthorizeWire struct {
	Interaction   string `json:"interaction"`
	URL           string `json:"url"`
	Message       string `json:"message"`
	UserCode      string `json:"userCode"`
	CallbackInput string `json:"callbackInput"`
	FlowID        string `json:"flowId"`
	FlowExpiresAt int64  `json:"flowExpiresAt"`
}

type authStatusWire struct {
	FlowID    string `json:"flowId"`
	State     string `json:"state"`
	ExpiresAt int64  `json:"expiresAt"`
	Reason    string `json:"reason"`
}

func requireAttendedTier(t *testing.T) {
	t.Helper()

	if os.Getenv(envRunIntegration) != "1" || os.Getenv(envRunAttended) != "1" {
		t.Skipf("set %s=1 and %s=1 to run the attended provider-auth tier", envRunIntegration, envRunAttended)
	}
}

// attendedCodexHome is a fresh home the login installs into. CODEX_HOME must
// pre-exist — codex exits 1 rather than creating it — and a home under the
// system temp directory makes codex decline to create its helper binaries, so
// the home is created here under the repository's own scratch tree.
func attendedCodexHome(t *testing.T) string {
	t.Helper()

	base, err := filepath.Abs(filepath.Join("..", ".tmp", "integration-attended-home"))
	if err != nil {
		t.Fatalf("resolve attended home base: %v", err)
	}

	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("create attended home base: %v", err)
	}

	home, err := os.MkdirTemp(base, "home-*")
	if err != nil {
		t.Fatalf("create attended home: %v", err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(home) })

	return home
}

func callAuthLeg(t *testing.T, ctx context.Context, conn *acp.ClientSideConnection, method string, params any, out any) error {
	t.Helper()

	raw, err := conn.CallExtension(ctx, method, params)
	if err != nil {
		return err
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode %s result: %v", method, err)
	}

	return nil
}

// TestAttendedProviderAuthDeviceLoginCompletes drives the one leg no unattended
// suite can prove: a ChatGPT device login a human approves at the provider. It
// fails rather than skips when nobody answers, because a silently green attended
// suite is worse than a red one.
func TestAttendedProviderAuthDeviceLoginCompletes(t *testing.T) {
	requireAttendedTier(t)

	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()

	home := attendedCodexHome(t)
	authRoot := t.TempDir()
	client := &recordingClient{}

	options := []codexacp.Option{
		codexacp.WithHome(home),
		codexacp.WithProviderAuthRoot(authRoot),
		codexacp.WithProviderAuthDirectHome(home),
	}

	// The attended tier runs under an operator's hands, and Darwin fails every
	// native launch closed without this explicit opt-in.
	if runtime.GOOS == "darwin" {
		options = append(options, codexacp.WithDarwinBestEffortContainment())
	}

	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{}, options...)

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	sessionID := string(session.SessionId)

	var methods authMethodsWire
	if err := callAuthLeg(t, ctx, conn, "_codex/auth/methods", map[string]any{"sessionId": sessionID}, &methods); err != nil {
		t.Fatalf("_codex/auth/methods: %v", err)
	}

	provider, method := deviceMethod(t, methods)

	var authorization authAuthorizeWire

	err = callAuthLeg(t, ctx, conn, "_codex/auth/authorize", map[string]any{
		"sessionId":          sessionID,
		"providerId":         provider,
		"connectionId":       "attended-connection",
		"methodsGeneration":  methods.Generation,
		"method":             method,
		"authorizeRequestId": "attended-request",
	}, &authorization)
	if err != nil {
		t.Fatalf("_codex/auth/authorize: %v", err)
	}

	if authorization.Interaction != "wait" {
		t.Fatalf("interaction = %q, want wait", authorization.Interaction)
	}

	t.Logf("open %s and enter code %s", authorization.URL, authorization.UserCode)

	deadline := time.UnixMilli(authorization.FlowExpiresAt)

	for time.Now().Before(deadline) {
		var status authStatusWire

		err := callAuthLeg(t, ctx, conn, "_codex/auth/status", map[string]any{
			"sessionId":  sessionID,
			"providerId": provider,
			"flowId":     authorization.FlowID,
		}, &status)
		if err != nil {
			t.Fatalf("_codex/auth/status: %v", err)
		}

		switch status.State {
		case "authenticated":
			assertAttendedHarvest(t, ctx, conn, sessionID, provider, authorization.FlowID)

			return
		case "pending":
			time.Sleep(5 * time.Second)
		default:
			t.Fatalf("flow reached %s/%s", status.State, status.Reason)
		}
	}

	t.Fatal("nobody approved the device login before its deadline")
}

func deviceMethod(t *testing.T, methods authMethodsWire) (string, string) {
	t.Helper()

	for provider, entries := range methods.Providers {
		for _, entry := range entries {
			if entry.Type == "oauth" {
				return provider, entry.ID
			}
		}
	}

	t.Fatal("the catalog published no oauth method")

	return "", ""
}

// assertAttendedHarvest proves the completed login is both resident and
// harvestable exactly once, then clears it from the home it installed into.
func assertAttendedHarvest(t *testing.T, ctx context.Context, conn *acp.ClientSideConnection, sessionID string, provider string, flowID string) {
	t.Helper()

	var inventory struct {
		Entries []struct {
			ProviderID        string `json:"providerId"`
			ConnectionID      string `json:"connectionId"`
			BindingGeneration int64  `json:"bindingGeneration"`
			ProofSource       string `json:"proofSource"`
		} `json:"entries"`
	}

	if err := callAuthLeg(t, ctx, conn, "_codex/auth/inventory", map[string]any{"sessionId": sessionID}, &inventory); err != nil {
		t.Fatalf("_codex/auth/inventory: %v", err)
	}

	if len(inventory.Entries) != 1 || inventory.Entries[0].ProofSource != "confirmed_present" {
		t.Fatalf("inventory = %+v", inventory.Entries)
	}

	var harvested struct {
		ConnectionID string `json:"connectionId"`
		Credential   struct {
			Type            string `json:"type"`
			Access          string `json:"access"`
			Refresh         string `json:"refresh"`
			AccessExpiresAt int64  `json:"accessExpiresAt"`
		} `json:"credential"`
	}

	err := callAuthLeg(t, ctx, conn, "_codex/auth/credential", map[string]any{
		"sessionId":  sessionID,
		"providerId": provider,
		"flowId":     flowID,
	}, &harvested)
	if err != nil {
		t.Fatalf("_codex/auth/credential: %v", err)
	}

	if harvested.Credential.Type != "oauth" || harvested.Credential.Access == "" || harvested.Credential.Refresh == "" {
		t.Fatal("the harvest returned no reinjectable oauth material")
	}

	if harvested.Credential.AccessExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("accessExpiresAt = %d is already past", harvested.Credential.AccessExpiresAt)
	}

	if err := callAuthLeg(t, ctx, conn, "_codex/auth/disconnect", map[string]any{
		"sessionId":         sessionID,
		"providerId":        provider,
		"connectionId":      inventory.Entries[0].ConnectionID,
		"bindingGeneration": inventory.Entries[0].BindingGeneration,
	}, nil); err != nil {
		t.Fatalf("_codex/auth/disconnect: %v", err)
	}
}
