// Package codexacp exposes the local Codex CLI as an Agent Client Protocol
// agent.
//
// Most hosts run the agent over a pair of JSON-RPC streams using [Serve].
// Serve starts Codex app-server sessions on demand, maps ACP requests into
// Codex thread and turn APIs, and streams ACP session updates back to the
// client. Hosts must complete ACP initialization before issuing session or
// other agent methods.
//
// Hosts should use [Serve] for the JSON-RPC transport. Codex authentication and
// account configuration remain owned by the local Codex installation unless the
// host explicitly supplies external ChatGPT auth tokens through ACP
// authenticate and [WithChatGPTAuthTokenRefresher].
//
// Hosts that need durable remote resume can provide [WithSessionStore]. A
// session store receives Codex rollout JSONL rows, can back session/list, and
// can hydrate rollout JSONL into a temporary file for session/load or
// session/resume when the local Codex thread state is absent. Codex-specific
// session import extension methods are advertised under _meta.codex when the
// agent is initialized.
//
// Hosts that need structured output can attach [CodexOptions] with
// [WithSessionCodexOptions] or use [WithSessionOutputSchema]. The schema is
// sent to Codex on each turn/start and parsed response objects are returned
// under _meta.codex.structuredOutput.
//
// Hosts that need adapter telemetry can provide OpenTelemetry providers with
// [WithTracerProvider] and [WithMeterProvider]. The package never configures
// global OpenTelemetry providers; the acp-go-codex binary handles env-based
// exporter setup for command-line use. Caller-supplied providers remain owned
// by the caller, including ForceFlush and Shutdown.
package codexacp
