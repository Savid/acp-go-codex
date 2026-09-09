# Real-native browser canary

This fixture drives `codex login` through the ordinary account-command path,
then uses passive `execve` tracing to require a real browser attempt and prove
every launcher resolved inside the generated shim. The runtime container has no
GUI, credentials, host mounts, or network.

- Codex CLI: `0.153.4`, from the [official release](https://github.com/openai/codex/releases/tag/rust-v0.153.4)
- linux x64 archive SHA-256: `f479424eca092484dc40d87ae28c44f4cc40234a60045d6131e493800d814a30`
- linux arm64 archive SHA-256: `5cda6182bd94c3a30f2eb63a495489ebf7f691fddb14d70f48c6c1a5071b6cde`
- Base image: `debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818`

`prepare.sh` downloads and verifies only the native release. Image construction
installs the exact `strace` package. The final container executes with Docker
`--network none`.
