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
- DID space portal: `did:<did>` (Receiver empty — bridgev2 parent portals are global); every conversation portal sets it as ChatInfo.ParentID
- Ghost: `sms:<peer-number>`
- Message: `sms:<voipms-id>` / `mms:<voipms-id>` (separate id spaces per method)
- UserLogin: `voipms:<account-email>`

## Sent/received attribution

Outbound echoes are `EventSender{IsFromMe: true}`. Without double puppeting
(README section) they render as the own-DID ghost, which GetUserInfo names
`Me (+1 …)`. Parts 2..n of a split SMS have no bridgev2 message row — their
poll echo is suppressed via SMSClient.markSentEcho/isSentEcho.

## VoIP.ms API gotchas (verified against the official docs + michaelkourlas/voipms-sms-client)

- Single endpoint `https://voip.ms/api/v1/rest.php` (NEVER `www.` — redirect drops POST bodies)
- We poll `getSMS` + `getMMS` separately (both return rows under the `sms` JSON key). Do NOT go back to `getMMS all_messages=1`: it only reveals MMS through `col_media*`, which the API frequently omits, so image-only MMS get misclassified/dropped. A cross-batch dedup drops any getSMS row mirroring an MMS row.
- Date filters are day-granular, max 92-day range; we always pass `timezone=-5` and parse timestamps as fixed UTC-5
- Empty results come back as `status:"no_sms"`/`no_mms`/`no_phonebook` (not an error); `invalid_did` is per-DID and non-fatal
- Message bodies are URL-encoded (`+` = space)
- `getMMS` often omits `col_media*` — `getMediaMMS(id, media_as_array=1)` is the reliable media source
- Phonebook: `getPhonebook`/`addPhonebook`/`setPhonebook`, entries under `phonebooks` with fields `phonebook` (id), `name`, `number`, `speed_dial`, `callerid`, `note`. Used for contact naming + `!matrisms rename`
- sendSMS caps at 160 chars (`sms_toolong`); sendMMS: 2048 chars text + 3 media ≤ ~1.3 MB each
- API sends are capped at 100 msgs/day by default (`limit_reached`); per-minute throttle exists (`api_limit_exceeded`)
- Dedup relies on bridgev2 ignoring remote messages whose network MessageID already exists — outbound sends store their VoIP.ms id at send time, so the poll echo no-ops
- VoIP.ms reassembles inbound multipart (long/UCS-2) SMS **server-side, in segment-arrival order, not UDH sequence order** — a long message can arrive as a single getSMS row with its ~67-char segments concatenated wrong (verified 2026-07-26: four copies of the same Leo notification, IDs 108872494/108875072/108877206/108878566, three scrambled in different orders, one correct). `SortMessages` only governs ordering *between* rows; within-row scrambles are repaired heuristically by `voipms.RepairScrambledBody` (block-permutation search over 67-unit UCS-2 / 153-char GSM-7 segments, gated on hard structural evidence — see `unscramble.go`; config `unscramble_segments`)

## Build & test

```bash
go test ./pkg/...                        # unit tests (no network)
go build -tags goolm ./cmd/matrisms      # pure-Go olm, no libolm needed
# Docker/CI builds use libolm (see Dockerfile), matching matrimail
```

On this Windows dev box: `PATH="/c/msys64/mingw64/bin:$PATH" CGO_ENABLED=1 go build -tags goolm ...` (go-sqlite3 needs CGO).
