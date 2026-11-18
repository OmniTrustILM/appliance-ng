#!/usr/bin/env bash
# Builds the first-boot binary for $ARCH and pushes the ilm-firstboot system extension image.
# The binary carries the installer and manager references that installer.sh and manager.sh push and cache.sh caches.
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "$0")/lib.sh"

ctx="$OUT/extension"
rm -rf "$ctx"
mkdir -p "$ctx"

# The binary embeds web/dist/app for wizard part 1, so the console is built first.
log "building the console"
(cd web && npm ci --ignore-scripts && npm run build)

log "building firstboot $VERSION for $ARCH, installer $INSTALLER_REF, manager $MANAGER_REF"
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath \
  -ldflags "-s -w -X main.version=$VERSION -X main.installerImage=$INSTALLER_REF -X main.managerImage=$MANAGER_REF" \
  -o "$ctx/firstboot" ./cmd/firstboot

yq ".metadata.version = \"$VERSION\"" extension/manifest.yaml >"$ctx/manifest.yaml"
cp extension/firstboot.yaml "$ctx/"

log "building and pushing $EXTENSION_REF"
docker buildx build --platform "linux/$ARCH" --provenance=false -f extension/Dockerfile \
  -t "$EXTENSION_REF" --push "$ctx"
