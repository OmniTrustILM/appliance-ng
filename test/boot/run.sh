#!/usr/bin/env bash
# Boot test: boots the appliance offline, drives the onboarding wizard through its API and waits until it is ready.
#
#   run.sh <iso|qcow2> <arch>
#
# The VM gets a random setup code preset through SMBIOS. While the node waits in maintenance mode, no resource Talos
# serves without credentials may show the preset. test/boot/drive then answers wizard part 1 on the first-boot
# service and, once the Appliance Manager imported the answers, signs in and answers wizard part 2 with one connector.
# Passes when /status reports ready within 25 minutes, the ILM UI answers 200, the manager's console is healthy and
# reports the setup complete, the connector registered with ILM Core, and the serial log holds no key material, setup
# code, password, TOTP seed or preset.
# Prints "PASS <seconds to ready>" or "FAIL <reason>"; exits 0 or 1. The VM is always stopped.
# VM state is kept in out/boot/<arch>-<media>/ and recreated on every run. TALOSCTL names the talosctl binary
# (default talosctl), which must match the Talos version of the release.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
VM="$REPO/test/boot/vm.sh"
STATUS_URL=http://127.0.0.1:18444/status
CONSOLE_URL=https://127.0.0.1:18443
APID=127.0.0.1:15000
UI_HOST=ilm.appliance.internal
UI_ADDR=127.0.0.1:10443
UI_URL="https://$UI_HOST:10443/administrator/"
# A public catalogue connector whose image is in the offline cache. The operator registers every connector as a v2
# connector, so it has to serve the v2 info, health and metrics interfaces.
COMPONENT=software-cryptography-provider
CONNECTOR_NAME='Software Cryptography Provider'
STATUS_TIMEOUT=180
READY_TIMEOUT=1500
LEAK_PATTERNS=(-e 'BEGIN ' -e 'PRIVATE KEY' -e 'client-key-data')
CODE_ALPHABET=23456789ABCDEFGHJKLMNPQRSTUVWXYZ
PRESET_KEY=io.otilm.appliance.setup-code-sha256=
ADMIN='admin'
TALOSCTL="${TALOSCTL:-talosctl}"

usage() { sed -n '4p' "$0" >&2; exit 2; }
[[ $# -eq 2 ]] || usage
media="$1" arch="$2"
case "$media" in iso) mode=iso ;; qcow2) mode=disk ;; *) usage ;; esac

export CPUS=4 MEM=12G DISK=50G
dir="$REPO/out/boot/$arch-$media"
drive="$dir/drive"
seed_file="$dir/totp.seed"
status=""
code="" code_hash="" password="" seed=""

log() { echo "[run] $*" >&2; }

random() { # random <characters> <length>: prints a random string of that length drawn from the characters
  local out
  out="$(LC_ALL=C tr -dc "$1" </dev/urandom | head -c "$2" || true)"
  [[ ${#out} -eq $2 ]] && printf '%s' "$out"
}

sha256() { # sha256 <text>: prints the hex SHA-256 of text
  if command -v sha256sum >/dev/null; then printf '%s' "$1" | sha256sum; else printf '%s' "$1" | shasum -a 256; fi |
    cut -d' ' -f1
}

# leaked prints whether the serial log holds key material or a secret of the wizard.
leaked() {
  local secrets=() secret
  for secret in "$code" "$code_hash" "$password" "$seed"; do
    if [[ -n "$secret" ]]; then secrets+=(-e "$secret"); fi
  done
  grep -qF "${LEAK_PATTERNS[@]}" "${secrets[@]}" "$dir/serial.log" 2>/dev/null
}

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

# The code is drawn from the setup code alphabet in upper case, so the appliance's normalisation keeps it intact.
code="$(random "$CODE_ALPHABET" 32)" || fail "could not draw the setup code"
code_hash="$(sha256 "$code")"
password="$(random 'A-Za-z0-9' 24)" || fail "could not draw the password"

# preset_hidden dumps every resource the node in maintenance mode serves without credentials and checks that none
# holds the preset key or the code's hash.
preset_hidden() {
  local flags=(--insecure --nodes "$APID") types type count=0
  types="$("$TALOSCTL" get resourcedefinitions "${flags[@]}" -o jsonpath='{.metadata.id}')" ||
    fail "talosctl could not list the resource definitions in maintenance mode"
  : >"$dir/maintenance-resources.yaml"
  for type in $types; do
    # Most types need credentials; maintenance mode refuses those.
    if "$TALOSCTL" get "$type" "${flags[@]}" -o yaml >>"$dir/maintenance-resources.yaml" 2>/dev/null; then
      count=$((count + 1))
    fi
  done
  log "dumped $count resource types readable in maintenance mode"
  [[ "$count" -gt 0 ]] || fail "no resource readable in maintenance mode"
  if grep -qiF -e "$PRESET_KEY" -e "$code_hash" "$dir/maintenance-resources.yaml"; then
    fail "a resource readable in maintenance mode shows the preset setup code"
  fi
}

drive_wizard() { # drive_wizard <part1|part2|check>
  local args=(--url "$CONSOLE_URL" --user "$ADMIN" --password "$password" --seed-file "$seed_file")
  case "$1" in
    # QEMU's user network with restrict=on offers no router or DNS server by DHCP.
    part1) args+=(--code "$code" --host-name "$UI_HOST" --gateway 10.0.2.2 --dns 10.0.2.3) ;;
    part2) args+=(--host-name "$UI_HOST" --component "$COMPONENT") ;;
    check) args+=(--connector "$CONNECTOR_NAME" --ui-url "https://$UI_HOST:10443" --ui-addr "$UI_ADDR") ;;
  esac
  "$drive" "$1" "${args[@]}" >&2 || fail "drive $1 failed"
}

"$VM" stop "$dir" >/dev/null 2>&1 || true
rm -rf "$dir"
mkdir -p "$dir"
(cd "$REPO" && go build -o "$drive" ./test/boot/drive) || fail "could not build test/boot/drive"
trap '"$VM" stop "$dir"' EXIT
SMBIOS_OEM="$PRESET_KEY$code_hash" "$VM" start "$mode" "$arch" "$dir" || fail "VM did not start"
start=$SECONDS

reached=false last="" part1=false part2=false
while :; do
  elapsed=$((SECONDS - start))
  if current="$(curl -sf --max-time 5 "$STATUS_URL")"; then
    status="$current" reached=true
    state="$(field state)/$(field step)"
    if [[ "$state" != "$last" ]]; then log "${elapsed}s $state $(field message)"; last="$state"; fi
    case "$(field state)" in
      setup)
        if ! $part1; then
          preset_hidden
          drive_wizard part1
          seed="$(cat "$seed_file")" part1=true
          log "$((SECONDS - start))s wizard part 1 applied"
        fi ;;
      addons | platform)
        if $part1 && ! $part2; then
          drive_wizard part2
          part2=true
          log "$((SECONDS - start))s wizard part 2 done"
        fi ;;
      ready) break ;;
    esac
  elif ! $reached && [[ "$elapsed" -ge "$STATUS_TIMEOUT" ]]; then
    fail "status not reachable within ${STATUS_TIMEOUT}s"
  fi
  [[ "$elapsed" -lt "$READY_TIMEOUT" ]] || fail "not ready within ${READY_TIMEOUT}s"
  sleep 5
done
ready=$((SECONDS - start))
$part2 || fail "ready without the wizard"

answer=""
for _ in $(seq 12); do
  answer="$(curl -sk -o /dev/null -w '%{http_code}' --max-time 10 \
    --resolve "$UI_HOST:10443:127.0.0.1" "$UI_URL" || true)"
  [[ "$answer" != 200 ]] || break
  sleep 5
done
[[ "$answer" == 200 ]] || fail "UI answered ${answer:-nothing}, expected 200"

answer="$(curl -sk -o /dev/null -w '%{http_code}' --max-time 10 "$CONSOLE_URL/healthz" || true)"
[[ "$answer" == 200 ]] || fail "console health check answered ${answer:-nothing}, expected 200"
drive_wizard check

if leaked; then
  fail "serial log contains key material or a secret of the wizard"
fi

echo "PASS $ready"
