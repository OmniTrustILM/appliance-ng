#!/usr/bin/env bash
# Boot test: boots the appliance offline and waits until it is ready.
#
#   run.sh <iso|qcow2> <arch>
#
# Passes when /status reports ready within 25 minutes and the ILM UI answers 200, and the serial log holds no key
# material. Prints "PASS <seconds to ready>" or "FAIL <reason>"; exits 0 or 1. The VM is always stopped.
# VM state is kept in out/boot/<arch>-<media>/ and recreated on every run.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
VM="$REPO/test/boot/vm.sh"
STATUS_URL=http://127.0.0.1:18443/status
UI_HOST=ilm.appliance.internal
UI_URL="https://$UI_HOST:10443/administrator/"
STATUS_TIMEOUT=180
READY_TIMEOUT=1500
LEAK_PATTERNS=(-e 'BEGIN ' -e 'PRIVATE KEY' -e 'client-key-data')

usage() { sed -n '4p' "$0" >&2; exit 2; }
[[ $# -eq 2 ]] || usage
media="$1" arch="$2"
case "$media" in iso) mode=iso ;; qcow2) mode=disk ;; *) usage ;; esac

export CPUS=4 MEM=12G DISK=50G
dir="$REPO/out/boot/$arch-$media"
status=""

log() { echo "[run] $*" >&2; }

# leaked prints whether the serial log holds key material.
leaked() { grep -qF "${LEAK_PATTERNS[@]}" "$dir/serial.log" 2>/dev/null; }

# fail never prints or keeps a serial log that holds key material: the log is uploaded by CI.
fail() {
  if leaked; then
    rm -f "$dir/serial.log"
    echo "--- serial log contains key material; log removed ---" >&2
  else
    # Long base64 runs (certificates, tokens) are masked so the public CI log never carries them.
    echo "--- last 100 serial lines (base64 runs masked) ---" >&2
    tail -n 100 "$dir/serial.log" | sed -E 's#[A-Za-z0-9+/=]{40,}#<masked>#g' >&2 || true
  fi
  echo "--- last status: ${status:-none} ---" >&2
  echo "FAIL $*"
  exit 1
}

field() { # field <name>: prints a string field of $status
  local name="$1"
  sed -n "s/.*\"$name\":\"\([^\"]*\)\".*/\1/p" <<<"$status"
}

"$VM" stop "$dir" >/dev/null 2>&1 || true
rm -rf "$dir"
trap '"$VM" stop "$dir"' EXIT
"$VM" start "$mode" "$arch" "$dir" || fail "VM did not start"
start=$SECONDS

reached=false last=""
while :; do
  elapsed=$((SECONDS - start))
  if current="$(curl -sf --max-time 5 "$STATUS_URL")"; then
    status="$current" reached=true
    state="$(field state)/$(field step)"
    if [[ "$state" != "$last" ]]; then log "${elapsed}s $state $(field message)"; last="$state"; fi
    [[ "$(field state)" != ready ]] || break
  elif ! $reached && [[ "$elapsed" -ge "$STATUS_TIMEOUT" ]]; then
    fail "status not reachable within ${STATUS_TIMEOUT}s"
  fi
  [[ "$elapsed" -lt "$READY_TIMEOUT" ]] || fail "not ready within ${READY_TIMEOUT}s"
  sleep 5
done
ready=$((SECONDS - start))

code=""
for _ in $(seq 12); do
  code="$(curl -sk -o /dev/null -w '%{http_code}' --max-time 10 \
    --resolve "$UI_HOST:10443:127.0.0.1" "$UI_URL" || true)"
  [[ "$code" != 200 ]] || break
  sleep 5
done
[[ "$code" == 200 ]] || fail "UI answered ${code:-nothing}, expected 200"

if leaked; then
  fail "serial log contains key material"
fi

echo "PASS $ready"
