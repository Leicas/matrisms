# matrisms — developer notes

Matrix ↔ SMS/MMS bridge for VoIP.ms on mautrix **bridgev2**. Sibling of
[matrimail](https://github.com/Leicas/matrimail); the connector skeleton,
credential encryption, reliability helpers, and CI pipeline are shared DNA.

## Layout

- `cmd/matrisms/main.go` — mxmain bootstrap; the connector is the only network-specific object
- `pkg/connector/` — bridgev2 glue:
  - `connector.go` — `SMSConnector` (NetworkConnector), Init/Start/Stop, chat/user info, client registry
  - `client.go` — `SMSClient` (NetworkAPI, one per login), capabilities, poller wiring
  - `client_send.go` — Matrix → SMS/MMS outbound (sendSMS split at 160 chars, sendMMS for media)
  - `remote_event.go` — `SMSMatrixEvent` (RemoteMessage), MMS media download → Matrix upload
  - `login.go` — credentials → DID selection → complete
  - `database.go` — `sms_accounts` + `sms_poll_cursors` tables; AES-256-GCM at-rest credential encryption (PBKDF2 from `MATRISMS_PASSPHRASE` / `./data/passphrase`)
  - `webhook.go` — VoIP.ms SMS URL Callback listener (doorbell only; triggers a poll, never injects payloads)
  - `config.go` + `example-config.yaml` — every new config key MUST be added to `upgradeConfig` or it gets wiped on config upgrade
- `pkg/voipms/` — self-contained REST client + poller (no mautrix imports)
- `pkg/common/ids.go` — canonical ID schemes (see below)
- `pkg/coordinator`, `pkg/reliability` — bridge-state aggregation, retry/backoff/circuit-breaker (copied from matrimail)

## ID conventions (load-bearing)

- Portal: `sms:<did>:<peer>` (normalized 11-digit), Receiver = UserLoginID
- Ghost: `sms:<peer-number>`
- Message: `sms:<voipms-id>` / `mms:<voipms-id>`
- UserLogin: `voipms:<account-email>`

## VoIP.ms API gotchas (verified against the official docs + michaelkourlas/voipms-sms-client)

- Single endpoint `https://voip.ms/api/v1/rest.php` (NEVER `www.` — redirect drops POST bodies)
- `getMMS` + `all_messages=1` returns SMS **and** MMS under the `sms` JSON key
- Date filters are day-granular, max 92-day range; we always pass `timezone=-5` and parse timestamps as fixed UTC-5
- Empty results come back as `status:"no_sms"`/`no_mms` (not an error); `invalid_did` is per-DID and non-fatal
- Message bodies are URL-encoded (`+` = space)
- `getMMS` often omits `col_media*` — `getMediaMMS(id, media_as_array=1)` is the reliable media source
- sendSMS caps at 160 chars (`sms_toolong`); sendMMS: 2048 chars text + 3 media ≤ ~1.3 MB each
- API sends are capped at 100 msgs/day by default (`limit_reached`); per-minute throttle exists (`api_limit_exceeded`)
- Dedup relies on bridgev2 ignoring remote messages whose network MessageID already exists — outbound sends store their VoIP.ms id at send time, so the poll echo no-ops

## Build & test

```bash
go test ./pkg/...                        # unit tests (no network)
go build -tags goolm ./cmd/matrisms      # pure-Go olm, no libolm needed
# Docker/CI builds use libolm (see Dockerfile), matching matrimail
```

On this Windows dev box: `PATH="/c/msys64/mingw64/bin:$PATH" CGO_ENABLED=1 go build -tags goolm ...` (go-sqlite3 needs CGO).
