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
// authenticate and [WithCodexChatGPTAuthTokenRefresher].
//
// Every Agent uses a [SessionStore], defaulting to an in-memory implementation.
// The store receives Codex rollout JSONL rows keyed by the ACP-visible session
// ID and subpath and is the authority for inactive session/list, session/load,
// and session/resume. Native threads absent from the store are never adopted.
// Hosts that need sessions to survive Agent restart can replace the default
// with [WithSessionStore].
// Terminal prompt responses and replay updates expose the native Codex turn ID
// and final assistant response-item ID under _meta.codex.
//
// Hosts that need structured output can attach [CodexOptions] with
// [WithSessionCodexOptions] or use [WithSessionOutputSchema]. The schema is
// sent to Codex on each turn/start and parsed response objects are returned
// under _meta.codex.structuredOutput.
//
// Hosts can call [CallForkSession] for the Codex fork extension method
// _codex/session/fork. Calling [RateLimitsMethod] (_codex/rateLimits) reports
// the harness's subscription rate-limit usage as a [RateLimitsResponse]; it is
// agent-level, takes an empty params object, and reports only harness-supplied
// values. Raw Codex events are emitted only when a session request opts in with
// [WithSessionRawEvents].
// The adapter advertises no ACP slash commands: slash-prefixed text such as
// /review, /plan, and /compact remains ordinary session/prompt input for Codex
// turn/start. Skills surfaces (`skills/list`, `$skill`, `type:"skill"` items)
// are NOT commands and must not be projected as `AvailableCommand` entries
// absent a documented native command projection. Re-entry criteria: documented
// `commands/list`+execute, or documented server-side `/x` parsing in
// `turn/start`, or documented skill→command projection.
//
// Hosts that need adapter telemetry can provide OpenTelemetry providers with
// [WithTracerProvider] and [WithMeterProvider]. The package never configures
// global OpenTelemetry providers; the acp-go-codex binary handles env-based
// exporter setup for command-line use, and launched Codex app-server processes
// receive Codex-native OTEL config derived from safe OTLP environment values.
// Caller-supplied providers remain owned by the caller, including ForceFlush
// and Shutdown.
package codexacp
