#!/usr/bin/env bash
# Builds the bundle file the boot test loads with BUNDLE=1: the platform bundle of release.yaml, signed with a
# throw-away cosign key and exported as an offline bundle file. It names the next ILM patch version, so that the
# appliance, which runs the platform of release.yaml, takes it for a newer one; its images are those of release.yaml.
# With SAME_ILM=1 it keeps release.yaml's ILM version and names the next appliance patch version as the bundle's
# instead, for an appliance built with an older ILM_VERSION: the platform update test installs it and rolls it back to
# the appliance's own platform, which a bundle of the appliance's version would stand in for.
#
#   bundle.sh <arch> <cosign key> <file>
#
# <cosign key> is the cosign.key of cosign generate-key-pair, with cosign.pub beside it, and its password is in
# COSIGN_PASSWORD (empty for a key made with COSIGN_PASSWORD=''). The bundle is pushed to REGISTRY (default
# localhost:5005, see build/lib.sh) as test/platform:<version>-<arch> for cosign to sign it, and signed with
# --tlog-upload=false --use-signing-config=false, so nothing reaches a public log. Needs go, jq, cosign v3 and oras
# 1.4 or later.
set -euo pipefail

usage() { sed -n '7p' "$0" >&2; exit 2; }
[[ $# -eq 3 ]] || usage
export ARCH="$1"
key="$(cd "$(dirname "$2")" && pwd)/$(basename "$2")"
file="$(mkdir -p "$(dirname "$3")" && cd "$(dirname "$3")" && pwd)/$(basename "$3")"
# shellcheck source-path=SCRIPTDIR source=../../build/lib.sh
source "$(dirname "$0")/../../build/lib.sh"
for tool in cosign oras jq; do
  command -v "$tool" >/dev/null || die "$tool is required"
done

work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
mkdir "$work/root" "$work/export"
ilm="$(yq '.platform.ilmVersion' release.yaml)"
bundle_version="$VERSION"
if [[ "${SAME_ILM:-0}" == 1 ]]; then
  bundle_version="${VERSION%.*}.$((${VERSION##*.} + 1))"
  yq ".version = \"$bundle_version\"" release.yaml >"$work/root/release.yaml"
else
  ilm="${ilm%.*}.$((${ilm##*.} + 1))"
  yq ".platform.ilmVersion = \"$ilm\"" release.yaml >"$work/root/release.yaml"
fi
ln -s "$PWD/addons" "$work/root/addons"
log "building the platform bundle $bundle_version with ILM $ilm for $ARCH"
go run ./cmd/bundle build --root "$work/root" --kind platform --arch "$ARCH" --out "$work/bundle"

tag="$bundle_version-$ARCH"
repo="$REGISTRY/test/platform"
digest="$(jq -er --arg tag "$tag" \
  '.manifests[] | select(.annotations["org.opencontainers.image.ref.name"] == $tag) | .digest' "$work/bundle/index.json")" ||
  die "the bundle has no index tagged $tag"
to_plain=() from_plain=()
if local_registry; then to_plain=(--to-plain-http) from_plain=(--from-plain-http); fi
log "pushing it to $repo:$tag and signing it"
oras copy ${to_plain[@]+"${to_plain[@]}"} --from-oci-layout "$work/bundle:$tag" "$repo:$tag"
cosign sign --key "$key" --tlog-upload=false --use-signing-config=false -y "$repo@$digest"

log "exporting it to $file"
oras copy -r ${from_plain[@]+"${from_plain[@]}"} "$repo:$tag" --to-oci-layout "$work/export:$tag"
tar -C "$work/export" -cf "$file" oci-layout index.json blobs
go run ./cmd/bundle verify --key "${key%.key}.pub" --arch "$ARCH" "$file"
