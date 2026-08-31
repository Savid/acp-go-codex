#!/bin/sh
set -eu

case "$(uname -m)" in
  x86_64) native_sha=2e863156ed35ecc5253b1e2f907a9143077b9f7cb51942070c61996471ff6e04 ;;
  aarch64) native_sha=cb5e8cb8a333a408ce6adbe0d4fad1845c69772c2216af7c1f88c98a11460dc6 ;;
  *) echo "unsupported browser-canary architecture: $(uname -m)" >&2; exit 1 ;;
esac

test -x /usr/local/bin/codex
printf '%s  %s\n' "$native_sha" /usr/local/bin/codex | sha256sum --check --strict
test "$(/usr/local/bin/codex --version)" = "codex-cli 0.146.0"

if [ "${1:-}" = "--verify-native" ]; then
  exit 0
fi

rm -f /canary/evidence/browser-escape /canary/evidence/exec.log /canary/evidence/test.log /canary/evidence/launchers
set +e
ACP_GO_CODEX_BROWSER_CANARY=1 timeout --signal=TERM --kill-after=15 120 \
  strace -f -qq -e trace=execve,execveat -o /canary/evidence/exec.log \
  /canary/browser-canary.test -test.run '^TestRealNativeBrowserContainment$' -test.v \
  >/canary/evidence/test.log 2>&1
status=$?
set -e
cat /canary/evidence/test.log
test "$status" -eq 0
test "$(grep -c '^--- PASS: TestRealNativeBrowserContainment' /canary/evidence/test.log || true)" -eq 1
! grep -q 'testing: warning: no tests to run' /canary/evidence/test.log
grep -q 'execve("/usr/local/bin/codex"' /canary/evidence/exec.log
! grep -Eq 'execveat\([^,]+, "", .*AT_EMPTY_PATH' /canary/evidence/exec.log

sed -n \
  -e 's/.*execve("\([^"]*\)".*/\1/p' \
  -e 's/.*execveat([^,]*, "\([^"]*\)".*/\1/p' \
  /canary/evidence/exec.log \
  | grep -E '/(open|xdg-open|x-www-browser|www-browser|sensible-browser|gio|firefox|google-chrome|google-chrome-stable|chromium|chromium-browser)$' \
  >/canary/evidence/launchers || true
test -s /canary/evidence/launchers
while IFS= read -r launcher; do
  case "$launcher" in
    /canary/scratch/acp-go-codex-browser-shim-*/open|\
    /canary/scratch/acp-go-codex-browser-shim-*/xdg-open|\
    /canary/scratch/acp-go-codex-browser-shim-*/x-www-browser|\
    /canary/scratch/acp-go-codex-browser-shim-*/www-browser|\
    /canary/scratch/acp-go-codex-browser-shim-*/sensible-browser) ;;
    *) echo "browser launcher escaped production shim: $launcher" >&2; exit 1 ;;
  esac
done </canary/evidence/launchers
test ! -e /canary/evidence/browser-escape
