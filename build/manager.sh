#!/usr/bin/env bash
# Builds the Appliance Manager for $ARCH and pushes its image as $MANAGER_REF, which cache.sh caches.
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "$0")/lib.sh"

ctx="$OUT/manager"
rm -rf "$ctx"
mkdir -p "$ctx"

# The binary embeds web/dist/app, so the console is built first.
log "building the console"
(cd web && npm ci && npm run build)

log "building manager $VERSION for $ARCH"
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
  -o "$ctx/manager" ./cmd/manager

log "building and pushing $MANAGER_REF"
docker buildx build --platform "linux/$ARCH" --provenance=false -f Dockerfile.manager \
  -t "$MANAGER_REF" --push "$ctx"
