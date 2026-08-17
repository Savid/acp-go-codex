package codexacp

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Method entry types.
const (
	authMethodTypeOAuth = "oauth"
	authMethodTypeAPI   = "api"
)

// authProviderOpenAI is the one account codex brokers: the OpenAI account its
// app-server signs in, addressed by the provider it belongs to rather than by
// the harness that drives it.
const authProviderOpenAI = "openai"

// Catalog method ids. Codex addresses a login by native type name, so an id is
// stable across catalogs rather than positional.
const (
	authMethodDeviceCode = "chatgptDeviceCode"
	authMethodAPIKey     = "apiKey"
)

// Display-field bounds. A value violating its bound is dropped, never
// truncated.
const (
	authMaxURLBytes      = 2048
	authMaxMessageBytes  = 2048
	authMaxUserCodeBytes = 64
	authMaxLabelBytes    = 256
)

// authMaxSecretBytes bounds the operator-supplied value a secret flow submits.
const authMaxSecretBytes = 1024

// authLabelAPIKey is the pinned display label of the API-key method, and the
// message a secret flow carries.
const authLabelAPIKey = "OpenAI API key" // #nosec G101 -- display label, not a credential.

// authUserCodePattern is anchored: a substring match accepts a code with markup
// wrapped around it.
var authUserCodePattern = regexp.MustCompile(`\A[A-Za-z0-9-]+\z`)

// authCatalogMethod is one entry of the current catalog.
type authCatalogMethod struct {
	ID    string
	Type  string
	Label string
}

type authMethodEntry struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Label string `json:"label"`
}

type authMethodsResult struct {
	Providers  map[string][]authMethodEntry `json:"providers"`
	Generation string                       `json:"generation"`
}

// pinnedAuthCatalog is codex's login catalog, pinned to the native login enum
// rather than enumerated: the app-server exposes no method-listing route, so
// the variant set is a version-pinned fact.
//
// Two of the five native variants never appear. `chatgpt` mints a loopback
// redirect_uri, and a loopback method is knowable from the catalog rather than
// only at mint time, so it is omitted outright. `amazonBedrock` is an unstable
// internal surface. `chatgptAuthTokens` is not a login a human drives: it
// installs an already-held credential, which arrives through the
// codex-chatgpt-auth-tokens ACP auth method instead.
var pinnedAuthCatalog = func() []authCatalogMethod {
	return []authCatalogMethod{
		{ID: authMethodDeviceCode, Type: authMethodTypeOAuth, Label: "Sign in with ChatGPT"},
		{ID: authMethodAPIKey, Type: authMethodTypeAPI, Label: authLabelAPIKey},
	}
}

// methods publishes the catalog and mints the generation naming this exact
// result. The catalog is what the adapter enumerates: it publishes no
// completeness claim and offers no free entry of an unlisted provider.
func (p *providerAuth) methods(_ context.Context, params json.RawMessage) (any, error) {
	fields, err := authParamFields(params, authFieldSessionID)
	if err != nil {
		return nil, err
	}

	sessionID, err := authRequiredString(fields, authFieldSessionID)
	if err != nil {
		return nil, err
	}

	if _, sessionErr := p.authSession(sessionID); sessionErr != nil {
		return nil, sessionErr
	}

	resolved, entries := buildAuthCatalog()
	if len(entries) == 0 {
		return nil, authFailed(authCauseNativeVeto, authProviderOpenAI, "", "")
	}

	generation, err := newAuthToken()
	if err != nil {
		return nil, authFailed(authCauseProcess, authProviderOpenAI, "", "")
	}

	p.mu.Lock()
	p.generation = generation
	p.catalog = map[string][]authCatalogMethod{authProviderOpenAI: resolved}
	p.mu.Unlock()

	return authMethodsResult{
		Providers:  map[string][]authMethodEntry{authProviderOpenAI: entries},
		Generation: generation,
	}, nil
}

// buildAuthCatalog applies the label bound to every pinned entry. An entry whose
// label violates its bound is omitted and the leg still succeeds; the leg fails
// closed only when no catalog survives at all.
func buildAuthCatalog() ([]authCatalogMethod, []authMethodEntry) {
	pinned := pinnedAuthCatalog()
	resolved := make([]authCatalogMethod, 0, len(pinned))
	entries := make([]authMethodEntry, 0, len(pinned))

	for _, method := range pinned {
		label, ok := authDisplayText(method.Label, authMaxLabelBytes)
		if !ok {
			continue
		}

		method.Label = label
		resolved = append(resolved, method)
		entries = append(entries, authMethodEntry{ID: method.ID, Type: method.Type, Label: label})
	}

	return resolved, entries
}

// authDisplayText normalises a native presentation string to NFC and measures
// its bounds and categories on that normalised form, which is also the form the
// adapter relays, persists, and returns. Normalising after measuring bounds a
// string nobody sends.
func authDisplayText(value string, maxBytes int) (string, bool) {
	normalized := norm.NFC.String(value)
	if normalized == "" || len(normalized) > maxBytes || !utf8.ValidString(normalized) {
		return "", false
	}

	for _, r := range normalized {
		if !authDisplayRune(r) {
			return "", false
		}
	}

	return normalized, true
}

// authDisplayRune restricts free text to Unicode categories L, N, P, S, and Zs.
// Every C* category is rejected, which is also what excludes every
// bidirectional override and embedding character: a label is the account name
// in the one place a human decides which account to bind.
func authDisplayRune(r rune) bool {
	switch {
	case unicode.IsLetter(r), unicode.IsNumber(r), unicode.IsPunct(r), unicode.IsSymbol(r):
		return true
	case unicode.Is(unicode.Zs, r):
		return true
	default:
		return false
	}
}

// authDisplayURL applies the url bound: at most 2048 bytes, scheme exactly
// https, no userinfo, no fragment.
func authDisplayURL(value string) (string, bool) {
	normalized := norm.NFC.String(value)
	if normalized == "" || len(normalized) > authMaxURLBytes {
		return "", false
	}

	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" || parsed.Host == "" {
		return "", false
	}

	return normalized, true
}

// authDisplayUserCode applies the userCode bound with an anchored pattern.
func authDisplayUserCode(value string) (string, bool) {
	normalized := norm.NFC.String(value)
	if normalized == "" || len(normalized) > authMaxUserCodeBytes || !authUserCodePattern.MatchString(normalized) {
		return "", false
	}

	return normalized, true
}

// authLoopbackHost reports whether a minted verification URL redirects through
// a loopback listener. The adapter cannot relay a URL whose completion lands on
// a socket the owner's browser cannot reach, so such a mint fails closed even
// though the catalog already omits codex's known-loopback variant.
func authLoopbackHost(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}

	if isLoopbackHostname(parsed.Hostname()) {
		return true
	}

	redirect := parsed.Query().Get("redirect_uri")
	if redirect == "" {
		return false
	}

	target, err := url.Parse(redirect)
	if err != nil {
		return false
	}

	return isLoopbackHostname(target.Hostname())
}

func isLoopbackHostname(host string) bool {
	switch strings.ToLower(host) {
	case "127.0.0.1", "::1", "localhost":
		return true
	default:
		return strings.HasSuffix(strings.ToLower(host), ".localhost")
	}
}

// validateAuthSecret bounds the operator-supplied value a secret flow submits.
// The rejected value is never persisted, logged, or echoed.
func validateAuthSecret(value string) error {
	if value == "" || len(value) > authMaxSecretBytes || !utf8.ValidString(value) {
		return invalidAuthField(authFieldInput)
	}

	for _, r := range value {
		if r == '\n' || r == '\r' || unicode.IsControl(r) {
			return invalidAuthField(authFieldInput)
		}
	}

	return nil
}
