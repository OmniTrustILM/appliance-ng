#!/usr/bin/env bash
# Builds the Talos installer with the ilm-firstboot extension, pushes it as $INSTALLER_REF and writes the pushed
# digest to $OUT/installer.digest.
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "$0")/lib.sh"

rm -f "$OUT/installer-$ARCH.tar"
imager installer --platform metal
log "pushing $INSTALLER_REF"
# shellcheck disable=SC2046 # insecure_flag prints zero or one word
pushed="$(crane push $(insecure_flag) "$OUT/installer-$ARCH.tar" "$INSTALLER_REF")" || die "cannot push $INSTALLER_REF"
[[ "${pushed##*@}" == sha256:* ]] || die "crane push printed no digest for $INSTALLER_REF"
echo "${pushed##*@}" >"$OUT/installer.digest"
rm -f "$OUT/installer-$ARCH.tar"
