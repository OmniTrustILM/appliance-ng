#!/usr/bin/env bash
# Builds the unsigned system and platform bundles of release.yaml into out/<arch>/bundles/<kind>/, with
# release-notes/<version>.md as their notes when it exists. BUILT_IMAGES lists, space-separated, the images built with
# the appliance (the installer and the manager) as <ref>@sha256:<hex> for the system bundle. With PUSH=1 it pushes each bundle to
# $REGISTRY/<kind>:$TAG (TAG defaults to <version>-<arch>) with oras and writes the pushed reference,
# by digest, to out/<arch>/bundles/<kind>.ref.
# shellcheck source=build/lib.sh
source "$(dirname "$0")/lib.sh"

TAG="${TAG:-$VERSION-$ARCH}"
notes="release-notes/$VERSION.md"
notes_flag=()
[[ -f "$notes" ]] && notes_flag=(--notes "$notes")
image_flags=()
for ref in ${BUILT_IMAGES:-}; do image_flags+=(--image "$ref"); done

for kind in system platform; do
  dir="$OUT/bundles/$kind"
  rm -rf -- "$dir"
  log "building the $kind bundle into $dir"
  built=()
  [[ "$kind" == system ]] && built=(${image_flags[@]+"${image_flags[@]}"})
  go run ./cmd/bundle build --kind "$kind" --arch "$ARCH" ${built[@]+"${built[@]}"} ${notes_flag[@]+"${notes_flag[@]}"} --out "$dir"

  [[ "${PUSH:-}" == 1 ]] || continue
  command -v oras >/dev/null || die "oras is required to push the bundles"
  digest="$(jq -er --arg tag "$VERSION-$ARCH" \
    '.manifests[] | select(.annotations["org.opencontainers.image.ref.name"] == $tag) | .digest' "$dir/index.json")" \
    || die "$dir/index.json has no bundle index tagged $VERSION-$ARCH"
  plain_http=()
  if local_registry; then plain_http=(--to-plain-http); fi
  log "pushing the $kind bundle to $REGISTRY/$kind:$TAG"
  oras copy ${plain_http[@]+"${plain_http[@]}"} --from-oci-layout "$dir:$VERSION-$ARCH" "$REGISTRY/$kind:$TAG"
  echo "$REGISTRY/$kind@$digest" >"$OUT/bundles/$kind.ref"
done
