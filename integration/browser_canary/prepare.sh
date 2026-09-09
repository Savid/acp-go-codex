#!/bin/sh
set -eu

version=0.153.4
case "$(uname -m)" in
  x86_64)
    archive=codex-x86_64-unknown-linux-musl.tar.gz
    member=codex-x86_64-unknown-linux-musl
    archive_sha=f479424eca092484dc40d87ae28c44f4cc40234a60045d6131e493800d814a30
    binary_sha=56ef98ab4032d317ab26e9b5e5a175650717351edb16ed9cde0cb6d1734d62da
    ;;
  aarch64|arm64)
    archive=codex-aarch64-unknown-linux-musl.tar.gz
    member=codex-aarch64-unknown-linux-musl
    archive_sha=5cda6182bd94c3a30f2eb63a495489ebf7f691fddb14d70f48c6c1a5071b6cde
    binary_sha=4d76e542c222ea8c75861d8c4ade60a1a332a63255ce1c60bdaebf7c2a2869e6
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
