FROM scratch
COPY manifest.yaml /manifest.yaml
COPY firstboot.yaml /rootfs/usr/local/etc/containers/firstboot.yaml
COPY firstboot /rootfs/usr/local/lib/containers/firstboot/firstboot

# The image only carries files for the Talos imager and never runs.
USER 65534:65534
