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
# IMAGE, when set, names the ISO or qcow2 file to boot instead.
# A disk already in <dir> is reused, so starting again resumes from it.
#
# Hosts: macOS on Apple silicon (arm64, HVF, edk2 from Homebrew's qemu); Linux with KVM (amd64 with OVMF from the
# ovmf package, arm64 with AAVMF from qemu-efi-aarch64). The guest has no route out (restrict=on) and a
# virtio-rng device. Host forwards, bound to 127.0.0.1: 18443->8443 (console), 18444->8444 (status), 10443->443 (UI),
# 15000->50000 (apid), 16443->6443 (the cluster's API server). HOST_ALIAS, when set, is an unused address of QEMU's
# user network at which the guest reaches 127.0.0.1 ports 2222 (SFTP) and 9000 (S3) of the host, through guest
# forwards that run nc; nothing else of the host is reachable. PORT_OFFSET, when set, is added to every host port and
# to the SFTP and S3 ports of both sides, so that the VM runs beside others.
# CPUS, MEM and DISK override 4 / 12G / 50G (DISK applies when the disk is created). SMBIOS_OEM, when set, is passed
# to the guest as an SMBIOS type 11 OEM string (no commas). Serial log: <dir>/serial.log.
# EXTRA_DISK=<size> adds a second blank disk with serial ilm-data (created with the VM). It sits at a fixed PCI
# address because a device in the automatic order would rename the NIC and lose its static address.
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

running() { # running <dir>: true only if the pid file names a live QEMU started for <dir>
  local dir pid
  dir="$(cd "$1" 2>/dev/null && pwd)" || return 1 # same absolute spelling start gives QEMU
  pid="$(cat "$dir/qemu.pid" 2>/dev/null)" || return 1
  # A stale pid file may name an unrelated process that reused the pid; only our QEMU has this pid file
  # on its command line.
  ps -p "$pid" -o command= 2>/dev/null | grep -qF -- "$dir/qemu.pid"
}

start() {
  local mode="$1" arch="$2" dir="$3" version name
  version="$(sed -n 's/^version: *//p' "$REPO/release.yaml")"
  name="ilm-appliance-$version-$arch"
  firmware "$arch"
  mkdir -p "$dir"
  dir="$(cd "$dir" && pwd)"
  running "$dir" && die "a VM is already running in $dir"
  if [[ -n "${HOST_ALIAS:-}" ]]; then
    command -v nc >/dev/null || die "nc is required for the guest forwards to the host"
  fi

  local drives=(-drive "if=none,id=hd0,file=$dir/disk.qcow2,format=qcow2,discard=unmap"
                -device "virtio-blk-pci,drive=hd0,bootindex=0,serial=ilm-system")
  case "$mode" in
    iso)
      local iso="${IMAGE:-$REPO/out/$arch/$name.iso}"
      [[ -f "$iso" ]] || die "$iso not found, run make images ARCH=$arch"
      [[ -f "$dir/disk.qcow2" ]] || qemu-img create -q -f qcow2 "$dir/disk.qcow2" "${DISK:-50G}"
      drives+=(-drive "if=none,id=cd0,file=$iso,media=cdrom,readonly=on"
               -device "virtio-scsi-pci,id=scsi0" -device "scsi-cd,drive=cd0,bootindex=1") ;;
    disk)
      local image="${IMAGE:-$REPO/out/$arch/$name.qcow2}"
      [[ -f "$image" ]] || die "$image not found, run make images ARCH=$arch"
      if [[ ! -f "$dir/disk.qcow2" ]]; then
        cp "$image" "$dir/disk.qcow2"
        if [[ -n "${DISK:-}" ]]; then qemu-img resize -q "$dir/disk.qcow2" "$DISK"; fi
      fi ;;
    *) usage ;;
  esac
  if [[ -n "${EXTRA_DISK:-}" ]]; then
    [[ -f "$dir/extra.qcow2" ]] || qemu-img create -q -f qcow2 "$dir/extra.qcow2" "$EXTRA_DISK"
    drives+=(-drive "if=none,id=hd1,file=$dir/extra.qcow2,format=qcow2,discard=unmap"
             -device "virtio-blk-pci,drive=hd1,addr=0x10,serial=ilm-data")
  fi
  [[ -f "$dir/vars.fd" ]] || cp "$VARS" "$dir/vars.fd"

  local o="${PORT_OFFSET:-0}"
  local fwd="hostfwd=tcp:127.0.0.1:$((18443 + o))-:8443,hostfwd=tcp:127.0.0.1:$((18444 + o))-:8444"
  fwd+=",hostfwd=tcp:127.0.0.1:$((10443 + o))-:443,hostfwd=tcp:127.0.0.1:$((15000 + o))-:50000"
  fwd+=",hostfwd=tcp:127.0.0.1:$((16443 + o))-:6443"
  if [[ -n "${HOST_ALIAS:-}" ]]; then
    local sftp=$((2222 + o)) s3=$((9000 + o))
    fwd+=",guestfwd=tcp:$HOST_ALIAS:$sftp-cmd:nc 127.0.0.1 $sftp,guestfwd=tcp:$HOST_ALIAS:$s3-cmd:nc 127.0.0.1 $s3"
  fi
  local smbios=()
  if [[ -n "${SMBIOS_OEM:-}" ]]; then smbios=(-smbios "type=11,value=$SMBIOS_OEM"); fi
  : >"$dir/serial.log"
  "$QEMU" -name "appliance-$arch-$mode" "${MACHINE[@]}" -smp "${CPUS:-4}" -m "${MEM:-12G}" \
    -drive "if=pflash,format=raw,readonly=on,file=$CODE" \
    -drive "if=pflash,format=raw,file=$dir/vars.fd" \
    "${drives[@]}" \
    -netdev "user,id=n0,restrict=on,$fwd" -device virtio-net-pci,netdev=n0 \
    -device virtio-rng-pci \
    "${smbios[@]}" \
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
