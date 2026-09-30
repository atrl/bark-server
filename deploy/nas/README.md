# NAS deployment

This deployment runs the Android-enabled Bark server and its own Cloudflare
Tunnel on a Synology x86-64 NAS. The API binds only to `127.0.0.1:18083`;
the public entry point is the HTTPS hostname in the tunnel configuration.
Device keys authorize sending. Android transport registration, sync, and ACK
additionally require the installation token. Do not put an interactive
Cloudflare Access login in front of mobile/API endpoints.

## Build and install

1. Run the Go tests and commit the verified source. Run `bash deploy/nas/build.sh`.
2. Transfer the resulting `dist/nas` build context to the NAS over SSH. Build on
   the NAS with `docker build --build-arg REVISION=<commit> -t bark-server:<commit> .`.
3. Place `compose.yaml` and a private `.env` in `/volume1/docker/bark`. Create
   `data`, `secrets`, `cloudflare`, `apk-releases`, and `release-tools` directories
   owned by `65532:65532`. Copy both release Python scripts into `release-tools`
   before starting the `release-sync` service.
   Keep private directories `0700`, secret files `0600`, and `.env` `0600`.
4. Create a dedicated named tunnel using `cloudflared tunnel create bark-nas`.
   Transfer its credential JSON directly to `cloudflare/credentials.json`;
   never add it to Git or logs. Fill `cloudflare/config.yml` from the example.
5. Validate the ingress file, then add the hostname with
   `cloudflared tunnel route dns bark-nas bark.example.com`.
   Do not overwrite an existing DNS record without checking its owner/use.
6. Set `BARK_IMAGE` and `BARK_PUBLIC_URL` in `.env`, then run
   `docker compose up -d`. Check container health, local `/ping`, public `/ping`,
   and public `/info` (its revision must equal the deployed commit).

Host networking lets the server use an existing NAS loopback outbound proxy
when necessary. The API still binds only to loopback. There are no router port
forwards, Docker socket mounts, privileged containers, or shared application
data volumes.

## Enable Firebase

Provision the Android app and FCM service account in the same Firebase project.
The Android `google-services.json` is build configuration. The service account
private key belongs only on the server, at `secrets/firebase-service-account.json`.
Configure:

```dotenv
BARK_FCM_PROJECT_ID=your-project-id
BARK_FCM_CREDENTIALS_PATH=/run/secrets/firebase-service-account.json
```

Then recreate the server with `docker compose up -d server`. Leave both settings
empty to operate without Firebase; this does not prove background FCM delivery.
Verify FCM and OAuth HTTPS connectivity from the NAS. If the NAS requires its
existing proxy, set `BARK_HTTPS_PROXY` and verify certificate validation remains
enabled. Cloudflare Tunnel exposes the inbound API; it does not carry the
phone's outbound connection to Google Play services.

An optional `google-egress` Compose profile provides a dedicated loopback proxy
at port 17890. Provision its private `egress/config.yaml` from an authorized,
working proxy configuration. Limit routes to `fcm.googleapis.com`,
`oauth2.googleapis.com`, and `accounts.google.com`, reject other destinations,
and keep TLS certificate verification enabled for both the proxy node and
Google HTTPS. Keep the directory owned by `65532:65532` with mode `0700` and its
configuration `0600`; it contains credentials and must never enter Git. Start
with `docker compose --profile google-egress up -d` and set
`BARK_HTTPS_PROXY=http://127.0.0.1:17890`. Recheck its health if credentials expire.
The existing NAS-wide proxy configuration is independent of this container.

Synology kernels can lack CPU quota and PIDs controllers. This file uses CPU
shares rather than hard CPU quotas; PID limits may be ignored on those kernels.

Install the APK built with the matching Firebase app configuration, register
this server in Bark, grant notification permission, and verify the app reports
FCM registration success. Test background delivery, offline recovery, and
duplicate handling. Keep provider acceptance separate from confirmed device
receipt and visible notification delivery.

## Updates and rollback

Keep the last verified image and take a protected backup of `data` before a
schema-changing release. Change only `BARK_IMAGE` and recreate `server`; the
dedicated tunnel and unrelated NAS applications need no restart. A code rollback
may also require restoring the matching data snapshot: never let an older server
silently reinterpret a newer delivery queue. Preserve the current snapshot
before restoring. Do not delete pending messages during rollback.

APK distribution and an actual device delivery test remain separate from a
healthy NAS container or HTTP 200 response.


## Android APK releases and automatic mirroring

The app checks the fixed public endpoint
`https://bark.atrl.me/android/releases/stable.json`. This endpoint returns a native
schema 1 JSON manifest, without the Bark `CommonResp` wrapper. It names the
package `day.bark.android`, version code/name, actual minimum SDK, NAS APK URL,
SHA-256, byte size, pinned signing-certificate SHA-256, release notes, and
publication timestamp. These release endpoints are public even if push APIs use
Basic Auth. A missing or invalid release returns HTTP 404 and does not affect
notification delivery. Stable manifests use `Cache-Control: no-store`; validated
immutable APKs support HEAD, ETag, and single byte-range downloads with long-lived
immutable caching. Directory listing, sidecar JSON downloads, arbitrary files,
path traversal, and symbolic links are rejected.

Prepare releases on the build machine or in the Android CI job, where Android SDK
`aapt` and `apksigner` and a working Java runtime are available. The script verifies
the actual APK package, version, minimum SDK, and current signer; it then calculates
its complete SHA-256 and byte size. The public original signing-certificate pin is
`60408b509647dc7689bca6435a3eef93d475d868232fe88d848e853c03ffe896`.
It never reads a signing keystore or Firebase service-account secret.

```sh
python3 deploy/nas/prepare-android-release.py prepare \
  --apk /path/to/signed-release.apk \
  --output-dir /path/to/new-release-bundle \
  --release-notes-file /path/to/release-notes.txt
```

Use `--aapt` and `--apksigner` if the SDK tools are not on PATH or under
`ANDROID_HOME`. On macOS with a Homebrew JDK, set `JAVA_HOME` to that JDK before
running the command. The output bundle contains exactly the signed APK,
`<apk-filename>.json` as an immutable version sidecar, and `stable.json`.
The sidecar lets already-announced APK URLs remain downloadable after a newer
stable release. Maximum APK size is 256 MiB; manifests are limited to 16 KiB and
release notes to 4,096 UTF-8 bytes.

The Android GitHub Actions release job should check out the verified server
revision containing this script, run `prepare` against its signed build, then
publish the generated bundle as release assets. Do not hand-edit hashes,
certificates, version metadata, or artifact byte sizes. The GitHub tag must be
`v<version_name>`. Production NAS releases use the **GitHub-produced artifact**;
a locally built APK with the same version code can have different bytes and must
not be published first. Keep local verification bundles in isolated temporary
directories.

The optional manual activation step works without an Android SDK on the NAS:

```sh
python3 /volume1/docker/bark/release-tools/prepare-android-release.py publish \
  --bundle-dir /path/to/transferred-bundle \
  --release-dir /volume1/docker/bark/apk-releases
```

Copy complete bundles over SSH before activating them. The publisher checks the
fixed package, certificate metadata, safe NAS URL, full APK SHA-256/size, sidecar
identity, and version order. Under a publication lock it makes the fully written
APK durable, publishes the immutable sidecar, then atomically switches
`stable.json` last. Interrupted publication leaves the previous stable manifest
active. It rejects lower version codes, an existing version code with different
bytes or metadata, and overwriting an immutable filename. Fix a bad app build by
publishing a higher version code. If an existing immutable artifact becomes
corrupt, publication fails explicitly; inspect and quarantine that damaged file
before retrying rather than silently overwriting release history.

Compose mounts `./apk-releases` read-only into the API server at `/releases` and
sets `BARK_ANDROID_RELEASES_DIR=/releases`. The separate `release-sync` container
runs Python 3.12 every 30 minutes and reads only the public stable release at
`https://api.github.com/repos/atrl/bark-android/releases/latest`. It mounts only
`./release-tools` (read-only) and `./apk-releases` (read-write), with no notification
data, signing keys, or Firebase secret mounts. It uses the dedicated outbound proxy
in `BARK_RELEASE_HTTPS_PROXY`, separately from the server's FCM proxy setting.
Allow only `api.github.com`, `github.com`, `objects.githubusercontent.com`,
`release-assets.githubusercontent.com`, and `github-releases.githubusercontent.com`
for release downloads, plus the existing Google FCM/OAuth hosts, on that dedicated
proxy. This is not a general-purpose outbound proxy; other destinations remain
rejected and certificate validation stays enabled.

The mirror downloads into `/releases/.sync-*` on NAS disk, not memory-backed
`/tmp`. It validates every redirect destination, bounded download length, release
tag, package/certificate metadata, and complete APK hash/size via the shared
publisher before activation. It also verifies an already-current artifact instead
of trusting file existence. It does **not** parse APK signatures on the NAS:
actual signature and package checks happen in CI preparation and again on the
phone before installation. A network, validation, or publication failure keeps
the last valid release and is retried on the next interval.

```sh
# Install/update these code files on the NAS before recreating release-sync:
#   prepare-android-release.py
#   sync-android-release.py
# Then trigger one bounded sync if needed:
docker compose run --rm release-sync python \
  /opt/bark-release/sync-android-release.py --release-dir /releases --once
```

The updater's download availability is separate from Android's permission to
install a package. First installation/update-source authorization and any system
confirmation remain controlled by Android.
