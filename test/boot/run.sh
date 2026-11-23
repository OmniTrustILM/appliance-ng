#!/usr/bin/env bash
# Boot test: boots the appliance offline, drives the onboarding wizard through its API and waits until it is ready.
#
#   [E2E=1] run.sh <iso|qcow2> <arch>
#
# The VM gets a random setup code preset through SMBIOS. While the node waits in maintenance mode, no resource Talos
# serves without credentials may show the preset. test/boot/drive then answers both wizard parts, checks the
# appliance, the recovery kit, a backup to an SFTP and an S3 server it runs on the host, and cluster access; see
# test/boot/drive for its checks. Passes when /status reports ready within 25 minutes, the ILM UI answers 200, every
# drive check passes and the serial log holds no key material, setup code, password, TOTP seed, preset, kit
# passphrase, target password or dump content. The passphrase and the target passwords are drawn per run and kept in
# files readable by the owner; the kit file and those files are deleted at the end.
# With E2E=1 the Playwright tests in test/e2e answer both wizard parts in Chromium instead of drive, and change
# settings in the console after the checks above, issuing the recovery kit that sign-in asks for; run npm ci and npx
# playwright install chromium in test/e2e first.
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
LEAK_PATTERNS=(-e 'BEGIN ' -e 'PRIVATE KEY' -e 'client-key-data' -e 'AGE-SECRET-KEY-' -e 'INSERT INTO' -e 'COPY public.')
CODE_ALPHABET=23456789ABCDEFGHJKLMNPQRSTUVWXYZ
PRESET_KEY=io.otilm.appliance.setup-code-sha256=
ADMIN='admin'
# QEMU's user network with restrict=on offers no router or DNS server by DHCP; wizard part 1 sets these.
KUBE_API=127.0.0.1:16443
# An unused address of QEMU's user network at which the guest reaches the SFTP and S3 servers on the host (see vm.sh).
HOST_ALIAS=10.0.2.100
GATEWAY=10.0.2.2
DNS=10.0.2.3
E2E="${E2E:-0}"
TALOSCTL="${TALOSCTL:-talosctl}"

usage() { sed -n '4p' "$0" >&2; exit 2; }
[[ $# -eq 2 ]] || usage
media="$1" arch="$2"
case "$media" in iso) mode=iso ;; qcow2) mode=disk ;; *) usage ;; esac

export CPUS=4 MEM=12G DISK=50G
dir="$REPO/out/boot/$arch-$media"
drive="$dir/drive"
seed_file="$dir/totp.seed"
kit_file="$dir/recovery-kit.tar.age"
kit_passphrase_file="$dir/kit.passphrase"
setup_cert="$dir/setup-cert.pem"
# serve-targets keeps the SFTP and S3 files and the CA of the S3 server's certificate in targets_dir.
targets_dir="$dir/targets"
sftp_password_file="$dir/sftp.password"
s3_secret_file="$dir/s3.secret"
targets_log="$dir/serve-targets.log"
targets_pid="" targets_fingerprint=""
status=""
code="" code_hash="" password="" seed="" kit_passphrase="" sftp_password="" s3_secret=""

log() { echo "[run] $*" >&2; }

random() { # random <characters> <length>: prints a random string of that length drawn from the characters
  local characters="$1" length="$2" out
  out="$(LC_ALL=C tr -dc "$characters" </dev/urandom | head -c "$length" || true)"
  [[ ${#out} -eq $length ]] && printf '%s' "$out"
}

sha256() { # sha256 <text>: prints the hex SHA-256 of text
  local text="$1"
  if command -v sha256sum >/dev/null; then printf '%s' "$text" | sha256sum; else printf '%s' "$text" | shasum -a 256; fi |
    cut -d' ' -f1
}

# leaked prints whether the serial log holds key material, dump content or a secret of the wizard, the kit or the targets.
leaked() {
  local secrets=() secret
  for secret in "$code" "$code_hash" "$password" "$seed" "$kit_passphrase" "$sftp_password" "$s3_secret"; do
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
kit_passphrase="$(random 'A-Za-z0-9' 24)" || fail "could not draw the kit passphrase"
sftp_password="$(random 'A-Za-z0-9' 24)" || fail "could not draw the SFTP password"
s3_secret="$(random 'A-Za-z0-9' 40)" || fail "could not draw the S3 secret key"

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

# capture_setup_certificate saves the self-signed certificate the first-boot service presents, for drive part1 to
# trust. It is trusted on first use, as a user does who has no way to compare the fingerprint tty1 shows.
capture_setup_certificate() {
  local _
  for _ in $(seq 36); do
    if openssl s_client -connect "${CONSOLE_URL#https://}" </dev/null 2>/dev/null |
      openssl x509 -out "$setup_cert" 2>/dev/null; then
      return
    fi
    sleep 5
  done
  fail "could not read the certificate of the first-boot service"
}

drive_wizard() { # drive_wizard <part1|part2|check|kit|backup|access> [<SFTP host key fingerprint>]
  local part="$1"
  local fingerprint="${2:-}"
  local args=(--url "$CONSOLE_URL" --user "$ADMIN" --password "$password" --seed-file "$seed_file"
    --host-name "$UI_HOST")
  case "$part" in
    # The node address is 10.0.2.15, drive's default --server-name.
    part1) args+=(--code "$code" --ca-file "$setup_cert" --gateway "$GATEWAY" --dns "$DNS") ;;
    part2) args+=(--component "$COMPONENT") ;;
    check) args+=(--connector "$CONNECTOR_NAME" --ui-url "https://$UI_HOST:10443" --ui-addr "$UI_ADDR") ;;
    kit) args+=(--out "$kit_file" --passphrase-file "$kit_passphrase_file" --kube-addr "$KUBE_API" --apid "$APID") ;;
    backup) args+=(--kit "$kit_file" --passphrase-file "$kit_passphrase_file" --dir "$targets_dir"
      --guest-host "$HOST_ALIAS" --sftp-fingerprint "$fingerprint" --sftp-password-file "$sftp_password_file"
      --s3-secret-file "$s3_secret_file" --kube-addr "$KUBE_API" --apid "$APID") ;;
    access) args+=(--kube-addr "$KUBE_API" --apid "$APID") ;;
    *) fail "unknown wizard part $part" ;;
  esac
  "$drive" "$part" "${args[@]}" >&2 || fail "drive $part failed"
}

# serve_targets starts the SFTP and S3 servers on the host and sets targets_fingerprint to the SFTP host key's.
serve_targets() {
  local _
  mkdir -p "$targets_dir"
  "$drive" serve-targets --sftp 127.0.0.1:2222 --s3 127.0.0.1:9000 --dir "$targets_dir" --guest-host "$HOST_ALIAS" \
    --sftp-password-file "$sftp_password_file" --s3-secret-file "$s3_secret_file" >"$targets_log" 2>&1 &
  targets_pid=$!
  for _ in $(seq 20); do
    targets_fingerprint="$(sed -n 's/^drive: sftp host key //p' "$targets_log")"
    [[ -z "$targets_fingerprint" ]] || break
    kill -0 "$targets_pid" 2>/dev/null || fail "drive serve-targets stopped: $(tail -n 3 "$targets_log")"
    sleep 0.5
  done
  [[ -n "$targets_fingerprint" ]] || fail "drive serve-targets did not start"
}

browse() { # browse <spec> [<title pattern>]: runs the tests of a test/e2e spec, or those matching, in Chromium
  local spec="$1" title="${2:-.}"
  (cd "$REPO/test/e2e" &&
    E2E_CONSOLE_URL="$CONSOLE_URL" E2E_HOST_NAME="$UI_HOST" E2E_CODE="$code" E2E_USER="$ADMIN" \
      E2E_PASSWORD="$password" E2E_SEED_FILE="$seed_file" E2E_GATEWAY="$GATEWAY" E2E_DNS="$DNS" \
      E2E_COMPONENT="$CONNECTOR_NAME" E2E_KIT_PASSPHRASE="$kit_passphrase" E2E_KIT_FILE="$kit_file" \
      npx --no-install playwright test "$spec" --grep "$title") >&2 ||
    fail "browser test $spec failed"
}

wizard() { # wizard <part1|part2>: answers that wizard part in the browser with E2E=1, else through the API
  local part="$1"
  if [[ "$E2E" != 1 ]]; then
    drive_wizard "$part"
    return
  fi
  case "$part" in
    part1) browse wizard.spec.ts 'wizard part 1' ;;
    part2) browse wizard.spec.ts 'wizard part 2' ;;
    *) fail "unknown wizard part $part" ;;
  esac
}

"$VM" stop "$dir" >/dev/null 2>&1 || true
rm -rf "$dir"
mkdir -p "$dir"
(cd "$REPO" && go build -o "$drive" ./test/boot/drive) || fail "could not build test/boot/drive"
if [[ "$E2E" == 1 ]]; then
  [[ -d "$REPO/test/e2e/node_modules/@playwright/test" ]] || fail "test/e2e has no node_modules: run npm ci there"
fi
trap 'rm -f "$kit_file" "$kit_passphrase_file" "$sftp_password_file" "$s3_secret_file"
  if [[ -n "$targets_pid" ]]; then kill "$targets_pid" 2>/dev/null || true; fi
  "$VM" stop "$dir"' EXIT
(umask 077 && printf '%s' "$kit_passphrase" >"$kit_passphrase_file" &&
  printf '%s' "$sftp_password" >"$sftp_password_file" && printf '%s' "$s3_secret" >"$s3_secret_file") ||
  fail "could not write the secret files"
HOST_ALIAS="$HOST_ALIAS" SMBIOS_OEM="$PRESET_KEY$code_hash" "$VM" start "$mode" "$arch" "$dir" || fail "VM did not start"
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
          capture_setup_certificate
          wizard part1
          seed="$(cat "$seed_file")" part1=true
          log "$((SECONDS - start))s wizard part 1 applied"
        fi ;;
      addons | platform)
        if $part1 && ! $part2; then
          wizard part2
          part2=true
          log "$((SECONDS - start))s wizard part 2 done"
        fi ;;
      ready) break ;;
      *) ;; # still booting; keep polling
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
if [[ "$E2E" == 1 ]]; then
  browse settings.spec.ts
  log "$((SECONDS - start))s console settings changed in the browser"
fi
# With E2E=1 the browser issued the kit; drive then only checks it.
drive_wizard kit
serve_targets
drive_wizard backup "$targets_fingerprint"
drive_wizard access

if leaked; then
  fail "serial log contains key material or a secret of the wizard"
fi

echo "PASS $ready"
