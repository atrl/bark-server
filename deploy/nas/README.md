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
   `data`, `secrets`, and `cloudflare` directories owned by `65532:65532`.
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
