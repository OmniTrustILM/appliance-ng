# appliance-ng

A turnkey ILM appliance built on Talos Linux. This repository holds the tooling that
builds and configures the appliance image.

## Build

Prerequisites: Docker with buildx, Go, `qemu-img`, `zstd`, `jq`, `crane` and `talosctl` v1.14.5, the Talos version in
`release.yaml`. A local registry holds the intermediate images:

```bash
docker run -d --name appliance-registry -p 5005:5000 registry:2
make images ARCH=arm64        # or ARCH=amd64; REGISTRY defaults to localhost:5005
```

The ISO, the qcow2 disk image (50 GB) and `SHA256SUMS` are written to `out/<arch>/`.

## Boot test

`make boot-test` boots a built image in QEMU with no route out and waits until the appliance is ready:

```bash
make boot-test MEDIA=iso ARCH=arm64     # MEDIA=iso|qcow2, ARCH=arm64|amd64
```

It passes when `GET /status` reports `ready` within 25 minutes, the ILM UI answers 200 and the serial log holds no
key material, and prints `PASS <seconds to ready>` or `FAIL <reason>`. The VM gets 4 CPUs, 12 GB of memory and a
50 GB disk; its state, including `serial.log`, is in `out/boot/<arch>-<media>/`.

Hosts: macOS on Apple silicon for arm64 (QEMU from Homebrew, HVF), Linux with KVM for the host's architecture
(`qemu-system-x86` and `ovmf` for amd64, `qemu-system-arm` and `qemu-efi-aarch64` for arm64). The VM forwards
`127.0.0.1:18443` to the status endpoint, `127.0.0.1:10443` to the UI and `127.0.0.1:15000` to the Talos API. To run a
VM by hand, use `test/boot/vm.sh start|stop|status`.

## CI

`.github/workflows/build.yaml` builds the images on every pull request and every push to `main`, for amd64
(`ubuntu-latest`) and arm64 (`ubuntu-24.04-arm`), and boot-tests the amd64 ISO with KVM. The arm64 runner has no KVM, so
arm64 is boot-tested locally on macOS. The build itself is the reusable workflow `.github/workflows/images.yaml`, called
by two jobs: `pr` runs with a read-only token against a `registry:2` container inside the job and never logs in to GHCR,
and `publish` (pushes to `main` and manual runs on `main`) has `packages: write`, pushes the intermediate images to
`ghcr.io/omnitrustilm/appliance-ng` and keeps the ISO, qcow2 and `SHA256SUMS` as artifacts for three days. The build
needs at least 40 GB of free disk after the runner clean-up and fails with a clear message otherwise.

## License

Licensed under the Apache License 2.0. See [LICENSE](LICENSE).
