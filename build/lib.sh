# Shared settings for the image build scripts; sourced, not run.
#
# Environment: ARCH (amd64 or arm64, default arm64) and REGISTRY (default localhost:5005,
# a local registry:2 container). Images are named $REGISTRY/<name>:<version>-<arch>.
# shellcheck shell=bash
set -euo pipefail
export LC_ALL=C

cd "$(dirname "${BASH_SOURCE[0]}")/.."

log() { echo "[$(basename "$0" .sh)] $*" >&2; }
die() { log "error: $*"; exit 1; }
# sha256 <file> prints the file's SHA-256 (sha256sum on Linux, shasum on macOS).
sha256() {
  local file="$1"
  if command -v sha256sum >/dev/null; then sha256sum "$file"; else shasum -a 256 "$file"; fi | cut -d' ' -f1
}

yq() {
  if type -P yq >/dev/null; then
    command yq "$@"
  else
    go run github.com/mikefarah/yq/v4@v4.54.1 "$@"
  fi
}

ARCH="${ARCH:-arm64}"
REGISTRY="${REGISTRY:-localhost:5005}"
case "$ARCH" in amd64 | arm64) ;; *) die "ARCH must be amd64 or arm64, not $ARCH" ;; esac

{ read -r VERSION; read -r TALOS_VERSION; read -r IMAGER; } < <(yq '.version, .talos.version, .talos.imager' release.yaml)
[[ "$IMAGER" == *":$TALOS_VERSION@sha256:"* ]] || die "release.yaml: talos.imager $IMAGER is not $TALOS_VERSION by digest"

OUT="out/$ARCH"
EXTENSION_REF="$REGISTRY/firstboot-extension:$VERSION-$ARCH"
INSTALLER_REF="$REGISTRY/installer:$VERSION-$ARCH"
# shellcheck disable=SC2034 # used by the scripts that source this file
readonly ARCH REGISTRY VERSION TALOS_VERSION IMAGER OUT EXTENSION_REF INSTALLER_REF
mkdir -p "$OUT"

# A registry on localhost is plain HTTP; the imager container reaches it through the Docker host
# (the --add-host mapping makes the name resolve on Linux as well as Docker Desktop).
local_registry() { [[ "$REGISTRY" == localhost:* ]]; }
insecure_flag() { if local_registry; then echo --insecure; fi; }
imager_ref() { echo "${1/#localhost:/host.docker.internal:}"; }

# imager <command> [args...] runs the Talos imager with $OUT mounted at /out, adding the
# first-boot extension, and keeps its log in $OUT/imager-<command>.log.
imager() {
  local cmd="$1" logfile="$OUT/imager-$1.log"
  shift
  # A registry that is not local may be private: give the imager (root in its container) the host's credentials.
  local docker_config="${DOCKER_CONFIG:-$HOME/.docker}/config.json" auth=()
  if ! local_registry && [[ -f "$docker_config" ]]; then auth=(-v "$docker_config:/root/.docker/config.json:ro"); fi
  log "imager $cmd (log: $logfile)"
  # shellcheck disable=SC2046 # insecure_flag prints zero or one word
  docker run --rm --add-host=host.docker.internal:host-gateway -v "$PWD/$OUT:/out" ${auth[@]+"${auth[@]}"} "$IMAGER" "$cmd" --arch "$ARCH" $(insecure_flag) \
    --system-extension-image "$(imager_ref "$EXTENSION_REF")" "$@" >"$logfile" 2>&1 ||
    { tail -20 "$logfile" >&2; die "imager $cmd failed"; }
}
