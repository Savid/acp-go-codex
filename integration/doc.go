// Package integration holds the tests that run against an installed codex.
//
// The tests are behind the integration build tag and ACP_GO_CODEX_RUN_INTEGRATION=1.
// The smoke tier spends no model tokens; ACP_GO_CODEX_RUN_LIVE_TOKENS=1 enables
// prompts that do.
package integration
