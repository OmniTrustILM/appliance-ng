#!/usr/bin/env bash
# Builds the first-boot binary for $ARCH and pushes the ilm-firstboot system extension image.
# The binary carries the installer reference that installer.sh pushes and cache.sh caches.
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "$0")/lib.sh"

ctx="$OUT/extension"
rm -rf "$ctx"
mkdir -p "$ctx"

log "building firstboot $VERSION for $ARCH, installer $INSTALLER_REF"
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath \
  -ldflags "-s -w -X main.version=$VERSION -X main.installerImage=$INSTALLER_REF" \
  -o "$ctx/firstboot" ./cmd/firstboot

yq ".metadata.version = \"$VERSION\"" extension/manifest.yaml >"$ctx/manifest.yaml"
cp extension/firstboot.yaml "$ctx/"

log "building and pushing $EXTENSION_REF"
docker buildx build --platform "linux/$ARCH" --provenance=false -f extension/Dockerfile \
  -t "$EXTENSION_REF" --push "$ctx"
