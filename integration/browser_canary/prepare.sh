#!/bin/sh
set -eu

# Official release source: https://github.com/openai/codex/releases/tag/rust-v0.146.0
version=0.146.0
case "$(uname -m)" in
  x86_64)
    archive=codex-x86_64-unknown-linux-musl.tar.gz
    member=codex-x86_64-unknown-linux-musl
    archive_sha=5ba3b9405543953081f661d0854d266f76e2abbe51d41349355a36de7673776a
    binary_sha=2e863156ed35ecc5253b1e2f907a9143077b9f7cb51942070c61996471ff6e04
    ;;
  aarch64|arm64)
    archive=codex-aarch64-unknown-linux-musl.tar.gz
    member=codex-aarch64-unknown-linux-musl
    archive_sha=975bac91562abeedeb8f79636d51a86649b31f34a9de6a3bcb059565b6cf1f87
    binary_sha=cb5e8cb8a333a408ce6adbe0d4fad1845c69772c2216af7c1f88c98a11460dc6
    ;;
  *)
    echo "unsupported browser-canary architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
output_dir="$repo_root/.tmp/browser-canary"
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM

url="https://github.com/openai/codex/releases/download/rust-v${version}/${archive}"
curl --fail --location --proto '=https' --retry 3 --silent --show-error --output "$work_dir/$archive" "$url"
printf '%s  %s\n' "$archive_sha" "$work_dir/$archive" | sha256sum -c -
tar -xzf "$work_dir/$archive" -C "$work_dir" "$member"
printf '%s  %s\n' "$binary_sha" "$work_dir/$member" | sha256sum -c -

mkdir -p "$output_dir"
install -m 0755 "$work_dir/$member" "$output_dir/native"
