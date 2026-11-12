#!/usr/bin/env bash
# Runs an appliance VM in QEMU for the boot test.
#
#   vm.sh start <iso|disk> <arch> <dir>   start a VM with its state in <dir>
#   vm.sh stop <dir>                      stop it
#   vm.sh status <dir>                    print running or stopped
#
# iso:  boots out/<arch>/ilm-appliance-<version>-<arch>.iso with a blank disk; the disk comes first in the boot
#       order, so an installed disk boots before the CD.
# disk: boots a copy of out/<arch>/ilm-appliance-<version>-<arch>.qcow2.
# A disk already in <dir> is reused, so starting again resumes from it.
#
# Hosts: macOS on Apple silicon (arm64, HVF, edk2 from Homebrew's qemu); Linux with KVM (amd64 with OVMF from the
# ovmf package, arm64 with AAVMF from qemu-efi-aarch64). The guest has no route out (restrict=on) and a
# virtio-rng device. Host forwards, bound to 127.0.0.1: 18443->8443 (status), 10443->443 (UI), 15000->50000 (apid).
# CPUS, MEM and DISK override 4 / 12G / 50G (DISK applies when the disk is created). Serial log: <dir>/serial.log.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"

log() { echo "[vm] $*" >&2; }
die() { log "error: $*"; exit 1; }

usage() { sed -n '2,6p' "$0" >&2; exit 2; }

first_file() { # first_file <path>...: prints the first path that exists
  local f
  for f in "$@"; do
    if [[ -f "$f" ]]; then echo "$f"; return 0; fi
  done
  return 1
}

# firmware <arch> sets QEMU, MACHINE, CODE and VARS for this host.
firmware() {
  local arch="$1" host share
  host="$(uname -s)/$(uname -m)"
  case "$host/$arch" in
    Darwin/arm64/arm64)
      share="$(brew --prefix qemu)/share/qemu"
      QEMU=qemu-system-aarch64
      MACHINE=(-machine "virt,highmem=on" -accel hvf -cpu host -device virtio-gpu-pci)
      CODE="$share/edk2-aarch64-code.fd"
      VARS="$share/edk2-arm-vars.fd" ;;
    Linux/x86_64/amd64)
      QEMU=qemu-system-x86_64
      MACHINE=(-machine q35 -accel kvm -cpu host)
      CODE="$(first_file /usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/OVMF/OVMF_CODE.fd)" ||
        die "OVMF firmware not found, install the ovmf package"
      VARS="${CODE/CODE/VARS}" ;;
    Linux/aarch64/arm64)
      QEMU=qemu-system-aarch64
      MACHINE=(-machine virt -accel kvm -cpu host -device virtio-gpu-pci)
      CODE="$(first_file /usr/share/AAVMF/AAVMF_CODE.fd)" ||
        die "AAVMF firmware not found, install the qemu-efi-aarch64 package"
      VARS=/usr/share/AAVMF/AAVMF_VARS.fd ;;
    *) die "cannot run a $arch VM with hardware acceleration on $host" ;;
  esac
  [[ -f "$VARS" ]] || die "UEFI variables template $VARS not found"
}

running() { # running <dir>
  local pid
  pid="$(cat "$1/qemu.pid" 2>/dev/null)" || return 1
  kill -0 "$pid" 2>/dev/null
}

start() {
  local mode="$1" arch="$2" dir="$3" version name
  version="$(sed -n 's/^version: *//p' "$REPO/release.yaml")"
  name="ilm-appliance-$version-$arch"
  firmware "$arch"
  mkdir -p "$dir"
  dir="$(cd "$dir" && pwd)"
  running "$dir" && die "a VM is already running in $dir"

  local drives=(-drive "if=none,id=hd0,file=$dir/disk.qcow2,format=qcow2,discard=unmap"
                -device "virtio-blk-pci,drive=hd0,bootindex=0")
  case "$mode" in
    iso)
      local iso="$REPO/out/$arch/$name.iso"
      [[ -f "$iso" ]] || die "$iso not found, run make images ARCH=$arch"
      [[ -f "$dir/disk.qcow2" ]] || qemu-img create -q -f qcow2 "$dir/disk.qcow2" "${DISK:-50G}"
      drives+=(-drive "if=none,id=cd0,file=$iso,media=cdrom,readonly=on"
               -device "virtio-scsi-pci,id=scsi0" -device "scsi-cd,drive=cd0,bootindex=1") ;;
    disk)
      local image="$REPO/out/$arch/$name.qcow2"
      [[ -f "$image" ]] || die "$image not found, run make images ARCH=$arch"
      if [[ ! -f "$dir/disk.qcow2" ]]; then
        cp "$image" "$dir/disk.qcow2"
        if [[ -n "${DISK:-}" ]]; then qemu-img resize -q "$dir/disk.qcow2" "$DISK"; fi
      fi ;;
    *) usage ;;
  esac
  [[ -f "$dir/vars.fd" ]] || cp "$VARS" "$dir/vars.fd"

  local fwd="hostfwd=tcp:127.0.0.1:18443-:8443,hostfwd=tcp:127.0.0.1:10443-:443,hostfwd=tcp:127.0.0.1:15000-:50000"
  : >"$dir/serial.log"
  "$QEMU" -name "appliance-$arch-$mode" "${MACHINE[@]}" -smp "${CPUS:-4}" -m "${MEM:-12G}" \
    -drive "if=pflash,format=raw,readonly=on,file=$CODE" \
    -drive "if=pflash,format=raw,file=$dir/vars.fd" \
    "${drives[@]}" \
    -netdev "user,id=n0,restrict=on,$fwd" -device virtio-net-pci,netdev=n0 \
    -device virtio-rng-pci \
    -display none \
    -serial "file:$dir/serial.log" \
    -pidfile "$dir/qemu.pid" -daemonize
  log "started $mode $arch in $dir (pid $(cat "$dir/qemu.pid"))"
}

stop() {
  local dir="$1" pid
  if ! running "$dir"; then log "no VM running in $dir"; return 0; fi
  pid="$(cat "$dir/qemu.pid")"
  kill "$pid"
  for _ in $(seq 20); do
    running "$dir" || break
    sleep 0.5
  done
  if running "$dir"; then kill -9 "$pid"; fi
  log "stopped pid $pid"
}

case "${1:-}" in
  start) [[ $# -eq 4 ]] || usage; start "$2" "$3" "$4" ;;
  stop) [[ $# -eq 2 ]] || usage; stop "$2" ;;
  status) [[ $# -eq 2 ]] || usage; if running "$2"; then echo running; else echo stopped; fi ;;
  *) usage ;;
esac
