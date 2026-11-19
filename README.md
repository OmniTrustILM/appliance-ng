# appliance-ng

A turnkey ILM appliance built on Talos Linux. This repository holds the tooling that
builds the appliance image and the Appliance Manager, the REST API and web console that set up and run it.

## First boot

Use a trusted network segment for the first boot. Until setup has installed the node, the Talos maintenance API on
port 50000 accepts a machine configuration from anyone who can reach it, without credentials. This release has no way
to lock it. The setup wizard on port 8443 is protected only by the setup code and the certificate check below.

1. Boot the ISO or disk image. The node takes an address from DHCP.
2. Switch to the first virtual console (Alt+F1 on a keyboard, or the VM's console) and read three lines:

   ```text
   Open https://<address>:8443/setup/
   Certificate SHA-256 <fingerprint>
   Setup code XXXXX-XXXXX
   ```

3. Open the address in a browser. The setup service uses a self-signed certificate, so the browser warns. Compare the
   certificate's SHA-256 fingerprint with the one on the console before you continue.
4. Enter the setup code. Letters may be lower case and the dash is optional. After five wrong attempts the code locks
   for a minute; each further wrong attempt doubles the lock, up to an hour.

The code is 10 characters from `23456789ABCDEFGHJKLMNPQRSTUVWXYZ`, new on every first boot, and is shown nowhere else.

### Preset setup code

A hypervisor can set the code instead, for unattended provisioning and for tests. Add an SMBIOS type 11 (OEM strings)
entry with the SHA-256 of the code, as 64 hexadecimal characters:

```text
io.otilm.appliance.setup-code-sha256=<sha256 of the code>
```

QEMU: `-smbios type=11,value=io.otilm.appliance.setup-code-sha256=<hash>`. The appliance then does not generate a code
and the console says that the code is preset. An entry that is not 64 hexadecimal characters is ignored and the
appliance generates a code.

Hash the code in upper case without dashes or spaces: the appliance compares the hash of what you type after
removing dashes and spaces and converting ASCII letters to upper case. The appliance cannot judge the strength of a
preset code, and the same attempt limit applies to it. Use at least 128 bits of entropy, for example 32 random
characters from the alphabet above. Anyone who controls the hypervisor can read the setting.

## Setup wizard

Setup has two parts.

**Part 1** runs on port 8443 before the node is installed:

1. **Setup code**, as above.
2. **Administrator**: a user name, a password and an authenticator app. The password needs at least 12 characters, must
   not be one of the 10,000 most common passwords and must not contain the user name. Scan the QR code, or type the
   secret, and confirm with a code. The secret is shown only once. Passkeys are not offered, because browsers refuse
   them on an IP address with an untrusted certificate.
3. **Node settings**: a fully qualified host name such as `ilm.example.com`, an IPv4 address (a static address with
   gateway, or the address from DHCP; reserve it in the DHCP server), DNS servers, NTP servers and an optional proxy.
4. **Review**, then apply. The node installs and reboots. Reload the page after about five minutes, at the static
   address if you chose one.

**Part 2** runs in the console after you sign in with the user name, password and authenticator code:

1. The **ILM host name**, which defaults to the node's host name.
2. The **certificate**. The appliance issues it from a built-in certificate authority. Download its root certificate
   and import it where clients should trust ILM. You can upload your own certificate later.
3. The **components** to install with ILM.
4. **Mail**, which you can skip.
5. **Review**, then watch the progress. The page refreshes every five seconds.

You can change the answers of part 2 until the first time ILM reports available. Once the manager runs, the console's
`/setup/` address goes to the sign-in page.

When ILM is ready, the last page shows its address, `https://<ILM host name>/`, and hands out the ILM administrator
credential once: a PKCS#12 client certificate protected by a password you choose (same rules as a console password).
The appliance does not keep the password and cannot hand the file out a second time for the same certificate. The
console itself uses no client certificate.

## Console

The console is at `https://<host name or node address>:8443/`. Sign in with your user name, password and the
6-digit authenticator code (the code of the previous and next 30 seconds is also accepted, for clock drift). Five
failed sign-ins for a user from one address within 15 minutes block that user at that address for 15 minutes, and 20
from one address block the address; sign-in from other addresses stays open. After five failed sign-ins for a user
from any address, an address that has failed for that user waits 2 seconds after the user's last attempt.
A session ends after 30 minutes without activity or after 8 hours, and when the manager restarts.

The console shows the state of the appliance on its start page, the settings below, a system page and the audit log.
The audit log lists every change with the user, the time, the action and the settings changed, never their values. It
is kept as `audit.log` in `/var/mnt/data/appliance` on the node, rotated at 10 MB, 10 files kept.

When a change needs a restart, the start page and the system page say so. Restart or shut down the node from the system
page.

## Settings

| Page | Setting | What it does | Reboot needed |
|---|---|---|---|
| Node | Gateway | Default route of the node (static address only) | No |
| Node | DNS servers | Resolvers of the node | No |
| Node | NTP servers | Time servers of the node | No |
| Node | HTTP and HTTPS proxy, hosts that bypass it | Proxy for the node's image pulls and for ILM | Yes, for the node; the console offers the restart |
| Certificate | Upload, signing request, built-in | Certificate that the console and ILM serve; both switch within about a minute | No |
| Trusted CAs | Add or remove CA certificates | CAs that ILM and the node trust, for example a private image registry | Yes, for the node |
| Registries | Host, user name, password | Credentials for private image registries | No |
| Components | ILM host name, components | The public name of ILM and the connectors installed with it | No |
| Mail | Server, port, security, from, credentials | Mail server that ILM sends notifications through; a button sends a test mail | No |
| System | Restart, shut down | Restarts or shuts down the node after confirmation | n/a |

The certificate must cover the host names of the node and of ILM. Add the node's IP address to it if you want to reach
the console by address without a warning. A signing request creates a key on the appliance and downloads the request;
you upload the signed certificate later. Going back to the built-in certificate is one button.

## Limitations

- The node's host name and address are read-only after setup. To change the public name, change the ILM host name and
  the certificate. To rename the machine or move it to another address, reinstall it.
- Only the ILM version in the image is offered.
- Connectors that implement only the v1 connector interface, such as email-notification-provider 1.3.0 and
  x509-compliance-provider 1.3.1, run but do not register with ILM.
- Passkeys are not offered; the administrator uses a password and an authenticator app. There is one role.
- The utils service and the CBOM repository cannot be configured.

## Build

Prerequisites: Docker with buildx, Go, `qemu-img`, `zstd`, `jq`, `crane` and `talosctl` v1.14.5, the Talos version in
`release.yaml`. A local registry holds the intermediate images:

```bash
docker run -d --name appliance-registry -p 5005:5000 registry:2
make images ARCH=arm64        # or ARCH=amd64; REGISTRY defaults to localhost:5005
```

The ISO, the qcow2 disk image (50 GB) and `SHA256SUMS` are written to `out/<arch>/`.

## Boot test

`make boot-test` boots a built image in QEMU with no route out, answers the setup wizard and waits until the appliance
is ready:

```bash
make boot-test MEDIA=iso ARCH=arm64     # MEDIA=iso|qcow2, ARCH=arm64|amd64
```

The test gives the VM a random setup code as a preset (see First boot), answers wizard part 1 and part 2 with one
connector, and passes when `GET /status` reports `ready` within 25 minutes, the ILM UI answers 200, the console is
healthy and reports the setup complete, the connector is registered with ILM, and the serial log holds no key
material, setup code, password or TOTP seed. It prints `PASS <seconds to ready>` or `FAIL <reason>`. The VM gets 4 CPUs,
12 GB of memory and a 50 GB disk; its state, including `serial.log`, is in `out/boot/<arch>-<media>/`.

By default `test/boot/drive` answers the wizard through the API. With `E2E=1` the Playwright tests in `test/e2e` answer
it in Chromium instead and then change settings in the console (DNS servers, a trusted CA, a component, the audit log):

```bash
(cd test/e2e && npm ci && npx playwright install --with-deps chromium)
E2E=1 make boot-test MEDIA=iso ARCH=amd64
```

On macOS, `npx playwright install chromium` is enough: `--with-deps` installs the system libraries Chromium needs on
Linux.

Hosts: macOS on Apple silicon for arm64 (QEMU from Homebrew, HVF), Linux with KVM for the host's architecture
(`qemu-system-x86` and `ovmf` for amd64, `qemu-system-arm` and `qemu-efi-aarch64` for arm64). The VM forwards
`127.0.0.1:18443` to the console, `127.0.0.1:18444` to the status endpoint, `127.0.0.1:10443` to the UI and
`127.0.0.1:15000` to the Talos API. To run a VM by hand, use `test/boot/vm.sh start|stop|status`.

## CI

`.github/workflows/ci.yaml` runs on every pull request and every push to `main`, with a read-only token: the Go
tests with coverage and the SonarCloud scan, the check that generated files are committed, golangci-lint, the
console's lint, tests, build and licence checks, the type check and licence check of the browser test in `test/e2e`,
and a server-side dry run of the add-on manifests on a kind cluster.

`.github/workflows/build.yaml` builds the images on every pull request and every push to `main`, for amd64
(`ubuntu-latest`) and arm64 (`ubuntu-24.04-arm`), and boot-tests the amd64 ISO with KVM and `E2E=1`; for that it
installs only the locked `test/e2e` dependencies and Chromium. The arm64 runner has no KVM, so arm64 is boot-tested
locally on macOS. The build itself is the reusable workflow `.github/workflows/images.yaml`, called by two jobs:
`pr` runs with a read-only token against a `registry:2` container inside the job and never logs in to GHCR, and
`publish` (pushes to `main` and manual runs on `main`) has `packages: write`, pushes the intermediate images to
`ghcr.io/omnitrustilm/appliance-ng` and keeps the ISO, qcow2 and `SHA256SUMS` as artifacts for three days. The build
needs at least 40 GB of free disk after the runner clean-up and fails with a clear message otherwise.

## License

Licensed under the Apache License 2.0. See [LICENSE](LICENSE). The OmniTrust and ILM logos in the console are not
covered by that licence; see [web/src/assets/brand/TRADEMARKS.md](web/src/assets/brand/TRADEMARKS.md).
