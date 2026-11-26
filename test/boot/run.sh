#!/usr/bin/env bash
# Boot test: boots the appliance offline, drives the onboarding wizard through its API and waits until it is ready.
#
#   [E2E=1] [RESTORE=1] [BUNDLE=1] run.sh <iso|qcow2> <arch>
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
# With RESTORE=1 the first VM also turns the CBOM repository on and stores a CBOM through ILM before the backup, and a
# second VM restores the first one's backup under another ILM host name; see test/boot/drive for its checks. The leak check then also covers the second VM's serial log, the credential's password and the ILM client's
# secret.
# With BUNDLE=1 drive then uploads the signed platform bundle file BUNDLE_FILE (default
# out/<arch>/platform-test.tar, made by test/boot/bundle.sh with the key whose public key the images were built
# with as BUNDLE_TEST_KEY), waits until it is loaded, runs a pod from the bundle's curl image by the address of the
# appliance's registry and the digest the bundle lists for its arch, and checks that a copy with one changed byte is refused; see test/boot/drive.
# Prints "PASS <seconds to ready>" (with RESTORE=1 "PASS <seconds> restored <seconds>") or "FAIL <reason>"; exits 0
# or 1. The VMs are always stopped.
# VM state is kept in out/boot/<arch>-<media>/ (the second VM's in out/boot/<arch>-<media>-restore/) and recreated on
# every run. TALOSCTL names the talosctl binary (default talosctl), which must match the Talos version of the release.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
VM="$REPO/test/boot/vm.sh"
STATUS_URL=http://127.0.0.1:18444/status
CONSOLE_URL=https://127.0.0.1:18443
APID=127.0.0.1:15000
UI_HOST=ilm.appliance.internal
UI_ADDR=127.0.0.1:10443
# A public catalogue connector whose image is in the offline cache. The operator registers every connector as a v2
# connector, so it has to serve the v2 info, health and metrics interfaces.
COMPONENT=software-cryptography-provider
CONNECTOR_NAME='Software Cryptography Provider'
STATUS_TIMEOUT=180
READY_TIMEOUT=1500
# The second VM of RESTORE=1 installs the platform during the restore.
RESTORE_READY_TIMEOUT=3000
LEAK_PATTERNS=(-e 'BEGIN ' -e 'PRIVATE KEY' -e 'client-key-data' -e 'AGE-SECRET-KEY-' -e 'INSERT INTO' -e 'COPY public.')
CODE_ALPHABET=23456789ABCDEFGHJKLMNPQRSTUVWXYZ
SECRET_ALPHABET=A-Za-z0-9
PRESET_KEY=io.otilm.appliance.setup-code-sha256=
ADMIN='admin'
# QEMU's user network with restrict=on offers no router or DNS server by DHCP; wizard part 1 sets these.
KUBE_API=127.0.0.1:16443
# An unused address of QEMU's user network at which the guest reaches the SFTP and S3 servers on the host (see vm.sh).
HOST_ALIAS=10.0.2.100
GATEWAY=10.0.2.2
DNS=10.0.2.3
E2E="${E2E:-0}"
RESTORE="${RESTORE:-0}"
BUNDLE="${BUNDLE:-0}"
# The second VM's node and ILM host name, which the restore sets instead of the backup's, and its administrator,
# whose name differs from the backup's account so that the restore adds that account instead of replacing this one.
RESTORE_HOST=restored.appliance.internal
RESTORE_ADMIN=restorer
TALOSCTL="${TALOSCTL:-talosctl}"
# The appliance's registry, by whose address the bundle's curl image is pulled by its digest, as a load creates no tag:
# the image cache holds the release's images under their upstream names only, so only a loaded bundle serves it.
ZOT=127.0.0.1:5443

usage() { sed -n '4p' "$0" >&2; exit 2; }
[[ $# -eq 2 ]] || usage
media="$1" arch="$2"
case "$media" in iso) mode=iso ;; qcow2) mode=disk ;; *) usage ;; esac
bundle_file="${BUNDLE_FILE:-$REPO/out/$arch/platform-test.tar}"
bundle_image=""

export CPUS=4 MEM=12G DISK=50G
first_dir="$REPO/out/boot/$arch-$media"
restore_dir="$first_dir-restore"
# dir, host and admin are those of the VM under test: the first one, then with RESTORE=1 the second one.
dir="$first_dir" host="$UI_HOST" admin="$ADMIN" setup_mode=new
drive="$dir/drive"
seed_file="$dir/totp.seed"
kit_file="$dir/recovery-kit.tar.age"
kit_passphrase_file="$dir/kit.passphrase"
setup_cert="$dir/setup-cert.pem"
# The ILM administrator's credential, kept for RESTORE=1, and the password it is encrypted with.
p12_file="$dir/admin.p12"
p12_password_file="$dir/admin.p12.password"
restored_kit_file="$restore_dir/recovery-kit.tar.age"
client_secret_file="$restore_dir/oidc-client.secret"
backup_file=""
# serve-targets keeps the SFTP and S3 files and the CA of the S3 server's certificate in targets_dir.
targets_dir="$dir/targets"
sftp_password_file="$dir/sftp.password"
s3_secret_file="$dir/s3.secret"
targets_log="$dir/serve-targets.log"
targets_pid="" targets_fingerprint=""
status=""
code="" code_hash="" password="" seed="" kit_passphrase="" sftp_password="" s3_secret="" p12_password="" client_secret=""
# The wizard secrets of the first VM once the second one is under test, and the serial logs to search.
earlier=() serials=("$first_dir/serial.log")

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

# leaked prints whether a serial log holds key material, dump content or a secret of the wizard, the kit, the targets,
# the ILM administrator's credential or the ILM client.
leaked() {
  local secrets=() secret client="$client_secret"
  # A check that failed after drive kept the ILM client's secret still has its serial logs scanned for it.
  if [[ -z "$client" && -s "$client_secret_file" ]]; then client="$(cat "$client_secret_file")"; fi
  for secret in "${earlier[@]}" "$code" "$code_hash" "$password" "$seed" "$kit_passphrase" "$sftp_password" \
    "$s3_secret" "$p12_password" "$client"; do
    if [[ -n "$secret" ]]; then secrets+=(-e "$secret"); fi
  done
  grep -qF "${LEAK_PATTERNS[@]}" "${secrets[@]}" "${serials[@]}" 2>/dev/null
}

# fail never prints or keeps a serial log that holds key material: the log is uploaded by CI.
fail() {
  if leaked; then
    rm -f "${serials[@]}"
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

draw_wizard_secrets() { # draw_wizard_secrets: draws the setup code and the administrator's password of a new VM
  # The code is drawn from the setup code alphabet in upper case, so the appliance's normalisation keeps it intact.
  code="$(random "$CODE_ALPHABET" 32)" || fail "could not draw the setup code"
  code_hash="$(sha256 "$code")"
  password="$(random "$SECRET_ALPHABET" 24)" || fail "could not draw the password"
}

draw_wizard_secrets
kit_passphrase="$(random "$SECRET_ALPHABET" 24)" || fail "could not draw the kit passphrase"
sftp_password="$(random "$SECRET_ALPHABET" 24)" || fail "could not draw the SFTP password"
s3_secret="$(random "$SECRET_ALPHABET" 40)" || fail "could not draw the S3 secret key"
p12_password="$(random "$SECRET_ALPHABET" 24)" || fail "could not draw the credential's password"

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
tar_blob() { # tar_blob <OCI layout tar> <digest>: prints the blob digest of the layout
  tar -xOf "$1" "blobs/sha256/${2#sha256:}"
}

bundle_digest() { # bundle_digest <bundle tar> <ref>: prints the digest the bundle's metadata lists for the image ref
  local file="$1" ref="$2" digest index metadata
  for digest in $(tar -xOf "$file" index.json |
    jq -r '.manifests[] | select(.mediaType == "application/vnd.oci.image.index.v1+json") | .digest'); do
    index="$(tar_blob "$file" "$digest")"
    if jq -e '.artifactType == "application/vnd.otilm.appliance.bundle.v1+json"' <<<"$index" >/dev/null; then
      metadata="$(tar_blob "$file" "$(jq -r '.manifests[0].digest' <<<"$index")")"
      tar_blob "$file" "$(jq -r '.config.digest' <<<"$metadata")" |
        jq -er --arg ref "$ref" '[.images[] | select(.ref == $ref) | .digest] | if length == 1 then .[0] else empty end'
      return
    fi
  done
  return 1
}

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

drive_wizard() { # drive_wizard <part1|part2|check|kit|backup|access|restore|bundle-load> [<SFTP host key fingerprint>]
  local part="$1"
  local fingerprint="${2:-}"
  local args=(--url "$CONSOLE_URL" --user "$admin" --password "$password" --seed-file "$seed_file"
    --host-name "$host")
  case "$part" in
    # The node address is 10.0.2.15, drive's default --server-name.
    part1) args+=(--code "$code" --ca-file "$setup_cert" --gateway "$GATEWAY" --dns "$DNS" --mode "$setup_mode") ;;
    part2) args+=(--component "$COMPONENT" --ca-file "$setup_cert") ;;
    check) args+=(--connector "$CONNECTOR_NAME" --ui-url "https://$host:10443" --ui-addr "$UI_ADDR"
      --credential "$p12_file" --credential-password-file "$p12_password_file") ;;
    kit) args+=(--out "$kit_file" --passphrase-file "$kit_passphrase_file" --kube-addr "$KUBE_API" --apid "$APID") ;;
    backup) args+=(--kit "$kit_file" --passphrase-file "$kit_passphrase_file" --dir "$targets_dir"
      --guest-host "$HOST_ALIAS" --sftp-fingerprint "$fingerprint" --sftp-password-file "$sftp_password_file"
      --s3-secret-file "$s3_secret_file" --kube-addr "$KUBE_API" --apid "$APID") ;;
    access) args+=(--kube-addr "$KUBE_API" --apid "$APID") ;;
    restore) args+=(--backup "$backup_file" --kit "$kit_file" --passphrase-file "$kit_passphrase_file"
      --ilm-host "$RESTORE_HOST" --out "$restored_kit_file" --kube-addr "$KUBE_API" --apid "$APID") ;;
    bundle-load) args+=(--file "$bundle_file" --image "$bundle_image" --kit "$kit_file"
      --passphrase-file "$kit_passphrase_file" --kube-addr "$KUBE_API" --apid "$APID") ;;
    *) fail "unknown wizard part $part" ;;
  esac
  "$drive" "$part" "${args[@]}" >&2 || fail "drive $part failed"
}

drive_marker() { # drive_marker <--create|--check>: creates or checks the ILM group restore-marker with the credential
  local action="$1"
  local args=(--url "$CONSOLE_URL" --host-name "$host" --credential "$p12_file"
    --credential-password-file "$p12_password_file" --ui-url "https://$host:10443" --ui-addr "$UI_ADDR")
  if [[ "$action" == --check ]]; then
    args+=(--kit "$restored_kit_file" --passphrase-file "$kit_passphrase_file" --kube-addr "$KUBE_API" --apid "$APID"
      --client-secret-out "$client_secret_file")
  fi
  "$drive" ilm-marker "$action" "${args[@]}" >&2 || fail "drive ilm-marker $action failed"
}

drive_cbom() { # drive_cbom <--create|--check>: turns the CBOM repository on and stores a CBOM, or checks it is there
  local action="$1"
  "$drive" cbom "$action" --url "$CONSOLE_URL" --user "$admin" --password "$password" --seed-file "$seed_file" \
    --host-name "$host" --credential "$p12_file" --credential-password-file "$p12_password_file" \
    --ui-url "https://$host:10443" --ui-addr "$UI_ADDR" >&2 || fail "drive cbom $action failed"
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
      E2E_SETUP_CERT_FILE="$setup_cert" npx --no-install playwright test "$spec" --grep "$title") >&2 ||
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

# set_up answers wizard part 1 of the VM under test once its first-boot service waits for it.
set_up() {
  preset_hidden
  capture_setup_certificate
  if [[ "$setup_mode" == restore ]]; then drive_wizard part1; else wizard part1; fi
  seed="$(cat "$seed_file")"
}

wizard_part2() { wizard part2; }

# restore_backup restores the first VM's backup onto the second one, which waits for it.
restore_backup() {
  drive_wizard restore
}

# until_ready <seconds> <setup> <manager>: polls the VM's /status until it is ready within the seconds, running the
# function setup once the first-boot service waits for wizard part 1 and manager once the manager has taken over. It
# sets ready to the seconds it took.
until_ready() {
  local timeout="$1" on_setup="$2" on_manager="$3"
  local start=$SECONDS reached=false last="" set_up_done=false managed=false current elapsed state
  while :; do
    elapsed=$((SECONDS - start))
    if current="$(curl -sf --max-time 5 "$STATUS_URL")"; then
      status="$current" reached=true
      state="$(field state)/$(field step)"
      if [[ "$state" != "$last" ]]; then log "${elapsed}s $state $(field message)"; last="$state"; fi
      case "$(field state)" in
        setup)
          if ! $set_up_done; then
            "$on_setup"
            set_up_done=true
            log "$((SECONDS - start))s wizard part 1 applied"
          fi ;;
        addons | platform)
          if $set_up_done && ! $managed; then
            "$on_manager"
            managed=true
            log "$((SECONDS - start))s $on_manager done"
          fi ;;
        ready) break ;;
        *) ;; # still booting; keep polling
      esac
    elif ! $reached && [[ "$elapsed" -ge "$STATUS_TIMEOUT" ]]; then
      fail "status not reachable within ${STATUS_TIMEOUT}s"
    fi
    [[ "$elapsed" -lt "$timeout" ]] || fail "not ready within ${timeout}s"
    sleep 5
  done
  ready=$((SECONDS - start))
  $managed || fail "ready without the wizard"
}

start_vm() { # start_vm: starts the VM under test with a preset of the setup code
  HOST_ALIAS="$HOST_ALIAS" SMBIOS_OEM="$PRESET_KEY$code_hash" "$VM" start "$mode" "$arch" "$dir" || fail "VM did not start"
}

ui_answers() { # ui_answers: waits until the ILM UI of the VM under test answers 200
  local answer="" _
  for _ in $(seq 12); do
    answer="$(curl -sk -o /dev/null -w '%{http_code}' --max-time 10 \
      --resolve "$host:10443:127.0.0.1" "https://$host:10443/administrator/" || true)"
    [[ "$answer" != 200 ]] || break
    sleep 5
  done
  [[ "$answer" == 200 ]] || fail "UI answered ${answer:-nothing}, expected 200"
}

# restore_test stops the first VM, sets up the second one on a new disk to restore and restores the first VM's
# backup onto it, then checks the first VM's credential, Keycloak and the CBOM on it.
restore_test() {
  local backups=("$targets_dir"/sftp/*.ilmbackup)
  [[ ${#backups[@]} -eq 1 && -f "${backups[0]}" ]] || fail "the SFTP target holds no single backup to restore"
  backup_file="${backups[0]}"
  "$VM" stop "$dir" || fail "the first VM did not stop"
  # The disk is not needed any more; dropping it leaves room for the second VM's.
  rm -f "$dir/disk.qcow2"
  log "first VM stopped, restoring $(basename "$backup_file") onto a second VM"
  earlier=("$code" "$code_hash" "$password" "$seed")
  dir="$restore_dir" host="$RESTORE_HOST" admin="$RESTORE_ADMIN" setup_mode=restore
  seed_file="$dir/totp.seed" setup_cert="$dir/setup-cert.pem" seed="" status=""
  serials+=("$dir/serial.log")
  draw_wizard_secrets
  start_vm
  until_ready "$RESTORE_READY_TIMEOUT" set_up restore_backup
  restored=$ready
  ui_answers
  drive_marker --check
  drive_cbom --check
  client_secret="$(cat "$client_secret_file")" || fail "drive ilm-marker kept no client secret"
}

for d in "$first_dir" "$restore_dir"; do
  "$VM" stop "$d" >/dev/null 2>&1 || true
  rm -rf "$d"
done
mkdir -p "$dir"
(cd "$REPO" && go build -o "$drive" ./test/boot/drive) || fail "could not build test/boot/drive"
if [[ "$E2E" == 1 ]]; then
  [[ -d "$REPO/test/e2e/node_modules/@playwright/test" ]] || fail "test/e2e has no node_modules: run npm ci there"
fi
if [[ "$BUNDLE" == 1 ]]; then
  [[ -f "$bundle_file" ]] || fail "$bundle_file not found, make it with test/boot/bundle.sh"
  command -v jq >/dev/null || fail "jq is required"
  # shellcheck disable=SC2016 # yq and build/lib.sh are expanded by the inner shell
  curl_refs="$(cd "$REPO" && ARCH="$arch" bash -c 'source build/lib.sh &&
    yq ".images[] | select(.ref | test(\"^hub\\.omnitrustregistry\\.com/ilm/curl:\")) | .ref" release.yaml')" ||
    fail "could not read release.yaml"
  [[ -n "$curl_refs" && "$curl_refs" != *$'\n'* ]] || fail "release.yaml does not name exactly one curl image"
  curl_digest="$(bundle_digest "$bundle_file" "$curl_refs")" || fail "the bundle does not list $curl_refs once"
  bundle_image="$ZOT/${curl_refs%:*}@$curl_digest"
fi
trap 'rm -f "$kit_file" "$kit_passphrase_file" "$sftp_password_file" "$s3_secret_file" "$p12_file" \
    "$p12_password_file" "$restored_kit_file" "$client_secret_file"
  if [[ -n "$targets_pid" ]]; then kill "$targets_pid" 2>/dev/null || true; fi
  "$VM" stop "$first_dir"
  if [[ "$RESTORE" == 1 ]]; then "$VM" stop "$restore_dir"; fi' EXIT
(umask 077 && printf '%s' "$kit_passphrase" >"$kit_passphrase_file" &&
  printf '%s' "$sftp_password" >"$sftp_password_file" && printf '%s' "$s3_secret" >"$s3_secret_file" &&
  printf '%s' "$p12_password" >"$p12_password_file") ||
  fail "could not write the secret files"
start_vm
until_ready "$READY_TIMEOUT" set_up wizard_part2
ui_answers

answer="$(curl -sk -o /dev/null -w '%{http_code}' --max-time 10 "$CONSOLE_URL/healthz" || true)"
[[ "$answer" == 200 ]] || fail "console health check answered ${answer:-nothing}, expected 200"
drive_wizard check
if [[ "$E2E" == 1 ]]; then
  browse settings.spec.ts
  log "console settings changed in the browser"
fi
# With E2E=1 the browser issued the kit; drive then only checks it.
drive_wizard kit
if [[ "$RESTORE" == 1 ]]; then
  drive_marker --create
  drive_cbom --create
fi
serve_targets
drive_wizard backup "$targets_fingerprint"
drive_wizard access
if [[ "$BUNDLE" == 1 ]]; then drive_wizard bundle-load; fi
first_ready=$ready
if [[ "$RESTORE" == 1 ]]; then restore_test; fi

if leaked; then
  fail "serial log contains key material or a secret of the wizard"
fi

if [[ "$RESTORE" == 1 ]]; then echo "PASS $first_ready restored $restored"; else echo "PASS $first_ready"; fi
