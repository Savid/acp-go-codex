package codexacp

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/savid/acp-go-codex/internal/codex"
)

const testStoredLogin = `{
  "auth_mode": "chatgpt",
  "last_refresh": "2026-07-21T00:01:59.716288Z",
  "OPENAI_API_KEY": null,
  "tokens": {
    "access_token": "canary-access",
    "refresh_token": "canary-refresh",
    "id_token": "canary-identity",
    "account_id": "canary-account"
  }
}`

func restoreCredentialHooks(t *testing.T) {
	t.Helper()

	readFile, keystore := authReadFile, authKeystoreRead

	t.Cleanup(func() { authReadFile, authKeystoreRead = readFile, keystore })
}

func seedStoredLogin(t *testing.T, home string, contents string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(home, authAuthFileName), []byte(contents), 0o600); err != nil {
		t.Fatalf("seed credential store: %v", err)
	}
}

// authenticatedFlow drives a device login to authenticated and returns its id.
func (f *providerAuthFixture) authenticatedFlow(t *testing.T) string {
	t.Helper()

	flow := f.authorize(t, authMethodDeviceCode, "request-1")
	f.client.account = codex.Account{AuthMode: codex.AuthModeChatGPT}

	f.broker.loginCompleted(t.Context(), codex.LoginCompletion{LoginID: "login-1", Success: true})

	if state := f.status(t, flow.FlowID).State; state != authStateAuthenticated {
		t.Fatalf("state = %q, want authenticated", state)
	}

	return flow.FlowID
}

func TestProviderCredentialMarshalsTheFlatWireForm(t *testing.T) {
	credential := ProviderCredential{
		Type: ProviderCredentialOAuth,
		OAuth: &ProviderOAuthCredential{
			Refresh:         "refresh",
			Access:          "access",
			AccessExpiresAt: 1700000000000,
			AccountID:       "account",
		},
	}

	raw, err := json.Marshal(credential)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if fields["type"] != string(ProviderCredentialOAuth) {
		t.Fatalf("type = %v", fields["type"])
	}

	if _, present := fields["enterpriseUrl"]; present {
		t.Fatal("an absent optional field was emitted")
	}

	if _, err := json.Marshal(ProviderCredential{Type: ProviderCredentialOAuth}); err == nil {
		t.Fatal("a credential with no variant marshalled")
	}
}

func TestProviderCredentialDecodesStrictly(t *testing.T) {
	valid := `{"type":"oauth","refresh":"r","access":"a","accessExpiresAt":1}`

	var credential ProviderCredential
	if err := json.Unmarshal([]byte(valid), &credential); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if credential.OAuth.Refresh != "r" || credential.OAuth.Access != "a" {
		t.Fatalf("credential = %+v", credential.OAuth)
	}

	cases := map[string]string{
		"unknown":    `{"type":"oauth","refresh":"r","access":"a","accessExpiresAt":1,"extra":true}`,
		"duplicate":  `{"type":"oauth","refresh":"r","access":"a","accessExpiresAt":1,"refresh":"r"}`,
		"variant":    `{"type":"api","key":"k"}`,
		"norefresh":  `{"type":"oauth","refresh":"","access":"a","accessExpiresAt":1}`,
		"noaccess":   `{"type":"oauth","refresh":"r","access":"","accessExpiresAt":1}`,
		"noexpiry":   `{"type":"oauth","refresh":"r","access":"a","accessExpiresAt":0}`,
		"malformed":  `{"type":`,
		"nonobject":  `[]`,
		"badnesting": `{"type":"oauth","refresh":"r","access":"a","accessExpiresAt":1,`,
	}

	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			var decoded ProviderCredential
			if err := json.Unmarshal([]byte(payload), &decoded); err == nil {
				t.Fatal("a rejected payload decoded")
			}
		})
	}
}

func TestRejectDuplicateJSONKeysWalkFailures(t *testing.T) {
	if err := rejectDuplicateJSONKeys([]byte(`x`)); err == nil {
		t.Fatal("a non-object walked cleanly")
	}

	if err := rejectDuplicateJSONKeys([]byte(`{"a":1,`)); err == nil {
		t.Fatal("a truncated object walked cleanly")
	}

	if err := rejectDuplicateJSONKeys([]byte(`{"a":}`)); err == nil {
		t.Fatal("an undecodable value walked cleanly")
	}
}

func TestAuthCredentialHarvestsTheFencedSlot(t *testing.T) {
	restoreAuthClock(t)

	fixture := newProviderAuthFixture(t)
	seedStoredLogin(t, fixture.home, testStoredLogin)

	flowID := fixture.authenticatedFlow(t)

	now := time.Now()
	authNow = func() time.Time { return now }

	result, err := fixture.call(t, AuthCredentialMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"flowId":     flowID,
	})
	if err != nil {
		t.Fatalf("credential: %v", err)
	}

	harvested, _ := result.(authCredentialResult)
	if harvested.ConnectionID != "connection-1" || harvested.Revision != 1 || harvested.BindingGeneration != 1 {
		t.Fatalf("binding = %+v", harvested)
	}

	if harvested.Credential.OAuth.Access != "canary-access" || harvested.Credential.OAuth.Refresh != "canary-refresh" {
		t.Fatal("the harvested credential is not the stored one")
	}

	if harvested.Credential.OAuth.AccountID != "canary-account" {
		t.Fatalf("accountId = %q", harvested.Credential.OAuth.AccountID)
	}

	want := now.Add(codexAccessTokenLifetime).UnixMilli()
	if harvested.Credential.OAuth.AccessExpiresAt != want {
		t.Fatalf("accessExpiresAt = %d, want %d", harvested.Credential.OAuth.AccessExpiresAt, want)
	}

	raw, err := json.Marshal(harvested)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}

	for _, banned := range []string{"canary-identity", "last_refresh", "id_token"} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("the harvest forwarded native bookkeeping %q", banned)
		}
	}
}

func TestAuthCredentialHarvestsAtMostOncePerFlow(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	seedStoredLogin(t, fixture.home, testStoredLogin)

	flowID := fixture.authenticatedFlow(t)

	params := map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"flowId":     flowID,
	}

	if _, err := fixture.call(t, AuthCredentialMethod, params); err != nil {
		t.Fatalf("credential: %v", err)
	}

	_, err := fixture.call(t, AuthCredentialMethod, params)
	if err == nil {
		t.Fatal("a second harvest succeeded")
	}

	requireAuthCause(t, err, authCauseFlowState)
}

func TestAuthCredentialRefusesAnIncompleteFlow(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	seedStoredLogin(t, fixture.home, testStoredLogin)

	flow := fixture.authorize(t, authMethodDeviceCode, "request-1")

	_, err := fixture.call(t, AuthCredentialMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"flowId":     flow.FlowID,
	})
	if err == nil {
		t.Fatal("a pending flow was harvested")
	}

	requireAuthCause(t, err, authCauseFlowState)

	if fixture.status(t, flow.FlowID).State != authStatePending {
		t.Fatal("a flow-state refusal consumed the flow")
	}
}

func TestAuthCredentialAddressingFailures(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	seedStoredLogin(t, fixture.home, testStoredLogin)

	flowID := fixture.authenticatedFlow(t)

	base := map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"flowId":     flowID,
	}

	for _, field := range []string{"sessionId", "providerId", "flowId"} {
		params := map[string]any{}
		for key, value := range base {
			params[key] = value
		}

		delete(params, field)

		if _, err := fixture.call(t, AuthCredentialMethod, params); err == nil {
			t.Fatalf("credential accepted a request missing %q", field)
		}
	}

	for _, extra := range []string{"connectionId", "revision", "bindingGeneration"} {
		params := map[string]any{"extra": extra}
		for key, value := range base {
			params[key] = value
		}

		params[extra] = "supplied"
		delete(params, "extra")

		_, err := fixture.call(t, AuthCredentialMethod, params)
		if err == nil {
			t.Fatalf("credential accepted a caller-supplied %q", extra)
		}

		requireInvalidField(t, err, extra)
	}
}

func TestAuthCredentialFenceMismatchFailsClosed(t *testing.T) {
	cases := map[string]func(authLedgerRecord) authLedgerRecord{
		"connection": func(record authLedgerRecord) authLedgerRecord {
			record.ConnectionID = "other"

			return record
		},
		"revision": func(record authLedgerRecord) authLedgerRecord {
			record.Revision += 5

			return record
		},
		"generation": func(record authLedgerRecord) authLedgerRecord {
			record.BindingGeneration += 5

			return record
		},
		"flow": func(record authLedgerRecord) authLedgerRecord {
			record.FlowID = "other"

			return record
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newProviderAuthFixture(t)
			seedStoredLogin(t, fixture.home, testStoredLogin)

			flowID := fixture.authenticatedFlow(t)

			record, _, err := fixture.broker.ledger.read(authProviderOpenAI)
			if err != nil {
				t.Fatalf("ledger read: %v", err)
			}

			if writeErr := fixture.broker.ledger.write(mutate(record)); writeErr != nil {
				t.Fatalf("rewrite ledger: %v", writeErr)
			}

			_, err = fixture.call(t, AuthCredentialMethod, map[string]any{
				"sessionId":  fixture.sessionID,
				"providerId": authProviderOpenAI,
				"flowId":     flowID,
			})
			if err == nil {
				t.Fatal("a mismatched fence was harvested")
			}

			requireAuthCause(t, err, authCauseHarvestFailed)
		})
	}
}

func TestAuthCredentialFailsWithoutALedgerEntry(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	seedStoredLogin(t, fixture.home, testStoredLogin)

	flowID := fixture.authenticatedFlow(t)

	if err := os.Remove(fixture.broker.ledger.path(authProviderOpenAI)); err != nil {
		t.Fatalf("remove ledger entry: %v", err)
	}

	_, err := fixture.call(t, AuthCredentialMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"flowId":     flowID,
	})
	if err == nil {
		t.Fatal("credential answered with no ledger entry")
	}

	requireAuthCause(t, err, authCauseHarvestFailed)
}

func TestAuthCredentialFailsWhenTheStoreCannotBeRead(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	flowID := fixture.authenticatedFlow(t)

	_, err := fixture.call(t, AuthCredentialMethod, map[string]any{
		"sessionId":  fixture.sessionID,
		"providerId": authProviderOpenAI,
		"flowId":     flowID,
	})
	if err == nil {
		t.Fatal("credential answered with no stored login")
	}

	requireAuthCause(t, err, authCauseHarvestFailed)
}

func TestReadStoredCredentialAcrossStoreModes(t *testing.T) {
	fixture := newProviderAuthFixture(t)
	seedStoredLogin(t, fixture.home, testStoredLogin)

	stored, err := fixture.broker.readStoredCredential()
	if err != nil {
		t.Fatalf("file mode: %v", err)
	}

	if stored.AccessToken != "canary-access" {
		t.Fatalf("stored = %+v", stored)
	}

	for _, mode := range []string{authStoreModeAuto, authStoreModeEphemeral, "invented"} {
		t.Run(mode, func(t *testing.T) {
			gated := newProviderAuthFixture(t, WithCodexConfigOverrides(map[string]any{authStoreConfigKey: mode}))
			seedStoredLogin(t, gated.home, testStoredLogin)

			if _, err := gated.broker.readStoredCredential(); err == nil {
				t.Fatalf("%s harvested a store with no determinate authority", mode)
			}
		})
	}
}

func TestReadStoredCredentialFromTheKeystore(t *testing.T) {
	restoreCredentialHooks(t)

	fixture := newProviderAuthFixture(t, WithCodexConfigOverrides(map[string]any{authStoreConfigKey: authStoreModeKeyring}))

	var lookups []string

	authKeystoreRead = func(service string, account string) (string, error) {
		lookups = append(lookups, service+"/"+account)

		return testStoredLogin, nil
	}

	stored, err := fixture.broker.readStoredCredential()
	if err != nil {
		t.Fatalf("keyring mode: %v", err)
	}

	if stored.RefreshToken != "canary-refresh" {
		t.Fatalf("stored = %+v", stored)
	}

	want := authKeystoreService + "/" + authKeystoreAccount(fixture.home)
	if len(lookups) != 1 || lookups[0] != want {
		t.Fatalf("lookups = %v, want %q", lookups, want)
	}

	authKeystoreRead = func(string, string) (string, error) { return "", errors.New("absent") }

	if _, err := fixture.broker.readStoredCredential(); err == nil {
		t.Fatal("an absent keystore item was harvested")
	}
}

func TestReadStoredCredentialRejectsUnusableStores(t *testing.T) {
	cases := map[string]string{
		"malformed":  `{`,
		"apikeymode": `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-canary"}`,
		"notokens":   `{"auth_mode":"chatgpt"}`,
		"noaccess":   `{"auth_mode":"chatgpt","tokens":{"refresh_token":"r"}}`,
		"norefresh":  `{"auth_mode":"chatgpt","tokens":{"access_token":"a"}}`,
	}

	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newProviderAuthFixture(t)
			seedStoredLogin(t, fixture.home, contents)

			if _, err := fixture.broker.readStoredCredential(); err == nil {
				t.Fatal("an unusable store was harvested")
			}
		})
	}
}

func TestAuthKeystoreAccountPartitionsByHome(t *testing.T) {
	if authKeystoreAccount("/a") == authKeystoreAccount("/b") {
		t.Fatal("two homes share one keystore account")
	}

	if !strings.HasPrefix(authKeystoreAccount("/a"), "cli|") {
		t.Fatalf("account = %q", authKeystoreAccount("/a"))
	}

	if len(authKeystoreAccount("/a")) != len("cli|")+16 {
		t.Fatalf("account = %q", authKeystoreAccount("/a"))
	}
}

func TestResolveAuthStoreMode(t *testing.T) {
	home := t.TempDir()

	if mode := resolveAuthStoreMode(Options{}, home); mode != authStoreModeFile {
		t.Fatalf("mode with no config = %q", mode)
	}

	override := Options{Config: map[string]any{authStoreConfigKey: authStoreModeKeyring}}
	if mode := resolveAuthStoreMode(override, home); mode != authStoreModeKeyring {
		t.Fatalf("mode with an override = %q", mode)
	}

	empty := Options{Config: map[string]any{authStoreConfigKey: ""}}
	if mode := resolveAuthStoreMode(empty, home); mode != authStoreModeFile {
		t.Fatalf("mode with an empty override = %q", mode)
	}

	nonString := Options{Config: map[string]any{authStoreConfigKey: 7}}
	if mode := resolveAuthStoreMode(nonString, home); mode != authStoreModeFile {
		t.Fatalf("mode with a non-string override = %q", mode)
	}

	config := filepath.Join(home, authConfigFileName)
	if err := os.WriteFile(config, []byte("model = \"gpt\"\ncli_auth_credentials_store = \"keyring\"\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if mode := resolveAuthStoreMode(Options{}, home); mode != authStoreModeKeyring {
		t.Fatalf("mode from config = %q", mode)
	}

	if err := os.WriteFile(config, []byte("model = \"gpt\"\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if mode := resolveAuthStoreMode(Options{}, home); mode != authStoreModeFile {
		t.Fatalf("mode from a config without the key = %q", mode)
	}
}

func TestTopLevelTOMLScalar(t *testing.T) {
	cases := map[string]struct {
		contents string
		want     string
		found    bool
	}{
		"quoted":     {"cli_auth_credentials_store = \"keyring\"\n", "keyring", true},
		"bare":       {"cli_auth_credentials_store = keyring # note\n", "keyring", true},
		"spaced":     {"  cli_auth_credentials_store   =   \"auto\"  \n", "auto", true},
		"unbalanced": {"cli_auth_credentials_store = \"keyring\n", "keyring", true},
		"aftertable": {"[tui]\ncli_auth_credentials_store = \"keyring\"\n", "", false},
		"absent":     {"model = \"gpt\"\n", "", false},
		"noequals":   {"cli_auth_credentials_store\n", "", false},
	}

	for name, testCase := range cases {
		got, found := topLevelTOMLScalar(testCase.contents, authStoreConfigKey)
		if found != testCase.found || (found && got != testCase.want) {
			t.Fatalf("%s: got %q/%v, want %q/%v", name, got, found, testCase.want, testCase.found)
		}
	}
}

func TestProviderCredentialRejectsAnUnacceptedVariantWithNoExtraFields(t *testing.T) {
	var credential ProviderCredential
	if err := json.Unmarshal([]byte(`{"type":"api"}`), &credential); err == nil {
		t.Fatal("an unaccepted variant decoded")
	}
}

// keystoreFixtureMarker is written by the credential-residence fixture's
// entrypoint. The matrix below needs a live Secret Service, so it runs inside
// that container and nowhere else.
const keystoreFixtureMarker = "/run/acp-go-codex-keystore/marker"

// TestKeystoreResidenceMatrix proves which credential store the read path wins
// from under each configured mode. Both stores hold a distinguishable canary, so
// a mode that read the wrong one is visible rather than merely unproven.
func TestKeystoreResidenceMatrix(t *testing.T) {
	if _, err := os.Stat(keystoreFixtureMarker); err != nil {
		t.Skip("the credential-residence matrix runs inside the keystore fixture container")
	}

	cases := map[string]struct {
		mode    string
		wins    string
		harvest bool
	}{
		"file":      {mode: authStoreModeFile, wins: "file-canary-access", harvest: true},
		"keyring":   {mode: authStoreModeKeyring, wins: "keystore-canary-access", harvest: true},
		"auto":      {mode: authStoreModeAuto},
		"ephemeral": {mode: authStoreModeEphemeral},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newProviderAuthFixture(t, WithCodexConfigOverrides(map[string]any{
				authStoreConfigKey: testCase.mode,
			}))

			seedStoredLogin(t, fixture.home, storedLoginWithAccess("file-canary-access"))
			seedKeystoreCanary(t, fixture.home, storedLoginWithAccess("keystore-canary-access"))

			stored, err := fixture.broker.readStoredCredential()
			if !testCase.harvest {
				if err == nil {
					t.Fatalf("%s harvested a store with no determinate authority", testCase.mode)
				}

				return
			}

			if err != nil {
				t.Fatalf("%s read: %v", testCase.mode, err)
			}

			if stored.AccessToken != testCase.wins {
				t.Fatalf("%s read %q, want %q", testCase.mode, stored.AccessToken, testCase.wins)
			}
		})
	}
}

func storedLoginWithAccess(access string) string {
	return `{"auth_mode":"chatgpt","tokens":{"access_token":"` + access +
		`","refresh_token":"canary-refresh","account_id":"canary-account"}}`
}

// seedKeystoreCanary plants canary material through the platform tool rather
// than through the read path, so the assertion is not a round trip of one
// library against itself.
func seedKeystoreCanary(t *testing.T, home string, contents string) {
	t.Helper()

	command := exec.Command("secret-tool", "store", "--label=codex-canary",
		"service", authKeystoreService,
		"username", authKeystoreAccount(home))
	command.Stdin = strings.NewReader(contents)

	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("seed keystore canary: %v: %s", err, output)
	}
}
