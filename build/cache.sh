#!/usr/bin/env bash
# Builds the image cache (OCI layout) from every image of the release plus the installer.
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "$0")/lib.sh"

command -v talosctl >/dev/null || die "talosctl $TALOS_VERSION is not on PATH"
client="$(talosctl version --client --short | awk '/^Talos/ { print $2 }')"
[[ "$client" == "$TALOS_VERSION" ]] || die "talosctl is $client, release.yaml needs $TALOS_VERSION"

# Prints "<ref> <digest>" for every release image of this architecture.
release_images() {
  yq ".images[] | select((.arches // []) | length == 0 or contains([\"$ARCH\"])) | .ref + \" \" + .digest" release.yaml
}

# The cache is looked up by tag, so a tag that moved since release.yaml was written would ship other content.
# This catches it early; verify_cache below proves what cache-create actually stored.
check_digests() {
  local ref want got failed=0
  while read -r ref want; do
    got="$(crane digest "$ref")" || die "cannot resolve $ref"
    if [[ "$got" != "$want" ]]; then
      log "digest mismatch: $ref is $got, release.yaml has $want"
      failed=1
    fi
  done < <(release_images)
  [[ "$failed" == 0 ]] || die "release.yaml digests do not match the registries"
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

# cache-create pulls tag-only references again, so a tag could move after check_digests. The cache is a single-layer
# OCI image whose layer holds a registry tree: manifests/<host>/<repository>/reference/<tag> is the manifest (index)
# bytes served for a tag and manifests/<host>/<repository>/digest/sha256-<hex> the one served for a digest.
verify_cache() {
  local cache="$OUT/image-cache.oci" blob layer tree ref want name host path tag file got failed=0
  blob="$(jq -r '.manifests[0].digest' "$cache/index.json")"
  layer="$(jq -r '.layers[0].digest' "$cache/blobs/${blob/://}")"
  tree="$(mktemp -d)"
  tar -xzf "$cache/blobs/${layer/://}" -C "$tree" --exclude 'blob/*'
  while read -r ref want; do
    name="${ref%%@*}"
    tag="${name##*:}"
    name="${name%:*}"
    host="${name%%/*}"
    if [[ "$name" != */* ]]; then
      host=docker.io path="library/$name"
    elif ! [[ "$host" =~ [.:] || "$host" == localhost ]]; then
      host=docker.io path="$name"
    else
      path="${name#*/}"
    fi
    if [[ "$host" == docker.io ]] && [[ "$path" != */* ]]; then path="library/$path"; fi
    if [[ "$ref" == *@sha256:* ]]; then
      file="$tree/manifests/${host//:/_}/$path/digest/${want/:/-}"
    else
      file="$tree/manifests/${host//:/_}/$path/reference/$tag"
    fi
    if [[ ! -f "$file" ]]; then
      log "cache has no manifest for $ref"
      failed=1
      continue
    fi
    got="sha256:$(sha256 "$file")"
    if [[ "$got" != "$want" ]]; then
      log "cache content mismatch: $ref is $got in the cache, release.yaml has $want"
      failed=1
    fi
  done < <(release_images)
  rm -rf "$tree"
  [[ "$failed" == 0 ]] || die "the image cache does not contain the images pinned in release.yaml"
}

command -v jq >/dev/null || die "jq is not on PATH"
log "verifying cached image digests"
verify_cache
