# Bark

<img src="https://wx3.sinaimg.cn/mw690/0060lm7Tly1g0nfnjjxbbj30sg0sg757.jpg" width=200px height=200px />

[Bark](https://github.com/Finb/Bark) is an iOS App which allows you to push customed notifications to your iPhone.

## Installation

### For Docker User

![Docker Automated build](https://img.shields.io/docker/automated/finab/bark-server.svg) ![Image Size](https://img.shields.io/docker/image-size/finab/bark-server?sort=date) ![License](https://img.shields.io/github/license/finb/bark-server)

The docker image is already available, you can use the following command to run the bark server:

``` sh
docker run -dt --name bark -p 8080:8080 -v `pwd`/bark-data:/data finab/bark-server
```

You can also use the GitHub Container Registry image:

``` sh
docker run -dt --name bark -p 8080:8080 -v `pwd`/bark-data:/data ghcr.io/finb/bark-server
```

If you use the docker-compose tool, you can copy docker-copose.yaml under this project to any directory and run it:

``` sh
mkdir bark-server && cd bark-server
curl -sL https://github.com/Finb/bark-server/raw/master/deploy/docker-compose.yaml > docker-compose.yaml
docker compose up -d
```

### For General User 

- 1、Download precompiled binaries from the [releases](https://github.com/Finb/bark-server/releases) page
- 2、Add executable permissions to the bark-server binary: `chmod +x bark-server`
- 3、Start bark-server: `./bark-server --addr 0.0.0.0:8080 --data ./bark-data`
- 4、Test the server: `curl localhost:8080/ping`

**Note: Bark-server uses the `/data` directory to store data by default. Make sure that bark-server has permission to write to the `/data` directory, otherwise use the `-d` option to change the directory.**

### For Developer

Developers can compile this project by themselves, and the dependencies required for compilation:

- Golang 1.18+
- Go Mod Enabled(env `GO111MODULE=on`)
- Go Mod Proxy Enabled(env `GOPROXY=https://goproxy.cn`)
- [go-task](https://taskfile.dev/installation/) Installed

Run the following command to compile this project:

```sh
# Cross compile all platforms
task

# Compile the specified platform (please refer to Taskfile.yaml)
task linux_amd64
task linux_amd64_v3
```

**Note: The linux amd64 v3 architecture was added in go 1.18, see [https://github.com/golang/go/wiki/MinimumRequirements#amd64](https://github.com/golang/go/wiki/MinimumRequirements#amd64)**

### Use MySQL instead of Bbolt

Just run the server with `-dsn=user:pass@tcp(mysql_host)/bark`, it will use MySQL instead of file database Bbolt

## Others

* [API_V2.md](docs/API_V2.md).
* [MCP.md](docs/MCP.md).

## Android FCM and reliable delivery

This fork keeps the existing Bark push API and adds optional Firebase Cloud
Messaging HTTP v1 delivery. Build this fork's binary/image; upstream prebuilt Bark
images do not contain these Android endpoints. Google Play publication is not
required, but the phone needs working Google Play services and access to FCM.
The NAS needs HTTPS access to `oauth2.googleapis.com` and `fcm.googleapis.com`.

Configure these environment variables on the server:

```sh
BARK_FCM_PROJECT_ID=your-firebase-project-id
GOOGLE_APPLICATION_CREDENTIALS=/run/secrets/firebase-service-account.json
BARK_PUBLIC_URL=https://bark.example.com
```

`BARK_PUBLIC_URL` is the canonical external HTTPS URL, including any server URL
prefix, without a trailing slash, credentials, query, or fragment. The Firebase
service account JSON is a **server secret**: mount it read-only, restrict its file
permissions, and never package it in the APK, container image, or Git repository.
Enable the Firebase Cloud Messaging API and grant the account permission to send
messages in that project. Android's `google-services.json` is a separate client
configuration file, not the server private key. See Firebase's
[HTTP v1 setup](https://firebase.google.com/docs/cloud-messaging/send/v1-api) and
[Android setup](https://firebase.google.com/docs/cloud-messaging/android/get-started).

All three variables are required for FCM. With incomplete configuration, FCM
transport registration returns HTTP 503; reliable polling remains available.
Invalid complete configuration fails startup rather than claiming FCM is ready.
There is no test push or outbound send during configuration or transport
registration unless an existing pending outbox message is ready for delivery.
The `serverless` memory-only mode does not enable FCM or durable storage. For NAS
operation, keep `/data` on a persistent local volume and run one server instance
against it. Do not share the JSON outbox directory between multiple writers.

### Device protocol

First use the existing `/register` endpoint with an installation token
`android:<random-installation-secret>`. The Bark device key is a send address;
it is not sufficient to read messages or change the receiving installation.
All endpoints below require `X-Bark-Device-Token` with the **full** installation
token. When server Basic Auth is enabled, that authentication is also required.

| Request | JSON body / response `data` |
| --- | --- |
| `POST /android/transport/:device_key` | Body: `{"provider":"fcm","token":"FCM-registration-token","notification_mode":"notification"}`. Response: `{"provider":"fcm","server_url":"https://bark.example.com"}`. |
| `POST /android/transport/:device_key` | Body: `{"provider":"poll"}` clears the FCM binding and selects reliable polling. |
| `GET /android/sync/:device_key?timeout=30&limit=50` | `{"messages":[{"delivery_id":"UUID","payload":{},"created_at_millis":0,"fcm_accepted":false,"notification_tag":"bark:..."}],"more":false}`. Timeout is 0–60 seconds; limit is 1–100. An empty result is HTTP 200 with an empty messages array. |
| `POST /android/ack/:device_key` | Body: `{"delivery_ids":["UUID"]}`. Idempotent HTTP 200, including already acknowledged or unknown IDs. At most 100 IDs per request. |

Responses retain the existing `CommonResp` envelope (`code`, `message`, `data`,
`timestamp`). Persist each received delivery in the client inbox before ACK;
identify retries with `delivery_id`, which is independent from Bark's optional
business `id`. Updates and deletions may share a business ID while remaining
separate deliveries. ACKs can remove messages only from the authenticated device.

`notification_tag` is an opaque, stable notification identity generated from the
canonical server URL, device key, and business ID (or delivery ID if absent), using
SHA-256. Both the sync response and the FCM data include this value; FCM also
includes `bark_delivery_id`, `bark_server_url`, and the compatible
`bark_notification_tag`. Use the supplied tag with Android notification ID `0`
for local rendering, replacement, and deletion. FCM uses the `bark_default`
notification channel, which the Android app must create before registration.

Re-registering an existing Android key with the same installation token is safe
to retry. Replacing its token or unregistering with `device_token=deleted` requires
the old `X-Bark-Device-Token`. Unregistration clears pending deliveries, unbinds
FCM, deletes the device mapping, and prevents reuse of the revoked key. Changing
from Android to a different platform requires a new key. None of these receive
credentials or the Bark device key are included in the FCM data payload or request
logs; logs contain route templates rather than URLs or request bodies.

### Persistence, retries, and display limits

Pending messages and transport bindings are saved under `/data/android-delivery`
using synced temporary files and atomic replacement. Old payload-only queues are
migrated on their next mutation. A successful push means **stored by Bark**, and
`fcm_accepted=true` means **accepted by Google**, not displayed on the phone.
Sync does not delete a message. Only authenticated ACK, authorized device deletion,
or the explicitly legacy destructive poll API can remove it.

The default limits are 1,024 pending messages and 4 MiB of encoded outbox per
device, with a 32 KiB JSON payload limit per message. Configure
`BARK_ANDROID_QUEUE_LIMIT` (1–100,000), `BARK_ANDROID_QUEUE_MAX_BYTES`
(65,536–16,777,216), and `BARK_ANDROID_MESSAGE_MAX_BYTES` (1,024–262,144) to change
these limits; the per-message limit cannot exceed the queue byte limit. An
oversized message is rejected with HTTP 413 before persistence. A new delivery
that would exceed the count or byte capacity is rejected with HTTP 503 while all
earlier messages remain. Delivery/ACK metadata may add a small amount above the
append byte limit. Files larger than 32 MiB are refused for reading and retained
for explicit repair, never truncated. The file store loads one device queue at a
time, so retain the conservative byte limit on a NAS with a 256 MiB container
budget; large numbers of concurrently syncing devices still need separate load
measurement. There is no automatic expiry of unacknowledged messages. Transient FCM failures are retried
with exponential delay (1 minute up to 64 minutes), honoring a longer
`Retry-After`. Unregistered FCM tokens are cleared; messages remain available for
sync. Registering a replacement token unblocks pending deliveries. Provider
acceptance stops FCM retransmission while the outbox still awaits client ACK.
A sync read defers competing FCM delivery once for two minutes, allowing client
persistence and ACK; repeated reads do not extend this lease. A lost sync response
therefore leaves the delivery recoverable and eventually eligible for FCM again.
As with other networked queues, a lost provider response can cause a retry after
Google accepted it. Stable notification tags and client delivery IDs provide
deduplication; this is not an exactly-once display guarantee.

Ordinary messages use notification plus minimal data so Android/Google Play
services can show them without a Bark process. Encrypted messages expose only a
generic “收到加密消息” notification to FCM; ciphertext, IV, and fallback body/title
stay in the authenticated outbox. The client can request
`notification_mode="data"` to enforce local decryption or active group-mute
settings. Delete, call, autocopy, custom sound, action, volume, and explicit TTL
messages also use data-only delivery because their semantics require app code.
Data-only delivery and local rendering remain subject to Android background
execution restrictions. A force-stopped app must be opened manually to resume
receiving. Notification permission, device connectivity, expired Google messages,
and notification dismissal can all separate provider acceptance from actual
visibility; clients should use local display/click evidence for notification
deduplication rather than treating acceptance as proof of display. See Firebase's
[receive behavior](https://firebase.google.com/docs/cloud-messaging/android/receive-messages).

`GET /android/poll/:device_key` remains available only for old registrations that
have not used the new transport/sync protocol. Its historical consume-on-read
semantics cannot protect against a lost response. After a successful transport
registration or first sync, it returns HTTP 409 and cannot bypass ACK. Upgrade
clients to `/sync` even when Firebase is not configured. The default server write
timeout is 75 seconds so a 60-second long poll can complete; reverse proxies should
allow at least this duration and avoid caching these authenticated endpoints.
