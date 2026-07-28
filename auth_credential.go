package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ProviderCredentialType discriminates the closed credential union.
type ProviderCredentialType string

// ProviderCredentialOAuth is the only variant codex emits: its brokered login
// yields a ChatGPT refresh/access pair, and its API-key login yields a value the
// harness reads from the environment rather than a credential this surface
// hands back.
const ProviderCredentialOAuth ProviderCredentialType = "oauth"

// ProviderOAuthCredential is the reinjection material of a completed OAuth
// login. Native bookkeeping — identity tokens, refresh timestamps, plan labels —
// is owned by the harness and never crosses this boundary.
type ProviderOAuthCredential struct {
	Refresh         string `json:"refresh"`
	Access          string `json:"access"`
	AccessExpiresAt int64  `json:"accessExpiresAt"`
	AccountID       string `json:"accountId,omitempty"`
	EnterpriseURL   string `json:"enterpriseUrl,omitempty"`
}

// ProviderCredential is the closed, flat credential union. It marshals to one
// object whose type selects exactly one variant's fields.
type ProviderCredential struct {
	Type  ProviderCredentialType
	OAuth *ProviderOAuthCredential
}

type providerOAuthCredentialWire struct {
	Type            ProviderCredentialType `json:"type"`
	Refresh         string                 `json:"refresh"`
	Access          string                 `json:"access"`
	AccessExpiresAt int64                  `json:"accessExpiresAt"`
	AccountID       string                 `json:"accountId,omitempty"`
	EnterpriseURL   string                 `json:"enterpriseUrl,omitempty"`
}

// MarshalJSON writes the flat wire form.
func (credential ProviderCredential) MarshalJSON() ([]byte, error) {
	if credential.Type != ProviderCredentialOAuth || credential.OAuth == nil {
		return nil, errors.New("provider credential carries no accepted variant")
	}

	return json.Marshal(providerOAuthCredentialWire{
		Type:            ProviderCredentialOAuth,
		Refresh:         credential.OAuth.Refresh,
		Access:          credential.OAuth.Access,
		AccessExpiresAt: credential.OAuth.AccessExpiresAt,
		AccountID:       credential.OAuth.AccountID,
		EnterpriseURL:   credential.OAuth.EnterpriseURL,
	})
}

// UnmarshalJSON decodes strictly: an unknown field, a duplicate field, an empty
// required string, and any variant this package does not emit are rejected
// rather than partially decoded.
func (credential *ProviderCredential) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()

	var wire providerOAuthCredentialWire
	if err := decoder.Decode(&wire); err != nil {
		return fmt.Errorf("decode provider credential: %w", err)
	}

	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}

	if wire.Type != ProviderCredentialOAuth {
		return fmt.Errorf("provider credential type %q is not accepted", wire.Type)
	}

	if wire.Refresh == "" || wire.Access == "" || wire.AccessExpiresAt <= 0 {
		return errors.New("provider credential is missing required oauth fields")
	}

	credential.Type = ProviderCredentialOAuth
	credential.OAuth = &ProviderOAuthCredential{
		Refresh:         wire.Refresh,
		Access:          wire.Access,
		AccessExpiresAt: wire.AccessExpiresAt,
		AccountID:       wire.AccountID,
		EnterpriseURL:   wire.EnterpriseURL,
	}

	return nil
}

// rejectDuplicateJSONKeys walks the object once, because encoding/json lets a
// repeated key silently win rather than reporting it.
func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))

	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("decode provider credential: %w", err)
	}

	seen := map[string]struct{}{}

	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode provider credential: %w", err)
		}

		key, _ := token.(string)
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("provider credential repeats field %q", key)
		}

		seen[key] = struct{}{}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("decode provider credential: %w", err)
		}
	}

	return nil
}

// codexAccessTokenLifetime is the ChatGPT access-token lifetime codex's store
// records nothing about: the store carries only an ISO-8601 last_refresh, and
// the exact expiry lives inside a token this surface never decodes. Measured at
// codex 0.145.0 against a completed ChatGPT login, whose access token was issued
// at last_refresh and expired 864,000 seconds later.
const codexAccessTokenLifetime = 240 * time.Hour

// Credential-store selection. The store is a configured choice rather than a
// platform fact, so the leg reads the configured mode instead of assuming the
// default: an operator who set keyring or ephemeral has a worker whose
// credential a file-shaped read can never see.
const (
	authStoreConfigKey     = "cli_auth_credentials_store"
	authStoreModeFile      = "file"
	authStoreModeKeyring   = "keyring"
	authStoreModeAuto      = "auto"
	authStoreModeEphemeral = "ephemeral"

	authAuthFileName   = "auth.json"
	authConfigFileName = "config.toml"
)

var authReadFile = os.ReadFile

// Native store keys. Codex writes its own spelling, so the read pulls the
// allowlisted keys by name rather than mapping the whole object onto a struct.
const (
	storeKeyAuthMode     = "auth_mode"
	storeKeyTokens       = "tokens"
	storeKeyAccessToken  = "access_token"  // #nosec G101 -- native field name, not a token.
	storeKeyRefreshToken = "refresh_token" // #nosec G101 -- native field name, not a token.
	storeKeyAccountID    = "account_id"
)

// codexStoredLogin is the credential-bearing native store projected onto the
// keys this surface reinjects. Every other key — the identity token, the API
// key, the refresh timestamp — is dropped rather than forwarded.
type codexStoredLogin struct {
	AuthMode     string
	AccessToken  string
	RefreshToken string
	AccountID    string
}

// decodeStoredLogin projects the native store through the allowlist.
func decodeStoredLogin(raw []byte) (codexStoredLogin, error) {
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return codexStoredLogin{}, fmt.Errorf("decode codex credential store: %w", err)
	}

	stored := codexStoredLogin{AuthMode: storeString(fields, storeKeyAuthMode)}

	tokens, _ := fields[storeKeyTokens].(map[string]any)
	stored.AccessToken = storeString(tokens, storeKeyAccessToken)
	stored.RefreshToken = storeString(tokens, storeKeyRefreshToken)
	stored.AccountID = storeString(tokens, storeKeyAccountID)

	return stored, nil
}

func storeString(fields map[string]any, key string) string {
	value, _ := fields[key].(string)

	return value
}

type authCredentialResult struct {
	ConnectionID      string             `json:"connectionId"`
	Revision          int64              `json:"revision"`
	BindingGeneration int64              `json:"bindingGeneration"`
	Credential        ProviderCredential `json:"credential"`
}

// credential harvests exactly the slot this connection's own ledger entry
// names, once per flow, from the credential store the home is configured to
// use. A credential the adapter did not install is not the adapter's to hand
// out, whatever is sitting in the slot.
func (p *providerAuth) credential(_ context.Context, params json.RawMessage) (any, error) {
	fields, err := authParamFields(params, authFieldSessionID, authFieldProviderID, authFieldFlowID)
	if err != nil {
		return nil, err
	}

	sessionID, err := authRequiredString(fields, authFieldSessionID)
	if err != nil {
		return nil, err
	}

	providerID, err := authRequiredString(fields, authFieldProviderID)
	if err != nil {
		return nil, err
	}

	flowID, err := authRequiredString(fields, authFieldFlowID)
	if err != nil {
		return nil, err
	}

	session, err := p.authSession(sessionID)
	if err != nil {
		return nil, err
	}

	flow, err := p.addressFlow(session.id, providerID, flowID)
	if err != nil {
		return nil, err
	}

	if claimErr := p.claimHarvest(flow); claimErr != nil {
		return nil, claimErr
	}

	record, ok, err := p.ledger.read(providerID)
	if err != nil || !ok {
		return nil, p.harvestFailed(flow)
	}

	if record.FlowID != flow.id ||
		record.ConnectionID != flow.connectionID ||
		record.Revision != flow.revision ||
		record.BindingGeneration != flow.bindingGeneration {
		return nil, p.harvestFailed(flow)
	}

	stored, err := p.readStoredCredential()
	if err != nil {
		return nil, p.harvestFailed(flow)
	}

	return authCredentialResult{
		ConnectionID:      record.ConnectionID,
		Revision:          record.Revision,
		BindingGeneration: record.BindingGeneration,
		Credential: ProviderCredential{
			Type: ProviderCredentialOAuth,
			OAuth: &ProviderOAuthCredential{
				Refresh:         stored.RefreshToken,
				Access:          stored.AccessToken,
				AccessExpiresAt: authNow().Add(codexAccessTokenLifetime).UnixMilli(),
				AccountID:       stored.AccountID,
			},
		},
	}, nil
}

// claimHarvest admits the single harvest a completed flow allows and holds the
// claim for the duration of the attempt, so two concurrent calls cannot both
// read the slot. A flow-state refusal is one the adapter made itself, so it
// consumes nothing and performs no transition.
func (p *providerAuth) claimHarvest(flow *authFlow) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if flow.state != authStateAuthenticated && flow.state != authStateSaved {
		return authFailed(authCauseFlowState, flow.providerID, flow.method.ID, flow.id)
	}

	if flow.harvested {
		return authFailed(authCauseFlowState, flow.providerID, flow.method.ID, flow.id)
	}

	flow.harvested = true

	return nil
}

// harvestFailed releases the claim the attempt was holding and returns the
// leg's closed error. At-most-once governs the credential a harvest hands back,
// and an attempt that handed back nothing has nothing to be once about — so
// spending the claim on it would answer every retry flow_state and make the
// cause that actually stopped the harvest unrecoverable. The flow itself is
// left where it was: credential runs only against an already-terminal flow, and
// terminalizing one of those again is what the no-transition rows forbid.
func (p *providerAuth) harvestFailed(flow *authFlow) error {
	p.mu.Lock()
	flow.harvested = false
	p.mu.Unlock()

	return authFailed(authCauseHarvestFailed, flow.providerID, flow.method.ID, flow.id)
}

// readStoredCredential reads the consented home's configured credential store.
// Two modes have no readable answer and both fail closed rather than harvest a
// store that may not be authoritative: auto resolves to whichever store answers
// first, and ephemeral leaves nothing anywhere.
func (p *providerAuth) readStoredCredential() (codexStoredLogin, error) {
	mode, err := resolveAuthStoreMode(p.agent.options, p.directHome)
	if err != nil {
		return codexStoredLogin{}, err
	}

	var raw []byte

	switch mode {
	case authStoreModeFile:
		raw, err = authReadFile(filepath.Join(p.directHome, authAuthFileName))
	case authStoreModeKeyring:
		raw, err = readKeystoreCredential(p.directHome)
	default:
		return codexStoredLogin{}, fmt.Errorf("credential store mode %q has no determinate authority", mode)
	}

	if err != nil {
		return codexStoredLogin{}, fmt.Errorf("read codex credential store: %w", err)
	}

	stored, err := decodeStoredLogin(raw)
	if err != nil {
		return codexStoredLogin{}, err
	}

	if stored.AuthMode != codexAuthModeChatGPT {
		return codexStoredLogin{}, errors.New("codex credential store holds no ChatGPT login")
	}

	if stored.AccessToken == "" || stored.RefreshToken == "" {
		return codexStoredLogin{}, errors.New("codex credential store holds an incomplete ChatGPT login")
	}

	return stored, nil
}

// codexAuthModeChatGPT is the auth_mode a completed ChatGPT login writes.
const codexAuthModeChatGPT = "chatgpt"

// resolveAuthStoreMode reads the configured store rather than assuming the
// default. An adapter-supplied override wins because it is what the app-server
// itself runs under; otherwise the value comes from the home's own config file.
// A home with no config file is the documented default; a config file that
// cannot be read establishes nothing, and a home configured keyring or
// ephemeral whose selector went unread would take a file-shaped harvest of a
// store that is not authoritative.
func resolveAuthStoreMode(options Options, home string) (string, error) {
	if value, ok := options.Config[authStoreConfigKey].(string); ok && value != "" {
		return value, nil
	}

	contents, err := authReadFile(filepath.Join(home, authConfigFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return authStoreModeFile, nil
		}

		return "", fmt.Errorf("read codex credential store mode: %w", err)
	}

	if value, ok := topLevelTOMLScalar(string(contents), authStoreConfigKey); ok {
		return value, nil
	}

	return authStoreModeFile, nil
}

// topLevelTOMLScalar reads one top-level scalar out of a config file. The
// selector is a top-level key by definition, so the scan stops at the first
// table header rather than pretending to parse the whole document.
func topLevelTOMLScalar(contents string, key string) (string, bool) {
	for line := range strings.SplitSeq(contents, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			return "", false
		}

		name, value, found := strings.Cut(trimmed, "=")
		if !found || strings.TrimSpace(name) != key {
			continue
		}

		return tomlScalarValue(value), true
	}

	return "", false
}

func tomlScalarValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if quoted, ok := strings.CutPrefix(trimmed, `"`); ok {
		if end := strings.Index(quoted, `"`); end >= 0 {
			return quoted[:end]
		}

		return quoted
	}

	if comment := strings.Index(trimmed, "#"); comment >= 0 {
		trimmed = trimmed[:comment]
	}

	return strings.TrimSpace(trimmed)
}

// slotOccupied asks the harness whether an account is resident, rather than
// inspecting a file. Which store is authoritative is a configured choice, and a
// well-formed file can sit beside a live keystore item indefinitely, so only the
// harness's own read distinguishes the two.
func (p *providerAuth) slotOccupied(ctx context.Context, session *session, providerID string) (bool, error) {
	client := session.nativeAuthClient()
	if client == nil {
		return false, authFailed(authCauseTransport, providerID, "", "")
	}

	callCtx, cancel := context.WithTimeout(ctx, authNativeCallTimeout)
	defer cancel()

	account, err := client.AccountRead(callCtx)
	if err != nil {
		return false, authFailed(authCauseHarvestFailed, providerID, "", "")
	}

	return account.AuthMode != "", nil
}
