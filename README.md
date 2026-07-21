# matrisms

A bidirectional Matrix ↔ SMS/MMS bridge for [VoIP.ms](https://voip.ms), built on [mautrix bridgev2](https://docs.mau.fi/bridges/general/index.html). Sibling project of [matrimail](https://github.com/Leicas/matrimail) (Matrix ↔ Email).

Text from Matrix using your own phone number(s). Each conversation gets its own room; incoming SMS and MMS (pictures, audio, video) appear the moment they arrive.

## Features

- **Two-way SMS**: send and receive plain texts; long messages are split automatically
- **MMS media**: inbound pictures/audio/video are re-uploaded to your Matrix media repo; outbound images are sent as MMS (up to ~1.3 MB, JPG/PNG/GIF)
- **Multiple numbers**: bridge any or all SMS-enabled DIDs on your account; each (your number, contact) pair gets its own room
- **Instant delivery** (optional): a small webhook listener for the VoIP.ms *SMS URL Callback* so messages arrive instantly instead of on the next poll
- **Encrypted credential storage**: your API password is AES-256-GCM encrypted at rest (PBKDF2 key from a passphrase you control)
- **Poll-based with self-healing**: polling (default 30 s) is the source of truth; webhook is just a doorbell, so nothing is lost if the callback misfires

## How it works

VoIP.ms has no push API, so the bridge polls `getMMS` (with `all_messages=1`, covering SMS and MMS) per bridged number and dedupes by message ID. Outbound messages go through `sendSMS` / `sendMMS`; the API echo of your own send is deduplicated automatically. Timestamps are handled in the API's fixed UTC-5 convention.

## Setup

### 1. VoIP.ms side

1. **Main Menu → SOAP & REST/JSON API**:
   - set an **API password** (separate from your portal password)
   - **Enable** the API
   - add your server's IP to **Enable IP Addresses** (or `0.0.0.0` to allow all — less secure)
2. Make sure SMS is enabled on the DID(s) you want to bridge (**DID Numbers → Manage DIDs → edit DID → SMS/MMS**).

> Note: VoIP.ms caps **API** sends at 100 messages/day by default (portal sends are unlimited). Contact VoIP.ms support to raise it. SMS/MMS works with US/Canada numbers only.

### 2. Bridge

```bash
# build (needs Go 1.25+; use -tags goolm to avoid the libolm C dependency)
go build -tags goolm -o matrisms ./cmd/matrisms

# generate the example config, edit homeserver settings
./matrisms -e -c config.yaml
$EDITOR config.yaml

# generate the appservice registration and install it in your homeserver
./matrisms -g -c config.yaml -r registration.yaml

# run
./matrisms -c config.yaml
```

Or with Docker:

```bash
docker compose up -d
```

### 3. Login from Matrix

DM the bridge bot (`@matrismsbot:yourserver`) and send:

```
login
```

Enter your VoIP.ms account email and API password, pick which numbers to bridge, done. Incoming texts create rooms as they arrive; to text someone new:

```
text 5551234567 hey there!
```

### Optional: instant delivery webhook

1. In `config.yaml`, set `network.webhook.enabled: true` and a `secret`.
2. Expose the listener (default port `29332`) to the internet (reverse proxy recommended).
3. In the VoIP.ms portal, set the DID's **SMS URL Callback** to:

```
https://your.host/sms?token=<secret>&to={TO}&from={FROM}&id={ID}
```

The callback just triggers an immediate poll, so delivery stays correct even if VoIP.ms retries or the callback is misconfigured. The listener answers `ok` as required by the *URL callback retry* option.

## Bot commands

| Command | Description |
|---|---|
| `login` / `logout` | Connect / remove a VoIP.ms account |
| `text <number> [message...]` | Open a room for a number (optionally send right away); use `text from:<your-num> <number> ...` with multiple DIDs |
| `list` | List bridged numbers |
| `status` | Connection / polling status |
| `ping` | Liveness check |
| `passphrase` | Where the DB encryption passphrase lives (admin) |

## Configuration reference

See the generated `config.yaml` — the `network:` block covers:

- `voipms.poll_interval_seconds` (default 30)
- `voipms.startup_backfill_hours` (default 24, `-1` to disable)
- `voipms.max_upload_bytes` — inbound MMS media cap (default 25 MiB)
- `webhook.*` — instant-delivery listener
- `logging.sanitized` — redact phone numbers/bodies from logs

Credentials are **never** in the config file: they're stored AES-256-GCM-encrypted in the bridge database. The key derives from `MATRISMS_PASSPHRASE` (env) or `./data/passphrase` (auto-generated). Keep that passphrase with your backups.

## License

AGPL-3.0 — see [LICENSE](LICENSE).
