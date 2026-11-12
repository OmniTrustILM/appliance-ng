#!/usr/bin/env bash
# Builds the Talos installer with the ilm-firstboot extension and pushes it as $INSTALLER_REF.
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "$0")/lib.sh"

rm -f "$OUT/installer-$ARCH.tar"
imager installer --platform metal
log "pushing $INSTALLER_REF"
# shellcheck disable=SC2046 # insecure_flag prints zero or one word
crane push $(insecure_flag) "$OUT/installer-$ARCH.tar" "$INSTALLER_REF"
rm -f "$OUT/installer-$ARCH.tar"
