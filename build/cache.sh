#!/usr/bin/env bash
# Builds the image cache (OCI layout) from every image of the release plus the installer.
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "$0")/lib.sh"

command -v talosctl >/dev/null || die "talosctl $TALOS_VERSION is not on PATH"
client="$(talosctl version --client --short | awk '/^Talos/ { print $2 }')"
[ "$client" = "$TALOS_VERSION" ] || die "talosctl is $client, release.yaml needs $TALOS_VERSION"

# The cache is looked up by tag, so a tag that moved since release.yaml was written would ship other content.
check_digests() {
  local ref want got failed=0
  while read -r ref want; do
    got="$(crane digest "$ref")" || die "cannot resolve $ref"
    if [ "$got" != "$want" ]; then
      log "digest mismatch: $ref is $got, release.yaml has $want"
      failed=1
    fi
  done < <(yq ".images[] | select((.arches // []) | length == 0 or contains([\"$ARCH\"])) | .ref + \" \" + .digest" release.yaml)
  [ "$failed" = 0 ] || die "release.yaml digests do not match the registries"
}

command -v crane >/dev/null || die "crane is not on PATH"
log "checking image digests"
check_digests

log "creating $OUT/image-cache.oci (log: $OUT/cache-create.log)"
# shellcheck disable=SC2046 # insecure_flag prints zero or one word
go run ./cmd/release images --arch "$ARCH" --installer "$INSTALLER_REF" |
  talosctl images cache-create --platform "linux/$ARCH" --force $(insecure_flag) --images=- \
    --image-cache-path "$OUT/image-cache.oci" --image-layer-cache-path out/layer-cache \
    >"$OUT/cache-create.log" 2>&1 || { tail -20 "$OUT/cache-create.log" >&2; die "cache-create failed"; }
du -sh "$OUT/image-cache.oci" >&2
