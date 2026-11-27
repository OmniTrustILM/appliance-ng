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
   Open https://<address>:8443/appliance/setup/
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
`/appliance/setup/` address goes to the sign-in page.

When ILM is ready, the last page shows its address, `https://<ILM host name>/`, and hands out the ILM administrator
credential once: a PKCS#12 client certificate protected by a password you choose (same rules as a console password).
The appliance does not keep the password and cannot hand the file out a second time for the same certificate. The
console itself uses no client certificate.

## Console

The console is at `https://<host name or node address>:8443/appliance/`; other addresses on port 8443 lead there.
Sign in with your user name, password and the 6-digit authenticator code (the code of the previous and next 30 seconds
is also accepted, for clock drift). Five
failed sign-ins for a user from one address within 15 minutes block that user at that address for 15 minutes; sign-in
from other addresses stays open. 20 sign-in attempts from one address within 15 minutes, successful or not, block the
address for 15 minutes. After five failed sign-ins for a user from any address, every sign-in for that user waits 2
seconds after the user's last attempt. An IPv6 address counts with the other addresses of its /64.
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
| Components | CBOM repository, its store on the appliance or an external S3 bucket | Where ILM keeps CBOMs; turned off, the CBOMs stored on the appliance are kept and still backed up | No |
| Mail | Server, port, security, from, credentials | Mail server that ILM sends notifications through; a button sends a test mail | No |
| System | Restart, shut down | Restarts or shuts down the node after confirmation | n/a |

The certificate must cover the host names of the node and of ILM. Add the node's IP address to it if you want to reach
the console by address without a warning. A signing request creates a key on the appliance and downloads the request;
you upload the signed certificate later. Going back to the built-in certificate is one button.

## Disk

The disk needs at least 50 GB. The node keeps up to 24 GiB for its own use and 8 GiB for the image cache; the data
volume takes the rest and holds the database, the CBOM store, the local backups and the audit log. Size the disk for
your data: a disk enlarged later grows the data volume at the next boot.

If the machine has a second empty disk of at least 20 GB at installation, the data volume goes there instead and the
system disk keeps only the node. USB disks are never chosen; with several, the largest is used. Keep that disk
attached: the node does not start the data volume without it.

The dashboard shows the data volume's size and free space and warns when less than a fifth is free.

## Recovery kit

The recovery kit is one file, encrypted with a passphrase you choose when you issue it on the Recovery kit page. It
holds the private key the backups are encrypted to, a break-glass kubeconfig with the `cluster-admin` role, a
talosconfig with the `os:admin` role valid for one year, and the appliance's identity. The appliance keeps only the
public key, so neither it nor anyone else can open a backup without the kit. Store the kit and its passphrase apart.

Backups start once you confirm that the kit is stored; until then sign-in asks for it. The dashboard warns 30 days
before the kit's talosconfig expires.

- **Issue again** with the current kit and its passphrase: same backup key, a new passphrase and new break-glass
  credentials. The earlier kubeconfig stops working; the earlier talosconfig stays valid until it expires.
- **Replace** a lost kit: a new backup key and new break-glass credentials. Backups made before can only be opened
  with the lost kit, so take a new backup right away.

## Backups

A backup is one file, `<host>-<UTC time>-<appliance version>.ilmbackup`, encrypted to the kit's backup key. It holds
the appliance's settings and secrets, the console accounts, the ILM secrets, a `pg_dump` of the ILM database and the
objects of the CBOM store on the appliance. It does not hold the node's own keys, the message broker's queues or the
audit log.

The Backups page backs up now, lists the last three backups kept on the data volume and downloads them. A backup is
refused while less than twice the size of the last one is free. Targets receive a copy on their own schedule:

| Target | Settings |
|---|---|
| S3 | Endpoint, bucket, prefix, region, access key, secret key, optional CA |
| SFTP | Host, port, path, user, password or private key; the page shows the server's host key to accept |
| NFS | Server, export, path, version (4.1 by default); the export must be writable by uid and gid 65532 |

Each target has a schedule (cron, `0 2 * * *` by default, in a time zone you choose) and a retention (14 backups by
default; only this appliance's backups are deleted). A Test button writes, reads back and deletes a small file. The
proxy applies to S3 and SFTP. The dashboard shows each target's last copy and error, and warns when a target had no
copy for twice its interval or when the last restore test is more than 90 days old.

An external database is dumped when the appliance reaches it; otherwise the backup holds the settings only and says
so. An external S3 store for CBOMs is yours to back up.

## Restore

**Onto a fresh appliance**, which also tests your backups: choose "Restore from a backup" in wizard part 1, answer it
as for a new appliance and, after the reboot, sign in. The restore page takes the backup, uploaded or fetched from a
target you enter there, and the recovery kit with its passphrase. It verifies the whole backup and shows what it
holds. The backup's ILM version must be the one in the image.

- **Rename:** the ILM host name defaults to the backup's; enter another to restore ILM under a new name. The built-in
  CA issues the certificate for the new names; an uploaded certificate is kept, and the console warns if it does not
  cover them.
- **Services:** keep the backup's database, messaging and identity provider, or choose managed or external ones; the
  database dump loads into either. The dump replaces the content of an external database, which the summary names: to
  test a backup while the appliance it comes from still runs, choose another database, and another broker or virtual
  host.
- **Accounts:** the backup's console accounts are added and the administrator of part 1 is kept; an account of the
  same name is the backup's.

When the restore is done, issue the recovery kit again from the backup's kit, as the console asks. The restore counts
as the last restore test. A restore that stopped, for example after a manager restart, is resumed with the kit;
backups stay refused until a restore is done.

**In place:** the Backups page restores a local backup of the running ILM version onto this appliance. Everything
since the backup is lost, including CBOMs stored since. The settings, secrets, ILM data and console accounts become
the backup's, except the node settings, the recovery kit, the administrator who starts the restore, and the database,
messaging and identity provider this appliance uses with their credentials. The ILM host name stays this appliance's.

## Cluster access

The Cluster access page issues a kubeconfig for the cluster's API server on port 6443 of the node, with the `view`
or `cluster-admin` role, valid for 1 to 24 hours or long-lived with a name for automation, and revokes it. It also
issues a talosconfig for the Talos API on port 50000 with the `os:reader` or `os:admin` role, valid for 1 to 8 hours;
a talosconfig cannot be revoked and is listed until it expires. The appliance keeps neither file. Every issue and
revocation is in the audit log.

## External services

Wizard part 2 and the Components page choose, for each of these, the managed one installed with ILM or an external
one. A test button checks an external one before you save. Switching between managed and external after setup moves
data: take a backup, set up a fresh appliance and choose the new service on the restore page. A restore loads the
backup's database dump into an external database and replaces its content.

- **Database:** PostgreSQL host, port, database, user and password. The TLS mode is `disable`, `require` or
  `verify-full` with the CA of the server's certificate; left unset, the drivers use TLS when the server offers it
  without verifying it. ILM's core and scheduler apply the mode; ILM's auth service and Keycloak encrypt when the
  server offers TLS but never verify its certificate, so make the server accept only TLS.
- **Messaging:** RabbitMQ host, port, virtual host, user and password. With TLS, ILM connects with AMQPS to the
  broker's TLS port, given as the port, and verifies the broker against the CA.
- **Identity:** an OpenID Connect provider with its issuer, client ID, client secret, scopes and optional audiences;
  no Keycloak is installed. Without audiences ILM accepts every token the issuer signs, whatever client it was issued
  for. Signing in to ILM with the administrator certificate keeps working whatever the provider does.

The manager writes the provider and the CBOM repository's address into ILM's settings with its own ILM administrator
certificate, registered once by the manager and kept in the Secret `appliance-ilm-identity`; the dashboard reports the
outcome as `ILMSettingsApplied`. If the manager's user is deleted in ILM, delete that Secret and the manager registers
again.

The same pages install ILM's utils service or leave it out.

## CBOM repository

The CBOM repository keeps the CBOMs ILM stores, either in an S3 store on the data volume or in an external S3 bucket
(endpoint, bucket, region, access key, secret key, optional CA). ILM reaches it inside the cluster at
`http://cbom-repository-service.ilm.svc:8080`; it is not published, because it has no authentication of its own.
Turned off, the repository is removed while the store on the appliance keeps running with its CBOMs.

## Updates

The Updates page lists the releases the appliance can install. **Check for updates** asks the online repository,
`ghcr.io/omnitrustilm/appliance-ng`; a failed check names the reason. Without internet access, upload a bundle file
(see Offline update file under Releases). The manager verifies the signature, the architecture and the compatibility
with what runs, and loads nothing that fails. Loaded bundles are listed with the reason when one cannot be installed
yet, for example because an earlier release must be installed first. An upload, a load or an update is refused while
the data volume has too little free space (twice the bundle's size or more), and while a backup, a restore or another
update runs.

**Platform update** changes the ILM version. It takes a backup to every target first, pins it and then installs the
bundle's ILM operator and switches ILM to the new version.

**Roll back** undoes the last platform update. It needs the pinned backup taken before the update, the platform bundle
that ran before, and the recovery kit with its passphrase; it restores the backup, so data changed since is lost. It is
refused when the update installed a newer ILM operator (an operator is never downgraded), when the platform uses an
external database, when the update failed before it asked for the new ILM version, and when the backup or the previous
bundle is gone. The refusal names the reason; the way back is then to restore the backup onto a fresh appliance.

**System update** changes the manager, Talos, Kubernetes and the add-ons. Expect about 10 minutes of downtime: the
manager restarts, then the node reboots. The Updates page waits for the appliance and asks to sign in again once it is
back; ILM needs a few minutes more. After the reboot a health check runs, and if the node is not healthy, Talos is
rolled back automatically. The passed health check is the point of no return: Kubernetes is upgraded next and never
downgraded, and the only way back is to restore the backup onto a fresh appliance of the previous release.

- If the new manager never starts, power-cycle the node. The previous manager comes back and fails the update after
  15 minutes.
- If an update installed Talos but could not reboot the node, the update fails and says so: the next reboot starts the
  new Talos without the health check.
- A system update that failed past the point of no return, at the Kubernetes or add-ons step, can be installed again
  from the Updates page: it goes on from the step it failed at.
- If the node boots the previous Talos after the health check passed, through a manual rollback or a firmware
  fallback, the previous manager runs no step and keeps the add-ons held, also once the update ended and after later
  platform updates, so that it never applies its add-ons on the upgraded Kubernetes. A power cycle into the new Talos lets the update
  finish or the appliance run again; otherwise contact support.
- A system update moves Kubernetes by at most one minor version; a bundle that skips one is refused.
- After a system update the node keeps the image cache of the release it was installed with. Images it needs before
  the registry is up come from the node's container store.
- A finished system update replaces the record of an earlier platform update, which can then no longer be rolled
  back.

## Limitations

- The node's host name and address are read-only after setup. To change the public name, change the ILM host name and
  the certificate. To rename the machine or move it to another address, reinstall it.
- Updates are installed one at a time, from a signed bundle; there is no downgrade.
- Connectors that implement only the v1 connector interface, such as email-notification-provider 1.3.0 and
  x509-compliance-provider 1.3.1, run but do not register with ILM. The Components page marks them as not registered
  and shows their URL; register them in ILM with that URL. Other connectors register on their own and count as ready
  once ILM records the registration, also while it waits for approval.
- Passkeys are not offered; the administrator uses a password and an authenticator app. There is one role.
- The CBOM repository is reachable only by ILM, so ILM's platform settings page shows it as not reachable from the
  browser.

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
healthy and reports the setup complete, the manager applied its ILM settings, the connector is registered with ILM,
the recovery kit opens and works, a backup reaches an SFTP and an S3 server the test runs on the host, cluster access
works, and the serial log holds no key material, setup code, password or TOTP seed. It prints
`PASS <seconds to ready>` or `FAIL <reason>`. The VM gets 4 CPUs, 12 GB of memory and a 50 GB disk; its state,
including `serial.log`, is in `out/boot/<arch>-<media>/`.

With `RESTORE=1` the first VM also creates an ILM group and stores a CBOM in the CBOM repository before the backup. A
second VM then restores that backup under another ILM host name and the test checks, with the first VM's ILM
administrator certificate, that the group and the CBOM are back and that Keycloak accepts the restored client. It
prints `PASS <seconds> restored <seconds>`.

With `SYSTEM_UPDATE=1` the VM boots the qcow2 of a previous release, installs the system bundle of the checked-out
code from the console and checks that the appliance runs the new versions with its data intact. It then power-cycles
the node and checks that it comes back by itself:

```bash
SYSTEM_UPDATE=1 FROM=test make boot-test MEDIA=qcow2 ARCH=arm64
```

`FROM=test` starts from a build of the current commit stamped `0.0.0-test`; use `FROM=<tag>` once a tag exists. The
test needs `BUNDLE_TEST_KEY` (the public key that both builds trust) and the system bundle made with
`KIND=system test/boot/bundle.sh`; see the comments in `test/boot/run.sh` and `test/boot/previous.sh`. If the host
ports are taken, set `PORT_OFFSET` to move all of them.

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

## Releases

Tags are `X.Y.Z`, without a `v`. The maintainer sets `version` in `release.yaml`, commits
`release-notes/X.Y.Z.md` and cuts the tag by hand from `main`. The tag runs `.github/workflows/release.yaml`. It
publishes the images, runs the long tests it can and, once they pass, pushes the system and platform bundles of each
architecture to `ghcr.io/omnitrustilm/appliance-ng/{system,platform}:X.Y.Z-<arch>`, signs them, attaches an SPDX SBOM
of each as an attestation and creates a draft GitHub release with the notes, the SBOMs and `cosign.pub`. The maintainer
publishes the draft. The system bundle carries the installer and manager images the run built. Signatures
are made with the key behind `cosign.pub` and are not written to a public transparency log. A manual run with
`dry-run` pushes the bundles under `ghcr.io/omnitrustilm/appliance-ng/dev/` as `<commit>-<arch>`, signs them with a
throw-away key pair made in the run, keeps its public key as the `dry-run-cosign-pub-<arch>` artifact and drafts no
release.

The long VM tests (`restore`, `bundle-load`, `platform-update`, `system-update`) are in
`.github/workflows/long-tests.yaml`. They run on pull requests labelled `long-tests` and on manual dispatch. The tag
run does not run `bundle-load`, `platform-update` or `system-update`, because they need a test signing key: run them
through the label before tagging.

`make bundles ARCH=amd64` builds the unsigned bundles into `out/<arch>/bundles/`; with `PUSH=1 REGISTRY=<registry>`
it also pushes them with oras.

`.github/workflows/platform-pin.yaml` runs daily and on demand. When the ILM operator has a newer release, it pins
that release, its ILM version and the platform image digests from its bill of materials, and opens a pull request.
`pin` reads the bill of materials from the operator module, so by hand the module moves first:

```bash
go get github.com/OmniTrustILM/operator@v<version> && go mod tidy
go run ./cmd/release pin --operator <version> [--ilm <version>]
```

Connector catalogue changes stay manual. The pull request is opened with the workflow token, which starts no CI:
close and reopen it to run the checks.

### VM images

Releases carry no ISO or qcow2. Check out the release tag and build them as described in Build:

```bash
git checkout X.Y.Z
make images ARCH=amd64
```

### Offline update file

On a machine with internet access and oras 1.4.0 or newer, this writes a bundle with its signature and SBOM into one
file for an appliance without internet access:

```bash
oras copy -r ghcr.io/omnitrustilm/appliance-ng/system:X.Y.Z-amd64 --to-oci-layout system-X.Y.Z-amd64:X.Y.Z-amd64 \
  && tar -C system-X.Y.Z-amd64 -cf system-X.Y.Z-amd64.tar oci-layout index.json blobs
```

The platform bundle is the same with `platform` in place of `system`.

### Verifying a bundle

The signatures are in no transparency log, so cosign checks them against `cosign.pub` alone:

```bash
cosign verify --key cosign.pub --insecure-ignore-tlog=true ghcr.io/omnitrustilm/appliance-ng/system:X.Y.Z-amd64
cosign verify-attestation --key cosign.pub --insecure-ignore-tlog=true --type spdxjson \
  ghcr.io/omnitrustilm/appliance-ng/system:X.Y.Z-amd64
```

An offline file verifies without network access, with the key the Appliance Manager embeds:

```bash
go run ./cmd/bundle verify --arch amd64 system-X.Y.Z-amd64.tar
```

## License

Licensed under the Apache License 2.0. See [LICENSE](LICENSE). The OmniTrust and ILM logos in the console are not
covered by that licence; see [web/src/assets/brand/TRADEMARKS.md](web/src/assets/brand/TRADEMARKS.md).
