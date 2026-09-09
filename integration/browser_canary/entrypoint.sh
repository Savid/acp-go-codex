#!/bin/sh
set -eu

case "$(uname -m)" in
  x86_64) native_sha=56ef98ab4032d317ab26e9b5e5a175650717351edb16ed9cde0cb6d1734d62da ;;
  aarch64) native_sha=4d76e542c222ea8c75861d8c4ade60a1a332a63255ce1c60bdaebf7c2a2869e6 ;;
  *) echo "unsupported browser-canary architecture: $(uname -m)" >&2; exit 1 ;;
esac

test -x /usr/local/bin/codex
printf '%s  %s\n' "$native_sha" /usr/local/bin/codex | sha256sum --check --strict
test "$(/usr/local/bin/codex --version)" = "codex-cli 0.153.4"

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
