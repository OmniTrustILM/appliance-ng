#!/usr/bin/env bash
# Builds the boot ISO and the qcow2 disk image, both with the extension and the image cache,
# and writes SHA256SUMS.
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "$0")/lib.sh"

name="ilm-appliance-$VERSION-$ARCH"
[[ -d "$OUT/image-cache.oci" ]] || die "$OUT/image-cache.oci is missing, run cache.sh first"
rm -f "$OUT/metal-$ARCH.iso" "$OUT/metal-$ARCH.raw" "$OUT/metal-$ARCH.raw.zst" \
  "$OUT/$name.iso" "$OUT/$name.qcow2" "$OUT/SHA256SUMS"

imager iso --image-cache /out/image-cache.oci
mv "$OUT/metal-$ARCH.iso" "$OUT/$name.iso"

imager metal --image-cache /out/image-cache.oci
log "converting the raw disk image to $name.qcow2 (50G)"
zstd -d -q --rm "$OUT/metal-$ARCH.raw.zst"
qemu-img convert -O qcow2 "$OUT/metal-$ARCH.raw" "$OUT/$name.qcow2"
qemu-img resize -q "$OUT/$name.qcow2" 50G
rm -f "$OUT/metal-$ARCH.raw"

(cd "$OUT" && shasum -a 256 "$name.iso" "$name.qcow2" >SHA256SUMS)
ls -lh "$OUT/$name.iso" "$OUT/$name.qcow2" >&2
