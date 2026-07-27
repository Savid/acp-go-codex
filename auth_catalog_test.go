package codexacp

import (
	"errors"
	"strings"
	"testing"
)

func TestAuthMethodsPublishesThePinnedCatalog(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	result, err := fixture.call(t, AuthMethodsMethod, map[string]any{"sessionId": fixture.sessionID})
	if err != nil {
		t.Fatalf("methods: %v", err)
	}

	methods, _ := result.(authMethodsResult)
	if methods.Generation == "" {
		t.Fatal("methods returned no generation")
	}

	entries := methods.Providers[authProviderOpenAI]
	if len(entries) != 2 {
		t.Fatalf("catalog has %d entries, want 2", len(entries))
	}

	if entries[0].ID != authMethodDeviceCode || entries[0].Type != authMethodTypeOAuth {
		t.Fatalf("first entry = %+v", entries[0])
	}

	if entries[1].ID != authMethodAPIKey || entries[1].Type != authMethodTypeAPI {
		t.Fatalf("second entry = %+v", entries[1])
	}

	for _, id := range []string{"chatgpt", "amazonBedrock", "chatgptAuthTokens"} {
		for _, entry := range entries {
			if entry.ID == id {
				t.Fatalf("catalog published the omitted variant %q", id)
			}
		}
	}
}

func TestAuthMethodsMintsAFreshGenerationPerCall(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	first := fixture.mintGeneration(t)
	second := fixture.mintGeneration(t)

	if first == second {
		t.Fatal("two methods calls returned the same generation")
	}
}

func TestAuthMethodsFailsClosedWhenNoCatalogSurvives(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	restore := pinnedAuthCatalog
	pinnedAuthCatalog = func() []authCatalogMethod {
		return []authCatalogMethod{{ID: "broken", Type: authMethodTypeAPI, Label: "\u202eevil"}}
	}

	defer func() { pinnedAuthCatalog = restore }()

	_, err := fixture.call(t, AuthMethodsMethod, map[string]any{"sessionId": fixture.sessionID})
	if err == nil {
		t.Fatal("methods succeeded with no catalog")
	}

	requireAuthCause(t, err, authCauseNativeVeto)
}

func TestAuthMethodsFailsWhenTheGenerationCannotBeMinted(t *testing.T) {
	fixture := newProviderAuthFixture(t)

	restore := authRandRead
	authRandRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }

	defer func() { authRandRead = restore }()

	_, err := fixture.call(t, AuthMethodsMethod, map[string]any{"sessionId": fixture.sessionID})
	if err == nil {
		t.Fatal("methods succeeded without a generation")
	}

	requireAuthCause(t, err, authCauseProcess)
}

func TestBuildAuthCatalogOmitsAnEntryWhoseLabelViolatesItsBound(t *testing.T) {
	restore := pinnedAuthCatalog
	pinnedAuthCatalog = func() []authCatalogMethod {
		return []authCatalogMethod{
			{ID: "good", Type: authMethodTypeAPI, Label: "Good"},
			{ID: "bad", Type: authMethodTypeAPI, Label: strings.Repeat("x", authMaxLabelBytes+1)},
		}
	}

	defer func() { pinnedAuthCatalog = restore }()

	resolved, entries := buildAuthCatalog()
	if len(resolved) != 1 || len(entries) != 1 || entries[0].ID != "good" {
		t.Fatalf("catalog = %+v", entries)
	}
}

func TestAuthDisplayTextBounds(t *testing.T) {
	cases := map[string]struct {
		value string
		want  bool
	}{
		"plain":       {"OpenAI API key", true},
		"punctuation": {"Sign in — with ChatGPT!", true},
		"space":       {"a b", true},
		"empty":       {"", false},
		"oversize":    {strings.Repeat("x", authMaxLabelBytes+1), false},
		"control":     {"tab\there", false},
		"bidi":        {"\u202eevil", false},
		"newline":     {"a\nb", false},
	}

	for name, testCase := range cases {
		got, ok := authDisplayText(testCase.value, authMaxLabelBytes)
		if ok != testCase.want {
			t.Fatalf("%s: ok = %v, want %v", name, ok, testCase.want)
		}

		if ok && got == "" {
			t.Fatalf("%s: accepted an empty value", name)
		}
	}
}

func TestAuthDisplayTextNormalisesBeforeMeasuring(t *testing.T) {
	decomposed := "é"

	got, ok := authDisplayText(decomposed, authMaxLabelBytes)
	if !ok {
		t.Fatal("a decomposed label was rejected")
	}

	if got == decomposed {
		t.Fatal("the relayed value was not normalised")
	}
}

func TestAuthDisplayTextRejectsInvalidUTF8(t *testing.T) {
	if _, ok := authDisplayText(string([]byte{0xff, 0xfe}), authMaxLabelBytes); ok {
		t.Fatal("invalid UTF-8 was accepted")
	}
}

func TestAuthDisplayURLBounds(t *testing.T) {
	cases := map[string]struct {
		value string
		want  bool
	}{
		"https":      {"https://auth.openai.com/codex/device", true},
		"empty":      {"", false},
		"oversize":   {"https://example.com/" + strings.Repeat("a", authMaxURLBytes), false},
		"http":       {"http://auth.openai.com/codex/device", false},
		"userinfo":   {"https://user:pass@auth.openai.com/", false},
		"fragment":   {"https://auth.openai.com/#code", false},
		"nohost":     {"https:///codex/device", false},
		"unparsable": {"https://auth.openai.com/\x7f\x00", false},
	}

	for name, testCase := range cases {
		if _, ok := authDisplayURL(testCase.value); ok != testCase.want {
			t.Fatalf("%s: ok = %v, want %v", name, ok, testCase.want)
		}
	}
}

func TestAuthDisplayUserCodeUsesAnAnchoredPattern(t *testing.T) {
	cases := map[string]bool{
		"U9KH-GPDJ7":    true,
		"":              false,
		"</script>ABCD": false,
		"ABCD</script>": false,
		"has space":     false,
		strings.Repeat("A", authMaxUserCodeBytes+1): false,
	}

	for value, want := range cases {
		if _, ok := authDisplayUserCode(value); ok != want {
			t.Fatalf("%q: ok = %v, want %v", value, ok, want)
		}
	}
}

func TestAuthLoopbackHostDetection(t *testing.T) {
	cases := map[string]bool{
		"https://auth.openai.com/codex/device":                          false,
		"https://127.0.0.1/callback":                                    true,
		"https://[::1]/callback":                                        true,
		"https://localhost/callback":                                    true,
		"https://app.localhost/callback":                                true,
		"https://auth.openai.com/a?redirect_uri=http://127.0.0.1:1455/": true,
		"https://auth.openai.com/a?redirect_uri=https://example.com/":   false,
		"https://auth.openai.com/a?redirect_uri=%3A%3A":                 false,
		"://": false,
	}

	for value, want := range cases {
		if got := authLoopbackHost(value); got != want {
			t.Fatalf("%q: loopback = %v, want %v", value, got, want)
		}
	}
}

func TestValidateAuthSecretBounds(t *testing.T) {
	cases := map[string]bool{
		"sk-live-key": true,
		"":            false,
		strings.Repeat("k", authMaxSecretBytes+1): false,
		"has\nnewline":       false,
		"has\rreturn":        false,
		"has\tcontrol":       false,
		string([]byte{0xff}): false,
	}

	for value, valid := range cases {
		err := validateAuthSecret(value)
		if (err == nil) != valid {
			t.Fatalf("%q: err = %v, want valid = %v", value, err, valid)
		}
	}
}
