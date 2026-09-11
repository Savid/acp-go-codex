// Package codexacp exposes the Codex CLI as an Agent Client Protocol agent.
//
// Most hosts run the agent over a pair of JSON-RPC streams using [Serve].
// Serve starts one `codex app-server` for the agent, opens one Codex thread
// per ACP session on it, maps ACP requests onto the app-server protocol, and
// streams ACP session updates back to the client. Codex inherits the
// adapter's environment and keeps its rollouts in its own home, so a session
// started over ACP can be continued natively with `codex resume` after the
// adapter closes.
//
// Hosts that need durable remote resume provide [WithSessionStore]. The
// store mirrors the rollout rows and backs session/list, session/load, and
// session/resume when the native rollout is absent.
//
// Hosts that need adapter telemetry provide OpenTelemetry providers with
// [WithTracerProvider] and [WithMeterProvider]; the package never configures
// global providers.
package codexacp
