# Real-native browser canary

This fixture drives `codex login` through the ordinary account-command path,
then uses passive `execve` tracing to require a real browser attempt and prove
every launcher resolved inside the generated shim. The runtime container has no
GUI, credentials, host mounts, or network.

- Codex CLI: `0.146.0`, from the [official release](https://github.com/openai/codex/releases/tag/rust-v0.146.0)
- linux x64 archive SHA-256: `5ba3b9405543953081f661d0854d266f76e2abbe51d41349355a36de7673776a`
- linux arm64 archive SHA-256: `975bac91562abeedeb8f79636d51a86649b31f34a9de6a3bcb059565b6cf1f87`
- Base image: `debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818`

`prepare.sh` downloads and verifies only the native release. Image construction
installs the exact `strace` package. The final container executes with Docker
`--network none`.
